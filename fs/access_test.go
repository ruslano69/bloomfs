package fs

import (
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// Access implements the standard owner/group/other DAC algorithm, with root
// bypassing read/write.
func TestAccessMatrix(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := f.Create(f.Root(), "f") // created 0o644
	if err := f.Chown(id, 1000, 2000); err != nil {
		t.Fatal(err)
	}
	if err := f.Chmod(id, 0o640); err != nil { // rw- r-- ---
		t.Fatal(err)
	}

	cases := []struct {
		name          string
		uid, gid      uint32
		gids          []uint32
		mask          uint32
		wantPermitted bool
	}{
		{"owner read", 1000, 9, nil, accessR, true},
		{"owner write", 1000, 9, nil, accessW, true},
		{"owner exec", 1000, 9, nil, accessX, false}, // no x bit
		{"group read (primary)", 7, 2000, nil, accessR, true},
		{"group write", 7, 2000, nil, accessW, false}, // group is r-- only
		{"group read (supplementary)", 7, 9, []uint32{2000}, accessR, true},
		{"other read", 7, 9, nil, accessR, false}, // other is ---
		{"existence (mask 0)", 7, 9, nil, 0, true},
		{"root read", 0, 0, nil, accessR, true},
		{"root write", 0, 0, nil, accessW, true},
		{"root exec on non-exec file", 0, 0, nil, accessX, false},
	}
	for _, c := range cases {
		err := f.Access(id, c.mask, c.uid, c.gid, c.gids)
		permitted := err == nil
		if permitted != c.wantPermitted {
			t.Errorf("%s: Access err=%v (permitted=%v), want permitted=%v", c.name, err, permitted, c.wantPermitted)
		}
		if !permitted && err != ErrPermission {
			t.Errorf("%s: denial err=%v, want ErrPermission", c.name, err)
		}
	}

	// root may execute a file that has at least one x bit.
	if err := f.Chmod(id, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := f.Access(id, accessX, 0, 0, nil); err != nil {
		t.Errorf("root exec on 0711 = %v, want permitted", err)
	}

	// Access on a missing inode surfaces the lookup error, not ErrPermission.
	if err := f.Access(99999, accessR, 0, 0, nil); err == nil || err == ErrPermission {
		t.Errorf("Access(missing) = %v, want a lookup error", err)
	}
}

// A file unlinked while a handle is open survives until the handle is closed
// (POSIX unlink-of-open), and its inode is reclaimed only on the last Close —
// this is the path the FUSE binding now exercises by opening a handle per file.
func TestUnlinkWhileOpen(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := f.Create(f.Root(), "f")
	if err := f.WriteFile(id, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	beforeFree := f.StatFS().FilesFree

	h, err := f.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Unlink(f.Root(), "f"); err != nil {
		t.Fatal(err)
	}
	// Name is gone, but the open handle keeps the data readable.
	if _, ok, _ := f.Lookup(f.Root(), "f"); ok {
		t.Fatal("name still resolves after unlink")
	}
	data, err := h.Read()
	if err != nil || string(data) != "payload" {
		t.Fatalf("read through handle after unlink = (%q,%v), want payload", data, err)
	}
	if f.StatFS().FilesFree != beforeFree {
		t.Fatalf("inode reclaimed while still open (FilesFree=%d, want %d)", f.StatFS().FilesFree, beforeFree)
	}

	// Last close reclaims the inode.
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if f.StatFS().FilesFree != beforeFree+1 {
		t.Fatalf("inode not reclaimed on close (FilesFree=%d, want %d)", f.StatFS().FilesFree, beforeFree+1)
	}
}
