package block

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

func fill(b byte) []byte {
	d := make([]byte, Size)
	for i := range d {
		d[i] = b
	}
	return d
}

func testRoundTrip(t *testing.T, d Device) {
	t.Helper()
	if err := d.WriteBlock(0, fill(0xAA)); err != nil {
		t.Fatalf("write block 0: %v", err)
	}
	if err := d.WriteBlock(d.Blocks()-1, fill(0x55)); err != nil {
		t.Fatalf("write last block: %v", err)
	}
	got, err := d.ReadBlock(0)
	if err != nil || !bytes.Equal(got, fill(0xAA)) {
		t.Fatalf("read block 0 mismatch (err=%v)", err)
	}
	got, err = d.ReadBlock(d.Blocks() - 1)
	if err != nil || !bytes.Equal(got, fill(0x55)) {
		t.Fatalf("read last block mismatch (err=%v)", err)
	}

	if _, err := d.ReadBlock(d.Blocks()); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("expected ErrOutOfRange, got %v", err)
	}
	if err := d.WriteBlock(0, []byte{1, 2, 3}); !errors.Is(err, ErrShortData) {
		t.Fatalf("expected ErrShortData, got %v", err)
	}
}

func TestMemDevice(t *testing.T)  { testRoundTrip(t, NewMem(8)) }

func TestFileDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "img.bin")
	d, err := Create(path, 8)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	testRoundTrip(t, d)
	if err := d.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen and confirm the bytes persisted across close.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d2.Close()
	if d2.Blocks() != 8 {
		t.Fatalf("Blocks after reopen = %d, want 8", d2.Blocks())
	}
	got, err := d2.ReadBlock(0)
	if err != nil || !bytes.Equal(got, fill(0xAA)) {
		t.Fatalf("persisted block 0 mismatch (err=%v)", err)
	}
}
