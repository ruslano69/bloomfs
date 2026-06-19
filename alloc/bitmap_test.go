package alloc

import (
	"errors"
	"testing"
)

func TestAllocFreeContiguous(t *testing.T) {
	b := New(64)
	b.Reserve(0, 5) // superblock + inode table
	if b.Used() != 5 {
		t.Fatalf("Used after reserve = %d, want 5", b.Used())
	}

	a1, err := b.Alloc(3)
	if err != nil || a1 != 5 {
		t.Fatalf("Alloc(3) = %d,%v; want 5,nil", a1, err)
	}
	a2, err := b.Alloc(2)
	if err != nil || a2 != 8 {
		t.Fatalf("Alloc(2) = %d,%v; want 8,nil", a2, err)
	}
	if b.Used() != 10 {
		t.Fatalf("Used = %d, want 10", b.Used())
	}

	// Free the first run; the next Alloc(3) should reuse the freed hole at 5.
	b.Free(a1, 3)
	a3, err := b.Alloc(3)
	if err != nil || a3 != 5 {
		t.Fatalf("Alloc after free = %d,%v; want 5,nil", a3, err)
	}
}

func TestAllocNoSpace(t *testing.T) {
	b := New(10)
	if _, err := b.Alloc(20); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("expected ErrNoSpace, got %v", err)
	}
	// Fragmentation: free at 0 and 2 leaves no run of 2.
	b2 := New(4)
	b2.Reserve(1, 1)
	b2.Reserve(3, 1)
	if _, err := b2.Alloc(2); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("expected ErrNoSpace for fragmented map, got %v", err)
	}
}

func TestBitmapRoundTrip(t *testing.T) {
	b := New(200)
	b.Reserve(0, 5)
	if _, err := b.Alloc(7); err != nil {
		t.Fatal(err)
	}

	got, err := Unmarshal(b.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if got.Total() != b.Total() || got.Used() != b.Used() {
		t.Fatalf("round-trip mismatch: total %d/%d used %d/%d",
			got.Total(), b.Total(), got.Used(), b.Used())
	}
	// A reloaded bitmap must keep allocating consistently (no double-handing-out).
	a, err := got.Alloc(1)
	if err != nil || a != 12 { // 0..4 reserved, 5..11 allocated, next free = 12
		t.Fatalf("Alloc after reload = %d,%v; want 12,nil", a, err)
	}
}
