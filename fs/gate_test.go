package fs

import (
	"strconv"
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// TestGateAvoidsColdDirLoads is the P0 payoff measurement: a negative lookup in a
// COLD directory (not in the dir cache) must be answered by the global filter
// without loading — decrypting, decompressing, rebuilding — that directory from
// disk. We count ReadBlock calls with the gate on vs off.
func TestGateAvoidsColdDirLoads(t *testing.T) {
	cd := &countingDev{Device: block.NewMem(4096)}
	f, err := Format(cd, testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()

	const dirs, filesPer = 200, 3
	ids := make([]uint64, dirs)
	for i := range ids {
		id, err := f.Mkdir(root, dirName(i))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		for j := 0; j < filesPer; j++ {
			if _, err := f.Create(id, fileName(j)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}

	// No remount: the gate stays well-sized within the session (capacity floor +
	// commit-time rebuild), so cold lookups are gated correctly without reloading.

	// Force every subdirectory cold (simulate eviction / a fresh working set).
	evictAll := func() {
		for _, id := range ids {
			f.dcache.remove(id)
		}
	}

	// --- Gate ON: a true miss in a cold dir must touch zero blocks. ---
	evictAll()
	before := cd.reads
	for _, id := range ids {
		if _, ok, _ := f.Lookup(id, "does-not-exist"); ok {
			t.Fatal("phantom hit")
		}
	}
	withGate := cd.reads - before
	// A true miss is rejected with zero reads; only a Bloom false positive (~1% at
	// the tuned FP) slips through to a load that then finds nothing — correct, just
	// not free. So a handful of reads is expected, not hundreds.
	if withGate > dirs/20 {
		t.Fatalf("gate ON: %d ReadBlock for %d cold negative lookups, above the ~1%% FP budget", withGate, dirs)
	}

	// --- Gate OFF: the same lookups must each load their directory. ---
	f.gateEnabled = false
	evictAll()
	before = cd.reads
	for _, id := range ids {
		if _, ok, _ := f.Lookup(id, "does-not-exist"); ok {
			t.Fatal("phantom hit")
		}
	}
	withoutGate := cd.reads - before
	f.gateEnabled = true

	if withoutGate < dirs {
		t.Fatalf("gate OFF: %d ReadBlock for %d cold lookups, expected at least one load each", withoutGate, dirs)
	}
	t.Logf("cold negative lookups over %d dirs: gate ON = %d ReadBlock, gate OFF = %d ReadBlock (avoided %d)",
		dirs, withGate, withoutGate, withoutGate-withGate)
}

// TestGateRebuildKeepsFPBoundedInSession closes gaps (1) undersizing and (2)
// tombstone growth WITHOUT a remount: it grows the live set well past the filter's
// initial capacity and deletes a large fraction, committing along the way, then
// checks the gate is correctly re-sized (capacity grew, tombstones reset) and
// still gates cold negative lookups within ~the FP budget.
func TestGateRebuildKeepsFPBoundedInSession(t *testing.T) {
	cd := &countingDev{Device: block.NewMem(4096)}
	f, err := Format(cd, testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()

	// Create 3000 files in one directory — far past the 1024 floor, forcing the
	// growth rebuild. Commit periodically so the rebuild fires at a consistent point.
	const total = 3000
	for i := 0; i < total; i++ {
		if _, err := f.Create(root, "file"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
		if i%1000 == 999 {
			if err := f.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Delete half — drives tombstones past their threshold.
	for i := 0; i < total; i += 2 {
		if err := f.Unlink(root, "file"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}

	// After the rebuild at commit: capacity grew past the floor, tombstones reset.
	if cap := f.gate.Capacity(); cap <= 1024 {
		t.Fatalf("gate did not grow: capacity %d still at/below floor", cap)
	}
	if tomb := f.gate.Tombstones(); tomb > f.gate.Capacity()/tombstoneFractionTest {
		t.Fatalf("tombstones not reset by rebuild: %d", tomb)
	}

	// Cold negative lookups in the (single, now-cold) directory must still be
	// gated: a name never created returns absent without reload, within FP budget.
	f.dcache.remove(root)
	before := cd.reads
	const probes = 500
	for i := 0; i < probes; i++ {
		if _, ok, _ := f.Lookup(root, "ghost"+strconv.Itoa(i)); ok {
			t.Fatal("phantom hit")
		}
	}
	reads := cd.reads - before
	if reads > probes/10 {
		t.Fatalf("in-session gate degraded: %d reads for %d cold misses (FP too high)", reads, probes)
	}
	// Every surviving (odd) file must still be found — no false negative.
	f.dcache.remove(root)
	for i := 1; i < total; i += 2 {
		if _, ok, _ := f.Lookup(root, "file"+strconv.Itoa(i)); !ok {
			t.Fatalf("false negative: surviving file%d not found", i)
		}
	}
	t.Logf("after growth+deletes: capacity=%d live=%d tombstones=%d; %d cold misses cost %d reads",
		f.gate.Capacity(), f.gate.Live(), f.gate.Tombstones(), probes, reads)
}

// tombstoneFractionTest mirrors globalfilter's internal trigger for the assertion
// above (rebuild resets tombstones below capacity/4).
const tombstoneFractionTest = 4

// TestGateNoFalseNegativeOnColdHit guards correctness: an EXISTING file in a cold
// directory must still be found (the gate admits a real key, the dir then loads).
func TestGateNoFalseNegativeOnColdHit(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	d, err := f.Mkdir(root, "data")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Create(d, "present.txt"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}

	f.dcache.remove(d) // cold
	if _, ok, _ := f.Lookup(d, "present.txt"); !ok {
		t.Fatal("false negative: existing file in a cold dir reported absent")
	}
	f.dcache.remove(d)
	if _, ok, _ := f.Lookup(d, "absent.txt"); ok {
		t.Fatal("phantom hit for absent name")
	}
}

// TestGateSnapshotSkipsMountWalk is the final-optimization measurement: a mount
// that loads the persisted gate snapshot reads far fewer blocks than one that
// rebuilds the gate by walking the tree — and stays correct.
func TestGateSnapshotSkipsMountWalk(t *testing.T) {
	cd := &countingDev{Device: block.NewMem(4096)}
	f, err := Format(cd, testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	const dirs, filesPer = 500, 3
	for i := 0; i < dirs; i++ {
		id, err := f.Mkdir(root, dirName(i))
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < filesPer; j++ {
			if _, err := f.Create(id, fileName(j)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	snap, err := f.SnapshotGate()
	if err != nil || len(snap) == 0 {
		t.Fatalf("snapshot: %v (len %d)", err, len(snap))
	}

	// Mount A: rebuild by walking the tree.
	cd.reads = 0
	if _, err := Mount(cd, testKey()); err != nil {
		t.Fatal(err)
	}
	walkReads := cd.reads

	// Mount B: load the snapshot — no tree walk.
	cd.reads = 0
	fb, err := MountWithGate(cd, testKey(), snap)
	if err != nil {
		t.Fatal(err)
	}
	loadReads := cd.reads

	// walk = metadata-mount floor + one load per directory; snapshot = floor only.
	// The win is the eliminated per-directory walk (~dirs reads), which scales with
	// the tree while the floor is fixed.
	eliminated := walkReads - loadReads
	t.Logf("mount ReadBlock: walk=%d, snapshot=%d -> walk eliminated %d (~%d dirs); snapshot pays only the metadata floor",
		walkReads, loadReads, eliminated, dirs)
	if eliminated < dirs {
		t.Fatalf("snapshot did not eliminate the per-dir walk: walk=%d load=%d eliminated=%d, want >= %d",
			walkReads, loadReads, eliminated, dirs)
	}

	// Correctness of the snapshot-loaded gate: a real file in a cold dir is found,
	// an absent name is gated away.
	dirID, ok, _ := fb.Lookup(root, dirName(7))
	if !ok {
		t.Fatal("dir7 missing after snapshot mount")
	}
	fb.dcache.remove(dirID)
	if _, ok, _ := fb.Lookup(dirID, fileName(1)); !ok {
		t.Fatal("false negative: real file not found via snapshot-loaded gate")
	}
	fb.dcache.remove(dirID)
	if _, ok, _ := fb.Lookup(dirID, "ghost"); ok {
		t.Fatal("phantom hit via snapshot-loaded gate")
	}

	// A stale snapshot (state advanced past its seq) must fall back to the walk.
	if _, err := fb.Create(root, "newcomer"); err != nil {
		t.Fatal(err)
	}
	if err := fb.Commit(); err != nil { // seq advances; snap is now stale
		t.Fatal(err)
	}
	stale, err := MountWithGate(cd, testKey(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := stale.Lookup(root, "newcomer"); !ok {
		t.Fatal("stale snapshot accepted: newcomer not found (gate not rebuilt)")
	}
}

func dirName(i int) string  { return "dir" + strconv.Itoa(i) }
func fileName(j int) string { return "f" + strconv.Itoa(j) }
