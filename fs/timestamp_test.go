package fs

import (
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// fixedClock installs a settable clock on f and returns a pointer to drive it.
func fixedClock(f *FS, start uint64) *uint64 {
	clk := new(uint64)
	*clk = start
	f.clock = func() uint64 { return *clk }
	return clk
}

// Create stamps all three times (a/m/ctime) to "now" (§F6).
func TestTimestampsOnCreate(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	clk := fixedClock(f, 1000)
	id, _ := f.Create(f.Root(), "a")
	in, _ := f.inodes.Get(id)
	if in.Atime != *clk || in.Mtime != *clk || in.Ctime != *clk {
		t.Fatalf("create times = a%d m%d c%d, want all %d", in.Atime, in.Mtime, in.Ctime, *clk)
	}
}

// A write bumps mtime+ctime but leaves atime (noatime read path, §F6).
func TestWriteBumpsMtime(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	clk := fixedClock(f, 1000)
	id, _ := f.Create(f.Root(), "a")
	*clk = 2000
	if err := f.WriteFile(id, []byte("data")); err != nil {
		t.Fatal(err)
	}
	in, _ := f.inodes.Get(id)
	if in.Mtime != 2000 || in.Ctime != 2000 {
		t.Fatalf("after write m%d c%d, want 2000", in.Mtime, in.Ctime)
	}
	if in.Atime != 1000 {
		t.Fatalf("atime = %d, want 1000 (unchanged by write)", in.Atime)
	}
}

// A hard link changes only ctime (link count), not mtime; unlink also bumps ctime.
func TestLinkUnlinkBumpsCtime(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	clk := fixedClock(f, 1000)
	a, _ := f.Create(root, "A")

	*clk = 2000
	if err := f.Link(root, "B", a); err != nil {
		t.Fatal(err)
	}
	in, _ := f.inodes.Get(a)
	if in.Ctime != 2000 || in.Mtime != 1000 {
		t.Fatalf("after link c%d m%d, want c2000 m1000", in.Ctime, in.Mtime)
	}

	*clk = 3000
	if err := f.Unlink(root, "A"); err != nil {
		t.Fatal(err)
	}
	in, _ = f.inodes.Get(a)
	if in.Nlink != 1 || in.Ctime != 3000 {
		t.Fatalf("after unlink nlink%d c%d, want nlink1 c3000", in.Nlink, in.Ctime)
	}
}

// Creating an entry bumps the parent directory's mtime+ctime.
func TestParentDirTimestampOnCreate(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	clk := fixedClock(f, 5000)
	if _, err := f.Create(root, "x"); err != nil {
		t.Fatal(err)
	}
	in, _ := f.inodes.Get(root)
	if in.Mtime != *clk || in.Ctime != *clk {
		t.Fatalf("parent dir m%d c%d, want %d", in.Mtime, in.Ctime, *clk)
	}
}

// Timestamps are part of the CoW snapshot and survive a remount.
func TestTimestampsSurviveRemount(t *testing.T) {
	mem := block.NewMem(4096)
	f, err := Format(mem, testKey())
	if err != nil {
		t.Fatal(err)
	}
	clk := fixedClock(f, 1234)
	id, _ := f.Create(f.Root(), "f")
	*clk = 5678
	if err := f.WriteFile(id, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := f.Fsync(); err != nil {
		t.Fatal(err)
	}

	f2, err := Mount(mem, testKey())
	if err != nil {
		t.Fatal(err)
	}
	rid, ok, _ := f2.Lookup(f2.Root(), "f")
	if !ok {
		t.Fatal("file lost across remount")
	}
	in, _ := f2.inodes.Get(rid)
	if in.Atime != 1234 || in.Mtime != 5678 || in.Ctime != 5678 {
		t.Fatalf("after remount a%d m%d c%d, want a1234 m5678 c5678", in.Atime, in.Mtime, in.Ctime)
	}
}
