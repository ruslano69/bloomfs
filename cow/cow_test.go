package cow

import (
	"errors"
	"testing"

	"github.com/ruslano69/bloomfs/alloc"
	"github.com/ruslano69/bloomfs/block"
	"github.com/ruslano69/bloomfs/dedup"
)

// flakyDevice simulates power loss: the failAt-th WriteBlock corrupts its target
// (a torn sector) and fails; every subsequent write fails too. This exercises
// both "crash before the flip" and "torn uberblock" recovery paths.
type flakyDevice struct {
	block.Device
	failAt int // 1-based index of the write that loses power; 0 = never
	n      int
}

func (d *flakyDevice) WriteBlock(num uint64, data []byte) error {
	d.n++
	if d.failAt != 0 && d.n >= d.failAt {
		if d.n == d.failAt {
			// Model a block that did not finish persisting: the stored content and
			// its checksum no longer agree. We corrupt the checksum region so a
			// torn uberblock is rejected by parseUber on remount.
			torn := make([]byte, len(data))
			copy(torn, data)
			for i := uberChecksumOff; i < uberChecksumOff+32 && i < len(torn); i++ {
				torn[i] ^= 0xFF
			}
			_ = d.Device.WriteBlock(num, torn)
		}
		return errors.New("flaky: simulated power loss")
	}
	return d.Device.WriteBlock(num, data)
}

func dkey(b byte) dedup.Key {
	var k dedup.Key
	k[0] = b
	return k
}

// activity does one allocation + one dedup insert, mutating bm and ddt.
func activity(t *testing.T, bm *alloc.Bitmap, ddt *dedup.Table, id byte, count uint64) {
	t.Helper()
	start, err := bm.Alloc(count)
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	ddt.Add(dkey(id), dedup.Entry{Start: start, Count: uint32(count)})
}

func format(t *testing.T) *block.MemDevice {
	t.Helper()
	dev := block.NewMem(2048)
	if _, err := Format(dev, 1000, 64*1024); err != nil {
		t.Fatalf("format: %v", err)
	}
	return dev
}

func TestFormatMount(t *testing.T) {
	dev := format(t)
	ub, bm, ddt, _, err := Mount(dev)
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if ub.Seq != 1 {
		t.Fatalf("Seq = %d, want 1", ub.Seq)
	}
	if ddt.Len() != 0 {
		t.Fatalf("fresh ddt len = %d, want 0", ddt.Len())
	}
	if bm.Used() != ub.DataStart {
		t.Fatalf("reserved = %d, want DataStart %d", bm.Used(), ub.DataStart)
	}
}

func TestCommitPersists(t *testing.T) {
	dev := format(t)
	ub, bm, ddt, tbl, _ := Mount(dev)

	activity(t, bm, ddt, 1, 3)
	activity(t, bm, ddt, 2, 5)
	wantUsed, wantLen := bm.Used(), ddt.Len()

	if _, err := Commit(dev, ub, bm, ddt, tbl, 0, 1); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Remount from scratch: the committed state must be exactly what we built.
	ub2, bm2, ddt2, _, err := Mount(dev)
	if err != nil {
		t.Fatalf("remount: %v", err)
	}
	if ub2.Seq != 2 || bm2.Used() != wantUsed || ddt2.Len() != wantLen {
		t.Fatalf("after commit: seq=%d used=%d len=%d; want seq=2 used=%d len=%d",
			ub2.Seq, bm2.Used(), ddt2.Len(), wantUsed, wantLen)
	}
}

// commitTo seq2, returning the seq2 uberblock and its recorded used/len so crash
// tests can assert rollback to it.
func commitToSeq2(t *testing.T, dev block.Device) (*Uberblock, uint64, int) {
	t.Helper()
	ub, bm, ddt, tbl, _ := Mount(dev)
	activity(t, bm, ddt, 1, 3)
	used, length := bm.Used(), ddt.Len()
	ub2, err := Commit(dev, ub, bm, ddt, tbl, 0, 1)
	if err != nil {
		t.Fatalf("seq2 commit: %v", err)
	}
	return ub2, used, length
}

func TestCrashDuringMetadata(t *testing.T) {
	dev := format(t)
	ub2, used2, len2 := commitToSeq2(t, dev)

	// Build more state, then crash on the FIRST write of the next commit — i.e.
	// while writing the metadata snapshot, before the uberblock flip.
	_, bm, ddt, tbl, _ := Mount(dev)
	activity(t, bm, ddt, 1, 3)
	activity(t, bm, ddt, 2, 9)
	flaky := &flakyDevice{Device: dev, failAt: 1}
	if _, err := Commit(flaky, ub2, bm, ddt, tbl, 0, 1); err == nil {
		t.Fatal("expected commit to fail mid-metadata")
	}

	assertRolledBackToSeq2(t, dev, used2, len2)
}

func TestCrashDuringUberblock(t *testing.T) {
	dev := format(t)
	ub2, used2, len2 := commitToSeq2(t, dev)

	_, bm, ddt, tbl, _ := Mount(dev)
	activity(t, bm, ddt, 2, 7)
	// Metadata writes (MetaBlocks of them) succeed; the very next write is the
	// uberblock — tear it. The checksum must reject the torn slot on remount.
	flaky := &flakyDevice{Device: dev, failAt: int(ub2.MetaBlocks) + 1}
	if _, err := Commit(flaky, ub2, bm, ddt, tbl, 0, 1); err == nil {
		t.Fatal("expected commit to fail during uberblock flip")
	}

	assertRolledBackToSeq2(t, dev, used2, len2)
}

func assertRolledBackToSeq2(t *testing.T, dev block.Device, wantUsed uint64, wantLen int) {
	t.Helper()
	ub, bm, ddt, _, err := Mount(dev)
	if err != nil {
		t.Fatalf("remount after crash: %v", err)
	}
	if ub.Seq != 2 {
		t.Fatalf("rolled to seq %d, want consistent seq 2", ub.Seq)
	}
	if bm.Used() != wantUsed || ddt.Len() != wantLen {
		t.Fatalf("state not cleanly rolled back: used=%d len=%d; want used=%d len=%d",
			bm.Used(), ddt.Len(), wantUsed, wantLen)
	}
}
