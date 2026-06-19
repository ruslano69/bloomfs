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
//	[MetaA .. MetaB)  metadata slot A    (bitmap + dedup-table snapshot)
//	[MetaB .. Inode)  metadata slot B
//	[Inode .. Data)   inode table
//	[Data .. )        data region (allocator-managed clusters)

// Format lays out a fresh image on dev with inodeCount inode slots and ddtReserve
// bytes of headroom for the dedup-table snapshot, then writes the first commit
// (Seq 1). Returns the committed uberblock.
func Format(dev block.Device, inodeCount, ddtReserve uint64) (*Uberblock, error) {
	total := dev.Blocks()
	inodeBlocks := (inodeCount + inode.PerBlock - 1) / inode.PerBlock
	if inodeBlocks == 0 {
		inodeBlocks = 1
	}
	bitmapBytes := 8 + (total+7)/8
	metaBlocks := (64 + bitmapBytes + ddtReserve + block.Size - 1) / block.Size

	metaA := uint64(2)
	metaB := metaA + metaBlocks
	inodeTable := metaB + metaBlocks
	dataStart := inodeTable + inodeBlocks
	if total <= dataStart {
		return nil, fmt.Errorf("cow: device too small: need > %d blocks, have %d", dataStart, total)
	}

	bm := alloc.New(total)
	bm.Reserve(0, dataStart) // uberblocks + metadata slots + inode table are off-limits
	ddt := dedup.New()

	ub := &Uberblock{
		Magic:       uberMagic,
		Seq:         1,
		BlockSize:   block.Size,
		ActiveMeta:  0,
		TotalBlocks: total,
		MetaA:       metaA,
		MetaB:       metaB,
		MetaBlocks:  metaBlocks,
		InodeTable:  inodeTable,
		InodeCount:  inodeBlocks * inode.PerBlock,
		DataStart:   dataStart,
		RootInode:   0,
	}

	zero := make([]byte, block.Size)
	for i := uint64(0); i < inodeBlocks; i++ {
		if err := dev.WriteBlock(inodeTable+i, zero); err != nil {
			return nil, err
		}
	}
	if err := writeSnapshot(dev, ub, bm, ddt); err != nil {
		return nil, err
	}
	if err := writeUber(dev, ub); err != nil {
		return nil, err
	}
	return ub, nil
}

// Mount reads both uberblock slots, picks the highest-sequence valid one, and
// loads its allocator bitmap and dedup table. A torn commit is invisible:
// parseUber rejects the corrupt slot, so the previous consistent commit is used.
func Mount(dev block.Device) (*Uberblock, *alloc.Bitmap, *dedup.Table, error) {
	var best *Uberblock
	for _, slot := range []uint64{UberSlot0, UberSlot1} {
		raw, err := dev.ReadBlock(slot)
		if err != nil {
			return nil, nil, nil, err
		}
		if ub, err := parseUber(raw); err == nil {
			if best == nil || ub.Seq > best.Seq {
				best = ub
			}
		}
	}
	if best == nil {
		return nil, nil, nil, ErrNotFormatted
	}

	buf := make([]byte, best.MetaBlocks*block.Size)
	start := best.metaStart()
	for i := uint64(0); i < best.MetaBlocks; i++ {
		blk, err := dev.ReadBlock(start + i)
		if err != nil {
			return nil, nil, nil, err
		}
		copy(buf[i*block.Size:], blk)
	}
	bm, err := alloc.Unmarshal(buf[:best.BitmapLen])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cow: load bitmap: %w", err)
	}
	ddt, err := dedup.Unmarshal(buf[best.BitmapLen : best.BitmapLen+best.DDTLen])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cow: load dedup table: %w", err)
	}
	return best, bm, ddt, nil
}

// Commit atomically records a new consistent state: it snapshots bm+ddt into the
// inactive metadata slot (synced), then flips the uberblock (synced). On any
// failure the previous commit remains valid; remounting rolls back to it.
func Commit(dev block.Device, prev *Uberblock, bm *alloc.Bitmap, ddt *dedup.Table, rootInode uint64) (*Uberblock, error) {
	next := *prev // inherit geometry
	next.Seq = prev.Seq + 1
	next.ActiveMeta = 1 - prev.ActiveMeta // alternate metadata slot
	next.RootInode = rootInode

	if err := writeSnapshot(dev, &next, bm, ddt); err != nil {
		return nil, err
	}
	if err := writeUber(dev, &next); err != nil { // the atomic flip
		return nil, err
	}
	return &next, nil
}

// writeSnapshot serializes bm+ddt into ub's active metadata slot, recording the
// lengths on ub, and syncs.
func writeSnapshot(dev block.Device, ub *Uberblock, bm *alloc.Bitmap, ddt *dedup.Table) error {
	bmBytes := bm.Marshal()
	ddBytes := ddt.Marshal()
	if uint64(len(bmBytes)+len(ddBytes)) > ub.MetaBlocks*block.Size {
		return ErrMetaTooBig
	}
	ub.BitmapLen = uint32(len(bmBytes))
	ub.DDTLen = uint32(len(ddBytes))

	buf := make([]byte, ub.MetaBlocks*block.Size)
	copy(buf, bmBytes)
	copy(buf[len(bmBytes):], ddBytes)

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
