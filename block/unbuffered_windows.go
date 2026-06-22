//go:build windows

package block

import (
	"os"
	"syscall"
)

// fileFlagNoBuffering opens a handle whose reads/writes bypass the Windows system
// cache. I/O must be sector-aligned in offset, length and buffer address (see
// sectorAlign / alignedBuf).
const fileFlagNoBuffering = 0x20000000

func openUnbufferedFile(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		fileFlagNoBuffering,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
