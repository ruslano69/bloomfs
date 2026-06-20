package cow

import (
	"fmt"

	"github.com/ruslano69/bloomfs/alloc"
	"github.com/ruslano69/bloomfs/block"
	"github.com/ruslano69/bloomfs/dedup"
	"github.com/ruslano69/bloomfs/inode"
)

// On-disk geometry (computed by Format, recorded in the uberblock):
//
//	block 0           uberblock slot 0   (ping-pong with slot 1)
//	block 1           uberblock slot 1
//	[MetaA .. MetaB)  metadata slot A    (bitmap + dedup-table + inode-table)
//	[MetaB .. Data)   metadata slot B
//	[Data .. )        data region (allocator-managed clusters)
//
// The inode table is part of the CoW metadata snapshot — there is no fixed
// in-place inode region. Each commit writes the whole [bitmap | ddt | inode]
// snapshot into the inactive metadata slot, then flips the uberblock; a crash
// before the flip loses every uncommitted metadata change (§B1, §E).

// Format lays out a fresh image on dev with inodeCount inode slots and ddtReserve
// bytes of headroom for the dedup-table snapshot, then writes the first commit
// (Seq 1). Returns the committed uberblock. The metadata slot is sized to hold
// the bitmap, the dedup-table reserve and a full inode table at once.
func Format(dev block.Device, inodeCount, ddtReserve uint64) (*Uberblock, error) {
	total := dev.Blocks()
	if inodeCount == 0 {
		inodeCount = inode.PerBlock
	}
	bitmapBytes := 8 + (total+7)/8
	inodeReserve := inodeCount * inode.Size
	metaBlocks := (64 + bitmapBytes + ddtReserve + inodeReserve + block.Size - 1) / block.Size

	metaA := uint64(2)
	metaB := metaA + metaBlocks
	dataStart := metaB + metaBlocks
	if total <= dataStart {
		return nil, fmt.Errorf("cow: device too small: need > %d blocks, have %d", dataStart, total)
	}

	bm := alloc.New(total)
	bm.Reserve(0, dataStart) // uberblocks + both metadata slots are off-limits
	ddt := dedup.New()
	tbl := inode.NewTable(inodeCount)

	ub := &Uberblock{
		Magic:       uberMagic,
		Seq:         1,
		BlockSize:   block.Size,
		ActiveMeta:  0,
		TotalBlocks: total,
		MetaA:       metaA,
		MetaB:       metaB,
		MetaBlocks:  metaBlocks,
		InodeCount:  inodeCount,
		DataStart:   dataStart,
		RootInode:   0,
		NextInode:   1, // inode 0 is the root directory
	}

	if err := writeSnapshot(dev, ub, bm, ddt, tbl, nil); err != nil {
		return nil, err
	}
	if err := writeUber(dev, ub); err != nil {
		return nil, err
	}
	return ub, nil
}

// Mount reads both uberblock slots, picks the highest-sequence valid one, and
// loads its allocator bitmap, dedup table and inode table. A torn commit is
// invisible: parseUber rejects the corrupt slot, so the previous consistent
// commit is used.
func Mount(dev block.Device) (*Uberblock, *alloc.Bitmap, *dedup.Table, *inode.Table, error) {
	var best *Uberblock
	for _, slot := range []uint64{UberSlot0, UberSlot1} {
		raw, err := dev.ReadBlock(slot)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if ub, err := parseUber(raw); err == nil {
			if best == nil || ub.Seq > best.Seq {
				best = ub
			}
		}
	}
	if best == nil {
		return nil, nil, nil, nil, ErrNotFormatted
	}

	buf := make([]byte, best.MetaBlocks*block.Size)
	start := best.metaStart()
	for i := uint64(0); i < best.MetaBlocks; i++ {
		blk, err := dev.ReadBlock(start + i)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		copy(buf[i*block.Size:], blk)
	}
	bmEnd := uint64(best.BitmapLen)
	ddEnd := bmEnd + uint64(best.DDTLen)
	inEnd := ddEnd + uint64(best.InodeLen)
	bm, err := alloc.Unmarshal(buf[:bmEnd])
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("cow: load bitmap: %w", err)
	}
	ddt, err := dedup.Unmarshal(buf[bmEnd:ddEnd])
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("cow: load dedup table: %w", err)
	}
	tbl, err := inode.UnmarshalTable(buf[ddEnd:inEnd], best.InodeCount)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("cow: load inode table: %w", err)
	}
	return best, bm, ddt, tbl, nil
}

// Commit atomically records a new consistent state: it snapshots bm+ddt+tbl into
// the inactive metadata slot (synced), then flips the uberblock (synced). On any
// failure the previous commit remains valid; remounting rolls back to it.
//
// scratch is an optional reusable serialization buffer (>= MetaBlocks*BlockSize):
// the hot path passes a persistent one so a commit allocates nothing; nil makes
// writeSnapshot allocate a fresh buffer.
func Commit(dev block.Device, prev *Uberblock, bm *alloc.Bitmap, ddt *dedup.Table, tbl *inode.Table, rootInode, nextInode uint64, scratch []byte) (*Uberblock, error) {
	next := *prev // inherit geometry
	next.Seq = prev.Seq + 1
	next.ActiveMeta = 1 - prev.ActiveMeta // alternate metadata slot
	next.RootInode = rootInode
	next.NextInode = nextInode

	if err := writeSnapshot(dev, &next, bm, ddt, tbl, scratch); err != nil {
		return nil, err
	}
	if err := writeUber(dev, &next); err != nil { // the atomic flip
		return nil, err
	}
	// Only now that the commit is durable may clusters freed during this
	// transaction be reused (§F1): the snapshot just written still marks them
	// used, so they are reclaimed one commit later, and a crash before the flip
	// above left them pinned — never overwriting the rolled-back-to state.
	bm.ApplyDeferred()
	return &next, nil
}

// writeSnapshot serializes bm+ddt+tbl into ub's active metadata slot, recording
// the three lengths on ub, and syncs. The three structures are marshaled
// contiguously into one buffer with no intermediate slices; the tail past their
// combined length is never read on Mount (the uberblock carries each section's
// length), so a reused scratch buffer with stale tail bytes is safe.
func writeSnapshot(dev block.Device, ub *Uberblock, bm *alloc.Bitmap, ddt *dedup.Table, tbl *inode.Table, scratch []byte) error {
	bmLen := bm.MarshalLen()
	ddLen := ddt.MarshalLen()
	inLen := tbl.MarshalLen()
	need := ub.MetaBlocks * block.Size
	if uint64(bmLen+ddLen+inLen) > need {
		return ErrMetaTooBig
	}

	buf := scratch
	if uint64(len(buf)) < need {
		buf = make([]byte, need)
	}
	bm.MarshalInto(buf[:bmLen])
	ddt.MarshalInto(buf[bmLen : bmLen+ddLen])
	tbl.MarshalInto(buf[bmLen+ddLen : bmLen+ddLen+inLen])
	ub.BitmapLen = uint32(bmLen)
	ub.DDTLen = uint32(ddLen)
	ub.InodeLen = uint32(inLen)

	start := ub.metaStart()
	for i := uint64(0); i < ub.MetaBlocks; i++ {
		if err := dev.WriteBlock(start+i, buf[i*block.Size:(i+1)*block.Size]); err != nil {
			return err
		}
	}
	return dev.Sync()
}

// writeUber writes ub to slot (Seq % 2) and syncs. Because consecutive sequence
// numbers have opposite parity, a new commit never overwrites the slot holding
// the previous commit — that is what makes the flip safe.
func writeUber(dev block.Device, ub *Uberblock) error {
	b, err := ub.MarshalBinary()
	if err != nil {
		return err
	}
	if err := dev.WriteBlock(ub.Seq%2, b); err != nil {
		return err
	}
	return dev.Sync()
}
