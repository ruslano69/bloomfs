// Package fs is the integration layer (Stage D): it wires the directory index
// (dir), metadata (inode), the data pipeline (store) and the Copy-on-Write
// durability layer (cow) into a single filesystem, exposed through a small
// VFS-style API (Mkdir/Create/Lookup/Read/Write/Readdir/Unlink/Commit).
//
// This is deliberately decoupled from any kernel mount: a FUSE/WinFsp binding
// would be a thin shim forwarding to these methods. The whole layer is therefore
// testable without a mount, on any platform.
//
// Stage D limitations (documented, not silent):
//   - One data extent per inode: a file or directory's contents are a single
//     store.Write (which may span several clusters). Multi-extent files and the
//     external block-map tree (§4.4) come later.
//   - The inode table is written in place, outside the CoW transaction. Data,
//     the free-space bitmap and the dedup table ARE CoW-committed (§B1), so a
//     clean Commit + remount is consistent; making the inode table itself CoW is
//     a follow-up.
package fs

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ruslano69/bloomfs/alloc"
	"github.com/ruslano69/bloomfs/block"
	"github.com/ruslano69/bloomfs/cow"
	"github.com/ruslano69/bloomfs/dedup"
	"github.com/ruslano69/bloomfs/dir"
	"github.com/ruslano69/bloomfs/inode"
	"github.com/ruslano69/bloomfs/store"
)

const (
	defaultInodeCount = 4096
	defaultDDTReserve = 256 * 1024
)

var (
	ErrNotDir   = errors.New("fs: not a directory")
	ErrNotFile  = errors.New("fs: not a regular file")
	ErrExists   = errors.New("fs: name already exists")
	ErrNotFound = errors.New("fs: no such entry")
	ErrCorrupt  = errors.New("fs: corrupt directory data")
)

// FS is a mounted BloomFS instance. Not safe for concurrent use (locking is §B6).
type FS struct {
	dev    block.Device
	ub     *cow.Uberblock
	bm     *alloc.Bitmap
	ddt    *dedup.Table
	inodes *inode.Store
	bs     *store.BlockStore
}

// Format creates a fresh filesystem on dev and returns it mounted. A nil key
// selects a plaintext pool (§5.5 opt-out); otherwise key is the AES-XTS key.
func Format(dev block.Device, key []byte) (*FS, error) {
	if _, err := cow.Format(dev, defaultInodeCount, defaultDDTReserve); err != nil {
		return nil, err
	}
	f, err := Mount(dev, key)
	if err != nil {
		return nil, err
	}
	// inode 0 is the root directory (empty).
	root := &inode.Inode{Type: inode.TypeDir, Nlink: 2, Permissions: 0o75}
	if err := f.inodes.Put(f.ub.RootInode, root); err != nil {
		return nil, err
	}
	if err := f.Commit(); err != nil {
		return nil, err
	}
	return f, nil
}

// Mount opens an existing filesystem on dev. key must match how it was formatted
// (nil for a plaintext pool).
func Mount(dev block.Device, key []byte) (*FS, error) {
	ub, bm, ddt, err := cow.Mount(dev)
	if err != nil {
		return nil, err
	}
	bs, err := store.New(dev, bm, ddt, key)
	if err != nil {
		return nil, err
	}
	return &FS{
		dev:    dev,
		ub:     ub,
		bm:     bm,
		ddt:    ddt,
		inodes: inode.NewStore(dev, ub.InodeTable),
		bs:     bs,
	}, nil
}

// Root returns the root directory's inode id.
func (f *FS) Root() uint64 { return f.ub.RootInode }

// Commit persists the current state durably (CoW transaction, §B1).
func (f *FS) Commit() error {
	ub, err := cow.Commit(f.dev, f.ub, f.bm, f.ddt, f.ub.RootInode, f.ub.NextInode)
	if err != nil {
		return err
	}
	f.ub = ub
	return nil
}

// --- inode helpers ---

// allocInode reserves a fresh inode id (bump allocator, persisted via NextInode).
func (f *FS) allocInode() uint64 {
	id := f.ub.NextInode
	f.ub.NextInode++
	return id
}

// setData stores blob as the inode's single data extent. It writes the new
// extent before releasing the old one (so a write failure never loses data, and
// rewriting identical content is a no-op via dedup). An empty blob clears data.
func (f *FS) setData(id uint64, in *inode.Inode, blob []byte) error {
	var newRef *store.Ref
	if len(blob) > 0 {
		r, err := f.bs.Write(blob)
		if err != nil {
			return err
		}
		newRef = &r
	}
	if in.Size > 0 { // release the previous extent (dedup refcount, §5.4)
		f.bs.Release(store.UnmarshalRef(in.BlockMap[:store.RefSize]))
	}
	in.BlockMap = [64]byte{}
	if newRef == nil {
		in.Size = 0
	} else {
		copy(in.BlockMap[:store.RefSize], newRef.Marshal())
		in.Size = uint64(len(blob))
		in.Flags |= inode.FlagInlineExtents
	}
	return f.inodes.Put(id, in)
}

// getData reads the inode's data extent (empty if Size == 0).
func (f *FS) getData(in *inode.Inode) ([]byte, error) {
	if in.Size == 0 {
		return nil, nil
	}
	return f.bs.Read(store.UnmarshalRef(in.BlockMap[:store.RefSize]))
}

// --- directory helpers ---

// loadDir reads a directory inode and rebuilds its in-memory index (Bloom
// segments + XXH3) from the persisted entries (§3.3 rebuild-from-keys).
func (f *FS) loadDir(id uint64) (*dir.Directory, *inode.Inode, error) {
	in, err := f.inodes.Get(id)
	if err != nil {
		return nil, nil, err
	}
	if in.Type != inode.TypeDir {
		return nil, nil, ErrNotDir
	}
	blob, err := f.getData(in)
	if err != nil {
		return nil, nil, err
	}
	d := dir.New()
	entries, err := decodeDirEntries(blob)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		d.Add(e.Name, e.Inode)
	}
	return d, in, nil
}

// storeDir serializes a directory's entries back into its inode.
func (f *FS) storeDir(id uint64, in *inode.Inode, d *dir.Directory) error {
	return f.setData(id, in, encodeDirEntries(d.Entries()))
}

// --- VFS operations ---

// Lookup resolves name in directory parent to an inode id.
func (f *FS) Lookup(parent uint64, name string) (uint64, bool, error) {
	d, _, err := f.loadDir(parent)
	if err != nil {
		return 0, false, err
	}
	id, ok := d.Find(name)
	return uint64(id), ok, nil
}

// Readdir lists the names in directory id.
func (f *FS) Readdir(id uint64) ([]string, error) {
	d, _, err := f.loadDir(id)
	if err != nil {
		return nil, err
	}
	return d.List(), nil
}

// create adds a new inode of the given type under parent/name.
func (f *FS) create(parent uint64, name string, typ uint8, perms uint8) (uint64, error) {
	d, pin, err := f.loadDir(parent)
	if err != nil {
		return 0, err
	}
	if _, ok := d.Find(name); ok {
		return 0, ErrExists
	}
	id := f.allocInode()
	child := &inode.Inode{Type: typ, Nlink: 1, Permissions: perms}
	if typ == inode.TypeDir {
		child.Nlink = 2
	}
	if err := f.inodes.Put(id, child); err != nil {
		return 0, err
	}
	d.Add(name, dir.InodeID(id))
	if err := f.storeDir(parent, pin, d); err != nil {
		return 0, err
	}
	return id, nil
}

// Mkdir creates a subdirectory.
func (f *FS) Mkdir(parent uint64, name string) (uint64, error) {
	return f.create(parent, name, inode.TypeDir, 0o75)
}

// Create creates an empty regular file.
func (f *FS) Create(parent uint64, name string) (uint64, error) {
	return f.create(parent, name, inode.TypeRegular, 0o64)
}

// WriteFile replaces the contents of regular file id.
func (f *FS) WriteFile(id uint64, data []byte) error {
	in, err := f.inodes.Get(id)
	if err != nil {
		return err
	}
	if in.Type != inode.TypeRegular {
		return ErrNotFile
	}
	return f.setData(id, in, data)
}

// ReadFile returns the contents of regular file id.
func (f *FS) ReadFile(id uint64) ([]byte, error) {
	in, err := f.inodes.Get(id)
	if err != nil {
		return nil, err
	}
	if in.Type != inode.TypeRegular {
		return nil, ErrNotFile
	}
	return f.getData(in)
}

// Unlink removes name from directory parent, releasing the child's data.
func (f *FS) Unlink(parent uint64, name string) error {
	d, pin, err := f.loadDir(parent)
	if err != nil {
		return err
	}
	id, ok := d.Find(name)
	if !ok {
		return ErrNotFound
	}
	child, err := f.inodes.Get(uint64(id))
	if err != nil {
		return err
	}
	if child.Size > 0 { // free the child's data extent
		f.bs.Release(store.UnmarshalRef(child.BlockMap[:store.RefSize]))
	}
	if !d.Delete(name) {
		return ErrNotFound
	}
	return f.storeDir(parent, pin, d)
}

// --- directory entry serialization ---
//
// Layout: count u32, then per entry: nameLen u16, inode u64, name bytes.

func encodeDirEntries(entries []dir.Entry) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, uint32(len(entries)))
	var hdr [10]byte
	for _, e := range entries {
		binary.LittleEndian.PutUint16(hdr[0:], uint16(len(e.Name)))
		binary.LittleEndian.PutUint64(hdr[2:], uint64(e.Inode))
		buf = append(buf, hdr[:]...)
		buf = append(buf, e.Name...)
	}
	return buf
}

func decodeDirEntries(b []byte) ([]dir.Entry, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if len(b) < 4 {
		return nil, ErrCorrupt
	}
	n := binary.LittleEndian.Uint32(b)
	off := 4
	out := make([]dir.Entry, 0, n)
	for i := uint32(0); i < n; i++ {
		if off+10 > len(b) {
			return nil, fmt.Errorf("%w: truncated header", ErrCorrupt)
		}
		nl := int(binary.LittleEndian.Uint16(b[off:]))
		id := dir.InodeID(binary.LittleEndian.Uint64(b[off+2:]))
		off += 10
		if off+nl > len(b) {
			return nil, fmt.Errorf("%w: truncated name", ErrCorrupt)
		}
		out = append(out, dir.Entry{Name: string(b[off : off+nl]), Inode: id})
		off += nl
	}
	return out, nil
}
