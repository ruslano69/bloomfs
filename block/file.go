package block

import "os"

// FileDevice is a Device backed by a fixed-size file image (the emulated disk
// of §1.2). Blocks are addressed by offset = num * Size.
type FileDevice struct {
	f      *os.File
	blocks uint64
}

var _ Device = (*FileDevice)(nil)

// Create makes (truncating any existing file) an image of `blocks` blocks.
func Create(path string, blocks uint64) (*FileDevice, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(int64(blocks * Size)); err != nil {
		f.Close()
		return nil, err
	}
	return &FileDevice{f: f, blocks: blocks}, nil
}

// Open opens an existing image, inferring the block count from its size.
func Open(path string) (*FileDevice, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &FileDevice{f: f, blocks: uint64(st.Size()) / Size}, nil
}

// ReadBlock reads block num into a fresh buffer.
func (d *FileDevice) ReadBlock(num uint64) ([]byte, error) {
	if num >= d.blocks {
		return nil, ErrOutOfRange
	}
	out := make([]byte, Size)
	if _, err := d.f.ReadAt(out, int64(num*Size)); err != nil {
		return nil, err
	}
	return out, nil
}

// WriteBlock writes one block at num.
func (d *FileDevice) WriteBlock(num uint64, data []byte) error {
	if num >= d.blocks {
		return ErrOutOfRange
	}
	if len(data) != Size {
		return ErrShortData
	}
	_, err := d.f.WriteAt(data, int64(num*Size))
	return err
}

func (d *FileDevice) Blocks() uint64 { return d.blocks }
func (d *FileDevice) Sync() error    { return d.f.Sync() }
func (d *FileDevice) Close() error   { return d.f.Close() }
