package prometheus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Captured on a TS-X73A running QuTS hero h5.2.9 (2026-09-27): only system mirrors and swap.
const fixtureMdstat = `Personalities : [linear] [raid0] [raid1] [raid10] [raid6] [raid5] [raid4] [multipath]
md322 : active raid1 sdd5[2](S) sdc5[1] sdb5[0]
      31868416 blocks super 1.0 [2/2] [UU]
      bitmap: 0/1 pages [0KB], 65536KB chunk

md321 : active raid1 sda5[3](S) nvme1n1p5[2] nvme0n1p5[0]
      31868416 blocks super 1.0 [2/2] [UU]
      bitmap: 0/1 pages [0KB], 65536KB chunk

md13 : active raid1 sda4[133] nvme1n1p4[129] nvme0n1p4[128] sdd4[132] sdc4[131] sdb4[130]
      458880 blocks super 1.0 [128/6] [UUUUUU__________________________________________________________________________________________________________________________]
      bitmap: 1/1 pages [4KB], 65536KB chunk

md9 : active raid1 sda1[133] nvme1n1p1[129] nvme0n1p1[128] sdd1[132] sdc1[131] sdb1[130]
      530048 blocks super 1.0 [128/6] [UUUUUU__________________________________________________________________________________________________________________________]
      bitmap: 1/1 pages [4KB], 65536KB chunk

unused devices: <none>
`

func TestParseMdstatFixture(t *testing.T) {
	arrays := parseMdstat(fixtureMdstat)
	require.Len(t, arrays, 4)
	byDev := map[string]mdArray{}
	for _, a := range arrays {
		byDev[a.device] = a
	}
	assert.Equal(t, 6, byDev["md9"].active)
	assert.Equal(t, 6, byDev["md9"].total, "[128/6] with six U counts as 6 expected members")
	assert.Equal(t, "raid1", byDev["md9"].level)
	assert.Equal(t, 2, byDev["md322"].active)
	assert.Equal(t, 2, byDev["md322"].total, "spare (S) member does not count")

	m := mdMetrics(arrays)
	for _, x := range m {
		if x.name == "node_md_degraded" {
			assert.Equal(t, 0.0, x.value, x.attr)
		}
	}
}

func TestParseMdstatDegraded(t *testing.T) {
	arrays := parseMdstat("md0 : active raid1 sda1[0]\n      1000 blocks [2/1] [U_]\n\nmd1 : inactive sdb1[1](S)\n      1000 blocks super 1.2\n")
	require.Len(t, arrays, 2)
	assert.Equal(t, 1, arrays[0].active)
	assert.Equal(t, 2, arrays[0].total)
	m := mdMetrics(arrays)
	degraded := map[string]float64{}
	for _, x := range m {
		if x.name == "node_md_degraded" {
			degraded[x.attr] = x.value
		}
	}
	assert.Equal(t, 1.0, degraded[`device="md0",level="raid1"`])
	assert.Equal(t, 1.0, degraded[`device="md1",level=""`], "inactive arrays are degraded and carry no level")
}
