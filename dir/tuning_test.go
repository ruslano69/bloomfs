package dir

import (
	"fmt"
	"testing"
)

// buildDir fills a directory of `total` files at the given per-segment capacity.
func buildDir(total, capacity int, fp float64) (*Directory, []string) {
	d := newWithCap(capacity, fp)
	ns := names(total)
	for i, n := range ns {
		d.Add(n, InodeID(i+1))
	}
	return d, ns
}

func missNames(total int) []string {
	m := make([]string, total)
	for i := range m {
		m[i] = fmt.Sprintf("absent_%06d.dat", i)
	}
	return m
}

// BenchmarkSweep explores how per-segment capacity trades lookup latency against
// segment count, at a fixed total file count. Fewer/larger segments => fewer
// Bloom Tests per lookup (the linked-list scan, §3.5) => faster, at the cost of
// larger per-rebuild work on mutation. Deep CPU profiling is deferred to the
// Rust port; this only surfaces the ratios.
func BenchmarkSweep(b *testing.B) {
	const total = 16000
	caps := []int{1000, 2000, 4000, 8000, 16000}
	for _, c := range caps {
		d, ns := buildDir(total, c, BloomFP)
		miss := missNames(total)
		segs := d.Segments()

		b.Run(fmt.Sprintf("cap=%05d_segs=%02d/hit", c, segs), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, ok := d.Find(ns[i%total]); !ok {
					b.Fatal("miss on present")
				}
			}
		})
		b.Run(fmt.Sprintf("cap=%05d_segs=%02d/miss", c, segs), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, ok := d.Find(miss[i%total]); ok {
					b.Fatal("hit on absent")
				}
			}
		})
	}
}

// TestSegmentMemory reports filter memory per segment and the directory total
// across (capacity, fp) combos — the memory side of the ratio search. Run with
// `go test -run TestSegmentMemory -v ./dir`.
func TestSegmentMemory(t *testing.T) {
	const total = 16000
	combos := []struct {
		capacity int
		fp       float64
	}{
		{4000, 0.001}, {4000, 0.01}, {4000, 0.05},
		{1000, 0.01}, {2000, 0.01}, {8000, 0.01}, {16000, 0.01},
	}
	t.Logf("total files = %d", total)
	t.Logf("%-7s %-7s %-6s %-10s %-11s %-9s", "cap", "fp", "segs", "bits/seg", "bytes/seg", "total KB")
	for _, c := range combos {
		d, _ := buildDir(total, c.capacity, c.fp)
		bitsPerSeg := d.segments[0].filter.Cap()
		bytesPerSeg := bitsPerSeg / 8
		totalKB := float64(uint(d.Segments())*bytesPerSeg) / 1024
		t.Logf("%-7d %-7.3f %-6d %-10d %-11d %-9.1f",
			c.capacity, c.fp, d.Segments(), bitsPerSeg, bytesPerSeg, totalKB)
	}
}
