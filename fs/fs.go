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
	"sync"

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

// FS is a mounted BloomFS instance.
//
// Concurrency (§B6): a single RWMutex gives the read/write contract that the
// kernel's parallel Lookup bombardment needs — many concurrent readers
// (Lookup/Readdir/ReadFile), one exclusive writer (Mkdir/Create/WriteFile/
// Unlink/Commit). Open directories are cached (dcache) so a hot Lookup is a
// pure RAM hit on the Bloom-segmented index instead of a disk read + decrypt +
// decompress + rebuild. The cache is a sync.Map so concurrent readers can
// populate it; the cached directory's contents are only mutated under the
// exclusive write lock, so readers never race a writer.
type FS struct {
	mu     sync.RWMutex
	dcache sync.Map // uint64 inode id -> *cachedDir (open-directory cache, §B11)

	dev    block.Device
	ub     *cow.Uberblock
	bm     *alloc.Bitmap
	ddt    *dedup.Table
	inodes *inode.Store
	bs     *store.BlockStore

	// openCount tracks live open handles per inode (RAM-only). A file with
	// Nlink == 0 (no names) but openCount > 0 stays alive until the last Close
	// (POSIX unlink-of-open-file, §E3). Not persisted: a crash drops all handles.
	openCount map[uint64]uint32
}

// cachedDir is a resident open directory: its in-memory Bloom-segmented index
// plus the inode it persists to.
type cachedDir struct {
	dir *dir.Directory
	in  *inode.Inode
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
		dev:       dev,
		ub:        ub,
		bm:        bm,
		ddt:       ddt,
		inodes:    inode.NewStore(dev, ub.InodeTable),
		bs:        bs,
		openCount: make(map[uint64]uint32),
	}, nil
}

// Root returns the root directory's inode id.
func (f *FS) Root() uint64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.ub.RootInode
}

// Commit persists the current state durably (CoW transaction, §B1).
func (f *FS) Commit() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commitLocked()
}

// commitLocked performs the commit assuming the write lock is held.
func (f *FS) commitLocked() error {
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

// openDir returns the resident open directory for id, loading and caching it on
// first access (so a hot Lookup is a RAM hit, not a disk read + decrypt +
// decompress + rebuild). Caller holds f.mu (read or write).
func (f *FS) openDir(id uint64) (*cachedDir, error) {
	if v, ok := f.dcache.Load(id); ok {
		return v.(*cachedDir), nil
	}
	h, err := f.loadDirFromDisk(id)
	if err != nil {
		return nil, err
	}
	// Two concurrent readers may both load on a miss; LoadOrStore keeps one.
	actual, _ := f.dcache.LoadOrStore(id, h)
	return actual.(*cachedDir), nil
}

// loadDirFromDisk reads a directory inode and rebuilds its Bloom-segmented index
// from the persisted entries (§3.3 rebuild-from-keys).
func (f *FS) loadDirFromDisk(id uint64) (*cachedDir, error) {
	in, err := f.inodes.Get(id)
	if err != nil {
		return nil, err
	}
	if in.Type != inode.TypeDir {
		return nil, ErrNotDir
	}
	blob, err := f.getData(in)
	if err != nil {
		return nil, err
	}
	entries, err := decodeDirEntries(blob)
	if err != nil {
		return nil, err
	}
	d := dir.New()
	for _, e := range entries {
		d.Add(e.Name, e.Inode)
	}
	return &cachedDir{dir: d, in: in}, nil
}

// storeDir serializes a cached directory back into its inode. Caller holds the
// write lock (the cached directory was just mutated).
func (f *FS) storeDir(id uint64, h *cachedDir) error {
	return f.setData(id, h.in, encodeDirEntries(h.dir.Entries()))
}

// --- VFS operations ---

// Lookup resolves name in directory parent to an inode id.
func (f *FS) Lookup(parent uint64, name string) (uint64, bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	h, err := f.openDir(parent)
	if err != nil {
		return 0, false, err
	}
	id, ok := h.dir.Find(name)
	return uint64(id), ok, nil
}

// Readdir lists the names in directory id.
func (f *FS) Readdir(id uint64) ([]string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	h, err := f.openDir(id)
	if err != nil {
		return nil, err
	}
	return h.dir.List(), nil
}

// create adds a new inode of the given type under parent/name.
func (f *FS) create(parent uint64, name string, typ uint8, perms uint8) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.openDir(parent)
	if err != nil {
		return 0, err
	}
	if _, ok := h.dir.Find(name); ok {
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
	h.dir.Add(name, dir.InodeID(id))
	if err := f.storeDir(parent, h); err != nil {
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
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.RLock()
	defer f.mu.RUnlock()
	in, err := f.inodes.Get(id)
	if err != nil {
		return nil, err
	}
	if in.Type != inode.TypeRegular {
		return nil, ErrNotFile
	}
	return f.getData(in)
}

// Unlink removes name from directory parent. The link count drops by one; the
// inode and its data are reclaimed only when no names remain (Nlink == 0, §E4)
// AND no open handle references it (openCount == 0, §E3) — otherwise the file
// stays readable through its handle until the last Close.
func (f *FS) Unlink(parent uint64, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.openDir(parent)
	if err != nil {
		return err
	}
	id, ok := h.dir.Find(name)
	if !ok {
		return ErrNotFound
	}
	child, err := f.inodes.Get(uint64(id))
	if err != nil {
		return err
	}

	h.dir.Delete(name)
	if err := f.storeDir(parent, h); err != nil {
		return err
	}

	if child.Nlink > 0 {
		child.Nlink--
	}
	if child.Nlink == 0 && f.openCount[uint64(id)] == 0 {
		return f.reclaim(uint64(id), child)
	}
	return f.inodes.Put(uint64(id), child) // persist the decremented link count
}

// reclaim frees an inode's data and clears it (caller holds the write lock). The
// inode slot itself is leaked under the bump allocator; a free-inode list is a
// later refinement (§B2).
func (f *FS) reclaim(id uint64, in *inode.Inode) error {
	if in.Size > 0 {
		f.bs.Release(store.UnmarshalRef(in.BlockMap[:store.RefSize]))
	}
	return f.inodes.Put(id, &inode.Inode{}) // zeroed: Nlink 0, Size 0, no data
}

// Link creates a hard link: a second name in parent pointing at an existing
// non-directory inode (§E4). POSIX forbids hard links to directories.
func (f *FS) Link(parent uint64, name string, targetID uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	target, err := f.inodes.Get(targetID)
	if err != nil {
		return err
	}
	if target.Type == inode.TypeDir {
		return ErrNotFile
	}
	h, err := f.openDir(parent)
	if err != nil {
		return err
	}
	if _, ok := h.dir.Find(name); ok {
		return ErrExists
	}
	target.Nlink++
	if err := f.inodes.Put(targetID, target); err != nil {
		return err
	}
	h.dir.Add(name, dir.InodeID(targetID))
	return f.storeDir(parent, h)
}

// Rename moves srcName in srcParent to dstName in dstParent. If dstName already
// exists it is atomically replaced (its link count drops, reclaimed if it hits
// zero, §E1/§E2). Atomicity holds across Commit/Fsync: after a successful
// commit you observe either the old or the new layout, never a half state (the
// CoW transaction, §B1). The intra-session window is bounded by the
// inode-table-in-place limitation (documented).
func (f *FS) Rename(srcParent uint64, srcName string, dstParent uint64, dstName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	sh, err := f.openDir(srcParent)
	if err != nil {
		return err
	}
	id, ok := sh.dir.Find(srcName)
	if !ok {
		return ErrNotFound
	}
	dh, err := f.openDir(dstParent)
	if err != nil {
		return err
	}

	// Overwrite: drop the existing destination's link first.
	if oldID, ok := dh.dir.Find(dstName); ok {
		if uint64(oldID) == uint64(id) { // renaming onto itself: no-op
			return nil
		}
		old, err := f.inodes.Get(uint64(oldID))
		if err != nil {
			return err
		}
		if old.Nlink > 0 {
			old.Nlink--
		}
		if old.Nlink == 0 && f.openCount[uint64(oldID)] == 0 {
			if err := f.reclaim(uint64(oldID), old); err != nil {
				return err
			}
		} else if err := f.inodes.Put(uint64(oldID), old); err != nil {
			return err
		}
		dh.dir.Delete(dstName)
	}

	dh.dir.Add(dstName, id)
	sh.dir.Delete(srcName)

	// Persist destination first, then source. If src == dst they are the same
	// cached handle, so a single store covers both edits.
	if dstParent == srcParent {
		return f.storeDir(srcParent, sh)
	}
	if err := f.storeDir(dstParent, dh); err != nil {
		return err
	}
	return f.storeDir(srcParent, sh)
}

// Fsync makes all changes durable (POSIX fsync, §E7). BloomFS gives the strong
// guarantee: after Fsync returns successfully, the data survives power loss.
// It is the CoW commit point (§B1).
func (f *FS) Fsync() error { return f.Commit() }

// Handle is an open reference to a file. It keeps the inode alive even after the
// file is unlinked, until Close (§E3).
type Handle struct {
	fs     *FS
	id     uint64
	closed bool
}

// Open returns a handle to regular file id and records the open reference.
func (f *FS) Open(id uint64) (*Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, err := f.inodes.Get(id)
	if err != nil {
		return nil, err
	}
	if in.Type != inode.TypeRegular {
		return nil, ErrNotFile
	}
	f.openCount[id]++
	return &Handle{fs: f, id: id}, nil
}

// Read returns the file's current contents through the handle — this works even
// if the file has been unlinked (§E3).
func (h *Handle) Read() ([]byte, error) {
	h.fs.mu.RLock()
	defer h.fs.mu.RUnlock()
	in, err := h.fs.inodes.Get(h.id)
	if err != nil {
		return nil, err
	}
	return h.fs.getData(in)
}

// Close drops the open reference. If the file was unlinked while open and this
// is the last handle, its storage is reclaimed now (§E3).
func (h *Handle) Close() error {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	f := h.fs
	if f.openCount[h.id] > 0 {
		f.openCount[h.id]--
	}
	if f.openCount[h.id] == 0 {
		delete(f.openCount, h.id)
		in, err := f.inodes.Get(h.id)
		if err != nil {
			return err
		}
		if in.Nlink == 0 { // unlinked while open: reclaim now
			return f.reclaim(h.id, in)
		}
	}
	return nil
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
