package fs

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// Archive-loop scenario: write files in a tight loop, keeping only the last N by
// unlinking the oldest. Without auto-commit-on-ENOSPC the deferred-free clusters
// pile up and the bitmap eventually looks full even though only N files are live.
// Checks (1) StatFS().BlocksFree stays > 0 (deferred frees are accounted for) and
// (2) no ENOSPC across many iterations on a small device.
func TestArchiveLoopNoENOSPC(t *testing.T) {
	// 512 blocks (~2 MiB): tight enough to expose deferred-free exhaustion.
	f, err := Format(block.NewMem(512), nil)
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	payload := make([]byte, block.Size) // one 4 KiB block per archive
	for i := range payload {
		payload[i] = 0xAB
	}
	const keep = 5
	var names []string
	freeStart := f.StatFS().BlocksFree

	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("archive_%03d.bin", i)
		id, err := f.Create(root, name)
		if err != nil {
			t.Fatalf("iter %d: create returned error (ENOSPC?): %v", i, err)
		}
		if err := f.WriteFile(id, payload); err != nil {
			t.Fatalf("iter %d: WriteFile returned error (ENOSPC?): %v", i, err)
		}
		names = append(names, name)
		if len(names) > keep {
			if err := f.Unlink(root, names[0]); err != nil {
				t.Fatalf("iter %d: unlink: %v", i, err)
			}
			names = names[1:]
		}
		if free := f.StatFS().BlocksFree; free == 0 {
			t.Fatalf("iter %d: StatFS reported 0 free blocks (deferred frees not counted)", i)
		}
	}

	// After an explicit commit all deferred frees are applied; the free count
	// should return to near its initial value (only `keep` files live).
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	if free := f.StatFS().BlocksFree; free <= freeStart/2 {
		t.Fatalf("after commit free=%d, want close to initial %d", free, freeStart)
	}
}

// Fallocate returns nil when there is room for the worst-case (no-dedup)
// estimate, and ErrNoSpace when there clearly is not.
func TestFallocateSpaceCheck(t *testing.T) {
	f, err := Format(block.NewMem(512), nil)
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()

	if err := f.Fallocate(root, block.Size); err != nil {
		t.Fatalf("4 KiB should fit on a 512-block device, got %v", err)
	}
	twoMiB := uint64(512 * block.Size)
	if err := f.Fallocate(root, twoMiB); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("2 MiB fallocate on a nearly-full device: got %v, want ErrNoSpace", err)
	}
	if err := f.Fallocate(root, 0); err != nil {
		t.Fatalf("zero-length fallocate must be ok, got %v", err)
	}
}

// The dir cache is capped at dirCacheCap entries: walking more directories than
// the cap must not grow the cache beyond it (i.e. no OOM on a full-tree find).
func TestDirCacheLRUCap(t *testing.T) {
	f, err := Format(block.NewMem(8192), nil)
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()

	n := dirCacheCap + 10
	ids := make([]uint64, 0, n)
	for i := 0; i < n; i++ {
		id, err := f.Mkdir(root, fmt.Sprintf("sub_%04d", i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}

	// A lookup inside each sub-dir forces it to be opened (populating the cache).
	for _, id := range ids {
		if _, _, err := f.Lookup(id, "nonexistent"); err != nil {
			t.Fatal(err)
		}
	}

	f.dcache.mu.Lock()
	size := len(f.dcache.entries)
	f.dcache.mu.Unlock()
	if size > dirCacheCap {
		t.Fatalf("cache size %d exceeds cap %d", size, dirCacheCap)
	}
}

// Moving a directory into its own descendant must fail with ErrInvalid — it
// would otherwise detach the subtree and leak its inodes/blocks forever.
func TestRenameIntoOwnDescendantRejected(t *testing.T) {
	f, err := Format(block.NewMem(8192), nil)
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	// Build /a/b/c.
	a, _ := f.Mkdir(root, "a")
	b, _ := f.Mkdir(a, "b")
	c, _ := f.Mkdir(b, "c")

	// mv /a -> /a/b/c/a (a into its own grandchild) must be ErrInvalid.
	if err := f.Rename(root, "a", c, "a"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("moving a dir into its own descendant: got %v, want ErrInvalid", err)
	}
	// Direct child case: mv /a -> /a/b/a is also a loop.
	if err := f.Rename(root, "a", b, "a"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("moving a dir into its own child: got %v, want ErrInvalid", err)
	}

	// Sanity: a legitimate cross-dir move of `a` into a SIBLING still works.
	sib, _ := f.Mkdir(root, "sib")
	if err := f.Rename(root, "a", sib, "a"); err != nil {
		t.Fatalf("legitimate cross-dir rename failed: %v", err)
	}
	if got, ok, _ := f.Lookup(sib, "a"); !ok || got != a {
		t.Fatalf("after move, lookup sib/a = (%d,%v), want (%d,true)", got, ok, a)
	}
	// The subtree survived intact.
	if got, ok, _ := f.Lookup(a, "b"); !ok || got != b {
		t.Fatalf("subtree lost: a/b = (%d,%v)", got, ok)
	}
	if got, ok, _ := f.Lookup(b, "c"); !ok || got != c {
		t.Fatalf("subtree lost: b/c = (%d,%v)", got, ok)
	}
}
