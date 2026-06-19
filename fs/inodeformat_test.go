package fs

import (
	"errors"
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// The widened inode format (u32 uid/gid, u16 mode) must round-trip through a CoW
// commit + remount: a u8 field could hold neither real uids nor full mode bits.
func TestInodeFormatWideFields(t *testing.T) {
	dev := block.NewMem(4096)
	f, err := Format(dev, testKey())
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.Create(f.Root(), "f")
	if err != nil {
		t.Fatal(err)
	}
	in, _ := f.inodes.Get(id)
	in.UID, in.GID, in.Mode = 1000, 65534, 0o4755 // setuid + rwxr-xr-x: all > u8 range
	f.inodes.Put(id, in)
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}

	g, err := Mount(dev, testKey())
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.inodes.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != 1000 || got.GID != 65534 || got.Mode != 0o4755 {
		t.Fatalf("after remount: uid=%d gid=%d mode=%#o, want 1000/65534/04755",
			got.UID, got.GID, got.Mode)
	}
}

// allocInode stops cleanly with ErrNoInodes when the fixed-capacity table is
// exhausted, instead of failing later with an opaque out-of-range Put.
func TestInodeCeiling(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.ub.NextInode = f.inodes.Cap() // simulate a full table
	_, err = f.allocInode()
	f.mu.Unlock()
	if !errors.Is(err, ErrNoInodes) {
		t.Fatalf("allocInode at capacity = %v, want ErrNoInodes", err)
	}
}
