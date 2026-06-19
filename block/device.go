// Package block provides the BlockDevice abstraction (§1.2 of the spec): the
// whole filesystem sees the disk as an array of fixed-size blocks. Stage B
// ships an in-memory device (tests) and a file-image device.
//
// O_DIRECT and a own buffer cache (§B11) are deliberately out of scope here —
// Stage B uses ordinary buffered I/O plus an explicit Sync.
package block

import "errors"

// Size is the fixed block size in bytes (§2). 4 KiB matches common drives and
// holds exactly 32 inodes (§4.1).
const Size = 4096

var (
	// ErrOutOfRange is returned for a block index past the device end.
	ErrOutOfRange = errors.New("block: block number out of range")
	// ErrShortData is returned when a write payload is not exactly one block.
	ErrShortData = errors.New("block: data must be exactly one block")
)

// Device is the disk abstraction: read and write fixed-size blocks by index.
// ReadBlock returns a fresh copy the caller may retain and mutate.
type Device interface {
	ReadBlock(num uint64) ([]byte, error)
	WriteBlock(num uint64, data []byte) error
	Blocks() uint64 // total number of blocks
	Sync() error    // flush to durable storage
	Close() error
}

// MemDevice is an in-memory Device backed by a flat byte slice.
type MemDevice struct {
	data   []byte
	blocks uint64
}

var _ Device = (*MemDevice)(nil)

// NewMem returns an in-memory device of the given block count.
func NewMem(blocks uint64) *MemDevice {
	return &MemDevice{data: make([]byte, blocks*Size), blocks: blocks}
}

// ReadBlock returns a copy of block num.
func (d *MemDevice) ReadBlock(num uint64) ([]byte, error) {
	if num >= d.blocks {
		return nil, ErrOutOfRange
	}
	out := make([]byte, Size)
	copy(out, d.data[num*Size:(num+1)*Size])
	return out, nil
}

// WriteBlock overwrites block num with data (which must be exactly one block).
func (d *MemDevice) WriteBlock(num uint64, data []byte) error {
	if num >= d.blocks {
		return ErrOutOfRange
	}
	if len(data) != Size {
		return ErrShortData
	}
	copy(d.data[num*Size:(num+1)*Size], data)
	return nil
}

func (d *MemDevice) Blocks() uint64 { return d.blocks }
func (d *MemDevice) Sync() error    { return nil }
func (d *MemDevice) Close() error   { return nil }
