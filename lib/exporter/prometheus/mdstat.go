package prometheus

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	mdstatPath = "/proc/mdstat"
	// qnapMdMapThreshold: member maps longer than this are QNAP's oversized system-mirror maps.
	qnapMdMapThreshold = 16
)

// mdArray is one md device from /proc/mdstat.
type mdArray struct {
	device string
	state  string // active, inactive
	level  string // raid1, raid5, ...
	active int    // members shown as U
	total  int    // expected members, derived from the U/_ map (see parseMdstat)
}

var (
	// "md9 : active raid1 sda1[133] ..." — inactive arrays carry no level: "md1 : inactive sdb1[1](S)"
	mdHeaderRe = regexp.MustCompile(`^(md\d+)\s*:\s*(\S+)(?:\s+\(auto-read-only\))?\s+(.*)$`)
	mdStatusRe = regexp.MustCompile(`\[(\d+)/(\d+)\]\s+\[([U_]+)\]`)
)

// parseMdstat parses /proc/mdstat. `total` is the bracketed expected member count ([2/1] → 2),
// except for QNAP's oversized system-mirror maps ([128/6] [UUUUUU__...]) where it is the position
// of the last U plus one; this keeps [2/1] [U_] degraded and [128/6] healthy. Spares (S) never count.
func parseMdstat(content string) []mdArray {
	var arrays []mdArray
	var cur *mdArray
	for _, line := range strings.Split(content, "\n") {
		if sm := mdHeaderRe.FindStringSubmatch(line); len(sm) == 4 {
			a := mdArray{device: sm[1], state: sm[2]}
			if rest := strings.Fields(sm[3]); a.state == "active" && len(rest) > 0 {
				a.level = rest[0]
			}
			arrays = append(arrays, a)
			cur = &arrays[len(arrays)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if sm := mdStatusRe.FindStringSubmatch(line); len(sm) == 4 {
			mapStr := sm[3]
			cur.active = strings.Count(mapStr, "U")
			cur.total, _ = strconv.Atoi(sm[1])
			if len(mapStr) > qnapMdMapThreshold {
				// QNAP system mirrors: [128/6] [UUUUUU___...] — 128 slots, of which only the leading
				// ones are ever populated; treat the last U as the last expected member.
				cur.total = strings.LastIndex(mapStr, "U") + 1
			}
			cur = nil
		}
	}
	return arrays
}

func mdMetrics(arrays []mdArray) []metric {
	metrics := make([]metric, 0, 3*len(arrays))
	for _, a := range arrays {
		attr := fmt.Sprintf("device=%q,level=%q", a.device, a.level)
		degraded := 0.0
		if a.state != "active" || a.active < a.total {
			degraded = 1
		}
		metrics = append(metrics,
			metric{name: "node_md_disks", attr: attr + `,state="active"`, value: float64(a.active), metricType: "gauge",
				help: "Number of active members (U) of the md array"},
			metric{name: "node_md_disks", attr: attr + `,state="total"`, value: float64(a.total), metricType: "gauge",
				help: "Expected number of members of the md array"},
			metric{name: "node_md_degraded", attr: attr, value: degraded, metricType: "gauge",
				help: "1 when the md array is inactive or has fewer active members than expected"},
		)
	}
	return metrics
}

// getMdstatMetrics reads /proc/mdstat; absent file means no md and no error.
func getMdstatMetrics() ([]metric, error) {
	content, err := os.ReadFile(mdstatPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return mdMetrics(parseMdstat(string(content))), nil
}
