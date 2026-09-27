package prometheus

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Captured on a TS-X73A running QuTS hero h5.2.9 (2026-09-27).
const fixtureZpoolStatus = `  pool: zpool1
 state: ONLINE
status: Some supported features are not enabled on the pool. The pool can
    still be used, but some features are unavailable.
  scan: scrub repaired 0 in 2 days 00:36:53 with 0 errors on Thu Sep  3 02:36:56 2026
 prune: last pruned 1166760 entries, 132089760 entries are pruned ever
        total pruning count #176, avg. pruning rate = 6115507 (entry/sec)
expand: none requested
 ztier: [reloc] none requested
        [smart] none requested
config:

    NAME                                        STATE     READ WRITE CKSUM
    zpool1                                      ONLINE       0     0     0
      raidz1-0                                  ONLINE       0     0     0
        qzfs/enc_0/disk_0x4_5000C500E6B4AE7B_3  ONLINE       0     0     0
        qzfs/enc_0/disk_0x5_5000C500E684B7CF_3  ONLINE       0     0     0
        qzfs/enc_0/disk_0x6_5000C500E6B2A98B_3  ONLINE       0     0     0
    logs
      mirror-1                                  ONLINE       0     0     0
        qzfs/enc_0/disk_0x1_23115U800872_3      ONLINE       0     0     0
        qzfs/enc_0/disk_0x2_23115U800918_3      ONLINE       0     0     0

errors: No known data errors

  pool: zpool256
 state: ONLINE
status: Some supported features are not enabled on the pool. The pool can
    still be used, but some features are unavailable.
  scan: none requested
config:

    NAME                                        STATE     READ WRITE CKSUM
    zpool256                                    ONLINE       0     0     0
      mirror-0                                  ONLINE       0     0     0
        qzfs/enc_0/disk_0x1_23115U800872_2      ONLINE       0     0     0
        qzfs/enc_0/disk_0x2_23115U800918_2      ONLINE       0     0     0

errors: No known data errors
`

const fixtureZpoolStatusDegraded = `  pool: tank
 state: DEGRADED
status: One or more devices has experienced an unrecoverable error.
  scan: scrub in progress since Sun Sep 27 10:00:00 2026
        1.20T scanned at 1.00G/s, 800G issued at 600M/s, 2.00T total
        0B repaired, 40.00% done, 00:30:00 to go
config:

    NAME        STATE     READ WRITE CKSUM
    tank        DEGRADED     0     0     0
      raidz1-0  DEGRADED     0     0     0
        sda     ONLINE       0     0     0
        sdb     DEGRADED     0     0     3
        sdc     ONLINE       1     0     0

errors: No known data errors
`

func TestParseZpoolList(t *testing.T) {
	pools, err := parseZpoolList("zpool1\t29686813949952\t14293651652608\t15393162297344\t5\tONLINE\nzpool256\t253403070464\t12345\t253390725119\t-\tONLINE\n")
	require.NoError(t, err)
	require.Len(t, pools, 2)
	assert.Equal(t, "zpool1", pools[0].name)
	assert.Equal(t, 29686813949952.0, pools[0].size)
	assert.Equal(t, 14293651652608.0, pools[0].alloc)
	assert.Equal(t, 15393162297344.0, pools[0].free)
	assert.InDelta(t, 0.05, pools[0].frag, 1e-9)
	assert.Equal(t, "ONLINE", pools[0].health)
	assert.Equal(t, -1.0, pools[1].frag, "unknown fragmentation is negative")

	_, err = parseZpoolList("zpool1\tnotanumber\t1\t1\t0\tONLINE")
	assert.Error(t, err)
}

func TestParseZpoolStatusFixture(t *testing.T) {
	loc := time.FixedZone("CEST", 2*3600)
	st := parseZpoolStatus(fixtureZpoolStatus, loc)
	require.Len(t, st, 2)

	p := st["zpool1"]
	assert.Equal(t, "ONLINE", p.health)
	assert.False(t, p.scanInProgress)
	assert.True(t, p.hasLastScrub)
	assert.Equal(t, time.Date(2026, 9, 3, 2, 36, 56, 0, loc), p.lastScrub)
	assert.Equal(t, 0.0, p.lastScrubErrs)
	assert.Equal(t, 0.0, p.readErrors+p.writeErrors+p.cksumErrors)

	p2 := st["zpool256"]
	assert.Equal(t, "ONLINE", p2.health)
	assert.False(t, p2.hasLastScrub, "scan: none requested has no last scrub")
	assert.False(t, p2.scanInProgress)
}

func TestParseZpoolStatusDegraded(t *testing.T) {
	st := parseZpoolStatus(fixtureZpoolStatusDegraded, time.UTC)
	p, ok := st["tank"]
	require.True(t, ok)
	assert.Equal(t, "DEGRADED", p.health)
	assert.True(t, p.scanInProgress)
	assert.Equal(t, "scrub", p.scanKind)
	assert.False(t, p.hasLastScrub)
	assert.Equal(t, 1.0, p.readErrors, "leaf READ errors summed")
	assert.Equal(t, 0.0, p.writeErrors)
	assert.Equal(t, 3.0, p.cksumErrors, "leaf CKSUM errors summed, grouping vdev line not double counted")
}

func TestZpoolMetrics(t *testing.T) {
	st := parseZpoolStatus(fixtureZpoolStatus, time.UTC)
	p := st["zpool1"]
	p.size, p.alloc, p.free, p.frag = 100, 40, 60, 0.05
	m := zpoolMetrics(p)
	names := map[string]metric{}
	for _, x := range m {
		names[x.name+"{"+x.attr+"}"] = x
	}
	assert.Equal(t, 1.0, names[`node_zfs_pool_health{pool="zpool1",state="ONLINE"}`].value)
	assert.Equal(t, 0.0, names[`node_zfs_pool_scan_in_progress{pool="zpool1",scan="none"}`].value)
	assert.Equal(t, 0.0, names[`node_zfs_pool_errors_total{pool="zpool1",kind="checksum"}`].value)
	assert.Contains(t, names, `node_zfs_pool_last_scrub_timestamp_seconds{pool="zpool1"}`)

	p2 := st["zpool256"]
	m2 := zpoolMetrics(p2)
	for _, x := range m2 {
		assert.NotEqual(t, "node_zfs_pool_last_scrub_timestamp_seconds", x.name, "no scrub → no timestamp series")
	}
}

func TestParseZfsList(t *testing.T) {
	out := "zpool1\t14293651652608\t15393162297344\t1024\t0\t/zpool1\n" +
		"zpool1/zfs3\t13000000000000\t15393162297344\t12999999000000\t0\t/share/shared\n" +
		"zpool1/zfs1\t1000\t107374182400\t1000\t107374182400\t/share/Public\n" +
		"zpool256/zfs0\t1\t2\t3\t0\tnone\n" +
		"zpool256/zfs1\t1\t2\t3\t0\tlegacy\n"
	ds, err := parseZfsList(out)
	require.NoError(t, err)
	require.Len(t, ds, 3, "none/legacy mountpoints are skipped")
	shared := ds[1]
	assert.Equal(t, "zpool1/zfs3", shared.name)
	assert.Equal(t, "shared", datasetVolume(shared))
	assert.Equal(t, "zpool1", datasetPool(shared.name))

	m := zfsDatasetMetrics(shared)
	byName := map[string]float64{}
	for _, x := range m {
		byName[x.name] = x.value
	}
	assert.Equal(t, 15393162297344.0, byName["node_volume_avail_bytes"])
	assert.Equal(t, 13000000000000.0+15393162297344.0, byName["node_volume_size_bytes"])
	assert.Contains(t, m[0].attr, `filesystem="zfs"`)
	assert.Contains(t, m[0].attr, `volume="shared"`)

	pub := zfsDatasetMetrics(ds[2])
	assert.Equal(t, 107374182400.0, pub[0].value, "quota caps size when smaller than used+avail")
}

func TestParseZfsCount(t *testing.T) {
	assert.Equal(t, 0.0, parseZfsCount("0"))
	assert.Equal(t, 1200.0, parseZfsCount("1.2K"))
	assert.Equal(t, 3.0, parseZfsCount("3"))
}
