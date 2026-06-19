package fs

import (
	"fmt"
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// A reclaimed inode id is handed back out by the next allocation (§F5), instead
// of leaking under the bump allocator.
func TestInodeReuse(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	a, _ := f.Create(root, "a")
	b, _ := f.Create(root, "b")
	if a == b {
		t.Fatal("distinct files got the same id")
	}
	if err := f.Unlink(root, "a"); err != nil { // reclaims a's id
		t.Fatal(err)
	}
	c, _ := f.Create(root, "c")
	if c != a {
		t.Fatalf("expected reclaimed id %d to be reused, got %d", a, c)
	}
}

// Reusing an id bumps the inode generation, so a stale handle can't silently
// alias the new file.
func TestInodeReuseBumpsGeneration(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	a, _ := f.Create(root, "a")
	g0, _ := f.inodes.Get(a)
	f.Unlink(root, "a")
	c, _ := f.Create(root, "c")
	if c != a {
		t.Fatalf("id not reused: %d vs %d", c, a)
	}
	g1, _ := f.inodes.Get(c)
	if g1.Generation != g0.Generation+1 {
		t.Fatalf("generation = %d, want %d (bumped)", g1.Generation, g0.Generation+1)
	}
}

// The free-list is reconstructed at Mount from the committed table, so a freed id
// is reused even across a remount.
func TestInodeReuseAcrossRemount(t *testing.T) {
	mem := block.NewMem(4096)
	f, err := Format(mem, testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	a, _ := f.Create(root, "a")
	f.Create(root, "b")
	if err := f.Unlink(root, "a"); err != nil { // reclaim a
		t.Fatal(err)
	}
	if err := f.Fsync(); err != nil {
		t.Fatal(err)
	}

	f2, err := Mount(mem, testKey())
	if err != nil {
		t.Fatal(err)
	}
	c, _ := f2.Create(f2.Root(), "c")
	if c != a {
		t.Fatalf("after remount expected reuse of id %d, got %d", a, c)
	}
}

// Churn (create then unlink, repeatedly) must not grow the id space: ids are
// recycled, so NextInode stays bounded.
func TestInodeNoLeakUnderChurn(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	for i := 0; i < 500; i++ {
		name := fmt.Sprintf("tmp%d", i)
		id, err := f.Create(root, name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := f.WriteFile(id, []byte(name)); err != nil {
			t.Fatal(err)
		}
		if err := f.Unlink(root, name); err != nil {
			t.Fatal(err)
		}
	}
	// One live id was ever needed at a time, so the bump allocator should have
	// stopped almost immediately (root=0, first file=1, everything after reused).
	if f.ub.NextInode > 3 {
		t.Fatalf("NextInode = %d after 500 create/unlink cycles; ids are leaking", f.ub.NextInode)
	}
}
