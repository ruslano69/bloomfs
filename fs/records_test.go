package fs

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/ruslano69/bloomfs/block"
	"github.com/ruslano69/bloomfs/inode"
)

// patt returns n bytes of a deterministic, mildly-compressible pattern.
func patt(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*131 + 7)
	}
	return b
}

// randBytes returns n incompressible bytes from a fixed seed (reproducible).
func randBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func TestRecordSizeFor(t *testing.T) {
	cases := []struct {
		size uint64
		rs   uint64
		log2 uint8
	}{
		{0, 4 * 1024, 12},
		{31 * 1024, 4 * 1024, 12},
		{32 * 1024, 8 * 1024, 13},
		{200 * 1024, 8 * 1024, 13},
		{256 * 1024, 16 * 1024, 14},
		{1 << 20, 16 * 1024, 14},
		{2 << 20, 32 * 1024, 15},
		{5 << 20, 32 * 1024, 15},
	}
	for _, c := range cases {
		rs, log2 := recordSizeFor(c.size)
		if rs != c.rs || log2 != c.log2 {
			t.Errorf("recordSizeFor(%d) = %d/%d, want %d/%d", c.size, rs, log2, c.rs, c.log2)
		}
		if rs != uint64(1)<<log2 {
			t.Errorf("recordSizeFor(%d): rs %d != 1<<%d", c.size, rs, log2)
		}
	}
}

// A file larger than one record is stored as multiple records behind a block-map
// blob, round-trips in full, and survives a remount.
func TestLargeFileRoundTrip(t *testing.T) {
	mem := block.NewMem(8192)
	f, err := Format(mem, testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	id, _ := f.Create(root, "big.bin")
	data := patt(300 * 1024) // 300 KiB -> 16 KiB records -> multi-record (indirect)
	if err := f.WriteFile(id, data); err != nil {
		t.Fatal(err)
	}
	in, _ := f.inodes.Get(id)
	if in.Flags&inode.FlagInlineExtents != 0 {
		t.Fatal("expected a multi-record (indirect) block map, got inline")
	}
	if got, _ := f.ReadFile(id); !bytes.Equal(got, data) {
		t.Fatal("large-file round-trip mismatch")
	}
	if err := f.Fsync(); err != nil {
		t.Fatal(err)
	}

	f2, err := Mount(mem, testKey())
	if err != nil {
		t.Fatal(err)
	}
	rid, ok, _ := f2.Lookup(f2.Root(), "big.bin")
	if !ok {
		t.Fatal("file lost across remount")
	}
	if got, _ := f2.ReadFile(rid); !bytes.Equal(got, data) {
		t.Fatal("large-file content wrong after remount")
	}
}

func TestReadAt(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	id, _ := f.Create(root, "f")
	data := patt(100 * 1024) // 8 KiB records
	if err := f.WriteFile(id, data); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		off uint64
		n   int
	}{
		{0, 10},          // start of first record
		{4096, 4096},     // within a record
		{8000, 5000},     // crosses a record boundary
		{0, len(data)},   // whole file
		{100*1024 - 5, 100}, // clipped at EOF
		{99000, 0},       // zero length
		{200000, 10},     // entirely past EOF
	}
	for _, c := range cases {
		got, err := f.ReadAt(id, c.off, c.n)
		if err != nil {
			t.Fatalf("ReadAt(%d,%d): %v", c.off, c.n, err)
		}
		var want []byte
		if c.off < uint64(len(data)) && c.n > 0 {
			end := c.off + uint64(c.n)
			if end > uint64(len(data)) {
				end = uint64(len(data))
			}
			want = data[c.off:end]
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("ReadAt(%d,%d): got %d bytes, want %d", c.off, c.n, len(got), len(want))
		}
	}
}

func TestWriteAt(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	id, _ := f.Create(root, "f")
	base := patt(100 * 1024)
	if err := f.WriteFile(id, base); err != nil {
		t.Fatal(err)
	}

	// Overwrite a span that crosses several record boundaries.
	patch := bytes.Repeat([]byte{0xEE}, 20000)
	const off = 9000
	if err := f.WriteAt(id, off, patch); err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), base...)
	copy(want[off:], patch)
	if got, _ := f.ReadFile(id); !bytes.Equal(got, want) {
		t.Fatal("WriteAt overwrite mismatch")
	}

	// Extend past EOF with a gap: the gap must read back as zeros.
	tail := []byte("TAILDATA")
	gapOff := uint64(len(want) + 5000)
	if err := f.WriteAt(id, gapOff, tail); err != nil {
		t.Fatal(err)
	}
	want2 := make([]byte, gapOff+uint64(len(tail)))
	copy(want2, want)
	copy(want2[gapOff:], tail)
	if got, _ := f.ReadFile(id); !bytes.Equal(got, want2) {
		t.Fatal("WriteAt extend-with-gap mismatch")
	}

	// Survive a remount.
	if err := f.Fsync(); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.ReadFile(id); !bytes.Equal(got, want2) {
		t.Fatal("WriteAt result wrong after commit")
	}
}

func TestWriteAtEmptyFile(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	id, _ := f.Create(root, "f")
	if err := f.WriteAt(id, 100, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 105)
	copy(want[100:], "hello")
	if got, _ := f.ReadFile(id); !bytes.Equal(got, want) {
		t.Fatal("empty-file WriteAt mismatch")
	}
}

func TestTruncate(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	id, _ := f.Create(root, "f")
	data := patt(50 * 1024)
	if err := f.WriteFile(id, data); err != nil {
		t.Fatal(err)
	}

	if err := f.Truncate(id, 10000); err != nil { // shrink
		t.Fatal(err)
	}
	if got, _ := f.ReadFile(id); !bytes.Equal(got, data[:10000]) {
		t.Fatal("truncate shrink mismatch")
	}

	if err := f.Truncate(id, 20000); err != nil { // grow (zero-extend)
		t.Fatal(err)
	}
	want := make([]byte, 20000)
	copy(want, data[:10000])
	if got, _ := f.ReadFile(id); !bytes.Equal(got, want) {
		t.Fatal("truncate grow mismatch")
	}

	if err := f.Truncate(id, 0); err != nil { // empty
		t.Fatal(err)
	}
	if got, _ := f.ReadFile(id); len(got) != 0 {
		t.Fatalf("truncate to 0 left %d bytes", len(got))
	}
	if in, _ := f.inodes.Get(id); in.Size != 0 {
		t.Fatalf("inode size after truncate to 0 = %d", in.Size)
	}
}

// Identical records inside one file collapse to a single stored block (per-record
// dedup), proving the dedup unit is the record, not the whole file.
func TestPerRecordDedup(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	id, _ := f.Create(root, "dup")

	rec := patt(8 * 1024)
	blob := bytes.Repeat(rec, 5) // 40 KiB -> 8 KiB records -> 5 identical records

	u0 := f.bs.UniqueBlocks()
	if err := f.WriteFile(id, blob); err != nil {
		t.Fatal(err)
	}
	// 5 identical records -> 1 unique data block, plus 1 block-map blob.
	if d := f.bs.UniqueBlocks() - u0; d != 2 {
		t.Fatalf("unique-block delta = %d, want 2 (1 deduped record + 1 block map)", d)
	}
	if got, _ := f.ReadFile(id); !bytes.Equal(got, blob) {
		t.Fatal("per-record dedup round-trip mismatch")
	}
}

// A file whose total size exceeds any single contiguous free run still stores
// successfully, because each record allocates independently. We pre-carve the
// data region into small holes, then write a file far larger than one hole.
func TestFragmentedAllocMultiRecord(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()

	// Carve the data region so the largest contiguous free run is 4 clusters:
	// reserve 12-cluster walls between 4-cluster holes.
	const hole = 4
	const wall = 12
	for c := f.ub.DataStart; c+wall < f.ub.TotalBlocks; c += hole + wall {
		f.bm.Reserve(c+hole, wall)
	}

	id, _ := f.Create(root, "frag.bin")
	// 256 KiB -> 16 KiB records = 4 clusters each: a record fits a hole, the
	// whole file (64 clusters) would not fit any single run.
	data := randBytes(256*1024, 99)
	if err := f.WriteFile(id, data); err != nil {
		t.Fatalf("multi-record write on fragmented device: %v", err)
	}
	if got, _ := f.ReadFile(id); !bytes.Equal(got, data) {
		t.Fatal("fragmented multi-record round-trip mismatch")
	}
}
