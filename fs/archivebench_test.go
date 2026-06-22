package fs

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/ruslano69/bloomfs/block"
)

// TestArchiveBench measures the responsiveness reserve of the membership gate on
// the COLD path. The penalty is the real physical filesystem underneath: the
// image is read through an unbuffered handle (O_DIRECT / NO_BUFFERING), so each
// cold directory load is a genuine device read + AES-XTS decrypt + ZSTD
// decompress + index rebuild — no synthetic delay. The non-linearity (warmup,
// readahead, controller cache) comes for free from the real stack.
//
// Two views:
//   - cold ceiling: dcache evicted before every pass, so every baseline lookup is
//     a cold device load. gate ON vs OFF -> the physical penalty the filter avoids.
//   - warmup curve: dcache NOT evicted across iterations, so the RAM working set
//     (dcache, cap dirCacheCap) warms. Baseline p50 converges as hot dirs cache,
//     but p99 stays won because the cold tail (working set > cache) persists.
//
// Skipped unless BLOOMFS_ARCHIVE_BENCH is set. Run on the target disk:
//
//	BLOOMFS_ARCHIVE_BENCH=1 BLOOMFS_BENCH_DIR=/mnt/slowdisk \
//	go test ./fs -run TestArchiveBench -v -timeout 30m
func TestArchiveBench(t *testing.T) {
	if os.Getenv("BLOOMFS_ARCHIVE_BENCH") == "" {
		t.Skip("set BLOOMFS_ARCHIVE_BENCH=1 to run the archive benchmark")
	}
	benchDir := os.Getenv("BLOOMFS_BENCH_DIR")
	if benchDir == "" {
		benchDir = t.TempDir()
	}
	dirs := envInt("BLOOMFS_BENCH_DIRS", 2000)
	filesPer := envInt("BLOOMFS_BENCH_FILES", 4)
	iters := envInt("BLOOMFS_BENCH_ITERS", 4)

	path := filepath.Join(benchDir, "archive.img")
	inodes := uint64(dirs*(filesPer+1) + 1024)            // root + every dir + every file + slack
	blocks := inodes*2 + uint64(dirs*(filesPer+4)) + 8192 // metadata + data + slack

	// --- Build phase: buffered, fast. ---
	dev, err := block.Create(path, blocks)
	if err != nil {
		t.Fatal(err)
	}
	f, err := FormatWith(dev, testKey(), inodes, 256*1024)
	if err != nil {
		dev.Close()
		t.Fatal(err)
	}
	root := f.Root()
	ids := make([]uint64, dirs)
	for i := 0; i < dirs; i++ {
		id, err := f.Mkdir(root, "d"+strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		for j := 0; j < filesPer; j++ {
			if _, err := f.Create(id, "f"+strconv.Itoa(j)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	dev.Sync()
	dev.Close()

	// --- Measure phase: unbuffered backing, so cold = a real device read. ---
	udev, err := block.OpenUnbuffered(path)
	if err != nil {
		t.Fatal(err)
	}
	defer udev.Close()
	fm, err := Mount(udev, testKey())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("archive: %d dirs x %d files; backing %s (unbuffered); dcache cap=%d",
		dirs, filesPer, path, dirCacheCap)

	evictAll := func() {
		for _, id := range ids {
			fm.dcache.remove(id)
		}
	}
	scan := func() ([]time.Duration, time.Duration) {
		lat := make([]time.Duration, 0, dirs)
		total := time.Now()
		for _, id := range ids {
			start := time.Now()
			fm.Lookup(id, "no-such-name")
			lat = append(lat, time.Since(start))
		}
		return lat, time.Since(total)
	}

	// --- Cold ceiling: evict before every pass. ---
	t.Log("== cold ceiling (dcache evicted each pass) ==")
	for _, mode := range []struct {
		label string
		gate  bool
	}{{"gate-OFF", false}, {"gate-ON ", true}} {
		fm.gateEnabled = mode.gate
		for it := 0; it < iters; it++ {
			evictAll()
			lat, total := scan()
			p50, p99, p999 := pctiles(lat)
			t.Logf("  %s iter %d: p50=%-10v p99=%-10v p99.9=%-10v total=%v", mode.label, it, p50, p99, p999, total)
		}
	}

	// --- Warmup curve: baseline only, dcache NOT evicted across iterations. ---
	t.Log("== warmup curve (gate-OFF, dcache warms across iterations) ==")
	fm.gateEnabled = false
	evictAll()
	for it := 0; it < iters; it++ {
		lat, total := scan()
		p50, p99, p999 := pctiles(lat)
		t.Logf("  warm iter %d: p50=%-10v p99=%-10v p99.9=%-10v total=%v", it, p50, p99, p999, total)
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func pctiles(d []time.Duration) (p50, p99, p999 time.Duration) {
	if len(d) == 0 {
		return
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	at := func(p float64) time.Duration {
		i := int(p * float64(len(d)))
		if i >= len(d) {
			i = len(d) - 1
		}
		return d[i]
	}
	return at(0.50), at(0.99), at(0.999)
}
