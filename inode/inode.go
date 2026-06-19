// Package inode defines the 128-byte BloomFS inode (§4.1) and its fixed
// little-endian on-disk encoding, plus a store that packs 32 inodes per block.
//
// The Go struct is the in-memory form only. The on-disk form is the explicit
// encoding produced by MarshalBinary — Go struct memory layout is NOT used on
// disk (it is neither stable nor portable, §B15).
package inode

import (
	"encoding/binary"
	"errors"

	"github.com/ruslano69/bloomfs/block"
)

const (
	// Size is the on-disk inode size in bytes (§4.1): 32 per 4 KiB block, and a
	// 128-byte cache line on Apple Silicon.
	Size = 128
	// PerBlock is how many inodes fit in one block.
	PerBlock = block.Size / Size // 32
)

// Compile-time guarantee that inodes pack a block exactly (no slack, no
// overflow). If PerBlock*Size != block.Size this expression is negative and the
// uint conversion fails to compile.
const _ = uint(block.Size - PerBlock*Size)

// File type codes (§4.1).
const (
	TypeRegular uint8 = 0
	TypeDir     uint8 = 1
	TypeLink    uint8 = 2
)

// Flags bits.
const (
	FlagInlineExtents uint8 = 1 << 0 // block map is inline (else BlockMap holds a pointer)
	FlagCompressed    uint8 = 1 << 1
	FlagEncrypted     uint8 = 1 << 2
)

const (
	blockMapBytes = 64
	extentSize    = 16
	// InlineCap is how many inline extents fit in the block-map area.
	InlineCap = blockMapBytes / extentSize // 4
)

var (
	// ErrBadInodeSize is returned by UnmarshalBinary for a wrong-length buffer.
	ErrBadInodeSize = errors.New("inode: encoded inode must be exactly 128 bytes")
	// ErrTooManyExtents is returned by SetExtents past InlineCap.
	ErrTooManyExtents = errors.New("inode: too many inline extents")
)

// Extent describes a run of contiguous clusters (§4.4/§4.5).
type Extent struct {
	Start   uint64 // first cluster (block) index
	Blocks  uint32 // number of clusters in the run
	Logical uint32 // logical (uncompressed) bytes the run represents
}

// Inode is the in-memory metadata record. It never stores the file name (§4.2).
//
// On-disk layout (little-endian, 128 bytes). UID/GID are u32 and Mode is u16 so
// real POSIX ids (>255) and full permission bits (>0o377) fit — a u8 could hold
// neither, and widening the on-disk record after images exist would be a format
// migration, so it is sized correctly up front (§4.1).
//
//	0   Size        u64
//	8   Nlink       u32
//	12  Generation  u32
//	16  UID         u32
//	20  GID         u32
//	24  Atime       u64
//	32  Mtime       u64
//	40  Ctime       u64
//	48  Mode        u16   permission/setuid/sticky bits (type is in Type)
//	50  Type        u8
//	51  RecordLog2  u8
//	52  Flags       u8
//	53  reserved    [3]
//	56  Checksum    [8]
//	64  BlockMap    [64]   inline extents or external pointer
type Inode struct {
	Size           uint64
	Nlink          uint32
	Generation     uint32
	UID            uint32
	GID            uint32
	Atime          uint64
	Mtime          uint64
	Ctime          uint64
	Mode           uint16
	Type           uint8
	RecordSizeLog2 uint8
	Flags          uint8
	Checksum       [8]byte
	BlockMap       [blockMapBytes]byte
}

// MarshalBinary encodes the inode to exactly Size bytes.
func (in *Inode) MarshalBinary() ([]byte, error) {
	b := make([]byte, Size)
	binary.LittleEndian.PutUint64(b[0:], in.Size)
	binary.LittleEndian.PutUint32(b[8:], in.Nlink)
	binary.LittleEndian.PutUint32(b[12:], in.Generation)
	binary.LittleEndian.PutUint32(b[16:], in.UID)
	binary.LittleEndian.PutUint32(b[20:], in.GID)
	binary.LittleEndian.PutUint64(b[24:], in.Atime)
	binary.LittleEndian.PutUint64(b[32:], in.Mtime)
	binary.LittleEndian.PutUint64(b[40:], in.Ctime)
	binary.LittleEndian.PutUint16(b[48:], in.Mode)
	b[50] = in.Type
	b[51] = in.RecordSizeLog2
	b[52] = in.Flags
	// b[53:56] reserved (zero)
	copy(b[56:64], in.Checksum[:])
	copy(b[64:128], in.BlockMap[:])
	return b, nil
}

// UnmarshalBinary decodes exactly Size bytes into the receiver.
func (in *Inode) UnmarshalBinary(b []byte) error {
	if len(b) != Size {
		return ErrBadInodeSize
	}
	in.Size = binary.LittleEndian.Uint64(b[0:])
	in.Nlink = binary.LittleEndian.Uint32(b[8:])
	in.Generation = binary.LittleEndian.Uint32(b[12:])
	in.UID = binary.LittleEndian.Uint32(b[16:])
	in.GID = binary.LittleEndian.Uint32(b[20:])
	in.Atime = binary.LittleEndian.Uint64(b[24:])
	in.Mtime = binary.LittleEndian.Uint64(b[32:])
	in.Ctime = binary.LittleEndian.Uint64(b[40:])
	in.Mode = binary.LittleEndian.Uint16(b[48:])
	in.Type = b[50]
	in.RecordSizeLog2 = b[51]
	in.Flags = b[52]
	copy(in.Checksum[:], b[56:64])
	copy(in.BlockMap[:], b[64:128])
	return nil
}

// SetExtents encodes up to InlineCap extents into the inline block map and sets
// FlagInlineExtents. A zero-Blocks extent terminates the list on decode, so do
// not pass extents with Blocks == 0.
func (in *Inode) SetExtents(exts []Extent) error {
	if len(exts) > InlineCap {
		return ErrTooManyExtents
	}
	var bm [blockMapBytes]byte
	for i, e := range exts {
		off := i * extentSize
		binary.LittleEndian.PutUint64(bm[off:], e.Start)
		binary.LittleEndian.PutUint32(bm[off+8:], e.Blocks)
		binary.LittleEndian.PutUint32(bm[off+12:], e.Logical)
	}
	in.BlockMap = bm
	in.Flags |= FlagInlineExtents
	return nil
}

// Extents decodes the inline extents (a zero-Blocks entry terminates). Only
// meaningful when FlagInlineExtents is set.
func (in *Inode) Extents() []Extent {
	var out []Extent
	for i := 0; i < InlineCap; i++ {
		off := i * extentSize
		blocks := binary.LittleEndian.Uint32(in.BlockMap[off+8:])
		if blocks == 0 {
			break
		}
		out = append(out, Extent{
			Start:   binary.LittleEndian.Uint64(in.BlockMap[off:]),
			Blocks:  blocks,
			Logical: binary.LittleEndian.Uint32(in.BlockMap[off+12:]),
		})
	}
	return out
}
