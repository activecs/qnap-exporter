package prometheus

import (
	"errors"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pedropombeiro/qnapexporter/lib/utils"
)

// zfsValidity bounds how often zpool/zfs are executed; scrapes in between reuse the cache.
const zfsValidity = 30 * time.Second

// zpoolInfo is one imported pool as seen by `zpool list -Hp` merged with `zpool status`.
type zpoolInfo struct {
	name   string
	size   float64
	alloc  float64
	free   float64
	frag   float64 // 0-1, negative when unknown ("-")
	health string

	readErrors  float64
	writeErrors float64
	cksumErrors float64

	scanInProgress bool
	scanKind       string // "scrub" or "resilver" while in progress
	hasLastScrub   bool
	lastScrub      time.Time
	lastScrubErrs  float64
	statusParsed   bool
}

// zfsDataset is one line of `zfs list -Hp -o name,used,avail,refer,quota,mountpoint -t filesystem`.
type zfsDataset struct {
	name       string
	used       float64
	avail      float64
	refer      float64
	quota      float64 // 0 when unset
	mountpoint string
	volume     string // QNAP share/volume name when derivable, else last mountpoint component
}

// qutsSnapshotDir is the child dataset QuTS hero creates per volume; its mountpoint reveals the
// share name: zpool1/zfs18/RecentlySnapshot -> /share/ZFS18_DATA/shared/@Recently-Snapshot.
const (
	qutsSnapshotDataset = "RecentlySnapshot"
	qutsSnapshotDir     = "@Recently-Snapshot"
)

type zfsState struct {
	pools     []zpoolInfo
	datasets  []zfsDataset
	lastFetch time.Time

	// index of the first command variant that worked (-1 = not yet known); vendor ZFS builds
	// (QNAP QuTS hero) reject some options accepted by OpenZFS.
	zpoolListVariant int
	zfsListVariant   int
}

// zpoolListVariants are tried in order until one succeeds: parsable bytes with fragmentation,
// without fragmentation, then human-readable sizes.
var zpoolListVariants = [][]string{
	{"list", "-Hp", "-o", "name,size,alloc,free,frag,health"},
	{"list", "-Hp", "-o", "name,size,alloc,free,health"},
	{"list", "-H", "-o", "name,size,alloc,free,frag,health"},
	{"list", "-H", "-o", "name,size,alloc,free,health"},
}

// zfsListVariants: with and without quota, parsable and human-readable.
var zfsListVariants = [][]string{
	{"list", "-Hp", "-o", "name,used,avail,refer,quota,mountpoint", "-t", "filesystem"},
	{"list", "-Hp", "-o", "name,used,avail,refer,mountpoint", "-t", "filesystem"},
	{"list", "-H", "-o", "name,used,avail,refer,quota,mountpoint", "-t", "filesystem"},
	{"list", "-H", "-o", "name,used,avail,refer,mountpoint", "-t", "filesystem"},
}

var (
	zpoolPoolRe   = regexp.MustCompile(`(?m)^\s*pool:\s*(\S+)\s*$`)
	zpoolStateRe  = regexp.MustCompile(`(?m)^\s*state:\s*(\S+)`)
	zpoolScanRe   = regexp.MustCompile(`(?m)^\s*scan:\s*(.*)$`)
	zpoolScrubOK  = regexp.MustCompile(`scrub repaired \S+ in .*? with (\d+) errors on (.+)$`)
	zpoolResilvOK = regexp.MustCompile(`resilvered .* with (\d+) errors on (.+)$`)
	zpoolVdevRe   = regexp.MustCompile(`^\s*(\S+)\s+(\S+)\s+(\d[\d.]*[KMGT]?)\s+(\d[\d.]*[KMGT]?)\s+(\d[\d.]*[KMGT]?)\s*$`)
	zpoolGroupRe  = regexp.MustCompile(`^(mirror|raidz\d?|draid\d?|spare|replacing|indirect)-\d+$`)
	zpoolSectionH = map[string]bool{"logs": true, "cache": true, "spares": true, "special": true, "dedup": true}
)

// zpoolStatusDateLayout matches `Thu Sep  3 02:36:56 2026` (two spaces before a one-digit day).
const zpoolStatusDateLayout = "Mon Jan _2 15:04:05 2006"

// parseZpoolList parses `zpool list -Hp -o name,size,alloc,free,frag,health` (tab separated, no header).
func parseZpoolList(output string) ([]zpoolInfo, error) {
	pools := make([]zpoolInfo, 0, 2)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		// name size alloc free [frag] health
		if len(f) != 5 && len(f) != 6 {
			return nil, fmt.Errorf("parse zpool list line %q: expected 5 or 6 fields, got %d", line, len(f))
		}
		p := zpoolInfo{name: f[0], health: f[len(f)-1], frag: -1}
		var err error
		if p.size, err = parseZfsSize(f[1]); err != nil {
			return nil, fmt.Errorf("parse zpool %s size %q: %w", p.name, f[1], err)
		}
		if p.alloc, err = parseZfsSize(f[2]); err != nil {
			return nil, fmt.Errorf("parse zpool %s alloc %q: %w", p.name, f[2], err)
		}
		if p.free, err = parseZfsSize(f[3]); err != nil {
			return nil, fmt.Errorf("parse zpool %s free %q: %w", p.name, f[3], err)
		}
		if len(f) == 6 {
			if frag, err := strconv.ParseFloat(strings.TrimSuffix(f[4], "%"), 64); err == nil {
				p.frag = frag / 100
			}
		}
		pools = append(pools, p)
	}
	return pools, nil
}

// parseZpoolStatus parses the human-readable `zpool status` output for all pools. Unknown
// sections (QuTS hero prints prune:, expand:, ztier:) are ignored. Returned map is keyed by pool.
func parseZpoolStatus(output string, loc *time.Location) map[string]zpoolInfo {
	result := map[string]zpoolInfo{}
	blocks := zpoolPoolRe.Split(output, -1)
	names := zpoolPoolRe.FindAllStringSubmatch(output, -1)
	for i, m := range names {
		if i+1 >= len(blocks) {
			break
		}
		p := zpoolInfo{name: m[1], statusParsed: true}
		block := blocks[i+1]
		if sm := zpoolStateRe.FindStringSubmatch(block); len(sm) == 2 {
			p.health = sm[1]
		}
		if sm := zpoolScanRe.FindStringSubmatch(block); len(sm) == 2 {
			parseZpoolScan(&p, strings.TrimSpace(sm[1]), loc)
		}
		p.readErrors, p.writeErrors, p.cksumErrors = parseZpoolConfigErrors(block, p.name)
		result[p.name] = p
	}
	return result
}

// parseZpoolScan fills the scan-related fields from the text after "scan:".
func parseZpoolScan(p *zpoolInfo, scan string, loc *time.Location) {
	switch {
	case strings.HasPrefix(scan, "scrub in progress"):
		p.scanInProgress, p.scanKind = true, "scrub"
	case strings.HasPrefix(scan, "resilver in progress"):
		p.scanInProgress, p.scanKind = true, "resilver"
	}
	var errs, when string
	if sm := zpoolScrubOK.FindStringSubmatch(scan); len(sm) == 3 {
		errs, when = sm[1], sm[2]
	} else if sm := zpoolResilvOK.FindStringSubmatch(scan); len(sm) == 3 {
		errs, when = sm[1], sm[2]
	} else {
		return
	}
	t, err := time.ParseInLocation(zpoolStatusDateLayout, strings.TrimSpace(when), loc)
	if err != nil {
		return
	}
	p.hasLastScrub = true
	p.lastScrub = t
	p.lastScrubErrs, _ = strconv.ParseFloat(errs, 64)
}

// parseZpoolConfigErrors sums READ/WRITE/CKSUM over the leaf vdev lines of the config table,
// skipping the pool line itself, grouping vdevs (raidz1-0, mirror-1) and section headers (logs, cache).
func parseZpoolConfigErrors(block string, pool string) (readErr, writeErr, cksumErr float64) {
	idx := strings.Index(block, "config:")
	if idx < 0 {
		return 0, 0, 0
	}
	for _, line := range strings.Split(block[idx:], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "errors:") {
			if strings.HasPrefix(trimmed, "errors:") {
				break
			}
			continue
		}
		sm := zpoolVdevRe.FindStringSubmatch(line)
		if len(sm) != 6 || sm[1] == "NAME" || sm[1] == pool || zpoolGroupRe.MatchString(sm[1]) || zpoolSectionH[sm[1]] {
			continue
		}
		readErr += parseZfsCount(sm[3])
		writeErr += parseZfsCount(sm[4])
		cksumErr += parseZfsCount(sm[5])
	}
	return readErr, writeErr, cksumErr
}

// parseZfsSize parses a zfs/zpool size: exact bytes (-p) or human-readable (12.5T, 800G, 1.2M)
// with 1024-based units; "-" or "none" yield 0.
func parseZfsSize(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "-" || s == "none" || s == "" {
		return 0, nil
	}
	mult := 1.0
	switch s[len(s)-1] {
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	case 'G':
		mult = 1 << 30
	case 'T':
		mult = 1 << 40
	case 'P':
		mult = 1 << 50
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return v * mult, nil
}

// parseZfsCount parses zpool status counters, which may be abbreviated (1.2K) without -p.
func parseZfsCount(s string) float64 {
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "K"):
		mult = 1e3
	case strings.HasSuffix(s, "M"):
		mult = 1e6
	case strings.HasSuffix(s, "G"):
		mult = 1e9
	case strings.HasSuffix(s, "T"):
		mult = 1e12
	}
	v, err := strconv.ParseFloat(strings.TrimRight(s, "KMGT"), 64)
	if err != nil {
		return 0
	}
	return v * mult
}

// parseZfsList parses `zfs list -Hp -o name,used,avail,refer,quota,mountpoint -t filesystem`.
// Datasets without a usable mountpoint (none, legacy, -) are skipped.
func parseZfsList(output string) ([]zfsDataset, error) {
	datasets := make([]zfsDataset, 0, 8)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		// name used avail refer [quota] mountpoint
		if len(f) != 5 && len(f) != 6 {
			return nil, fmt.Errorf("parse zfs list line %q: expected 5 or 6 fields, got %d", line, len(f))
		}
		mp := f[len(f)-1]
		if mp == "none" || mp == "legacy" || mp == "-" || !strings.HasPrefix(mp, "/") {
			continue
		}
		d := zfsDataset{name: f[0], mountpoint: mp}
		var err error
		if d.used, err = parseZfsSize(f[1]); err != nil {
			return nil, fmt.Errorf("parse zfs %s used %q: %w", d.name, f[1], err)
		}
		if d.avail, err = parseZfsSize(f[2]); err != nil {
			return nil, fmt.Errorf("parse zfs %s avail %q: %w", d.name, f[2], err)
		}
		d.refer, _ = parseZfsSize(f[3])
		if len(f) == 6 {
			d.quota, _ = parseZfsSize(f[4])
		}
		datasets = append(datasets, d)
	}
	return annotateQutsVolumes(datasets), nil
}

// annotateQutsVolumes fills the volume label and drops QuTS hero's per-volume snapshot datasets.
// For a dataset D mounted at M, a child D/RecentlySnapshot mounted at M/<share>/@Recently-Snapshot
// names the share; everything else falls back to the last mountpoint component.
func annotateQutsVolumes(all []zfsDataset) []zfsDataset {
	shareOf := map[string]string{}
	for _, d := range all {
		if path.Base(d.name) != qutsSnapshotDataset || path.Base(d.mountpoint) != qutsSnapshotDir {
			continue
		}
		parent := path.Dir(d.name)
		shareDir := path.Dir(d.mountpoint) // M/<share>
		shareOf[parent] = path.Base(shareDir)
	}
	out := make([]zfsDataset, 0, len(all))
	for _, d := range all {
		if path.Base(d.name) == qutsSnapshotDataset || strings.Contains(d.mountpoint, qutsSnapshotDir) {
			continue
		}
		if share, ok := shareOf[d.name]; ok {
			d.volume = share
		} else {
			d.volume = datasetVolume(d)
		}
		out = append(out, d)
	}
	return out
}

// datasetPool returns the pool component of a dataset name (zpool1/zfs3 -> zpool1).
func datasetPool(name string) string {
	return strings.SplitN(name, "/", 2)[0]
}

// datasetVolume returns the volume label: last path component of the mountpoint, or of the dataset name.
func datasetVolume(d zfsDataset) string {
	if v := path.Base(d.mountpoint); v != "/" && v != "." && v != "" {
		return v
	}
	return path.Base(d.name)
}

// refreshZfs runs zpool/zfs when the cache expired. Errors are returned so the scrape reports them
// once per refresh; the previous cache is kept.
func (e *promExporter) refreshZfs() error {
	if !e.zfs.lastFetch.IsZero() && time.Since(e.zfs.lastFetch) < zfsValidity {
		return nil
	}
	e.zfs.lastFetch = time.Now()

	listOut, err := e.runZfsVariant(e.zpoolPath, zpoolListVariants, &e.zfs.zpoolListVariant)
	if err != nil {
		return fmt.Errorf("zpool list: %w", err)
	}
	pools, err := parseZpoolList(listOut)
	if err != nil {
		return err
	}

	statusOut, err := utils.ExecCommand(e.zpoolPath, "status", "-p")
	if err != nil {
		// Older or vendor builds may lack -p; fall back to the plain output.
		statusOut, err = utils.ExecCommand(e.zpoolPath, "status")
		if err != nil {
			return fmt.Errorf("zpool status: %w", err)
		}
	}
	statuses := parseZpoolStatus(statusOut, time.Local)
	for i := range pools {
		if s, ok := statuses[pools[i].name]; ok {
			list := pools[i]
			s.size, s.alloc, s.free, s.frag = list.size, list.alloc, list.free, list.frag
			if s.health == "" {
				s.health = list.health
			}
			pools[i] = s
		}
	}
	e.zfs.pools = pools

	if e.zfsPath != "" {
		zfsOut, err := e.runZfsVariant(e.zfsPath, zfsListVariants, &e.zfs.zfsListVariant)
		if err != nil {
			return fmt.Errorf("zfs list: %w", err)
		}
		if e.zfs.datasets, err = parseZfsList(zfsOut); err != nil {
			return err
		}
	}
	return nil
}

// runZfsVariant runs the remembered working variant, or tries them in order the first time and
// remembers the first that succeeds. Failures carry the command's stderr.
func (e *promExporter) runZfsVariant(bin string, variants [][]string, chosen *int) (string, error) {
	if *chosen > 0 && *chosen <= len(variants) {
		return execWithStderr(bin, variants[*chosen-1]...)
	}
	var lastErr error
	for i, args := range variants {
		out, err := execWithStderr(bin, args...)
		if err == nil {
			*chosen = i + 1
			if i > 0 {
				e.Logger.Printf("%s %v works (variant %d); earlier variants failed: %v", bin, args, i+1, lastErr)
			}
			return out, nil
		}
		lastErr = err
	}
	return "", lastErr
}

// execWithStderr is utils.ExecCommand with the process' stderr folded into the error.
func execWithStderr(bin string, args ...string) (string, error) {
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("%s %s: %w: %s", path.Base(bin), strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("%s %s: %w", path.Base(bin), strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// getZfsMetrics emits pool health/capacity/error/scrub metrics and dataset capacity metrics.
func (e *promExporter) getZfsMetrics() ([]metric, error) {
	if e.zpoolPath == "" {
		return nil, nil
	}
	err := e.refreshZfs()
	metrics := make([]metric, 0, 10*len(e.zfs.pools)+4*len(e.zfs.datasets))
	for _, p := range e.zfs.pools {
		metrics = append(metrics, zpoolMetrics(p)...)
	}
	for _, d := range e.zfs.datasets {
		metrics = append(metrics, zfsDatasetMetrics(d)...)
	}
	return metrics, err
}

func zpoolMetrics(p zpoolInfo) []metric {
	attr := fmt.Sprintf("pool=%q", p.name)
	health := 0.0
	if p.health == "ONLINE" {
		health = 1
	}
	m := []metric{
		{name: "node_zfs_pool_health", attr: fmt.Sprintf("%s,state=%q", attr, p.health), value: health, metricType: "gauge",
			help: "1 when the pool state is ONLINE, 0 otherwise (state label carries the zpool state)"},
		{name: "node_zfs_pool_size_bytes", attr: attr, value: p.size, metricType: "gauge", help: "Pool size in bytes"},
		{name: "node_zfs_pool_allocated_bytes", attr: attr, value: p.alloc, metricType: "gauge", help: "Pool allocated bytes"},
		{name: "node_zfs_pool_free_bytes", attr: attr, value: p.free, metricType: "gauge", help: "Pool free bytes"},
	}
	if p.frag >= 0 {
		m = append(m, metric{name: "node_zfs_pool_fragmentation_ratio", attr: attr, value: p.frag, metricType: "gauge",
			help: "Pool fragmentation as a ratio (0-1)"})
	}
	if p.statusParsed {
		m = append(m,
			metric{name: "node_zfs_pool_errors_total", attr: attr + `,kind="read"`, value: p.readErrors, metricType: "counter",
				help: "Sum of READ errors over the pool's leaf vdevs"},
			metric{name: "node_zfs_pool_errors_total", attr: attr + `,kind="write"`, value: p.writeErrors, metricType: "counter",
				help: "Sum of WRITE errors over the pool's leaf vdevs"},
			metric{name: "node_zfs_pool_errors_total", attr: attr + `,kind="checksum"`, value: p.cksumErrors, metricType: "counter",
				help: "Sum of CKSUM errors over the pool's leaf vdevs"},
		)
		scan := 0.0
		kind := "none"
		if p.scanInProgress {
			scan, kind = 1, p.scanKind
		}
		m = append(m, metric{name: "node_zfs_pool_scan_in_progress", attr: fmt.Sprintf("%s,scan=%q", attr, kind), value: scan,
			metricType: "gauge", help: "1 while a scrub or resilver is running"})
		if p.hasLastScrub {
			m = append(m,
				metric{name: "node_zfs_pool_last_scrub_timestamp_seconds", attr: attr, value: float64(p.lastScrub.Unix()),
					metricType: "gauge", help: "Unix time of the last completed scrub or resilver"},
				metric{name: "node_zfs_pool_last_scrub_errors", attr: attr, value: p.lastScrubErrs, metricType: "gauge",
					help: "Errors reported by the last completed scrub or resilver"},
			)
		}
	}
	return m
}

func zfsDatasetMetrics(d zfsDataset) []metric {
	attr := fmt.Sprintf("volume=%q,filesystem=\"zfs\",dataset=%q,pool=%q", d.volume, d.name, datasetPool(d.name))
	size := d.used + d.avail
	if d.quota > 0 && d.quota < size {
		size = d.quota
	}
	return []metric{
		{name: "node_volume_size_bytes", attr: attr, value: size, metricType: "gauge", help: "Dataset capacity (used + available, capped by quota)"},
		{name: "node_volume_avail_bytes", attr: attr, value: d.avail, metricType: "gauge", help: "Dataset available bytes"},
		{name: "node_volume_used_bytes", attr: attr, value: d.used, metricType: "gauge", help: "Dataset used bytes (incl. children and snapshots)"},
		{name: "node_volume_referenced_bytes", attr: attr, value: d.refer, metricType: "gauge", help: "Dataset referenced bytes"},
	}
}
