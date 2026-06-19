package store

import (
	"bytes"
	"testing"

	"github.com/ruslano69/bloomfs/alloc"
	"github.com/ruslano69/bloomfs/block"
	"github.com/ruslano69/bloomfs/layout"
)

// newStore formats an in-memory image and returns a BlockStore plus the device.
func newStore(t *testing.T) (*BlockStore, block.Device) {
	t.Helper()
	dev := block.NewMem(512)
	sb, err := layout.Format(dev, 1000)
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	a := alloc.New(dev.Blocks())
	a.Reserve(0, sb.DataStart) // keep the allocator out of metadata

	key := make([]byte, 64) // AES-256-XTS
	for i := range key {
		key[i] = byte(i + 1)
	}
	bs, err := New(dev, a, key)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(bs.Close)
	return bs, dev
}

func repeat(b byte, n int) []byte {
	d := make([]byte, n)
	for i := range d {
		d[i] = b
	}
	return d
}

func TestWriteReadRoundTrip(t *testing.T) {
	bs, _ := newStore(t)

	// Compressible 32 KiB block (recordsize 32K, §4.5).
	data := repeat('A', 32*1024)
	ref, err := bs.Write(data)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if ref.Raw {
		t.Fatal("expected highly compressible data to be stored compressed")
	}
	if ref.Payload >= ref.Logical {
		t.Fatalf("compression did not shrink: payload=%d logical=%d", ref.Payload, ref.Logical)
	}
	got, err := bs.Read(ref)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("round-trip data mismatch")
	}
}

func TestEncryptedOnDisk(t *testing.T) {
	bs, dev := newStore(t)
	data := repeat('Z', 8*1024)
	ref, err := bs.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	// The first stored cluster must not equal the plaintext prefix.
	raw, _ := dev.ReadBlock(ref.Start)
	if bytes.Equal(raw[:64], data[:64]) {
		t.Fatal("on-disk cluster equals plaintext — not encrypted")
	}
}

func TestDedup(t *testing.T) {
	bs, _ := newStore(t)

	data := repeat('Q', 16*1024)
	r1, _ := bs.Write(data)
	r2, _ := bs.Write(data) // identical content
	if bs.UniqueBlocks() != 1 {
		t.Fatalf("UniqueBlocks = %d, want 1 (dedup)", bs.UniqueBlocks())
	}
	if r1.Start != r2.Start || r1.Hash != r2.Hash {
		t.Fatalf("dedup refs differ: %+v vs %+v", r1, r2)
	}

	// A different block must be stored separately.
	other := repeat('W', 16*1024)
	r3, _ := bs.Write(other)
	if bs.UniqueBlocks() != 2 || r3.Start == r1.Start {
		t.Fatalf("distinct block not stored separately: unique=%d", bs.UniqueBlocks())
	}

	// Release semantics: first release keeps the block (refcount 2->1),
	// both reads still work; second release frees it.
	bs.Release(r1)
	if _, err := bs.Read(r2); err != nil {
		t.Fatalf("read after one release failed: %v", err)
	}
	if bs.UniqueBlocks() != 2 {
		t.Fatalf("block freed too early: unique=%d", bs.UniqueBlocks())
	}
	bs.Release(r2)
	if bs.UniqueBlocks() != 1 {
		t.Fatalf("block not freed on last release: unique=%d", bs.UniqueBlocks())
	}
}

func TestIncompressibleStoredRaw(t *testing.T) {
	bs, _ := newStore(t)
	// Pseudo-random, incompressible payload.
	data := make([]byte, 8*1024)
	x := uint32(2463534242)
	for i := range data {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		data[i] = byte(x)
	}
	ref, err := bs.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	if !ref.Raw {
		t.Fatal("incompressible data should be stored raw (§B9)")
	}
	got, err := bs.Read(ref)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("raw round-trip failed: err=%v", err)
	}
}

func TestCorruptionDetected(t *testing.T) {
	bs, dev := newStore(t)
	data := repeat('K', 8*1024)
	ref, err := bs.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the stored cluster (simulated bit-rot, §B13).
	blk, _ := dev.ReadBlock(ref.Start)
	blk[10] ^= 0xFF
	_ = dev.WriteBlock(ref.Start, blk)

	if _, err := bs.Read(ref); err == nil {
		t.Fatal("expected corruption to be detected by content-hash check")
	}
}
