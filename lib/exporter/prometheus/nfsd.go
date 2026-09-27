package prometheus

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	nfsdRPCPath       = "/proc/net/rpc/nfsd"
	nfsdThreadsPath   = "/proc/fs/nfsd/threads"
	nfsdPoolStatsPath = "/proc/fs/nfsd/pool_stats"
)

// nfsv4Ops maps NFSv4 operation numbers (RFC 7530/5661/7862/8276) to names; the index is the
// position in the proc4ops line. Slots 0-2 are unused.
var nfsv4Ops = []string{
	"", "", "",
	"access", "close", "commit", "create", "delegpurge", "delegreturn", "getattr", "getfh",
	"link", "lock", "lockt", "locku", "lookup", "lookupp", "nverify", "open", "openattr",
	"open_confirm", "open_downgrade", "putfh", "putpubfh", "putrootfh", "read", "readdir",
	"readlink", "remove", "rename", "renew", "restorefh", "savefh", "secinfo", "setattr",
	"setclientid", "setclientid_confirm", "verify", "write", "release_lockowner",
	"backchannel_ctl", "bind_conn_to_session", "exchange_id", "create_session", "destroy_session",
	"free_stateid", "get_dir_delegation", "getdeviceinfo", "getdevicelist", "layoutcommit",
	"layoutget", "layoutreturn", "secinfo_no_name", "sequence", "set_ssv", "test_stateid",
	"want_delegation", "destroy_clientid", "reclaim_complete", "allocate", "copy", "copy_notify",
	"deallocate", "io_advise", "layouterror", "layoutstats", "offload_cancel", "offload_status",
	"read_plus", "seek", "write_same", "clone", "getxattr", "setxattr", "listxattrs", "removexattr",
}

// nfsdStats is the parsed content of /proc/net/rpc/nfsd.
type nfsdStats struct {
	rcHits, rcMisses, rcNocache        float64
	ioRead, ioWrite                    float64
	threads                            float64
	rpcCalls, rpcBadCalls              float64
	rpcBadFmt, rpcBadAuth, rpcBadClnt  float64
	proc3Calls                         float64
	proc4Ops                           []float64 // by op number
	hasRPC, hasIO, hasThreads, hasProc float64
}

// nfsdPoolStats is the sum over pools of /proc/fs/nfsd/pool_stats columns.
type nfsdPoolStats struct {
	packetsArrived, socketsEnqueued, threadsWoken, threadsTimedout float64
	parsed                                                         bool
}

func parseNfsdRPC(content string) nfsdStats {
	var s nfsdStats
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		vals := make([]float64, 0, len(f)-1)
		for _, v := range f[1:] {
			x, _ := strconv.ParseFloat(v, 64)
			vals = append(vals, x)
		}
		switch f[0] {
		case "rc":
			if len(vals) >= 3 {
				s.rcHits, s.rcMisses, s.rcNocache = vals[0], vals[1], vals[2]
			}
		case "io":
			if len(vals) >= 2 {
				s.ioRead, s.ioWrite, s.hasIO = vals[0], vals[1], 1
			}
		case "th":
			s.threads, s.hasThreads = vals[0], 1
		case "rpc":
			if len(vals) >= 5 {
				s.rpcCalls, s.rpcBadCalls, s.rpcBadFmt, s.rpcBadAuth, s.rpcBadClnt, s.hasRPC = vals[0], vals[1], vals[2], vals[3], vals[4], 1
			}
		case "proc3":
			for _, v := range vals[1:] {
				s.proc3Calls += v
			}
		case "proc4ops":
			if len(vals) >= 1 {
				s.proc4Ops, s.hasProc = vals[1:], 1
			}
		}
	}
	return s
}

// parseNfsdPoolStats parses /proc/fs/nfsd/pool_stats by header name, summing all pools.
func parseNfsdPoolStats(content string) nfsdPoolStats {
	var ps nfsdPoolStats
	var cols []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			cols = strings.Fields(strings.TrimPrefix(trimmed, "#"))
			continue
		}
		f := strings.Fields(trimmed)
		for i, name := range cols {
			if i >= len(f) {
				break
			}
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				continue
			}
			switch name {
			case "packets-arrived":
				ps.packetsArrived += v
			case "sockets-enqueued":
				ps.socketsEnqueued += v
			case "threads-woken":
				ps.threadsWoken += v
			case "threads-timedout":
				ps.threadsTimedout += v
			}
		}
		ps.parsed = true
	}
	return ps
}

func nfsdMetrics(s nfsdStats, threadsFile string, ps nfsdPoolStats) []metric {
	metrics := make([]metric, 0, 40)
	metrics = append(metrics,
		metric{name: "node_nfsd_reply_cache_total", attr: `result="hits"`, value: s.rcHits, metricType: "counter", help: "NFS server reply cache hits"},
		metric{name: "node_nfsd_reply_cache_total", attr: `result="misses"`, value: s.rcMisses, metricType: "counter", help: "NFS server reply cache misses"},
		metric{name: "node_nfsd_reply_cache_total", attr: `result="nocache"`, value: s.rcNocache, metricType: "counter", help: "NFS server requests not cacheable"},
	)
	if s.hasIO > 0 {
		metrics = append(metrics,
			metric{name: "node_nfsd_io_bytes_total", attr: `direction="read"`, value: s.ioRead, metricType: "counter", help: "Bytes read by the NFS server"},
			metric{name: "node_nfsd_io_bytes_total", attr: `direction="write"`, value: s.ioWrite, metricType: "counter", help: "Bytes written by the NFS server"},
		)
	}
	threads := s.threads
	if t, err := strconv.ParseFloat(strings.TrimSpace(threadsFile), 64); err == nil && t > 0 {
		threads = t
	}
	if threads > 0 {
		metrics = append(metrics, metric{name: "node_nfsd_threads", value: threads, metricType: "gauge", help: "Number of nfsd threads"})
	}
	if s.hasRPC > 0 {
		metrics = append(metrics,
			metric{name: "node_nfsd_rpc_calls_total", value: s.rpcCalls, metricType: "counter", help: "RPC calls received by the NFS server"},
			metric{name: "node_nfsd_rpc_bad_total", attr: `reason="badfmt"`, value: s.rpcBadFmt, metricType: "counter", help: "Malformed RPC calls"},
			metric{name: "node_nfsd_rpc_bad_total", attr: `reason="badauth"`, value: s.rpcBadAuth, metricType: "counter", help: "RPC calls with bad authentication"},
			metric{name: "node_nfsd_rpc_bad_total", attr: `reason="badclnt"`, value: s.rpcBadClnt, metricType: "counter", help: "RPC calls from bad clients"},
		)
	}
	if s.proc3Calls > 0 {
		metrics = append(metrics, metric{name: "node_nfsd_requests_total", attr: `proto="nfsv3",op="all"`, value: s.proc3Calls, metricType: "counter", help: "NFS requests by protocol and operation"})
	}
	for op, v := range s.proc4Ops {
		if v == 0 {
			continue
		}
		name := fmt.Sprintf("op%d", op)
		if op < len(nfsv4Ops) && nfsv4Ops[op] != "" {
			name = nfsv4Ops[op]
		}
		metrics = append(metrics, metric{name: "node_nfsd_requests_total", attr: fmt.Sprintf(`proto="nfsv4",op=%q`, name), value: v, metricType: "counter",
			help: "NFS requests by protocol and operation"})
	}
	if ps.parsed {
		metrics = append(metrics,
			metric{name: "node_nfsd_packets_arrived_total", value: ps.packetsArrived, metricType: "counter", help: "Packets arrived at nfsd (all pools)"},
			metric{name: "node_nfsd_sockets_enqueued_total", value: ps.socketsEnqueued, metricType: "counter", help: "Sockets enqueued for nfsd (all pools); rate(enqueued) - rate(threads_woken) > 0 means requests waited for a thread"},
			metric{name: "node_nfsd_threads_woken_total", value: ps.threadsWoken, metricType: "counter", help: "nfsd threads woken to serve requests (all pools)"},
			metric{name: "node_nfsd_threads_timedout_total", value: ps.threadsTimedout, metricType: "counter", help: "nfsd threads that timed out idle (all pools)"},
		)
	}
	return metrics
}

// getNfsdMetrics reads the NFS server procfs files; a missing /proc/net/rpc/nfsd means the NFS
// server is not running and yields no metrics and no error.
func getNfsdMetrics() ([]metric, error) {
	rpcContent, err := os.ReadFile(nfsdRPCPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	threadsContent, _ := os.ReadFile(nfsdThreadsPath)
	poolContent, _ := os.ReadFile(nfsdPoolStatsPath)
	return nfsdMetrics(parseNfsdRPC(string(rpcContent)), string(threadsContent), parseNfsdPoolStats(string(poolContent))), nil
}
