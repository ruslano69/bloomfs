package fs

import (
	"errors"
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// Rmdir removes an empty directory and recycles its inode; Unlink refuses a
// directory (EISDIR) and Rmdir refuses a regular file (ENOTDIR).
func TestRmdirAndTypeChecks(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()

	d, err := f.Mkdir(root, "d")
	if err != nil {
		t.Fatal(err)
	}
	file, err := f.Create(root, "file")
	if err != nil {
		t.Fatal(err)
	}

	if err := f.Unlink(root, "d"); !errors.Is(err, ErrIsDir) {
		t.Fatalf("Unlink dir = %v, want ErrIsDir", err)
	}
	if err := f.Rmdir(root, "file"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("Rmdir file = %v, want ErrNotDir", err)
	}

	if err := f.Rmdir(root, "d"); err != nil {
		t.Fatalf("Rmdir empty dir: %v", err)
	}
	if _, ok, _ := f.Lookup(root, "d"); ok {
		t.Fatal("d still present after Rmdir")
	}
	// The reclaimed dir id is recycled for the next allocation.
	again, err := f.Create(root, "again")
	if err != nil {
		t.Fatal(err)
	}
	if again != d {
		t.Fatalf("reclaimed id not reused: got %d, freed %d", again, d)
	}
	_ = file
}

// Rmdir refuses a non-empty directory (ENOTEMPTY).
func TestRmdirNotEmpty(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	d, _ := f.Mkdir(root, "d")
	if _, err := f.Create(d, "inner"); err != nil {
		t.Fatal(err)
	}
	if err := f.Rmdir(root, "d"); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("Rmdir non-empty = %v, want ErrNotEmpty", err)
	}
	// Emptying it then removing succeeds.
	if err := f.Unlink(d, "inner"); err != nil {
		t.Fatal(err)
	}
	if err := f.Rmdir(root, "d"); err != nil {
		t.Fatalf("Rmdir after empty: %v", err)
	}
}

// A subdirectory bumps its parent's link count (its ".."); Rmdir drops it again.
func TestDirParentNlink(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()

	nlink := func(id uint64) uint32 {
		in, err := f.inodes.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		return in.Nlink
	}

	if got := nlink(root); got != 2 {
		t.Fatalf("fresh root nlink = %d, want 2", got)
	}
	a, _ := f.Mkdir(root, "a")
	if got := nlink(root); got != 3 {
		t.Fatalf("root nlink after mkdir = %d, want 3", got)
	}
	if got := nlink(a); got != 2 {
		t.Fatalf("new dir nlink = %d, want 2", got)
	}
	// A regular file does NOT bump the parent's link count.
	if _, err := f.Create(root, "f"); err != nil {
		t.Fatal(err)
	}
	if got := nlink(root); got != 3 {
		t.Fatalf("root nlink after create file = %d, want 3", got)
	}
	if err := f.Rmdir(root, "a"); err != nil {
		t.Fatal(err)
	}
	if got := nlink(root); got != 2 {
		t.Fatalf("root nlink after rmdir = %d, want 2", got)
	}
}

// Renaming a directory into itself is rejected (the trivial loop).
func TestRenameDirIntoItself(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	a, _ := f.Mkdir(root, "a")
	if err := f.Rename(root, "a", a, "a"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rename dir into itself = %v, want ErrInvalid", err)
	}
}

// Chmod on a directory must survive a later directory write (a create inside it):
// the open-directory cache and the inode table stay coherent (§F #4). It also
// survives a remount.
func TestChmodDirCoherentWithCache(t *testing.T) {
	dev := block.NewMem(8192)
	f, err := Format(dev, testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	d, _ := f.Mkdir(root, "d")

	// Cache the directory (Lookup opens it), then chmod it.
	if _, _, err := f.Lookup(root, "d"); err != nil {
		t.Fatal(err)
	}
	if err := f.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	// A subsequent write to the same directory must NOT revert the mode.
	if _, err := f.Create(d, "inside"); err != nil {
		t.Fatal(err)
	}
	if in, _ := f.inodes.Get(d); in.Mode != 0o700 {
		t.Fatalf("dir mode after create = %#o, want 0700 (cache clobbered the chmod)", in.Mode)
	}

	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	g, err := Mount(dev, testKey())
	if err != nil {
		t.Fatal(err)
	}
	if in, _ := g.inodes.Get(d); in.Mode != 0o700 {
		t.Fatalf("dir mode after remount = %#o, want 0700", in.Mode)
	}
}

// Chown/Utimes set fields and bump ctime; values survive a remount.
func TestChownUtimes(t *testing.T) {
	dev := block.NewMem(4096)
	f, err := Format(dev, testKey())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := f.Create(f.Root(), "f")

	if err := f.Chown(id, 1000, 65534); err != nil {
		t.Fatal(err)
	}
	if err := f.Utimes(id, 111, 222); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	g, _ := Mount(dev, testKey())
	in, err := g.inodes.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if in.UID != 1000 || in.GID != 65534 {
		t.Fatalf("owner = %d/%d, want 1000/65534", in.UID, in.GID)
	}
	if in.Atime != 111 || in.Mtime != 222 {
		t.Fatalf("times = %d/%d, want 111/222", in.Atime, in.Mtime)
	}
	if in.Ctime == 0 {
		t.Fatal("ctime not bumped by setattr")
	}
}
