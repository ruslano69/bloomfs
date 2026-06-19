package inode

import (
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// TestStorePacking verifies that inodes packed into shared blocks do not clobber
// each other and that ids crossing block boundaries land correctly.
func TestStorePacking(t *testing.T) {
	dev := block.NewMem(16)
	s := NewStore(dev, 1) // inode table starts at block 1

	// ids 0 and 31 share block 1; 32 and 33 are in block 2.
	ids := []uint64{0, 31, 32, 33}
	for _, id := range ids {
		in := &Inode{Size: id * 1000, Nlink: 1, UID: byte(id)}
		if err := s.Put(id, in); err != nil {
			t.Fatalf("Put(%d): %v", id, err)
		}
	}

	for _, id := range ids {
		got, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%d): %v", id, err)
		}
		if got.Size != id*1000 || got.UID != byte(id) {
			t.Fatalf("Get(%d) = {Size:%d UID:%d}, want {Size:%d UID:%d}",
				id, got.Size, got.UID, id*1000, byte(id))
		}
	}

	// Rewrite id 0 and confirm its block-mate id 31 is untouched.
	if err := s.Put(0, &Inode{Size: 999, UID: 200}); err != nil {
		t.Fatal(err)
	}
	mate, err := s.Get(31)
	if err != nil {
		t.Fatal(err)
	}
	if mate.Size != 31000 || mate.UID != 31 {
		t.Fatalf("block-mate id 31 clobbered: %+v", mate)
	}
}
