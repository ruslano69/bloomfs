// Package fs is the integration layer (Stage D): it wires the directory index
// (dir), metadata (inode), the data pipeline (store) and the Copy-on-Write
// durability layer (cow) into a single filesystem, exposed through a small
// VFS-style API (Mkdir/Create/Lookup/Read/Write/Readdir/Unlink/Commit).
//
// This is deliberately decoupled from any kernel mount: a FUSE/WinFsp binding
// would be a thin shim forwarding to these methods. The whole layer is therefore
// testable without a mount, on any platform.
//
// File contents are chunked into recordsize records (§4.5): the record size is
// derived from the file's total size and frozen once the file holds data. A
// single-record file stores its store.Ref inline in the inode block map; a
// multi-record file stores a ref-list block-map blob and keeps a ref to it
// inline (§4.4). Each record is an independent dedup/compress/encrypt unit, so
// addressable ReadAt/WriteAt touch only the records they overlap.
//
// The whole metadata set — the inode table, the free-space bitmap and the dedup
// table — is held in RAM and CoW-committed as one atomic snapshot (§B1). Nothing
// is written in place, so an uncommitted mutation vanishes after a crash and a
// committed one is all-or-nothing: this is what makes rename/unlink/hardlink
// crash-atomic and gives Fsync its strong durability guarantee (§E).
package fs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

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
	ErrNotDir     = errors.New("fs: not a directory")
	ErrNotFile    = errors.New("fs: not a regular file")
	ErrExists     = errors.New("fs: name already exists")
	ErrNotFound   = errors.New("fs: no such entry")
	ErrCorrupt    = errors.New("fs: corrupt directory data")
	ErrNoInodes   = errors.New("fs: inode table full")
	ErrIsDir      = errors.New("fs: is a directory")
	ErrNotEmpty   = errors.New("fs: directory not empty")
	ErrInvalid    = errors.New("fs: invalid argument")
	ErrPermission = errors.New("fs: permission denied")
)

// Access mask bits, matching POSIX access(2) / the FUSE access opcode.
const (
	accessX uint32 = 1 // X_OK
	accessW uint32 = 2 // W_OK
	accessR uint32 = 4 // R_OK
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
	inodes *inode.Table
	bs     *store.BlockStore

	// openCount tracks live open handles per inode (RAM-only). A file with
	// Nlink == 0 (no names) but openCount > 0 stays alive until the last Close
	// (POSIX unlink-of-open-file, §E3). Not persisted: a crash drops all handles.
	openCount map[uint64]uint32

	// freeInodes is the reuse stack of reclaimed inode ids (§F5). It is not
	// persisted: Mount rebuilds it by scanning the committed table for free slots
	// (Nlink == 0), so it always matches the rolled-back-to state after a crash.
	freeInodes []uint64

	// clock returns the current time in Unix nanoseconds; it stamps a/m/ctime on
	// mutations (§F6). A field (not a global) so tests can drive it deterministically.
	clock func() uint64

	// metaBuf is the reusable serialization buffer for the CoW metadata snapshot,
	// sized to one metadata slot and reused across commits so a commit allocates
	// nothing (it is only ever touched under the write lock during commitLocked).
	metaBuf []byte
}

// dirPageSize is the directory persistence unit: one entry list is split across
// fixed 4 KiB pages, each persisted as exactly one data record. A single
// create/unlink rewrites only the one page it touches (record-addressable
// WriteAt carries the rest over by ref), so a name change costs O(1) crypto
// work instead of re-serializing the whole directory (the old write-amplifying
// storeDir). Names are NAME_MAX-bounded, so an entry always fits in a page.
const dirPageSize = 4096

// cachedDir is a resident open directory: its in-memory Bloom-segmented index
// (the lookup authority) plus the page layout it persists to. The two are kept
// in sync through add/del; pages/pageOf describe where each name's bytes live so
// a mutation can rewrite just the affected page.
type cachedDir struct {
	dir    *dir.Directory
	in     *inode.Inode
	pages  [][]dir.Entry    // entries grouped by page index (on-disk layout)
	used   []int            // bytes occupied per page, including the count header
	pageOf map[string]int   // name -> page index, for locating a name on delete
	dirty  map[int]struct{} // pages changed since the last flush
}

// entryBytes is the on-disk size of one directory entry: a 10-byte header
// (nameLen u16 + inode u64) plus the name.
func entryBytes(name string) int { return 10 + len(name) }

// add links name -> id in both the Bloom index and the page layout, marking the
// chosen page dirty. It returns false if name already exists. New entries fill
// the first page with room (reusing space freed by deletes) or open a new page.
func (h *cachedDir) add(name string, id dir.InodeID) bool {
	if !h.dir.Add(name, id) {
		return false
	}
	need := entryBytes(name)
	p := -1
	for i, u := range h.used {
		if u+need <= dirPageSize {
			p = i
			break
		}
	}
	if p < 0 {
		p = len(h.pages)
		h.pages = append(h.pages, nil)
		h.used = append(h.used, 4) // 4-byte page header (entry count)
	}
	h.pages[p] = append(h.pages[p], dir.Entry{Name: name, Inode: id})
	h.used[p] += need
	h.pageOf[name] = p
	h.markDirty(p)
	return true
}

// del unlinks name from both the Bloom index and its page, marking that page
// dirty. It returns false if name was not present.
func (h *cachedDir) del(name string) bool {
	p, ok := h.pageOf[name]
	if !ok {
		return false
	}
	h.dir.Delete(name)
	page := h.pages[p]
	for i, e := range page {
		if e.Name == name {
			h.pages[p] = append(page[:i], page[i+1:]...)
			break
		}
	}
	h.used[p] -= entryBytes(name)
	delete(h.pageOf, name)
	h.markDirty(p)
	return true
}

func (h *cachedDir) markDirty(p int) {
	if h.dirty == nil {
		h.dirty = make(map[int]struct{})
	}
	h.dirty[p] = struct{}{}
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
	root := &inode.Inode{Type: inode.TypeDir, Nlink: 2, Mode: 0o755}
	now := f.clock()
	root.Atime, root.Mtime, root.Ctime = now, now, now
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
	ub, bm, ddt, inodes, err := cow.Mount(dev)
	if err != nil {
		return nil, err
	}
	bs, err := store.New(dev, bm, ddt, key)
	if err != nil {
		return nil, err
	}
	return &FS{
		dev:        dev,
		ub:         ub,
		bm:         bm,
		ddt:        ddt,
		inodes:     inodes,
		bs:         bs,
		openCount:  make(map[uint64]uint32),
		freeInodes: rebuildFreeInodes(inodes),
		clock:      func() uint64 { return uint64(time.Now().UnixNano()) },
		metaBuf:    make([]byte, ub.MetaBlocks*block.Size),
	}, nil
}

// touchMod stamps an inode's modification time and metadata-change time (§F6):
// a content/size change bumps both mtime and ctime.
func (f *FS) touchMod(in *inode.Inode) {
	t := f.clock()
	in.Mtime = t
	in.Ctime = t
}

// touchMeta stamps only the metadata-change time (link count, rename), per POSIX.
func (f *FS) touchMeta(in *inode.Inode) { in.Ctime = f.clock() }

// rebuildFreeInodes scans the committed table for reclaimable slots (Nlink == 0)
// within the high-water mark, reconstructing the reuse stack after a mount (§F5).
// The root inode keeps Nlink >= 2, so it is never collected.
func rebuildFreeInodes(t *inode.Table) []uint64 {
	var free []uint64
	for id := uint64(0); id < t.Count(); id++ {
		in, err := t.Get(id)
		if err != nil {
			break
		}
		if in.Nlink == 0 {
			free = append(free, id)
		}
	}
	return free
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
	ub, err := cow.Commit(f.dev, f.ub, f.bm, f.ddt, f.inodes, f.ub.RootInode, f.ub.NextInode, f.metaBuf)
	if err != nil {
		return err
	}
	f.ub = ub
	return nil
}

// --- inode helpers ---

// allocInode reserves an inode id, reusing a reclaimed one if available (§F5)
// and falling back to the bump allocator (persisted via NextInode) otherwise.
// The inode table is fixed-capacity in this prototype, so the bump allocator
// stops at Cap() with a clean ErrNoInodes instead of a later opaque Put failure.
func (f *FS) allocInode() (uint64, error) {
	if n := len(f.freeInodes); n > 0 {
		id := f.freeInodes[n-1]
		f.freeInodes = f.freeInodes[:n-1]
		return id, nil
	}
	if f.ub.NextInode >= f.inodes.Cap() {
		return 0, ErrNoInodes
	}
	id := f.ub.NextInode
	f.ub.NextInode++
	return id, nil
}

// recordSizeFor returns the record size in bytes and its log2 for a file of the
// given total size, per the SPEC §4.5 table. The record is the unit of
// compression and dedup; it is derived once and then frozen for the file.
func recordSizeFor(size uint64) (uint64, uint8) {
	switch {
	case size < 32*1024:
		return 4 * 1024, 12
	case size < 256*1024:
		return 8 * 1024, 13
	case size < 2*1024*1024:
		return 16 * 1024, 14
	default:
		return 32 * 1024, 15
	}
}

// splitRecords cuts blob into rs-sized records (the last one may be shorter).
func splitRecords(blob []byte, rs uint64) [][]byte {
	var out [][]byte
	for off := 0; off < len(blob); off += int(rs) {
		end := off + int(rs)
		if end > len(blob) {
			end = len(blob)
		}
		out = append(out, blob[off:end])
	}
	return out
}

// encodeRefs serializes a ref list into a block-map blob: count u32, then
// count × RefSize. decodeRefs is its inverse.
func encodeRefs(refs []store.Ref) []byte {
	buf := make([]byte, 4+len(refs)*store.RefSize)
	binary.LittleEndian.PutUint32(buf, uint32(len(refs)))
	for i, r := range refs {
		copy(buf[4+i*store.RefSize:], r.Marshal())
	}
	return buf
}

func decodeRefs(b []byte) ([]store.Ref, error) {
	if len(b) < 4 {
		return nil, ErrCorrupt
	}
	n := int(binary.LittleEndian.Uint32(b))
	if len(b) < 4+n*store.RefSize {
		return nil, fmt.Errorf("%w: truncated block map", ErrCorrupt)
	}
	out := make([]store.Ref, n)
	for i := 0; i < n; i++ {
		out[i] = store.UnmarshalRef(b[4+i*store.RefSize:])
	}
	return out, nil
}

// loadRefs returns the inode's data records in order. A single-record file holds
// its ref inline (FlagInlineExtents); a multi-record file holds a ref to a
// block-map blob, which is read and decoded here.
func (f *FS) loadRefs(in *inode.Inode) ([]store.Ref, error) {
	if in.Size == 0 {
		return nil, nil
	}
	if in.Flags&inode.FlagInlineExtents != 0 {
		return []store.Ref{store.UnmarshalRef(in.BlockMap[:store.RefSize])}, nil
	}
	blob, err := f.bs.Read(store.UnmarshalRef(in.BlockMap[:store.RefSize]))
	if err != nil {
		return nil, err
	}
	return decodeRefs(blob)
}

// storeRefs records refs as the inode's block map and sets Size/RecordSizeLog2.
// Zero refs clear the data; one ref goes inline; many refs are written as a
// block-map blob whose own ref is stored inline. Caller releases any prior data.
func (f *FS) storeRefs(in *inode.Inode, refs []store.Ref, size uint64, log2 uint8) error {
	in.BlockMap = [64]byte{}
	in.Flags &^= inode.FlagInlineExtents
	in.Size = size
	in.RecordSizeLog2 = log2
	switch len(refs) {
	case 0:
		in.Size = 0
		in.RecordSizeLog2 = 0
	case 1:
		copy(in.BlockMap[:store.RefSize], refs[0].Marshal())
		in.Flags |= inode.FlagInlineExtents
	default:
		mapRef, err := f.bs.Write(encodeRefs(refs))
		if err != nil {
			return err
		}
		copy(in.BlockMap[:store.RefSize], mapRef.Marshal())
	}
	return nil
}

// releaseData drops one reference to every cluster the inode owns: each data
// record plus, for a multi-record file, the block-map blob itself (§5.4). The
// underlying frees are deferred until the next commit (§F1).
func (f *FS) releaseData(in *inode.Inode) error {
	if in.Size == 0 {
		return nil
	}
	if in.Flags&inode.FlagInlineExtents != 0 {
		f.bs.Release(store.UnmarshalRef(in.BlockMap[:store.RefSize]))
		return nil
	}
	mapRef := store.UnmarshalRef(in.BlockMap[:store.RefSize])
	blob, err := f.bs.Read(mapRef)
	if err != nil {
		return err
	}
	refs, err := decodeRefs(blob)
	if err != nil {
		return err
	}
	for _, r := range refs {
		f.bs.Release(r)
	}
	f.bs.Release(mapRef)
	return nil
}

// setData replaces the inode's contents with blob, re-chunked at a recordsize
// derived from its new length. New records are written before the old data is
// released, so a write failure never loses data and rewriting identical content
// is a dedup no-op. An empty blob clears the file.
func (f *FS) setData(id uint64, in *inode.Inode, blob []byte) error {
	old := *in
	var refs []store.Ref
	var size uint64
	var log2 uint8
	if len(blob) > 0 {
		size = uint64(len(blob))
		var rs uint64
		rs, log2 = recordSizeFor(size)
		for _, rec := range splitRecords(blob, rs) {
			r, err := f.bs.Write(rec)
			if err != nil {
				return err // old data still intact; nothing recorded yet
			}
			refs = append(refs, r)
		}
	}
	if err := f.storeRefs(in, refs, size, log2); err != nil {
		return err
	}
	if err := f.releaseData(&old); err != nil {
		return err
	}
	f.touchMod(in) // a content change bumps mtime+ctime (§F6)
	return f.inodes.Put(id, in)
}

// getData reads the inode's full contents (empty if Size == 0).
func (f *FS) getData(in *inode.Inode) ([]byte, error) {
	if in.Size == 0 {
		return nil, nil
	}
	refs, err := f.loadRefs(in)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, in.Size)
	for _, r := range refs {
		rec, err := f.bs.Read(r)
		if err != nil {
			return nil, err
		}
		out = append(out, rec...)
	}
	return out, nil
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

// loadDirFromDisk reads a directory inode and rebuilds both its Bloom-segmented
// index (§3.3 rebuild-from-keys) and its page layout. The data is a sequence of
// fixed dirPageSize pages; each is decoded independently so the in-RAM layout
// mirrors the on-disk one and later flushes can rewrite individual pages.
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
	h := &cachedDir{dir: dir.New(), in: in, pageOf: make(map[string]int)}
	for off := 0; off < len(blob); off += dirPageSize {
		end := off + dirPageSize
		if end > len(blob) {
			end = len(blob)
		}
		entries, used, err := decodeDirPage(blob[off:end])
		if err != nil {
			return nil, err
		}
		p := len(h.pages)
		h.pages = append(h.pages, entries)
		h.used = append(h.used, used)
		for _, e := range entries {
			h.dir.Add(e.Name, e.Inode)
			h.pageOf[e.Name] = p
		}
	}
	return h, nil
}

// flushDir persists every page changed since the last flush, each as one
// record-addressable WriteAt at its fixed page offset — so untouched pages keep
// their existing refs (no re-hash, no re-encrypt) and only the dirty pages cost
// pipeline work. Caller holds the write lock.
func (f *FS) flushDir(id uint64, h *cachedDir) error {
	for p := range h.dirty {
		if err := f.writeAtLocked(id, h.in, uint64(p)*dirPageSize, encodeDirPage(h.pages[p])); err != nil {
			return err
		}
	}
	h.dirty = nil
	return nil
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

// Readdir lists the names in directory id (a one-shot consistent snapshot, taken
// under the read lock).
func (f *FS) Readdir(id uint64) ([]string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	h, err := f.openDir(id)
	if err != nil {
		return nil, err
	}
	return h.dir.List(), nil
}

// Stat is a point-in-time view of an inode's metadata, the input a kernel
// getattr needs. Mode holds the permission bits only; Type carries the object
// kind (the binding composes them into st_mode).
type Stat struct {
	Ino        uint64
	Size       uint64
	Nlink      uint32
	Generation uint32
	Mode       uint16
	Type       uint8
	UID        uint32
	GID        uint32
	Atime      uint64
	Mtime      uint64
	Ctime      uint64
}

// Stat returns the metadata of inode id.
func (f *FS) Stat(id uint64) (Stat, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	in, err := f.inodes.Get(id)
	if err != nil {
		return Stat{}, err
	}
	return Stat{
		Ino:        id,
		Size:       in.Size,
		Nlink:      in.Nlink,
		Generation: in.Generation,
		Mode:       in.Mode,
		Type:       in.Type,
		UID:        in.UID,
		GID:        in.GID,
		Atime:      in.Atime,
		Mtime:      in.Mtime,
		Ctime:      in.Ctime,
	}, nil
}

// FSStat is the filesystem-wide capacity report a kernel statfs (df) needs.
// Block counts are in BlockSize units; inode counts are whole inodes.
type FSStat struct {
	BlockSize  uint64 // bytes per allocation block (4 KiB)
	Blocks     uint64 // total data blocks
	BlocksFree uint64 // free data blocks
	Files      uint64 // total inode slots
	FilesFree  uint64 // free inode slots
}

// StatFS reports filesystem-wide capacity (data blocks and inode slots). The
// allocator bitmap tracks 4 KiB blocks directly, so its counters map straight
// onto statfs; free inodes are the table ceiling minus what the bump allocator
// has handed out, plus what the reuse stack holds back (§F5).
func (f *FS) StatFS() FSStat {
	f.mu.RLock()
	defer f.mu.RUnlock()
	used := f.ub.NextInode - uint64(len(f.freeInodes))
	cap := f.inodes.Cap()
	return FSStat{
		BlockSize:  block.Size,
		Blocks:     f.bm.Total(),
		BlocksFree: f.bm.Available(),
		Files:      cap,
		FilesFree:  cap - used,
	}
}

// Access reports whether the caller (uid/gid plus supplementary gids) may access
// inode id under mask (a bitwise-OR of accessR/accessW/accessX; mask 0 is an
// existence check). It implements the standard POSIX DAC algorithm — owner bits,
// then group bits, then other bits — with root (uid 0) bypassing read/write and
// only needing one execute bit for X. It returns ErrPermission on denial, or the
// lookup error if id does not exist. This is the same check the kernel performs
// under the FUSE `default_permissions` mount option, exposed here so it is unit-
// testable as any uid and usable for an explicit access() op.
func (f *FS) Access(id uint64, mask, uid, gid uint32, gids []uint32) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	in, err := f.inodes.Get(id)
	if err != nil {
		return err
	}
	if mask == 0 {
		return nil // F_OK: the inode exists
	}
	perm := uint32(in.Mode) & 0o777
	if uid == 0 {
		// root: unrestricted read/write; execute needs at least one x bit (a
		// directory is always traversable by root).
		if mask&accessX != 0 && in.Type != inode.TypeDir && perm&0o111 == 0 {
			return ErrPermission
		}
		return nil
	}
	var shift uint32
	switch {
	case uid == in.UID:
		shift = 6
	case gid == in.GID || containsGID(gids, in.GID):
		shift = 3
	default:
		shift = 0
	}
	if allowed := (perm >> shift) & 7; mask&allowed != mask {
		return ErrPermission
	}
	return nil
}

func containsGID(gids []uint32, want uint32) bool {
	for _, g := range gids {
		if g == want {
			return true
		}
	}
	return false
}

// Dirent is one directory entry: its name, inode id and object type. It carries
// what a kernel readdir reply needs without a follow-up stat per entry.
type Dirent struct {
	Name string
	Ino  uint64
	Type uint8
}

// Readdirents returns directory id's entries (name + id + type) as one consistent
// snapshot taken under the read lock — what the binding turns into a readdir reply.
func (f *FS) Readdirents(id uint64) ([]Dirent, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	h, err := f.openDir(id)
	if err != nil {
		return nil, err
	}
	entries := h.dir.Entries()
	out := make([]Dirent, 0, len(entries))
	for _, e := range entries {
		in, err := f.inodes.Get(uint64(e.Inode))
		if err != nil {
			return nil, err
		}
		out = append(out, Dirent{Name: e.Name, Ino: uint64(e.Inode), Type: in.Type})
	}
	return out, nil
}

// DirHandle is a directory stream over a point-in-time snapshot of the entries
// (§E9). Once opened, concurrent create/unlink/rename in the directory do not
// add, drop or duplicate names in the stream — the kernel's paginated readdir
// needs exactly this stability across many getdents calls.
type DirHandle struct {
	entries []string
	pos     int
}

// OpenDir captures a consistent snapshot of directory id's names for iteration.
// dir.List allocates a fresh slice, so the snapshot is frozen at this instant.
func (f *FS) OpenDir(id uint64) (*DirHandle, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	h, err := f.openDir(id)
	if err != nil {
		return nil, err
	}
	return &DirHandle{entries: h.dir.List()}, nil
}

// Next returns up to n names from the snapshot and advances the cursor; n <= 0
// returns all remaining names. It returns nil once the stream is exhausted.
func (d *DirHandle) Next(n int) []string {
	if d.pos >= len(d.entries) {
		return nil
	}
	end := len(d.entries)
	if n > 0 && d.pos+n < end {
		end = d.pos + n
	}
	out := d.entries[d.pos:end]
	d.pos = end
	return out
}

// create adds a new inode of the given type under parent/name.
func (f *FS) create(parent uint64, name string, typ uint8, mode uint16) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createLocked(parent, name, typ, mode)
}

// createLocked is the body of create with the write lock already held, so a
// caller can create an inode and populate it (e.g. Symlink writes the target)
// inside a single critical section. Caller holds f.mu.
func (f *FS) createLocked(parent uint64, name string, typ uint8, mode uint16) (uint64, error) {
	h, err := f.openDir(parent)
	if err != nil {
		return 0, err
	}
	if _, ok := h.dir.Find(name); ok {
		return 0, ErrExists
	}
	id, err := f.allocInode()
	if err != nil {
		return 0, err
	}
	// Inherit the slot's generation: reclaim bumps it on free, so a reused id
	// gets a fresh generation and stale handles to the old file don't alias it.
	prev, err := f.inodes.Get(id)
	if err != nil {
		return 0, err
	}
	child := &inode.Inode{Type: typ, Nlink: 1, Mode: mode, Generation: prev.Generation}
	if typ == inode.TypeDir {
		child.Nlink = 2
	}
	now := f.clock()
	child.Atime, child.Mtime, child.Ctime = now, now, now
	if err := f.inodes.Put(id, child); err != nil {
		return 0, err
	}
	h.add(name, dir.InodeID(id))
	// A new subdirectory's ".." links back to the parent, so the parent's link
	// count grows by one (POSIX directory nlink). flushDir persists h.in below.
	if typ == inode.TypeDir {
		h.in.Nlink++
	}
	if err := f.flushDir(parent, h); err != nil {
		return 0, err
	}
	return id, nil
}

// Mkdir creates a subdirectory.
func (f *FS) Mkdir(parent uint64, name string) (uint64, error) {
	return f.create(parent, name, inode.TypeDir, 0o755)
}

// Create creates an empty regular file.
func (f *FS) Create(parent uint64, name string) (uint64, error) {
	return f.create(parent, name, inode.TypeRegular, 0o644)
}

// Symlink creates a symbolic link name in parent pointing at target. The target
// path is stored as the link inode's data (through the normal record pipeline,
// so it is deduped/compressed/encrypted like any other content); Readlink reads
// it back. Mode is the conventional 0o777 — symlink permission bits are ignored
// by POSIX, the target's own permissions govern access.
func (f *FS) Symlink(parent uint64, name, target string) (uint64, error) {
	if target == "" {
		return 0, ErrInvalid
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id, err := f.createLocked(parent, name, inode.TypeLink, 0o777)
	if err != nil {
		return 0, err
	}
	in, err := f.inodes.Get(id)
	if err != nil {
		return 0, err
	}
	if err := f.setData(id, in, []byte(target)); err != nil {
		return 0, err
	}
	return id, nil
}

// Readlink returns the target path of symbolic link id (ErrInvalid if id is not
// a symlink).
func (f *FS) Readlink(id uint64) (string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	in, err := f.inodes.Get(id)
	if err != nil {
		return "", err
	}
	if in.Type != inode.TypeLink {
		return "", ErrInvalid
	}
	data, err := f.getData(in)
	if err != nil {
		return "", err
	}
	return string(data), nil
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

// ReadAt returns up to length bytes of file id starting at off, reading only the
// records the range overlaps (§4.5). A read past EOF returns the available
// prefix; a read entirely past EOF returns nil.
func (f *FS) ReadAt(id uint64, off uint64, length int) ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	in, err := f.inodes.Get(id)
	if err != nil {
		return nil, err
	}
	if in.Type != inode.TypeRegular {
		return nil, ErrNotFile
	}
	if length <= 0 || off >= in.Size {
		return nil, nil
	}
	end := off + uint64(length)
	if end > in.Size {
		end = in.Size
	}
	refs, err := f.loadRefs(in)
	if err != nil {
		return nil, err
	}
	rs := uint64(1) << in.RecordSizeLog2
	out := make([]byte, 0, end-off)
	for i := off / rs; i <= (end-1)/rs; i++ {
		rec, err := f.bs.Read(refs[i])
		if err != nil {
			return nil, err
		}
		recStart := i * rs
		recEnd := recStart + uint64(len(rec))
		lo, hi := off, end
		if recStart > lo {
			lo = recStart
		}
		if recEnd < hi {
			hi = recEnd
		}
		if lo < hi {
			out = append(out, rec[lo-recStart:hi-recStart]...)
		}
	}
	return out, nil
}

// WriteAt overwrites file id at offset off with data, extending the file (with a
// zero-filled gap if off is past EOF) as needed. Only the records the write
// overlaps are read-modified-written; untouched records keep their existing refs
// (so their dedup refcounts are undisturbed). The record size is frozen once the
// file holds data; an empty file derives it from the resulting size (§4.5).
func (f *FS) WriteAt(id uint64, off uint64, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, err := f.inodes.Get(id)
	if err != nil {
		return err
	}
	if in.Type != inode.TypeRegular {
		return ErrNotFile
	}
	return f.writeAtLocked(id, in, off, data)
}

// Append atomically writes data at the end of file id and returns the offset it
// landed at. It holds the write lock across the read-size + write, so concurrent
// appends can never lose, duplicate or interleave records (§E8).
func (f *FS) Append(id uint64, data []byte) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, err := f.inodes.Get(id)
	if err != nil {
		return 0, err
	}
	if in.Type != inode.TypeRegular {
		return 0, ErrNotFile
	}
	off := in.Size
	if err := f.writeAtLocked(id, in, off, data); err != nil {
		return 0, err
	}
	return off, nil
}

// writeAtLocked is the body of WriteAt/Append; the caller holds the write lock
// and has already fetched and type-checked in.
func (f *FS) writeAtLocked(id uint64, in *inode.Inode, off uint64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	oldSize := in.Size
	oldRefs, err := f.loadRefs(in)
	if err != nil {
		return err
	}
	// Capture the old block-map container (if any) before we overwrite BlockMap;
	// it is released after the new state is written.
	var oldContainer *store.Ref
	if oldSize > 0 && in.Flags&inode.FlagInlineExtents == 0 {
		c := store.UnmarshalRef(in.BlockMap[:store.RefSize])
		oldContainer = &c
	}

	writeEnd := off + uint64(len(data))
	newSize := oldSize
	if writeEnd > newSize {
		newSize = writeEnd
	}

	var rs uint64
	var log2 uint8
	if oldSize > 0 { // frozen recordsize
		log2 = in.RecordSizeLog2
		rs = uint64(1) << log2
	} else {
		rs, log2 = recordSizeFor(newSize)
	}

	recCount := (newSize + rs - 1) / rs
	newRefs := make([]store.Ref, recCount)
	var toRelease []store.Ref
	for i := uint64(0); i < recCount; i++ {
		recStart := i * rs
		recEnd := recStart + rs
		if recEnd > newSize {
			recEnd = newSize
		}
		touched := writeEnd > recStart && off < recEnd
		if !touched && i < uint64(len(oldRefs)) && recEnd <= oldSize {
			newRefs[i] = oldRefs[i] // carry over untouched, fully-backed record
			continue
		}
		rec := make([]byte, recEnd-recStart)
		if i < uint64(len(oldRefs)) { // preserve bytes outside the write
			old, err := f.bs.Read(oldRefs[i])
			if err != nil {
				return err
			}
			copy(rec, old)
		}
		lo, hi := off, writeEnd
		if recStart > lo {
			lo = recStart
		}
		if recEnd < hi {
			hi = recEnd
		}
		if lo < hi {
			copy(rec[lo-recStart:], data[lo-off:hi-off])
		}
		r, err := f.bs.Write(rec)
		if err != nil {
			return err
		}
		newRefs[i] = r
		if i < uint64(len(oldRefs)) {
			toRelease = append(toRelease, oldRefs[i])
		}
	}

	if err := f.storeRefs(in, newRefs, newSize, log2); err != nil {
		return err
	}
	for _, r := range toRelease {
		f.bs.Release(r)
	}
	if oldContainer != nil {
		f.bs.Release(*oldContainer)
	}
	f.touchMod(in) // a write bumps mtime+ctime (§F6)
	return f.inodes.Put(id, in)
}

// Truncate sets file id's size to newSize: shrinking drops the tail, growing
// zero-extends (§E5). This prototype re-chunks the whole file, so the record
// size is re-derived from the new size.
func (f *FS) Truncate(id uint64, newSize uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, err := f.inodes.Get(id)
	if err != nil {
		return err
	}
	if in.Type != inode.TypeRegular {
		return ErrNotFile
	}
	if newSize == in.Size {
		return nil
	}
	blob, err := f.getData(in)
	if err != nil {
		return err
	}
	if newSize <= uint64(len(blob)) {
		blob = blob[:newSize]
	} else {
		grown := make([]byte, newSize)
		copy(grown, blob)
		blob = grown
	}
	return f.setData(id, in, blob)
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
	if child.Type == inode.TypeDir {
		return ErrIsDir // directories are removed with Rmdir (POSIX EISDIR)
	}

	h.del(name)
	if err := f.flushDir(parent, h); err != nil {
		return err
	}

	if child.Nlink > 0 {
		child.Nlink--
	}
	if child.Nlink == 0 && f.openCount[uint64(id)] == 0 {
		return f.reclaim(uint64(id), child)
	}
	f.touchMeta(child)                     // link-count change bumps ctime (§F6)
	return f.inodes.Put(uint64(id), child) // persist the decremented link count
}

// Rmdir removes an empty subdirectory name from parent (POSIX rmdir). It returns
// ErrNotDir if name is a regular file, ErrNotEmpty if the directory still has
// entries, and reclaims the directory inode and its data otherwise. The parent's
// link count drops by one (the removed child's ".." no longer points to it).
func (f *FS) Rmdir(parent uint64, name string) error {
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
	if child.Type != inode.TypeDir {
		return ErrNotDir
	}
	ch, err := f.openDir(uint64(id))
	if err != nil {
		return err
	}
	if ch.dir.Len() > 0 {
		return ErrNotEmpty
	}

	h.del(name)
	if h.in.Nlink > 0 {
		h.in.Nlink-- // the child's ".." no longer counts toward the parent
	}
	if err := f.flushDir(parent, h); err != nil {
		return err
	}
	return f.reclaim(uint64(id), child)
}

// reclaim frees an inode's data, evicts any cached directory and recycles the id
// (§F5). Caller holds the write lock.
func (f *FS) reclaim(id uint64, in *inode.Inode) error {
	if err := f.releaseData(in); err != nil {
		return err
	}
	f.dcache.Delete(id) // drop any cached directory so a reused id can't alias it
	// Zero the slot but carry a bumped generation forward, then make the id
	// available for reuse (§F5).
	if err := f.inodes.Put(id, &inode.Inode{Generation: in.Generation + 1}); err != nil {
		return err
	}
	f.freeInodes = append(f.freeInodes, id)
	return nil
}

// mutateInode applies fn to inode id and persists it, keeping the open-directory
// cache coherent: if id is a cached directory, fn mutates the cached copy (h.in)
// — the very struct flushDir later serializes — so a metadata change can't be
// silently overwritten by a subsequent directory write (and vice versa). Caller
// holds the write lock.
func (f *FS) mutateInode(id uint64, fn func(*inode.Inode)) error {
	if v, ok := f.dcache.Load(id); ok {
		h := v.(*cachedDir)
		fn(h.in)
		return f.inodes.Put(id, h.in)
	}
	in, err := f.inodes.Get(id)
	if err != nil {
		return err
	}
	fn(in)
	return f.inodes.Put(id, in)
}

// Chmod sets the permission bits of inode id (POSIX chmod); ctime is bumped.
func (f *FS) Chmod(id uint64, mode uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mutateInode(id, func(in *inode.Inode) {
		in.Mode = mode
		in.Ctime = f.clock()
	})
}

// Chown sets the owner and group of inode id (POSIX chown); ctime is bumped.
func (f *FS) Chown(id uint64, uid, gid uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mutateInode(id, func(in *inode.Inode) {
		in.UID = uid
		in.GID = gid
		in.Ctime = f.clock()
	})
}

// Utimes sets the access and modification times of inode id (POSIX utimes);
// ctime is bumped to now since the metadata changed.
func (f *FS) Utimes(id uint64, atime, mtime uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mutateInode(id, func(in *inode.Inode) {
		in.Atime = atime
		in.Mtime = mtime
		in.Ctime = f.clock()
	})
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
	f.touchMeta(target) // link-count change bumps ctime (§F6)
	if err := f.inodes.Put(targetID, target); err != nil {
		return err
	}
	h.add(name, dir.InodeID(targetID))
	return f.flushDir(parent, h)
}

// Rename moves srcName in srcParent to dstName in dstParent. If dstName already
// exists it is atomically replaced (its link count drops, reclaimed if it hits
// zero, §E1/§E2). Atomicity holds across Commit/Fsync: after a successful
// commit you observe either the old or the new layout, never a half state, and
// a crash before the next commit rolls back every edit as a unit — the inode
// table is part of the CoW snapshot (§B1).
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
	// Reject the trivial directory loop: moving a directory into itself. (A
	// deeper loop — into its own descendant — is not yet guarded; see SPEC §F note.)
	if dstParent == uint64(id) {
		return ErrInvalid
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
		} else {
			f.touchMeta(old) // link-count change bumps ctime (§F6)
			if err := f.inodes.Put(uint64(oldID), old); err != nil {
				return err
			}
		}
		dh.del(dstName)
	}

	dh.add(dstName, id)
	sh.del(srcName)

	// The renamed inode's ctime changes (its link/location changed), §F6.
	if moved, err := f.inodes.Get(uint64(id)); err == nil {
		f.touchMeta(moved)
		if err := f.inodes.Put(uint64(id), moved); err != nil {
			return err
		}
	}

	// Persist destination first, then source. If src == dst they are the same
	// cached handle, so a single flush covers both edits (add and del marked
	// their pages dirty on the one handle).
	if dstParent == srcParent {
		return f.flushDir(srcParent, sh)
	}
	if err := f.flushDir(dstParent, dh); err != nil {
		return err
	}
	return f.flushDir(srcParent, sh)
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

// --- directory page serialization ---
//
// A page is a fixed dirPageSize buffer: count u32, then per entry nameLen u16,
// inode u64, name bytes; the remainder is zero padding. The caller guarantees
// the entries fit (used <= dirPageSize), so encoding never overflows.

func encodeDirPage(entries []dir.Entry) []byte {
	buf := make([]byte, dirPageSize)
	binary.LittleEndian.PutUint32(buf, uint32(len(entries)))
	off := 4
	for _, e := range entries {
		binary.LittleEndian.PutUint16(buf[off:], uint16(len(e.Name)))
		binary.LittleEndian.PutUint64(buf[off+2:], uint64(e.Inode))
		off += 10
		off += copy(buf[off:], e.Name)
	}
	return buf
}

// decodeDirPage parses one page, returning its entries and the byte count they
// occupy (including the 4-byte header) for the page's room accounting.
func decodeDirPage(b []byte) ([]dir.Entry, int, error) {
	if len(b) < 4 {
		return nil, 0, ErrCorrupt
	}
	n := binary.LittleEndian.Uint32(b)
	off := 4
	out := make([]dir.Entry, 0, n)
	for i := uint32(0); i < n; i++ {
		if off+10 > len(b) {
			return nil, 0, fmt.Errorf("%w: truncated header", ErrCorrupt)
		}
		nl := int(binary.LittleEndian.Uint16(b[off:]))
		id := dir.InodeID(binary.LittleEndian.Uint64(b[off+2:]))
		off += 10
		if off+nl > len(b) {
			return nil, 0, fmt.Errorf("%w: truncated name", ErrCorrupt)
		}
		out = append(out, dir.Entry{Name: string(b[off : off+nl]), Inode: id})
		off += nl
	}
	return out, off, nil
}
