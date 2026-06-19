package layout

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/ruslano69/bloomfs/block"
	"github.com/ruslano69/bloomfs/inode"
)

func TestFormatAndRead(t *testing.T) {
	dev := block.NewMem(64)
	sb, err := Format(dev, 100) // 100 inodes -> ceil(100/32)=4 table blocks
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if sb.InodeTable != 1 || sb.InodeCount != 128 || sb.DataStart != 5 {
		t.Fatalf("unexpected layout: InodeTable=%d InodeCount=%d DataStart=%d",
			sb.InodeTable, sb.InodeCount, sb.DataStart)
	}

	got, err := ReadSuperblock(dev)
	if err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	if *got != *sb {
		t.Fatalf("superblock round-trip mismatch:\n have %+v\n want %+v", got, sb)
	}
}

func TestFormatTooSmall(t *testing.T) {
	dev := block.NewMem(2) // 1 superblock + needs >2 for 100 inodes
	if _, err := Format(dev, 100); err == nil {
		t.Fatal("expected device-too-small error")
	}
}

func TestBadMagic(t *testing.T) {
	dev := block.NewMem(8) // never formatted
	if _, err := ReadSuperblock(dev); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("expected ErrBadMagic, got %v", err)
	}
}

// TestEndToEndFileImage is the headline Stage B check: format a real file image,
// write the root inode, close, reopen, and read everything back.
func TestEndToEndFileImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bloomfs.img")

	dev, err := block.Create(path, 256)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	sb, err := Format(dev, 1000)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	store := inode.NewStore(dev, sb.InodeTable)
	root := &inode.Inode{
		Nlink:          2, // "." and ".."
		Type:           inode.TypeDir,
		Permissions:    0o75,
		RecordSizeLog2: 12,
		Mtime:          42,
	}
	if err := store.Put(sb.RootInode, root); err != nil {
		t.Fatalf("put root inode: %v", err)
	}
	if err := dev.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen the image from scratch.
	dev2, err := block.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer dev2.Close()

	sb2, err := ReadSuperblock(dev2)
	if err != nil {
		t.Fatalf("reread superblock: %v", err)
	}
	if *sb2 != *sb {
		t.Fatalf("superblock changed across reopen:\n have %+v\n want %+v", sb2, sb)
	}

	store2 := inode.NewStore(dev2, sb2.InodeTable)
	got, err := store2.Get(sb2.RootInode)
	if err != nil {
		t.Fatalf("get root inode: %v", err)
	}
	if got.Type != inode.TypeDir || got.Nlink != 2 || got.Permissions != 0o75 || got.Mtime != 42 {
		t.Fatalf("root inode mismatch after reopen: %+v", got)
	}
}
