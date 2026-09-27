package prometheus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Captured on a TS-X73A running QuTS hero h5.2.9, kernel 6.6.32 (2026-09-27).
const fixtureNfsdRPC = `rc 0 0 21646995
fh 94 0 0 0 0
io 1092043631689 79449854601
th 64 0 0.000 0.000 0.000 0.000 0.000 0.000 0.000 0.000 0.000 0.000
ra 0 0 0 0 0 0 0 0 0 0 0 0
net 21646973 0 21646994 21
rpc 21646603 0 0 0 0
proc2 18 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
proc3 23 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
proc4 2 6 21646989
proc4ops 76 0 0 0 1769392 600861 370325 24013 0 547971 9501698 1276317 0 15890 40 15852 1073346 0 0 620061 0 0 13278 21685317 0 9 8277171 480934 3208 165617 40117 0 0 40117 0 176720 0 0 0 2120259 0 0 0 9 9 5 1667 0 0 0 0 0 0 3 21646961 0 1 0 2 6 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
wdeleg_getattr 0
`

// Real pool_stats from the same NAS: on kernel 6.x sockets-enqueued ≈ threads-woken on the normal
// path; only the difference (54 here) is requests that waited for a thread.
const fixturePoolStats = `# pool packets-arrived sockets-enqueued threads-woken threads-timedout
0 22050308 41214046 41213992 0
`

func TestParseNfsdRPCFixture(t *testing.T) {
	s := parseNfsdRPC(fixtureNfsdRPC)
	assert.Equal(t, 64.0, s.threads)
	assert.Equal(t, 1092043631689.0, s.ioRead)
	assert.Equal(t, 79449854601.0, s.ioWrite)
	assert.Equal(t, 21646603.0, s.rpcCalls)
	assert.Equal(t, 0.0, s.rpcBadFmt+s.rpcBadAuth+s.rpcBadClnt)
	assert.Equal(t, 21646995.0, s.rcNocache)
	assert.Equal(t, 0.0, s.proc3Calls)
	require.Len(t, s.proc4Ops, 76)
	assert.Equal(t, 9501698.0, s.proc4Ops[9], "GETATTR")
	assert.Equal(t, 21685317.0, s.proc4Ops[22], "PUTFH")
	assert.Equal(t, 480934.0, s.proc4Ops[26], "READDIR")
	assert.Equal(t, 21646961.0, s.proc4Ops[53], "SEQUENCE")
}

func TestNfsdMetricsFixture(t *testing.T) {
	m := nfsdMetrics(parseNfsdRPC(fixtureNfsdRPC), "64\n", parseNfsdPoolStats(fixturePoolStats))
	byKey := map[string]float64{}
	for _, x := range m {
		byKey[x.name+"{"+x.attr+"}"] = x.value
	}
	assert.Equal(t, 64.0, byKey["node_nfsd_threads{}"])
	assert.Equal(t, 1092043631689.0, byKey[`node_nfsd_io_bytes_total{direction="read"}`])
	assert.Equal(t, 21646603.0, byKey["node_nfsd_rpc_calls_total{}"])
	assert.Equal(t, 9501698.0, byKey[`node_nfsd_requests_total{proto="nfsv4",op="getattr"}`])
	assert.Equal(t, 480934.0, byKey[`node_nfsd_requests_total{proto="nfsv4",op="readdir"}`])
	assert.Equal(t, 41214046.0, byKey["node_nfsd_sockets_enqueued_total{}"])
	assert.Equal(t, 41213992.0, byKey["node_nfsd_threads_woken_total{}"])
	assert.Equal(t, 22050308.0, byKey["node_nfsd_packets_arrived_total{}"])
	_, hasZero := byKey[`node_nfsd_requests_total{proto="nfsv4",op="delegpurge"}`]
	assert.False(t, hasZero, "zero-count ops are not emitted")
	_, hasV3 := byKey[`node_nfsd_requests_total{proto="nfsv3",op="all"}`]
	assert.False(t, hasV3, "proc3 all zero → no nfsv3 series")
}

func TestParseNfsdPoolStatsHeaderOrder(t *testing.T) {
	ps := parseNfsdPoolStats("# pool sockets-enqueued packets-arrived threads-woken threads-timedout\n0 5 100 90 1\n1 7 200 190 2\n")
	assert.True(t, ps.parsed)
	assert.Equal(t, 300.0, ps.packetsArrived, "columns are matched by header name and summed over pools")
	assert.Equal(t, 12.0, ps.socketsEnqueued)
	assert.Equal(t, 3.0, ps.threadsTimedout)
	assert.False(t, parseNfsdPoolStats("").parsed)
}

func TestNfsdUnknownOpSlot(t *testing.T) {
	s := parseNfsdRPC("proc4ops 80 " + repeatZeros(79) + " 7\n")
	m := nfsdMetrics(s, "", nfsdPoolStats{})
	found := false
	for _, x := range m {
		if x.name == "node_nfsd_requests_total" && x.attr == `proto="nfsv4",op="op79"` {
			found = true
			assert.Equal(t, 7.0, x.value)
		}
	}
	assert.True(t, found, "slots beyond the known table are exposed as op<N>")
}

func repeatZeros(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += " "
		}
		out += "0"
	}
	return out
}
