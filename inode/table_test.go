package inode

import (
	"bytes"
	"testing"
)

// TestTablePutGet verifies independent slots round-trip and out-of-range ids fail.
func TestTablePutGet(t *testing.T) {
	tbl := NewTable(64)

	ids := []uint64{0, 1, 31, 32, 63}
	for _, id := range ids {
		in := &Inode{Size: id * 1000, Nlink: 1, UID: uint32(id)}
		if err := tbl.Put(id, in); err != nil {
			t.Fatalf("Put(%d): %v", id, err)
		}
	}
	for _, id := range ids {
		got, err := tbl.Get(id)
		if err != nil {
			t.Fatalf("Get(%d): %v", id, err)
		}
		if got.Size != id*1000 || got.UID != uint32(id) {
			t.Fatalf("Get(%d) = {Size:%d UID:%d}, want {Size:%d UID:%d}",
				id, got.Size, got.UID, id*1000, uint32(id))
		}
	}

	// Rewriting one slot leaves a neighbour untouched.
	if err := tbl.Put(0, &Inode{Size: 999, UID: 200}); err != nil {
		t.Fatal(err)
	}
	mate, _ := tbl.Get(1)
	if mate.Size != 1000 || mate.UID != 1 {
		t.Fatalf("neighbour clobbered: %+v", mate)
	}

	if _, err := tbl.Get(64); err != ErrOutOfRange {
		t.Fatalf("Get past capacity = %v, want ErrOutOfRange", err)
	}
	if err := tbl.Put(64, &Inode{}); err != ErrOutOfRange {
		t.Fatalf("Put past capacity = %v, want ErrOutOfRange", err)
	}
}

// TestTableHighWaterMark verifies Marshal only covers touched slots.
func TestTableHighWaterMark(t *testing.T) {
	tbl := NewTable(1024)
	if got := tbl.Marshal(); len(got) != 0 {
		t.Fatalf("empty table marshaled %d bytes, want 0", len(got))
	}
	tbl.Put(4, &Inode{Nlink: 1})
	if tbl.Count() != 5 {
		t.Fatalf("count = %d, want 5 (high-water mark id 4)", tbl.Count())
	}
	if got := tbl.Marshal(); len(got) != 5*Size {
		t.Fatalf("marshaled %d bytes, want %d", len(got), 5*Size)
	}
}

// TestTableMarshalRoundTrip verifies a table survives Marshal -> UnmarshalTable.
func TestTableMarshalRoundTrip(t *testing.T) {
	tbl := NewTable(256)
	for id := uint64(0); id < 100; id++ {
		tbl.Put(id, &Inode{Size: id, Nlink: uint32(id) + 1, Type: TypeRegular, UID: uint32(id)})
	}
	blob := tbl.Marshal()

	got, err := UnmarshalTable(blob, 256)
	if err != nil {
		t.Fatalf("UnmarshalTable: %v", err)
	}
	if got.Count() != tbl.Count() || got.Cap() != tbl.Cap() {
		t.Fatalf("count/cap mismatch: got %d/%d want %d/%d",
			got.Count(), got.Cap(), tbl.Count(), tbl.Cap())
	}
	if !bytes.Equal(got.Marshal(), blob) {
		t.Fatal("round-tripped table does not match original snapshot")
	}

	if _, err := UnmarshalTable(make([]byte, Size+1), 256); err != ErrBadInodeSize {
		t.Fatalf("ragged snapshot = %v, want ErrBadInodeSize", err)
	}
	if _, err := UnmarshalTable(make([]byte, 4*Size), 2); err != ErrOutOfRange {
		t.Fatalf("oversized snapshot = %v, want ErrOutOfRange", err)
	}
}
