// Package cow implements the Copy-on-Write durability layer (§B1, §D-1): the
// commit root is an "uberblock" stored in two alternating slots. A transaction
// writes new metadata — the allocator bitmap, the dedup table AND the inode
// table, as one [bitmap | ddt | inode] snapshot — to the inactive metadata slot,
// then writes a new uberblock (with a higher sequence number and a content
// checksum) to the inactive uberblock slot. That final block write is the atomic
// flip. A crash before it leaves the previous uberblock (and the metadata it
// points to) fully intact, so mount rolls back to the last consistent commit.
// There is never an in-place overwrite of live metadata.
//
// This supersedes the minimal layout.Superblock (Stage B) as the durable mount
// path.
package cow

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/crypto/blake2b"

	"github.com/ruslano69/bloomfs/block"
)

const (
	// uberMagic identifies an uberblock (a fixed, arbitrary 64-bit tag).
	uberMagic = 0xB100F5_C0FFEE_01
	// uberChecksumOff is where the BLAKE2b checksum of the preceding bytes lives.
	uberChecksumOff = 104
	// UberSlot0 / UberSlot1 are the two ping-pong uberblock blocks.
	UberSlot0 = 0
	UberSlot1 = 1
)

var (
	ErrNotFormatted = errors.New("cow: no valid uberblock (not formatted or fully corrupt)")
	ErrBadUber      = errors.New("cow: uberblock invalid (bad magic or checksum)")
	ErrMetaTooBig   = errors.New("cow: metadata snapshot exceeds the metadata slot")
)

// Uberblock is the self-describing commit root: geometry + which metadata slot
// holds this commit's snapshot + a checksum that detects a torn write.
//
// The active metadata slot is a single CoW snapshot laid out as
// [bitmap | dedup-table | inode-table], whose three lengths are recorded below.
// The inode table lives here (not in a fixed in-place region), which is what
// makes metadata mutations crash-atomic (§B1, §E).
type Uberblock struct {
	Magic       uint64
	Seq         uint64 // commit sequence; highest valid wins
	BlockSize   uint32
	ActiveMeta  uint8 // 0 => snapshot in MetaA, 1 => MetaB
	TotalBlocks uint64
	MetaA       uint64 // first block of metadata slot A
	MetaB       uint64 // first block of metadata slot B
	MetaBlocks  uint64 // size of each metadata slot, in blocks
	InodeCount  uint64 // inode-table capacity (number of slots)
	DataStart   uint64
	RootInode   uint64
	NextInode   uint64 // next free inode id (bump allocator high-water mark)
	BitmapLen   uint32 // bytes of bitmap snapshot in the active metadata slot
	DDTLen      uint32 // bytes of dedup-table snapshot, following the bitmap
	InodeLen    uint32 // bytes of inode-table snapshot, following the dedup table
}

// MarshalBinary encodes the uberblock into a full block with a trailing checksum.
func (ub *Uberblock) MarshalBinary() ([]byte, error) {
	b := make([]byte, block.Size)
	binary.LittleEndian.PutUint64(b[0:], ub.Magic)
	binary.LittleEndian.PutUint64(b[8:], ub.Seq)
	binary.LittleEndian.PutUint32(b[16:], ub.BlockSize)
	b[20] = ub.ActiveMeta
	binary.LittleEndian.PutUint64(b[24:], ub.TotalBlocks)
	binary.LittleEndian.PutUint64(b[32:], ub.MetaA)
	binary.LittleEndian.PutUint64(b[40:], ub.MetaB)
	binary.LittleEndian.PutUint64(b[48:], ub.MetaBlocks)
	binary.LittleEndian.PutUint64(b[56:], ub.InodeCount)
	binary.LittleEndian.PutUint64(b[64:], ub.DataStart)
	binary.LittleEndian.PutUint64(b[72:], ub.RootInode)
	binary.LittleEndian.PutUint64(b[80:], ub.NextInode)
	binary.LittleEndian.PutUint32(b[88:], ub.BitmapLen)
	binary.LittleEndian.PutUint32(b[92:], ub.DDTLen)
	binary.LittleEndian.PutUint32(b[96:], ub.InodeLen)
	sum := blake2b.Sum256(b[:uberChecksumOff])
	copy(b[uberChecksumOff:uberChecksumOff+32], sum[:])
	return b, nil
}

// parseUber validates magic + checksum and decodes an uberblock. A torn or
// corrupt slot fails here, so mount simply ignores it and uses the other slot.
func parseUber(b []byte) (*Uberblock, error) {
	if len(b) < block.Size {
		return nil, ErrBadUber
	}
	if binary.LittleEndian.Uint64(b[0:]) != uberMagic {
		return nil, ErrBadUber
	}
	sum := blake2b.Sum256(b[:uberChecksumOff])
	if !bytes.Equal(sum[:], b[uberChecksumOff:uberChecksumOff+32]) {
		return nil, ErrBadUber
	}
	return &Uberblock{
		Magic:       uberMagic,
		Seq:         binary.LittleEndian.Uint64(b[8:]),
		BlockSize:   binary.LittleEndian.Uint32(b[16:]),
		ActiveMeta:  b[20],
		TotalBlocks: binary.LittleEndian.Uint64(b[24:]),
		MetaA:       binary.LittleEndian.Uint64(b[32:]),
		MetaB:       binary.LittleEndian.Uint64(b[40:]),
		MetaBlocks:  binary.LittleEndian.Uint64(b[48:]),
		InodeCount:  binary.LittleEndian.Uint64(b[56:]),
		DataStart:   binary.LittleEndian.Uint64(b[64:]),
		RootInode:   binary.LittleEndian.Uint64(b[72:]),
		NextInode:   binary.LittleEndian.Uint64(b[80:]),
		BitmapLen:   binary.LittleEndian.Uint32(b[88:]),
		DDTLen:      binary.LittleEndian.Uint32(b[92:]),
		InodeLen:    binary.LittleEndian.Uint32(b[96:]),
	}, nil
}

// metaStart returns the first block of the active metadata slot.
func (ub *Uberblock) metaStart() uint64 {
	if ub.ActiveMeta == 1 {
		return ub.MetaB
	}
	return ub.MetaA
}
