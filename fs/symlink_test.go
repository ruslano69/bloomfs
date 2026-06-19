package fs

import (
	"testing"

	"github.com/ruslano69/bloomfs/block"
	"github.com/ruslano69/bloomfs/inode"
)

// A symlink stores its target as data and round-trips through a remount; its
// stat reports TypeLink with the target length as size.
func TestSymlinkRoundtrip(t *testing.T) {
	dev := block.NewMem(8192)
	f, err := Format(dev, testKey())
	if err != nil {
		t.Fatal(err)
	}
	const target = "../some/where/file.txt"
	id, err := f.Symlink(f.Root(), "link", target)
	if err != nil {
		t.Fatal(err)
	}

	got, err := f.Readlink(id)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("readlink = %q, want %q", got, target)
	}
	st, err := f.Stat(id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Type != inode.TypeLink || st.Size != uint64(len(target)) {
		t.Fatalf("stat = %+v, want type=link size=%d", st, len(target))
	}

	// Lookup must resolve the link by name, and it must survive a remount.
	lid, ok, err := f.Lookup(f.Root(), "link")
	if err != nil || !ok || lid != id {
		t.Fatalf("lookup link = (%d,%v,%v), want (%d,true,nil)", lid, ok, err, id)
	}
	if err := f.Fsync(); err != nil {
		t.Fatal(err)
	}
	f2, err := Mount(dev, testKey())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f2.Readlink(id); err != nil || got != target {
		t.Fatalf("readlink after remount = (%q,%v), want %q", got, err, target)
	}
}

// Readlink rejects non-symlinks; an empty target is invalid.
func TestSymlinkErrors(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := f.Create(f.Root(), "reg")
	if _, err := f.Readlink(reg); err != ErrInvalid {
		t.Fatalf("readlink(regular) = %v, want ErrInvalid", err)
	}
	if _, err := f.Symlink(f.Root(), "empty", ""); err != ErrInvalid {
		t.Fatalf("symlink(empty target) = %v, want ErrInvalid", err)
	}
}

// Unlinking a symlink frees its inode and its target data (it is not a directory,
// so it goes through the regular unlink/reclaim path).
func TestSymlinkUnlink(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	before := f.StatFS().FilesFree
	id, err := f.Symlink(f.Root(), "link", "/target")
	if err != nil {
		t.Fatal(err)
	}
	if f.StatFS().FilesFree != before-1 {
		t.Fatalf("FilesFree after symlink = %d, want %d", f.StatFS().FilesFree, before-1)
	}
	if err := f.Unlink(f.Root(), "link"); err != nil {
		t.Fatal(err)
	}
	if f.StatFS().FilesFree != before {
		t.Fatalf("FilesFree after unlink = %d, want %d (reclaimed)", f.StatFS().FilesFree, before)
	}
	// The id's slot is reclaimed (zeroed to an empty regular inode), so a stale
	// Readlink no longer sees a symlink.
	if _, err := f.Readlink(id); err != ErrInvalid {
		t.Fatalf("readlink after unlink = %v, want ErrInvalid", err)
	}
}

// StatFS reports sane capacity: block size, a non-zero block count with free
// <= total, and an inode-free count that drops as files are created.
func TestStatFS(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	s := f.StatFS()
	if s.BlockSize != block.Size {
		t.Fatalf("BlockSize = %d, want %d", s.BlockSize, block.Size)
	}
	if s.Blocks == 0 || s.BlocksFree > s.Blocks {
		t.Fatalf("blocks = %d free = %d, want 0 < free <= total", s.Blocks, s.BlocksFree)
	}
	if s.Files == 0 || s.FilesFree > s.Files {
		t.Fatalf("files = %d free = %d, want 0 < free <= total", s.Files, s.FilesFree)
	}

	beforeFree := s.FilesFree
	beforeBlocks := f.StatFS().BlocksFree
	id, _ := f.Create(f.Root(), "f")
	if err := f.WriteFile(id, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	after := f.StatFS()
	if after.FilesFree != beforeFree-1 {
		t.Fatalf("FilesFree = %d, want %d after one create", after.FilesFree, beforeFree-1)
	}
	if after.BlocksFree >= beforeBlocks {
		t.Fatalf("BlocksFree = %d, want < %d after a 4K write", after.BlocksFree, beforeBlocks)
	}
}
