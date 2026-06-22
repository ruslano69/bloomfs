//go:build linux

package block

import (
	"os"
	"syscall"
)

// openUnbufferedFile opens the image with O_DIRECT so reads go straight to the
// device, skipping the page cache. I/O must be block-aligned (see alignedBuf).
func openUnbufferedFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|syscall.O_DIRECT, 0o600)
}
