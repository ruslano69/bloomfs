package block

import (
	"os"
	"unsafe"
)

// UnbufferedDevice is a file-image Device that bypasses the OS page cache
// (O_DIRECT on Linux, FILE_FLAG_NO_BUFFERING on Windows), so every ReadBlock is a
// real device read — the physical-filesystem penalty that the page cache would
// otherwise hide. It exists for the archive benchmark, to measure the cold path
// honestly without injecting any synthetic delay; production uses the buffered
// FileDevice. On platforms with no unbuffered mode it falls back to buffered I/O
// (openUnbufferedFile), in which case the cold ceiling is understated.
type UnbufferedDevice struct {
	f      *os.File
	blocks uint64
}

var _ Device = (*UnbufferedDevice)(nil)

// OpenUnbuffered opens an existing image with OS caching disabled.
func OpenUnbuffered(path string) (*UnbufferedDevice, error) {
	f, err := openUnbufferedFile(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &UnbufferedDevice{f: f, blocks: uint64(st.Size()) / Size}, nil
}

// sectorAlign is the alignment unbuffered I/O requires. Size (4096) is a multiple
// of every common sector size (512/4096), so block offsets and lengths are
// already aligned; only the in-memory buffer needs aligning.
const sectorAlign = 4096

// alignedBuf returns a Size-byte slice whose backing array starts on a
// sectorAlign boundary (required by O_DIRECT / NO_BUFFERING).
func alignedBuf() []byte {
	b := make([]byte, Size+sectorAlign)
	off := int(uintptr(unsafe.Pointer(&b[0])) & (sectorAlign - 1))
	if off != 0 {
		off = sectorAlign - off
	}
	return b[off : off+Size]
}

// ReadBlock reads block num straight from the device into an aligned buffer.
func (d *UnbufferedDevice) ReadBlock(num uint64) ([]byte, error) {
	if num >= d.blocks {
		return nil, ErrOutOfRange
	}
	out := alignedBuf()
	if _, err := d.f.ReadAt(out, int64(num*Size)); err != nil {
		return nil, err
	}
	return out, nil
}

// WriteBlock writes one block (aligned). The benchmark builds with the buffered
// device and only reads through this one, but a correct WriteBlock keeps the type
// a usable Device.
func (d *UnbufferedDevice) WriteBlock(num uint64, data []byte) error {
	if num >= d.blocks {
		return ErrOutOfRange
	}
	if len(data) != Size {
		return ErrShortData
	}
	buf := alignedBuf()
	copy(buf, data)
	_, err := d.f.WriteAt(buf, int64(num*Size))
	return err
}

func (d *UnbufferedDevice) Blocks() uint64 { return d.blocks }
func (d *UnbufferedDevice) Sync() error    { return d.f.Sync() }
func (d *UnbufferedDevice) Close() error   { return d.f.Close() }
