package fs

import (
	"bytes"
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

func testKey() []byte {
	k := make([]byte, 64) // AES-256-XTS
	for i := range k {
		k[i] = byte(i*7 + 1)
	}
	return k
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

// TestEndToEndPersist is the headline Stage D check: build a directory tree with
// file content, commit, remount from scratch, and read everything back — the
// whole stack (dir + inode + store + cow) working as one persistent filesystem.
func TestEndToEndPersist(t *testing.T) {
	dev := block.NewMem(4096)
	key := testKey()

	f, err := Format(dev, key)
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	root := f.Root()

	docs, err := f.Mkdir(root, "docs")
	if err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	readme, err := f.Create(docs, "readme.txt")
	if err != nil {
		t.Fatalf("create readme: %v", err)
	}
	content := bytes.Repeat([]byte("BloomFS rocks. "), 5000) // ~73 KB
	if err := f.WriteFile(readme, content); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	notes, err := f.Create(root, "notes.md")
	if err != nil {
		t.Fatalf("create notes: %v", err)
	}
	if err := f.WriteFile(notes, []byte("# notes\n")); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	if err := f.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Remount from the same device; nothing in RAM carries over.
	f2, err := Mount(dev, key)
	if err != nil {
		t.Fatalf("remount: %v", err)
	}

	names, err := f2.Readdir(f2.Root())
	if err != nil {
		t.Fatalf("readdir root: %v", err)
	}
	if !contains(names, "docs") || !contains(names, "notes.md") {
		t.Fatalf("root listing wrong after remount: %v", names)
	}

	docs2, ok, err := f2.Lookup(f2.Root(), "docs")
	if err != nil || !ok {
		t.Fatalf("lookup docs: ok=%v err=%v", ok, err)
	}
	readme2, ok, err := f2.Lookup(docs2, "readme.txt")
	if err != nil || !ok {
		t.Fatalf("lookup readme: ok=%v err=%v", ok, err)
	}
	got, err := f2.ReadFile(readme2)
	if err != nil {
		t.Fatalf("read readme: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch after remount: got %d bytes, want %d", len(got), len(content))
	}
}

// TestDedupThroughFS verifies that two files with identical content share storage
// when written through the filesystem API.
func TestDedupThroughFS(t *testing.T) {
	dev := block.NewMem(4096)
	f, err := Format(dev, testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()

	blob := bytes.Repeat([]byte{0x5A}, 40*1024)
	a, _ := f.Create(root, "a.bin")
	b, _ := f.Create(root, "b.bin")

	if err := f.WriteFile(a, blob); err != nil {
		t.Fatal(err)
	}
	before := f.bs.UniqueBlocks()
	if err := f.WriteFile(b, blob); err != nil { // identical content
		t.Fatal(err)
	}
	if f.bs.UniqueBlocks() != before {
		t.Fatalf("identical content not deduplicated: unique %d -> %d", before, f.bs.UniqueBlocks())
	}
	for _, id := range []uint64{a, b} {
		got, err := f.ReadFile(id)
		if err != nil || !bytes.Equal(got, blob) {
			t.Fatalf("dedup read mismatch for inode %d (err=%v)", id, err)
		}
	}
}

// TestUnlink removes an entry and confirms it is gone from lookup and listing.
func TestUnlink(t *testing.T) {
	dev := block.NewMem(4096)
	f, err := Format(dev, testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()

	id, _ := f.Create(root, "tmp")
	if err := f.WriteFile(id, []byte("temporary")); err != nil {
		t.Fatal(err)
	}
	if err := f.Unlink(root, "tmp"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, ok, _ := f.Lookup(root, "tmp"); ok {
		t.Fatal("entry still present after unlink")
	}
	if names, _ := f.Readdir(root); contains(names, "tmp") {
		t.Fatalf("unlinked name still listed: %v", names)
	}
}

// TestPlaintextPool exercises the §5.5 opt-out: a pool formatted with a nil key
// stores compressed-but-unencrypted data and round-trips across remount.
func TestPlaintextPool(t *testing.T) {
	dev := block.NewMem(4096)
	f, err := Format(dev, nil)
	if err != nil {
		t.Fatalf("format plaintext: %v", err)
	}
	id, _ := f.Create(f.Root(), "x")
	data := []byte("no encryption here, just compression")
	if err := f.WriteFile(id, data); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}

	f2, err := Mount(dev, nil)
	if err != nil {
		t.Fatalf("remount plaintext: %v", err)
	}
	xid, ok, _ := f2.Lookup(f2.Root(), "x")
	if !ok {
		t.Fatal("x not found after remount")
	}
	got, err := f2.ReadFile(xid)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("plaintext round-trip mismatch: err=%v", err)
	}
}
