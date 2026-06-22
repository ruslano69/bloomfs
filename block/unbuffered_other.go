//go:build !windows && !linux

package block

import "os"

// openUnbufferedFile has no portable unbuffered mode on this platform, so it opens
// the image buffered. The archive benchmark still runs, but the cold ceiling will
// be understated by the OS page cache — note it when reading results.
func openUnbufferedFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0o600)
}
