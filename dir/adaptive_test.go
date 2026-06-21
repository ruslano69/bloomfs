package dir

import (
	"fmt"
	"testing"
)

// flatDir is a map-only directory with no Bloom filter — used as a baseline to
// find the crossover point where the Bloom-segmented structure starts paying off.
// This is NOT production code; it lives here only to drive BenchmarkFlatVsSegmented.
type flatDir struct {
	index map[uint64][]dirEntry
}

func newFlatDir(capacity int) *flatDir {
	return &flatDir{index: make(map[uint64][]dirEntry, capacity)}
}

func (f *flatDir) add(name string, inode InodeID) {
	h := nameHash(name)
	f.index[h] = append(f.index[h], dirEntry{name: name, inode: inode})
}

func (f *flatDir) find(name string) (InodeID, bool) {
	h := nameHash(name)
	for _, e := range f.index[h] {
		if e.name == name {
			return e.inode, true
		}
	}
	return 0, false
}

// BenchmarkFlatVsSegmented measures pure map (no Bloom filter) vs the current
// Bloom-segmented implementation at increasing directory sizes.
//
// The crossover point — where segmented/miss first beats flat/miss — determines
// FlatThreshold: below it, the flat map wins on both latency and memory;
// above it, the Bloom filter starts rejecting misses faster.
//
// Run with:
//
//	go test -bench=BenchmarkFlatVsSegmented -benchmem ./dir/
func BenchmarkFlatVsSegmented(b *testing.B) {
	sizes := []int{10, 50, 100, 200, 500, 1000, 2000, 5000, 10000}

	for _, n := range sizes {
		ns := names(n)
		miss := missNames(n)

		// Flat: one map, no filter.
		flat := newFlatDir(n)
		for i, name := range ns {
			flat.add(name, InodeID(i+1))
		}

		// Segmented: one large segment (no chain penalty) so we measure only the
		// filter cost, not the linked-list scan. Cap = max(n, MaxFilesPerSegment)
		// so the filter is sized for the actual population.
		segCap := n
		if segCap < MaxFilesPerSegment {
			segCap = MaxFilesPerSegment
		}
		seg := newWithCap(segCap, BloomFP)
		for i, name := range ns {
			seg.Add(name, InodeID(i+1))
		}

		label := fmt.Sprintf("n=%05d", n)

		b.Run(label+"/flat/hit", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, ok := flat.find(ns[i%n]); !ok {
					b.Fatal("unexpected miss")
				}
			}
		})
		b.Run(label+"/flat/miss", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, ok := flat.find(miss[i%n]); ok {
					b.Fatal("unexpected hit")
				}
			}
		})
		b.Run(label+"/segmented/hit", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, ok := seg.Find(ns[i%n]); !ok {
					b.Fatal("unexpected miss")
				}
			}
		})
		b.Run(label+"/segmented/miss", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, ok := seg.Find(miss[i%n]); ok {
					b.Fatal("unexpected hit")
				}
			}
		})
	}
}

// --- Spec tests for the future adaptive Directory (Phase P0) ---
//
// These tests describe the intended behaviour after the flat→segmented
// conversion is implemented. They are skipped until Directory gains:
//   - Directory.IsFlat() bool
//   - FlatThreshold constant
//
// To activate: remove t.Skip() and implement the production code.

// TestAdaptiveStartsFlat verifies that a new directory begins in flat mode.
func TestAdaptiveStartsFlat(t *testing.T) {
	t.Skip("Phase P0 not yet implemented: Directory.IsFlat() missing")

	d := New()
	if !d.IsFlat() {
		t.Fatal("new directory should start in flat mode")
	}
}

// TestAdaptiveStaysFlatBelowThreshold verifies that a directory remains flat
// as long as it holds fewer than FlatThreshold entries.
func TestAdaptiveStaysFlatBelowThreshold(t *testing.T) {
	t.Skip("Phase P0 not yet implemented: Directory.IsFlat() missing")

	d := New()
	ns := names(FlatThreshold - 1)
	for i, name := range ns {
		d.Add(name, InodeID(i+1))
	}
	if !d.IsFlat() {
		t.Fatalf("directory with %d entries (< FlatThreshold %d) should still be flat",
			FlatThreshold-1, FlatThreshold)
	}
	// All entries must be findable in flat mode.
	for i, name := range ns {
		got, ok := d.Find(name)
		if !ok || got != InodeID(i+1) {
			t.Fatalf("Find(%q) = (%d,%v) in flat mode, want (%d,true)", name, got, ok, i+1)
		}
	}
}

// TestAdaptiveConvertsAtThreshold verifies that adding the FlatThreshold-th
// entry triggers automatic conversion to the segmented structure, and that all
// previously added entries remain findable after conversion.
func TestAdaptiveConvertsAtThreshold(t *testing.T) {
	t.Skip("Phase P0 not yet implemented: Directory.IsFlat() missing")

	d := New()
	ns := names(FlatThreshold + 10)

	// Fill up to the threshold.
	for i := range FlatThreshold - 1 {
		d.Add(ns[i], InodeID(i+1))
	}
	if !d.IsFlat() {
		t.Fatal("should still be flat one entry before threshold")
	}

	// The threshold entry triggers conversion.
	d.Add(ns[FlatThreshold-1], InodeID(FlatThreshold))
	if d.IsFlat() {
		t.Fatal("should have converted to segmented at FlatThreshold")
	}

	// Every entry added before conversion must still resolve correctly.
	for i := range FlatThreshold {
		got, ok := d.Find(ns[i])
		if !ok || got != InodeID(i+1) {
			t.Fatalf("entry %d lost after flat→segmented conversion: Find=%d,%v", i, got, ok)
		}
	}

	// Entries added after conversion also work.
	for i := FlatThreshold; i < len(ns); i++ {
		d.Add(ns[i], InodeID(i+1))
	}
	for i := FlatThreshold; i < len(ns); i++ {
		got, ok := d.Find(ns[i])
		if !ok || got != InodeID(i+1) {
			t.Fatalf("post-conversion entry %d not found: Find=%d,%v", i, got, ok)
		}
	}
}

// TestAdaptiveMissAfterConversion verifies that absent names return false
// both in flat mode and in segmented mode (no false negatives at either stage).
func TestAdaptiveMissAfterConversion(t *testing.T) {
	t.Skip("Phase P0 not yet implemented: Directory.IsFlat() missing")

	d := New()
	present := names(FlatThreshold + 100)
	absent := missNames(FlatThreshold + 100)

	for i, name := range present {
		d.Add(name, InodeID(i+1))
	}

	// After conversion, absent names must not produce false hits.
	for _, name := range absent {
		if _, ok := d.Find(name); ok {
			t.Fatalf("false positive for %q after flat→segmented conversion", name)
		}
	}
}

// TestAdaptiveDeleteInFlatMode verifies Remove works correctly before conversion.
func TestAdaptiveDeleteInFlatMode(t *testing.T) {
	t.Skip("Phase P0 not yet implemented: Directory.IsFlat() missing")

	d := New()
	ns := names(10)
	for i, name := range ns {
		d.Add(name, InodeID(i+1))
	}
	if !d.IsFlat() {
		t.Skip("directory already converted; test is only for flat mode")
	}

	// Delete one entry; it must disappear and others must survive.
	target := ns[5]
	if !d.Delete(target) {
		t.Fatalf("Delete(%q) returned false in flat mode", target)
	}
	if _, ok := d.Find(target); ok {
		t.Fatalf("deleted entry %q still found in flat mode", target)
	}
	for i, name := range ns {
		if i == 5 {
			continue
		}
		if _, ok := d.Find(name); !ok {
			t.Fatalf("entry %q disappeared after unrelated delete in flat mode", name)
		}
	}
}

// --- Global filter correctness tests (Phase P0, globalfilter package) ---
//
// These tests describe the required behaviour of the GlobalFilter component.
// They live here as a spec; move to globalfilter/filter_test.go when the
// package exists.

// TestGlobalFilterDeleteThenRecreate is the critical correctness case:
// delete a name, then re-create it with the same name in the same directory.
// The file must be findable immediately after re-creation — no ENOENT.
//
// Dangerous sub-case: if rebuild() fired between the Unlink and the Create
// (removing the key from the Bloom filter entirely), the Create must still
// re-add the key to the filter before making the entry visible to readers.
func TestGlobalFilterDeleteThenRecreate(t *testing.T) {
	t.Skip("Phase P0 not yet implemented: globalfilter package missing")

	// Sequence: create → delete → create → must find.
	//
	// var gf globalfilter.GlobalFilter
	// const dirIno = 42
	// gf.Create(dirIno, "foo", 1)
	// gf.Unlink(dirIno, "foo")
	// gf.Create(dirIno, "foo", 2)   // re-create: same name, new inode
	//
	// result := gf.Lookup(dirIno, "foo")
	// if result == globalfilter.Absent {
	//     t.Fatal("file not found after delete+recreate: tombstone not cleared")
	// }
}

// TestGlobalFilterRebuildThenRecreate covers the dangerous sub-case:
// rebuild fires AFTER Unlink (removing the key from the filter), then Create
// must re-add it before the entry is visible.
func TestGlobalFilterRebuildThenRecreate(t *testing.T) {
	t.Skip("Phase P0 not yet implemented: globalfilter package missing")

	// Sequence: create → delete → trigger rebuild → create → must find.
	//
	// var gf globalfilter.GlobalFilter
	// const dirIno = 42
	// gf.Create(dirIno, "foo", 1)
	// gf.Unlink(dirIno, "foo")
	// gf.ForceRebuild()             // simulates hitting RebuildThreshold
	// gf.Create(dirIno, "foo", 2)
	//
	// result := gf.Lookup(dirIno, "foo")
	// if result == globalfilter.Absent {
	//     t.Fatal("file not found after rebuild+recreate: filter not updated")
	// }
}

// TestGlobalFilterCreateOrderingGuarantee documents the required step order
// inside Create: tombstone must be cleared and filter must be updated BEFORE
// the directory entry becomes visible. Verified via the global filter API,
// not via internal state inspection.
func TestGlobalFilterCreateOrderingGuarantee(t *testing.T) {
	t.Skip("Phase P0 not yet implemented: globalfilter package missing")

	// After gf.Create() returns, Lookup must never return Absent for that key.
	// This is the invariant; the internal ordering (steps 1→2→3) is what
	// enforces it. If any caller sees Absent after a successful Create, the
	// ordering contract was violated.
	//
	// var gf globalfilter.GlobalFilter
	// gf.Create(42, "bar", 99)
	// if gf.Lookup(42, "bar") == globalfilter.Absent {
	//     t.Fatal("invariant violated: Lookup returned Absent after Create")
	// }
}
