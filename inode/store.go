package inode

import "github.com/ruslano69/bloomfs/block"

// Store reads and writes inodes packed PerBlock to a block device, starting at
// TableStart. The InodeID is the 0-based index into the table.
type Store struct {
	dev        block.Device
	tableStart uint64
}

// NewStore returns a Store over dev whose inode table begins at tableStart.
func NewStore(dev block.Device, tableStart uint64) *Store {
	return &Store{dev: dev, tableStart: tableStart}
}

// locate returns the device block holding inode id and the byte offset within it.
func (s *Store) locate(id uint64) (blk uint64, off int) {
	return s.tableStart + id/PerBlock, int(id%PerBlock) * Size
}

// Get loads inode id.
func (s *Store) Get(id uint64) (*Inode, error) {
	blk, off := s.locate(id)
	raw, err := s.dev.ReadBlock(blk)
	if err != nil {
		return nil, err
	}
	var in Inode
	if err := in.UnmarshalBinary(raw[off : off+Size]); err != nil {
		return nil, err
	}
	return &in, nil
}

// Put stores inode id, preserving the other inodes that share its block
// (read-modify-write).
func (s *Store) Put(id uint64, in *Inode) error {
	blk, off := s.locate(id)
	raw, err := s.dev.ReadBlock(blk)
	if err != nil {
		return err
	}
	enc, err := in.MarshalBinary()
	if err != nil {
		return err
	}
	copy(raw[off:off+Size], enc)
	return s.dev.WriteBlock(blk, raw)
}
