// Package layout defines the on-disk anchor — the superblock — and a minimal
// Format that lays out a fresh image (§2 of the spec). The superblock is never
// encrypted, so the filesystem can always find its way in.
package layout

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ruslano69/bloomfs/block"
	"github.com/ruslano69/bloomfs/inode"
)

const (
	// Magic identifies a BloomFS image: bytes "BLOOMFS\x00" read little-endian.
	Magic = 0x0053464D4F4F4C42
	// Version is the on-disk format version (§2, §B15).
	Version = 1
	// SuperblockBlock is where the superblock lives.
	SuperblockBlock = 0
)

var (
	ErrBadMagic   = errors.New("layout: not a BloomFS image (bad magic)")
	ErrBadVersion = errors.New("layout: unsupported on-disk version")
)

// Superblock anchors the filesystem.
type Superblock struct {
	Magic       uint64
	Version     uint32
	BlockSize   uint32
	TotalBlocks uint64
	InodeTable  uint64 // first block of the inode table
	InodeCount  uint64 // number of inode slots
	RootInode   uint64 // inode id of the root directory
	DataStart   uint64 // first block of the data region (clusters managed by the allocator)
}

// MarshalBinary encodes the superblock into a full zero-padded block.
func (sb *Superblock) MarshalBinary() ([]byte, error) {
	b := make([]byte, block.Size)
	binary.LittleEndian.PutUint64(b[0:], sb.Magic)
	binary.LittleEndian.PutUint32(b[8:], sb.Version)
	binary.LittleEndian.PutUint32(b[12:], sb.BlockSize)
	binary.LittleEndian.PutUint64(b[16:], sb.TotalBlocks)
	binary.LittleEndian.PutUint64(b[24:], sb.InodeTable)
	binary.LittleEndian.PutUint64(b[32:], sb.InodeCount)
	binary.LittleEndian.PutUint64(b[40:], sb.RootInode)
	binary.LittleEndian.PutUint64(b[48:], sb.DataStart)
	return b, nil
}

// ReadSuperblock loads and validates the superblock from dev.
func ReadSuperblock(dev block.Device) (*Superblock, error) {
	raw, err := dev.ReadBlock(SuperblockBlock)
	if err != nil {
		return nil, err
	}
	sb := &Superblock{
		Magic:       binary.LittleEndian.Uint64(raw[0:]),
		Version:     binary.LittleEndian.Uint32(raw[8:]),
		BlockSize:   binary.LittleEndian.Uint32(raw[12:]),
		TotalBlocks: binary.LittleEndian.Uint64(raw[16:]),
		InodeTable:  binary.LittleEndian.Uint64(raw[24:]),
		InodeCount:  binary.LittleEndian.Uint64(raw[32:]),
		RootInode:   binary.LittleEndian.Uint64(raw[40:]),
		DataStart:   binary.LittleEndian.Uint64(raw[48:]),
	}
	if sb.Magic != Magic {
		return nil, ErrBadMagic
	}
	if sb.Version != Version {
		return nil, ErrBadVersion
	}
	return sb, nil
}

// Format lays out a fresh image on dev with at least inodeCount inode slots:
// superblock at block 0, then a zeroed inode table. It writes and syncs the
// superblock and returns it. (Free-space bitmap and root directory come in
// later stages, §B2.)
func Format(dev block.Device, inodeCount uint64) (*Superblock, error) {
	if inodeCount == 0 {
		inodeCount = inode.PerBlock
	}
	inodeBlocks := (inodeCount + inode.PerBlock - 1) / inode.PerBlock
	need := 1 + inodeBlocks // superblock + inode table
	if dev.Blocks() < need {
		return nil, fmt.Errorf("layout: device too small: have %d blocks, need %d", dev.Blocks(), need)
	}

	zero := make([]byte, block.Size)
	for i := uint64(0); i < inodeBlocks; i++ {
		if err := dev.WriteBlock(SuperblockBlock+1+i, zero); err != nil {
			return nil, err
		}
	}

	sb := &Superblock{
		Magic:       Magic,
		Version:     Version,
		BlockSize:   block.Size,
		TotalBlocks: dev.Blocks(),
		InodeTable:  SuperblockBlock + 1,
		InodeCount:  inodeBlocks * inode.PerBlock,
		RootInode:   0,
		DataStart:   SuperblockBlock + 1 + inodeBlocks,
	}
	enc, _ := sb.MarshalBinary()
	if err := dev.WriteBlock(SuperblockBlock, enc); err != nil {
		return nil, err
	}
	if err := dev.Sync(); err != nil {
		return nil, err
	}
	return sb, nil
}
