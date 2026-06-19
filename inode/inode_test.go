package inode

import (
	"errors"
	"reflect"
	"testing"
)

func sample() *Inode {
	in := &Inode{
		Size:           123456,
		Nlink:          2,
		Generation:     7,
		UID:            1000, // > 255: would not survive a u8 field (§4.1)
		GID:            65534,
		Type:           TypeRegular,
		Mode:           0o644, // full POSIX mode bits (> 0o377 needs > u8)
		RecordSizeLog2: 15,    // 32 KiB recordsize (§4.5)
		Flags:          FlagCompressed | FlagEncrypted,
		Atime:          1000,
		Mtime:          2000,
		Ctime:          3000,
		Checksum:       [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
	}
	return in
}

func TestMarshalSize(t *testing.T) {
	b, err := sample().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != Size {
		t.Fatalf("encoded length = %d, want %d", len(b), Size)
	}
}

func TestRoundTrip(t *testing.T) {
	in := sample()
	if err := in.SetExtents([]Extent{
		{Start: 100, Blocks: 8, Logical: 32768},
		{Start: 200, Blocks: 3, Logical: 12000},
	}); err != nil {
		t.Fatal(err)
	}

	b, _ := in.MarshalBinary()
	var got Inode
	if err := got.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, &got) {
		t.Fatalf("round-trip mismatch:\n have %+v\n want %+v", &got, in)
	}

	exts := got.Extents()
	if len(exts) != 2 || exts[0].Start != 100 || exts[1].Blocks != 3 {
		t.Fatalf("inline extents decoded wrong: %+v", exts)
	}
	if got.Flags&FlagInlineExtents == 0 {
		t.Fatal("FlagInlineExtents not set after SetExtents")
	}
}

func TestUnmarshalBadSize(t *testing.T) {
	var in Inode
	if err := in.UnmarshalBinary(make([]byte, Size-1)); !errors.Is(err, ErrBadInodeSize) {
		t.Fatalf("expected ErrBadInodeSize, got %v", err)
	}
}

func TestTooManyExtents(t *testing.T) {
	in := sample()
	too := make([]Extent, InlineCap+1)
	for i := range too {
		too[i] = Extent{Start: uint64(i + 1), Blocks: 1, Logical: 1}
	}
	if err := in.SetExtents(too); !errors.Is(err, ErrTooManyExtents) {
		t.Fatalf("expected ErrTooManyExtents, got %v", err)
	}
}
