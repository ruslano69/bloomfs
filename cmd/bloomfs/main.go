// Command bloomfs formats and mounts a BloomFS image (Stage E).
//
//	bloomfs format -size 64 disk.img        # 64 MiB image, formatted
//	bloomfs mount [-key HEX] disk.img /mnt   # mount until Ctrl-C / umount
//
// A -key of 64 hex chars (32 bytes) or 128 hex chars (64 bytes) selects AES-XTS;
// an empty key mounts a plaintext pool. Metadata is made durable on fsync and on
// a clean unmount (CoW commit); a hard crash rolls back to the last commit.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/ruslano69/bloomfs/block"
	bloomfs "github.com/ruslano69/bloomfs/fs"
	"github.com/ruslano69/bloomfs/fusefs"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "format":
		doFormat(os.Args[2:])
	case "mount":
		doMount(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  bloomfs format -size <MiB> <image>")
	fmt.Fprintln(os.Stderr, "  bloomfs mount [-key <hex>] [-debug] <image> <mountpoint>")
	os.Exit(2)
}

func parseKey(h string) []byte {
	if h == "" {
		return nil
	}
	k, err := hex.DecodeString(h)
	if err != nil {
		log.Fatalf("bad -key (must be hex): %v", err)
	}
	if len(k) != 32 && len(k) != 64 {
		log.Fatalf("bad -key: need 32 or 64 bytes (got %d)", len(k))
	}
	return k
}

func doFormat(args []string) {
	fl := flag.NewFlagSet("format", flag.ExitOnError)
	sizeMiB := fl.Uint64("size", 64, "image size in MiB")
	keyHex := fl.String("key", "", "AES-XTS key in hex (empty = plaintext)")
	fl.Parse(args)
	if fl.NArg() != 1 {
		usage()
	}
	path := fl.Arg(0)

	blocks := (*sizeMiB * 1024 * 1024) / block.Size
	dev, err := block.Create(path, blocks)
	if err != nil {
		log.Fatalf("create image: %v", err)
	}
	f, err := bloomfs.Format(dev, parseKey(*keyHex))
	if err != nil {
		log.Fatalf("format: %v", err)
	}
	// The freshly created root is owned by uid 0; hand it to the formatting user
	// so they can actually write to their own pool under default_permissions.
	if err := f.Chown(f.Root(), uint32(os.Getuid()), uint32(os.Getgid())); err != nil {
		log.Fatalf("chown root: %v", err)
	}
	if err := f.Fsync(); err != nil {
		log.Fatalf("commit: %v", err)
	}
	if err := dev.Close(); err != nil {
		log.Fatalf("close: %v", err)
	}
	fmt.Printf("formatted %s: %d blocks (%d MiB)\n", path, blocks, *sizeMiB)
}

func doMount(args []string) {
	fl := flag.NewFlagSet("mount", flag.ExitOnError)
	keyHex := fl.String("key", "", "AES-XTS key in hex (empty = plaintext)")
	debug := fl.Bool("debug", false, "log FUSE traffic")
	fl.Parse(args)
	if fl.NArg() != 2 {
		usage()
	}
	path, mountpoint := fl.Arg(0), fl.Arg(1)

	dev, err := block.Open(path)
	if err != nil {
		log.Fatalf("open image: %v", err)
	}
	f, err := bloomfs.Mount(dev, parseKey(*keyHex))
	if err != nil {
		log.Fatalf("mount fs: %v", err)
	}

	opts := &gofs.Options{}
	opts.Debug = *debug
	opts.MountOptions.FsName = "bloomfs"
	opts.MountOptions.Name = "bloomfs"
	// Let the kernel enforce POSIX permission checks from the attributes we
	// report (mode/uid/gid), so chmod/chown actually gate access at the mount.
	opts.MountOptions.Options = append(opts.MountOptions.Options, "default_permissions")
	// Honor a literal mode 0 (chmod 000) instead of go-fuse's default of coercing
	// zero permission bits to 0644/0755 — we want the bits we store to be the bits
	// the kernel enforces under default_permissions.
	opts.NullPermissions = true

	server, err := fusefs.Mount(mountpoint, f, opts)
	if err != nil {
		log.Fatalf("fuse mount: %v", err)
	}
	fmt.Printf("mounted %s at %s (Ctrl-C to unmount)\n", path, mountpoint)

	// Unmount cleanly on a signal so the kernel detaches before we commit+close.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		if err := server.Unmount(); err != nil {
			log.Printf("unmount: %v", err)
		}
	}()

	server.Wait()

	if err := f.Fsync(); err != nil {
		log.Printf("final commit: %v", err)
	}
	if err := dev.Close(); err != nil {
		log.Printf("close: %v", err)
	}
	fmt.Println("unmounted")
}
