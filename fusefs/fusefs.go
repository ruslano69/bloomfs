// Package fusefs binds a bloomfs filesystem to the kernel through FUSE (Stage E).
//
// Each kernel-visible node is a bnode: a bloomfs inode id plus a back-pointer to
// the FS. Every operation is a thin shim onto the bloomfs VFS API, which already
// provides crash-atomic metadata (CoW), recordsize-addressable IO, dedup,
// compression and encryption — the binding adds no storage logic of its own, it
// only translates kernel calls and error codes.
//
// Inode-number mapping: bloomfs ids start at 0 (root), but the kernel reserves
// node id 1 for the FUSE root. We therefore report st_ino = id+1, so the root
// (id 0) is ino 1 and no child can collide with it.
package fusefs

import (
	"context"
	"errors"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	bloomfs "github.com/ruslano69/bloomfs/fs"
	"github.com/ruslano69/bloomfs/inode"
)

// bnode is a kernel-visible node over a single bloomfs inode.
type bnode struct {
	fs.Inode
	bfs *bloomfs.FS
	id  uint64
}

// NewRoot returns the FUSE root node for f (bloomfs inode 0).
func NewRoot(f *bloomfs.FS) fs.InodeEmbedder {
	return &bnode{bfs: f, id: f.Root()}
}

// Mount attaches f at mountpoint and returns the running server; call
// server.Wait() to block until unmount, or server.Unmount() to detach.
func Mount(mountpoint string, f *bloomfs.FS, opts *fs.Options) (*fuse.Server, error) {
	return fs.Mount(mountpoint, NewRoot(f), opts)
}

func fuseIno(id uint64) uint64 { return id + 1 }

func typeBits(t uint8) uint32 {
	switch t {
	case inode.TypeDir:
		return syscall.S_IFDIR
	case inode.TypeLink:
		return syscall.S_IFLNK
	default:
		return syscall.S_IFREG
	}
}

func splitTime(ns uint64) (uint64, uint32) { return ns / 1e9, uint32(ns % 1e9) }

// caller returns the uid/gid of the process behind the current request, so a
// newly created object can be owned by whoever created it (POSIX). Falls back to
// root if the context carries no caller (e.g. internal calls).
func caller(ctx context.Context) (uint32, uint32) {
	if c, ok := fuse.FromContext(ctx); ok {
		return c.Uid, c.Gid
	}
	return 0, 0
}

func (n *bnode) child(id uint64, st bloomfs.Stat) fs.StableAttr {
	return fs.StableAttr{Mode: typeBits(st.Type), Ino: fuseIno(id), Gen: uint64(st.Generation)}
}

func fillAttr(a *fuse.Attr, st bloomfs.Stat) {
	a.Ino = fuseIno(st.Ino)
	a.Size = st.Size
	a.Mode = typeBits(st.Type) | uint32(st.Mode)
	a.Nlink = st.Nlink
	a.Owner.Uid = st.UID
	a.Owner.Gid = st.GID
	a.Atime, a.Atimensec = splitTime(st.Atime)
	a.Mtime, a.Mtimensec = splitTime(st.Mtime)
	a.Ctime, a.Ctimensec = splitTime(st.Ctime)
}

// errno maps bloomfs sentinel errors to POSIX codes; anything unexpected is EIO.
func errno(err error) syscall.Errno {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, bloomfs.ErrNotFound):
		return syscall.ENOENT
	case errors.Is(err, bloomfs.ErrExists):
		return syscall.EEXIST
	case errors.Is(err, bloomfs.ErrNotDir):
		return syscall.ENOTDIR
	case errors.Is(err, bloomfs.ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, bloomfs.ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, bloomfs.ErrNoInodes):
		return syscall.ENOSPC
	case errors.Is(err, bloomfs.ErrNoSpace):
		return syscall.ENOSPC
	case errors.Is(err, bloomfs.ErrInvalid):
		return syscall.EINVAL
	case errors.Is(err, bloomfs.ErrNotFile):
		return syscall.EINVAL
	case errors.Is(err, bloomfs.ErrPermission):
		return syscall.EACCES
	default:
		return syscall.EIO
	}
}

// bfile is an open file handle. It holds a bloomfs.Handle, which pins the inode
// for the lifetime of the kernel's open file description (so a file unlinked
// while open survives until the last Release, POSIX unlink-of-open, §E3). IO is
// still addressed by inode id through the FS — the handle only carries open
// state and liveness, not its own offset (the kernel supplies the offset).
type bfile struct {
	bfs *bloomfs.FS
	id  uint64
	h   *bloomfs.Handle
}

func (b *bfile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	data, err := b.bfs.ReadAt(b.id, uint64(off), len(dest))
	if err != nil {
		return nil, errno(err)
	}
	return fuse.ReadResultData(data), 0
}

func (b *bfile) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	if err := b.bfs.WriteAt(b.id, uint64(off), data); err != nil {
		return 0, errno(err)
	}
	return uint32(len(data)), 0
}

func (b *bfile) Flush(ctx context.Context) syscall.Errno               { return 0 }
func (b *bfile) Fsync(ctx context.Context, flags uint32) syscall.Errno { return errno(b.bfs.Fsync()) }
func (b *bfile) Release(ctx context.Context) syscall.Errno             { return errno(b.h.Close()) }

func (n *bnode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	id, ok, err := n.bfs.Lookup(n.id, name)
	if err != nil {
		return nil, errno(err)
	}
	if !ok {
		return nil, syscall.ENOENT
	}
	st, err := n.bfs.Stat(id)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, st)
	return n.NewInode(ctx, &bnode{bfs: n.bfs, id: id}, n.child(id, st)), 0
}

func (n *bnode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	st, err := n.bfs.Stat(n.id)
	if err != nil {
		return errno(err)
	}
	fillAttr(&out.Attr, st)
	return 0
}

func (n *bnode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if sz, ok := in.GetSize(); ok {
		if err := n.bfs.Truncate(n.id, sz); err != nil {
			return errno(err)
		}
	}
	if m, ok := in.GetMode(); ok {
		if err := n.bfs.Chmod(n.id, uint16(m&0o7777)); err != nil {
			return errno(err)
		}
	}
	uid, uok := in.GetUID()
	gid, gok := in.GetGID()
	if uok || gok {
		st, err := n.bfs.Stat(n.id)
		if err != nil {
			return errno(err)
		}
		if !uok {
			uid = st.UID
		}
		if !gok {
			gid = st.GID
		}
		if err := n.bfs.Chown(n.id, uid, gid); err != nil {
			return errno(err)
		}
	}
	at, aok := in.GetATime()
	mt, mok := in.GetMTime()
	if aok || mok {
		st, err := n.bfs.Stat(n.id)
		if err != nil {
			return errno(err)
		}
		atns, mtns := st.Atime, st.Mtime
		if aok {
			atns = uint64(at.UnixNano())
		}
		if mok {
			mtns = uint64(mt.UnixNano())
		}
		if err := n.bfs.Utimes(n.id, atns, mtns); err != nil {
			return errno(err)
		}
	}
	st, err := n.bfs.Stat(n.id)
	if err != nil {
		return errno(err)
	}
	fillAttr(&out.Attr, st)
	return 0
}

func (n *bnode) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	id, err := n.bfs.Create(n.id, name)
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	if err := n.bfs.Chmod(id, uint16(mode&0o7777)); err != nil {
		return nil, nil, 0, errno(err)
	}
	uid, gid := caller(ctx)
	if err := n.bfs.Chown(id, uid, gid); err != nil {
		return nil, nil, 0, errno(err)
	}
	st, err := n.bfs.Stat(id)
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	h, err := n.bfs.Open(id)
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	fillAttr(&out.Attr, st)
	return n.NewInode(ctx, &bnode{bfs: n.bfs, id: id}, n.child(id, st)), &bfile{bfs: n.bfs, id: id, h: h}, 0, 0
}

func (n *bnode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	id, err := n.bfs.Mkdir(n.id, name)
	if err != nil {
		return nil, errno(err)
	}
	if err := n.bfs.Chmod(id, uint16(mode&0o7777)); err != nil {
		return nil, errno(err)
	}
	uid, gid := caller(ctx)
	if err := n.bfs.Chown(id, uid, gid); err != nil {
		return nil, errno(err)
	}
	st, err := n.bfs.Stat(id)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, st)
	return n.NewInode(ctx, &bnode{bfs: n.bfs, id: id}, n.child(id, st)), 0
}

func (n *bnode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	id, err := n.bfs.Symlink(n.id, name, target)
	if err != nil {
		return nil, errno(err)
	}
	uid, gid := caller(ctx)
	if err := n.bfs.Chown(id, uid, gid); err != nil {
		return nil, errno(err)
	}
	st, err := n.bfs.Stat(id)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, st)
	return n.NewInode(ctx, &bnode{bfs: n.bfs, id: id}, n.child(id, st)), 0
}

func (n *bnode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, err := n.bfs.Readlink(n.id)
	if err != nil {
		return nil, errno(err)
	}
	return []byte(target), 0
}

func (n *bnode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	s := n.bfs.StatFS()
	out.Bsize = uint32(s.BlockSize)
	out.Frsize = uint32(s.BlockSize)
	out.Blocks = s.Blocks
	out.Bfree = s.BlocksFree
	out.Bavail = s.BlocksFree
	out.Files = s.Files
	out.Ffree = s.FilesFree
	out.NameLen = 255
	return 0
}

func (n *bnode) Unlink(ctx context.Context, name string) syscall.Errno {
	return errno(n.bfs.Unlink(n.id, name))
}

func (n *bnode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errno(n.bfs.Rmdir(n.id, name))
}

func (n *bnode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	np, ok := newParent.(*bnode)
	if !ok {
		return syscall.EXDEV
	}
	return errno(n.bfs.Rename(n.id, name, np.id, newName))
}

func (n *bnode) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	t, ok := target.(*bnode)
	if !ok {
		return nil, syscall.EXDEV
	}
	if err := n.bfs.Link(n.id, name, t.id); err != nil {
		return nil, errno(err)
	}
	st, err := n.bfs.Stat(t.id)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, st)
	return n.NewInode(ctx, &bnode{bfs: n.bfs, id: t.id}, n.child(t.id, st)), 0
}

func (n *bnode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	ents, err := n.bfs.Readdirents(n.id)
	if err != nil {
		return nil, errno(err)
	}
	out := make([]fuse.DirEntry, 0, len(ents))
	for _, e := range ents {
		out = append(out, fuse.DirEntry{Name: e.Name, Ino: fuseIno(e.Ino), Mode: typeBits(e.Type)})
	}
	return fs.NewListDirStream(out), 0
}

func (n *bnode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	h, err := n.bfs.Open(n.id)
	if err != nil {
		return nil, 0, errno(err)
	}
	if flags&syscall.O_TRUNC != 0 {
		if err := n.bfs.Truncate(n.id, 0); err != nil {
			h.Close()
			return nil, 0, errno(err)
		}
	}
	return &bfile{bfs: n.bfs, id: n.id, h: h}, 0, 0
}

func (n *bnode) Access(ctx context.Context, mask uint32) syscall.Errno {
	var uid, gid uint32
	if c, ok := fuse.FromContext(ctx); ok {
		uid, gid = c.Uid, c.Gid
	}
	return errno(n.bfs.Access(n.id, mask, uid, gid, nil))
}

func (n *bnode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	data, err := n.bfs.ReadAt(n.id, uint64(off), len(dest))
	if err != nil {
		return nil, errno(err)
	}
	return fuse.ReadResultData(data), 0
}

func (n *bnode) Write(ctx context.Context, f fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	if err := n.bfs.WriteAt(n.id, uint64(off), data); err != nil {
		return 0, errno(err)
	}
	return uint32(len(data)), 0
}

func (n *bnode) Fsync(ctx context.Context, f fs.FileHandle, flags uint32) syscall.Errno {
	return errno(n.bfs.Fsync())
}

// Allocate handles fallocate(2). We don't physically pre-reserve clusters (the
// store is content-addressed, so space for unwritten data can't be pinned), but
// we honor the space check so a caller using fallocate to reserve room fails
// early with ENOSPC instead of mid-write. Only mode 0 ("ensure space for
// [off, off+size) and extend size if needed") is supported; any flag
// (FALLOC_FL_KEEP_SIZE, FALLOC_FL_PUNCH_HOLE, …) means semantics we can't
// provide, so we return ENOTSUP and the kernel/libc falls back to emulation.
func (n *bnode) Allocate(ctx context.Context, f fs.FileHandle, off, size uint64, mode uint32) syscall.Errno {
	if mode != 0 {
		return syscall.ENOTSUP
	}
	return errno(n.bfs.Fallocate(n.id, size))
}

// Compile-time guarantees that bnode satisfies every kernel op the binding wires.
var (
	_ fs.NodeLookuper   = (*bnode)(nil)
	_ fs.NodeGetattrer  = (*bnode)(nil)
	_ fs.NodeSetattrer  = (*bnode)(nil)
	_ fs.NodeCreater    = (*bnode)(nil)
	_ fs.NodeMkdirer    = (*bnode)(nil)
	_ fs.NodeUnlinker   = (*bnode)(nil)
	_ fs.NodeRmdirer    = (*bnode)(nil)
	_ fs.NodeRenamer    = (*bnode)(nil)
	_ fs.NodeLinker     = (*bnode)(nil)
	_ fs.NodeSymlinker  = (*bnode)(nil)
	_ fs.NodeReadlinker = (*bnode)(nil)
	_ fs.NodeStatfser   = (*bnode)(nil)
	_ fs.NodeReaddirer  = (*bnode)(nil)
	_ fs.NodeOpener     = (*bnode)(nil)
	_ fs.NodeAccesser   = (*bnode)(nil)
	_ fs.NodeReader     = (*bnode)(nil)
	_ fs.NodeWriter     = (*bnode)(nil)
	_ fs.NodeFsyncer    = (*bnode)(nil)
	_ fs.NodeAllocater  = (*bnode)(nil)

	_ fs.FileReader   = (*bfile)(nil)
	_ fs.FileWriter   = (*bfile)(nil)
	_ fs.FileFlusher  = (*bfile)(nil)
	_ fs.FileFsyncer  = (*bfile)(nil)
	_ fs.FileReleaser = (*bfile)(nil)
)
