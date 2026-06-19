package fs

import (
	"bytes"
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// POSIX compliance cases (§E). These are session-level (no crash injection):
// full crash-atomicity of the inode-table changes awaits CoW of the inode table
// (see the note in fs.go and §E of the spec).

// E1: rename moves a name; the inode and content are unchanged.
func TestRename(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	id, _ := f.Create(root, "old.txt")
	if err := f.WriteFile(id, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := f.Rename(root, "old.txt", root, "new.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, ok, _ := f.Lookup(root, "old.txt"); ok {
		t.Fatal("old name still present after rename")
	}
	nid, ok, _ := f.Lookup(root, "new.txt")
	if !ok || nid != id {
		t.Fatalf("new name -> %d ok=%v, want %d", nid, ok, id)
	}
	got, _ := f.ReadFile(nid)
	if string(got) != "hello" {
		t.Fatalf("content lost across rename: %q", got)
	}
}

// E2: rename onto an existing name replaces it; the old target is reclaimed.
func TestRenameOverwrite(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	a, _ := f.Create(root, "a")
	f.WriteFile(a, []byte("AAA"))
	b, _ := f.Create(root, "b")
	f.WriteFile(b, []byte("BBB"))

	if err := f.Rename(root, "a", root, "b"); err != nil {
		t.Fatalf("rename overwrite: %v", err)
	}
	if _, ok, _ := f.Lookup(root, "a"); ok {
		t.Fatal("source name still present")
	}
	bid, ok, _ := f.Lookup(root, "b")
	if !ok || bid != a {
		t.Fatalf("dst should now be inode a (%d), got %d ok=%v", a, bid, ok)
	}
	if got, _ := f.ReadFile(bid); string(got) != "AAA" {
		t.Fatalf("overwrite content wrong: %q", got)
	}
	if old, _ := f.inodes.Get(b); old.Nlink != 0 || old.Size != 0 {
		t.Fatalf("old target not reclaimed: %+v", old)
	}
}

// E3: a file unlinked while open stays readable through its handle until Close.
func TestUnlinkOpenFile(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	id, _ := f.Create(root, "movie.mkv")
	f.WriteFile(id, []byte("frames"))

	h, err := f.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Unlink(root, "movie.mkv"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, ok, _ := f.Lookup(root, "movie.mkv"); ok {
		t.Fatal("name still in directory after unlink")
	}
	data, err := h.Read()
	if err != nil || string(data) != "frames" {
		t.Fatalf("read via handle after unlink failed: %q err=%v", data, err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if in, _ := f.inodes.Get(id); in.Nlink != 0 || in.Size != 0 {
		t.Fatalf("inode not reclaimed after last close: %+v", in)
	}
}

// E4: hard links — two names share one inode; the data lives until the last name
// is unlinked. No hard links to directories.
func TestHardlink(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	a, _ := f.Create(root, "A")
	f.WriteFile(a, []byte("shared"))

	if err := f.Link(root, "B", a); err != nil {
		t.Fatalf("link: %v", err)
	}
	if bid, ok, _ := f.Lookup(root, "B"); !ok || bid != a {
		t.Fatalf("B not linked to A's inode: %d ok=%v", bid, ok)
	}
	if in, _ := f.inodes.Get(a); in.Nlink != 2 {
		t.Fatalf("Nlink = %d, want 2", in.Nlink)
	}

	// unlink A: B still reads the shared content.
	f.Unlink(root, "A")
	if got, _ := f.ReadFile(a); string(got) != "shared" {
		t.Fatalf("content lost after unlinking one of two links: %q", got)
	}
	if in, _ := f.inodes.Get(a); in.Nlink != 1 {
		t.Fatalf("Nlink after one unlink = %d, want 1", in.Nlink)
	}

	// unlink B: last link gone -> reclaimed.
	f.Unlink(root, "B")
	if in, _ := f.inodes.Get(a); in.Nlink != 0 || in.Size != 0 {
		t.Fatalf("not reclaimed after both unlinks: %+v", in)
	}

	// hard link to a directory must fail.
	d, _ := f.Mkdir(root, "dir")
	if err := f.Link(root, "dirlink", d); err == nil {
		t.Fatal("hard link to directory should be rejected")
	}
}

// E7: uncommitted changes vanish after a crash (the "no grey zone" half of fsync
// durability). The inode table, bitmap and dedup table are one CoW snapshot, and
// clusters freed during an uncommitted transaction are pinned until commit
// (deferred-free, §F1) — so neither a newly created inode nor an uncommitted
// overwrite of committed data leaves a trace on remount.
func TestUncommittedChangesVanish(t *testing.T) {
	mem := block.NewMem(4096)
	f, err := Format(mem, testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()

	// Committed baseline: one file with content "v1".
	keep, _ := f.Create(root, "keep.txt")
	if err := f.WriteFile(keep, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := f.Fsync(); err != nil {
		t.Fatal(err)
	}

	// Uncommitted: a brand-new file (its inode + name used to leak via the
	// in-place inode table) AND an overwrite of the committed file (which frees
	// the committed extent — without deferred-free the new write would reuse and
	// clobber it on disk, corrupting the rollback target).
	ghost, _ := f.Create(root, "ghost.txt")
	if err := f.WriteFile(keep, []byte("v2-uncommitted")); err != nil {
		t.Fatal(err)
	}
	// No commit here — simulate power loss by remounting the raw device.

	f2, err := Mount(mem, testKey())
	if err != nil {
		t.Fatalf("remount: %v", err)
	}
	if _, ok, _ := f2.Lookup(f2.Root(), "ghost.txt"); ok {
		t.Fatal("uncommitted file survived the crash (grey zone)")
	}
	if in, err := f2.inodes.Get(ghost); err == nil && in.Nlink != 0 {
		t.Fatalf("uncommitted inode %d leaked to disk: %+v", ghost, in)
	}
	kid, ok, _ := f2.Lookup(f2.Root(), "keep.txt")
	if !ok {
		t.Fatal("committed file lost across crash")
	}
	if got, _ := f2.ReadFile(kid); !bytes.Equal(got, []byte("v1")) {
		t.Fatalf("uncommitted overwrite leaked into committed state: %q", got)
	}
}

// E7: data committed via Fsync survives a remount (durability of the committed
// state). Together with TestUncommittedChangesVanish this gives the strong
// fsync guarantee — committed state durable, uncommitted state gone (§E).
func TestFsyncDurability(t *testing.T) {
	mem := block.NewMem(4096)
	f, err := Format(mem, testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	id, _ := f.Create(root, "data.db")
	if err := f.WriteFile(id, []byte("committed")); err != nil {
		t.Fatal(err)
	}
	if err := f.Fsync(); err != nil {
		t.Fatalf("fsync: %v", err)
	}

	// Remount from the same device (simulated reboot): committed data is there.
	f2, err := Mount(mem, testKey())
	if err != nil {
		t.Fatalf("remount: %v", err)
	}
	rid, ok, _ := f2.Lookup(f2.Root(), "data.db")
	if !ok {
		t.Fatal("fsync'd file lost across remount")
	}
	if got, _ := f2.ReadFile(rid); !bytes.Equal(got, []byte("committed")) {
		t.Fatalf("fsync'd content wrong after remount: %q", got)
	}
}
