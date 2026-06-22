package globalfilter

import (
	"fmt"
	"testing"
)

// flatFS is the post-P0 baseline: plain per-directory maps with NO global filter.
// A miss costs one map lookup into the target directory's bucket array. This is
// what the global filter must beat to justify its memory and indirection.
type flatFS struct {
	dirs map[uint64]map[uint64][]entry
}

func newFlatFS() *flatFS { return &flatFS{dirs: make(map[uint64]map[uint64][]entry)} }

func (f *flatFS) create(dirIno uint64, name string, inode InodeID) {
	_, lo := key(dirIno, name)
	d := f.dirs[dirIno]
	if d == nil {
		d = make(map[uint64][]entry)
		f.dirs[dirIno] = d
	}
	d[lo] = append(d[lo], entry{name: name, inode: inode})
}

func (f *flatFS) lookup(dirIno uint64, name string) (InodeID, bool) {
	_, lo := key(dirIno, name)
	for _, e := range f.dirs[dirIno][lo] {
		if e.name == name {
			return e.inode, true
		}
	}
	return 0, false
}

// buildPopulation spreads `files` files across `dirsN` directories, round-robin.
// Returns present names and an equal-length set of absent names (same dirs).
func buildPopulation(files, dirsN int) (present, absent []struct {
	dir  uint64
	name string
}) {
	present = make([]struct {
		dir  uint64
		name string
	}, files)
	absent = make([]struct {
		dir  uint64
		name string
	}, files)
	for i := 0; i < files; i++ {
		d := uint64(i%dirsN) + 1
		present[i] = struct {
			dir  uint64
			name string
		}{d, fmt.Sprintf("file_%d", i)}
		absent[i] = struct {
			dir  uint64
			name string
		}{d, fmt.Sprintf("absent_%d", i)}
	}
	return present, absent
}

// BenchmarkGlobalVsFlat compares the global Bloom filter against plain per-dir
// maps at filesystem scale. The miss path is where the filter is supposed to
// win: reject before touching any directory map.
//
//	go test -bench=BenchmarkGlobalVsFlat -benchmem ./globalfilter/
func BenchmarkGlobalVsFlat(b *testing.B) {
	type scale struct {
		files, dirs int
	}
	scales := []scale{
		{10_000, 100},
		{100_000, 1_000},
		{1_000_000, 10_000},
	}

	for _, s := range scales {
		present, absent := buildPopulation(s.files, s.dirs)

		g := NewSized(s.files)
		flat := newFlatFS()
		for _, e := range present {
			g.Create(e.dir, e.name, 1)
			flat.create(e.dir, e.name, 1)
		}

		label := fmt.Sprintf("files=%d/dirs=%d", s.files, s.dirs)
		n := s.files

		b.Run(label+"/global/miss", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				e := absent[i%n]
				if _, ok := g.Lookup(e.dir, e.name); ok {
					b.Fatal("unexpected hit")
				}
			}
		})
		b.Run(label+"/flat/miss", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				e := absent[i%n]
				if _, ok := flat.lookup(e.dir, e.name); ok {
					b.Fatal("unexpected hit")
				}
			}
		})
		b.Run(label+"/global/hit", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				e := present[i%n]
				if _, ok := g.Lookup(e.dir, e.name); !ok {
					b.Fatal("unexpected miss")
				}
			}
		})
		b.Run(label+"/flat/hit", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				e := present[i%n]
				if _, ok := flat.lookup(e.dir, e.name); !ok {
					b.Fatal("unexpected miss")
				}
			}
		})
	}
}

// BenchmarkAdaptiveHitPath isolates the residency optimization: the SAME global
// filter is measured with its directories cold (filter on every lookup) and then
// resident (map-first, filter skipped). It quantifies how much of the hit-tax is
// reclaimed by going map-first on hot directories, and confirms a cold miss is
// still rejected fast by the filter.
//
//	go test -bench=BenchmarkAdaptiveHitPath -benchmem ./globalfilter/
func BenchmarkAdaptiveHitPath(b *testing.B) {
	const files, dirs = 1_000_000, 10_000
	present, absent := buildPopulation(files, dirs)
	g := NewSized(files)
	for _, e := range present {
		g.Create(e.dir, e.name, 1)
	}
	n := files

	// Phase 1 — all directories cold: every lookup pays the filter.
	for d := uint64(1); d <= dirs; d++ {
		g.Evict(d)
	}
	b.Run("cold-filtered/hit", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			e := present[i%n]
			if _, ok := g.Lookup(e.dir, e.name); !ok {
				b.Fatal("unexpected miss")
			}
		}
	})
	b.Run("cold-filtered/miss", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			e := absent[i%n]
			if _, ok := g.Lookup(e.dir, e.name); ok {
				b.Fatal("unexpected hit")
			}
		}
	})

	// Phase 2 — all directories resident: hit and miss go map-first.
	for d := uint64(1); d <= dirs; d++ {
		g.MarkResident(d)
	}
	b.Run("resident-mapfirst/hit", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			e := present[i%n]
			if _, ok := g.Lookup(e.dir, e.name); !ok {
				b.Fatal("unexpected miss")
			}
		}
	})
	b.Run("resident-mapfirst/miss", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			e := absent[i%n]
			if _, ok := g.Lookup(e.dir, e.name); ok {
				b.Fatal("unexpected hit")
			}
		}
	})
}
