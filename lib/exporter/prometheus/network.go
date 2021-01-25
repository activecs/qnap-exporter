package prometheus

import (
	"fmt"
	"math"
	"path"
	"strconv"
	"time"

	"github.com/go-ping/ping"
	"github.com/showwin/speedtest-go/speedtest"
	"gitlab.com/pedropombeiro/qnapexporter/lib/utils"
)

const speedtestValidity = 1 * time.Hour

func (e *promExporter) getNetworkStatsMetrics() ([]metric, error) {
	metrics := make([]metric, 0, len(e.ifaces)*2)
	for _, iface := range e.ifaces {
		rxMetric, err := getNetworkStatMetric("node_network_receive_bytes_total", "Total number of bytes received", iface, "rx")
		if err != nil {
			return nil, err
		}
		metrics = append(metrics, rxMetric)

		txMetric, err := getNetworkStatMetric("node_network_transmit_bytes_total", "Total number of bytes transmitted", iface, "tx")
		if err != nil {
			return nil, err
		}
		metrics = append(metrics, txMetric)
	}

	return metrics, nil
}

func getNetworkStatMetric(name string, help string, iface string, direction string) (metric, error) {
	str, err := utils.ReadFile(path.Join(netDir, iface, "statistics", direction+"_bytes"))
	if err != nil {
		return metric{}, err
	}

	value, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return metric{}, err
	}

	return metric{
		name:       name,
		attr:       fmt.Sprintf(`device=%q`, iface),
		value:      value,
		help:       help,
		metricType: "counter",
	}, nil
}

func (e *promExporter) getPingMetrics() ([]metric, error) {
	pinger, err := ping.NewPinger(e.PingTarget)
	if err != nil {
		return nil, err
	}

	pinger.SetPrivileged(true)
	pinger.Timeout = 2 * time.Second
	pinger.Count = 1
	err = pinger.Run() // Blocks until finished.
	if err != nil {
		return nil, err
	}

	stats := pinger.Statistics() // get send/receive/rtt stats
	value := float64(stats.AvgRtt.Seconds()) * 1000.0
	if stats.PacketLoss > 0 {
		value = math.NaN()
	}
	m := metric{
		name:      "node_network_external_roundtrip_time_ms",
		attr:      fmt.Sprintf("target=%q", pinger.IPAddr().String()),
		value:     value,
		timestamp: time.Now(),
	}

	return []metric{m}, nil
}

func (e *promExporter) getBandwidthMetrics() ([]metric, error) {
	expired := e.speedtestLastRun.IsZero() || time.Now().After(e.speedtestLastRun.Add(speedtestValidity))
	if len(e.speedtestTargets) == 0 {
		user, err := speedtest.FetchUserInfo()
		if err != nil {
			return nil, err
		}
		serverList, err := speedtest.FetchServerList(user)
		if err != nil {
			return nil, err
		}
		serverIDs := make([]int, 0)
		if e.SpeedtestServer != 0 {
			serverIDs = append(serverIDs, e.SpeedtestServer)
		}

		e.speedtestTargets, err = serverList.FindServer(serverIDs)
		if err != nil {
			return nil, err
		}
	}

	if expired {
		e.speedtestLastRun = time.Now()
	}

	metrics := make([]metric, 0, len(e.speedtestTargets)*2)
	for _, s := range e.speedtestTargets {
		attr := fmt.Sprintf("server=%q", s.ID)

		if expired {
			err := s.DownloadTest(false)
			if err != nil {
				return metrics, err
			}
		}
		metrics = append(metrics,
			metric{
				name:      "node_network_external_download_speed_bps",
				attr:      attr,
				timestamp: e.speedtestLastRun,
				value:     s.DLSpeed * 1000 * 1000,
			},
		)

		if expired {
			err := s.UploadTest(false)
			if err != nil {
				return metrics, err
			}
		}
		metrics = append(metrics,
			metric{
				name:      "node_network_external_upload_speed_bps",
				attr:      attr,
				timestamp: e.speedtestLastRun,
				value:     s.ULSpeed * 1000 * 1000,
			},
		)
	}

	return metrics, nil
}
