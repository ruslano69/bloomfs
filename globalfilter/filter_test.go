package globalfilter

import (
	"fmt"
	"sync"
	"testing"
)

// TestDeleteThenRecreate is the critical correctness case: delete a name, then
// re-create it (same name, new inode) in the same directory. It must be findable
// immediately — the tombstone must be cleared by Create.
func TestDeleteThenRecreate(t *testing.T) {
	g := New()
	const dir = 42
	g.Create(dir, "foo", 1)
	g.Unlink(dir, "foo")
	if _, ok := g.Lookup(dir, "foo"); ok {
		t.Fatal("found after unlink (tombstone should hide it)")
	}
	g.Create(dir, "foo", 2) // re-create
	id, ok := g.Lookup(dir, "foo")
	if !ok {
		t.Fatal("not found after delete+recreate: tombstone not cleared")
	}
	if id != 2 {
		t.Fatalf("recreate gave inode %d, want 2", id)
	}
}

// TestRebuildThenRecreate covers the dangerous sub-case: a rebuild fires AFTER
// the Unlink (removing the key from the filter), then Create must re-add it
// before the entry is visible.
func TestRebuildThenRecreate(t *testing.T) {
	g := New()
	const dir = 42
	g.Create(dir, "foo", 1)
	g.Unlink(dir, "foo")
	g.ForceRebuild() // simulates hitting RebuildThreshold; filter no longer has key
	if g.Tombstones() != 0 {
		t.Fatalf("rebuild should clear tombstones, have %d", g.Tombstones())
	}
	g.Create(dir, "foo", 2)
	if _, ok := g.Lookup(dir, "foo"); !ok {
		t.Fatal("not found after rebuild+recreate: filter not updated by Create")
	}
}

// TestCreateOrderingGuarantee documents the invariant: after Create returns,
// Lookup must never return Absent for that key.
func TestCreateOrderingGuarantee(t *testing.T) {
	g := New()
	g.Create(42, "bar", 99)
	if id, ok := g.Lookup(42, "bar"); !ok || id != 99 {
		t.Fatalf("invariant violated: Lookup(42,bar)=(%d,%v) after Create", id, ok)
	}
}

// TestNoDuplicateNames: a second Create of an existing name in the same dir is
// rejected; the original inode is preserved.
func TestNoDuplicateNames(t *testing.T) {
	g := New()
	if !g.Create(7, "a", 1) {
		t.Fatal("first create should succeed")
	}
	if g.Create(7, "a", 2) {
		t.Fatal("duplicate name should be rejected")
	}
	if id, _ := g.Lookup(7, "a"); id != 1 {
		t.Fatalf("duplicate create clobbered inode: got %d, want 1", id)
	}
}

// TestPerDirectoryScoping: the same name in two directories is two independent
// entries (the key is seeded by dir inode).
func TestPerDirectoryScoping(t *testing.T) {
	g := New()
	g.Create(1, "readme", 100)
	g.Create(2, "readme", 200)
	if id, ok := g.Lookup(1, "readme"); !ok || id != 100 {
		t.Fatalf("dir 1 readme = (%d,%v), want (100,true)", id, ok)
	}
	if id, ok := g.Lookup(2, "readme"); !ok || id != 200 {
		t.Fatalf("dir 2 readme = (%d,%v), want (200,true)", id, ok)
	}
	// Deleting in dir 1 must not affect dir 2.
	g.Unlink(1, "readme")
	if _, ok := g.Lookup(1, "readme"); ok {
		t.Fatal("dir 1 readme still present after unlink")
	}
	if id, ok := g.Lookup(2, "readme"); !ok || id != 200 {
		t.Fatalf("dir 2 readme disturbed by dir 1 unlink: (%d,%v)", id, ok)
	}
}

// TestNoFalseNegatives: every name that was created and not deleted must be
// found — across a population large enough to span a rebuild. A Bloom filter may
// never produce a false negative; this asserts it.
func TestNoFalseNegatives(t *testing.T) {
	g := New()
	g.rebuildThreshold = 64 // force several rebuilds during the run
	const dir = 9
	const n = 5000

	// Create n, delete the odd half (drives rebuilds), the even half stays live.
	for i := 0; i < n; i++ {
		g.Create(dir, fmt.Sprintf("file%d", i), InodeID(i+1))
	}
	for i := 1; i < n; i += 2 {
		g.Unlink(dir, fmt.Sprintf("file%d", i))
	}
	// Every even (live) entry must be found — no false negatives.
	for i := 0; i < n; i += 2 {
		id, ok := g.Lookup(dir, fmt.Sprintf("file%d", i))
		if !ok {
			t.Fatalf("false negative: file%d (live) not found", i)
		}
		if id != InodeID(i+1) {
			t.Fatalf("file%d resolved to %d, want %d", i, id, i+1)
		}
	}
	// Every odd (deleted) entry must be absent.
	for i := 1; i < n; i += 2 {
		if _, ok := g.Lookup(dir, fmt.Sprintf("file%d", i)); ok {
			t.Fatalf("deleted file%d still found", i)
		}
	}
}

// TestResidentMapFirstAgreesWithCold proves the map-first (resident) path gives
// identical answers to the filter-gated (cold) path — including a deleted entry,
// which the resident path must report absent via the map alone (no tombstone).
func TestResidentMapFirstAgreesWithCold(t *testing.T) {
	g := New()
	const dir = 5
	for i := 0; i < 1000; i++ {
		g.Create(dir, fmt.Sprintf("f%d", i), InodeID(i+1))
	}
	g.Unlink(dir, "f500")

	check := func(stage string) {
		if id, ok := g.Lookup(dir, "f499"); !ok || id != 500 {
			t.Fatalf("%s: f499 = (%d,%v), want (500,true)", stage, id, ok)
		}
		if _, ok := g.Lookup(dir, "f500"); ok {
			t.Fatalf("%s: deleted f500 still found", stage)
		}
		if _, ok := g.Lookup(dir, "absent"); ok {
			t.Fatalf("%s: absent name found", stage)
		}
	}
	check("cold")
	g.MarkResident(dir)
	check("resident") // map-first must match cold, incl. the delete
	g.Evict(dir)
	check("cold-again")
}

// TestConcurrentLookupDuringMutation runs parallel readers against a writer to
// prove the internal RWMutex (run under -race). Readers must never see a torn
// state; a name is either fully present or fully absent.
func TestConcurrentLookupDuringMutation(t *testing.T) {
	g := New()
	const dir = 1
	const n = 2000
	for i := 0; i < n; i++ {
		g.Create(dir, fmt.Sprintf("seed%d", i), InodeID(i+1))
	}

	var wg sync.WaitGroup
	// Writer: churn a disjoint key space (create/unlink) to drive filter mutation.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("churn%d", i)
			g.Create(dir, name, InodeID(1_000_000+i))
			g.Unlink(dir, name)
		}
	}()
	// Readers: the seed set is never mutated, so every lookup must hit.
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				if _, ok := g.Lookup(dir, fmt.Sprintf("seed%d", i)); !ok {
					t.Errorf("stable entry seed%d not found during mutation", i)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestLenTracksLiveSet sanity-checks the live counter through create/delete.
func TestLenTracksLiveSet(t *testing.T) {
	g := New()
	for i := 0; i < 100; i++ {
		g.Create(1, fmt.Sprintf("f%d", i), InodeID(i+1))
	}
	if g.Len() != 100 {
		t.Fatalf("Len=%d, want 100", g.Len())
	}
	for i := 0; i < 40; i++ {
		g.Unlink(1, fmt.Sprintf("f%d", i))
	}
	if g.Len() != 60 {
		t.Fatalf("Len=%d after 40 unlinks, want 60", g.Len())
	}
}
