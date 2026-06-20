//! BloomFS FUSE binding (Stage E / layer 10).
//!
//! A thin shim translating kernel calls onto the `bloomfs-fs` VFS API, which
//! already provides crash-atomic metadata (CoW), recordsize-addressable IO,
//! dedup, compression and encryption — this binding adds no storage logic of
//! its own, it only translates kernel calls and error codes.
//!
//! Inode-number mapping: bloomfs ids start at 0 (root), but the kernel reserves
//! node id 1 for the FUSE root. We therefore report `ino = id + 1`, so the root
//! (id 0) is ino 1 and no child can collide with it.
//!
//! Unlike the go-fuse binding (node-keyed), `fuser` is ino-keyed and stores an
//! opaque file handle per open. IO is still addressed by inode id through the
//! FS; the handle only carries open state and liveness (so a file unlinked while
//! open survives until the last release — POSIX unlink-of-open, §E3).

use std::collections::HashMap;
use std::ffi::OsStr;
use std::path::Path;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use bloomfs_block::Device;
use bloomfs_fs::{Error, Handle, Stat, FS};
use bloomfs_inode::{TYPE_DIR, TYPE_LINK};
use fuser::{
    FileAttr, FileType, Filesystem, MountOption, ReplyAttr, ReplyCreate, ReplyData, ReplyDirectory,
    ReplyEmpty, ReplyEntry, ReplyOpen, ReplyStatfs, ReplyWrite, Request, TimeOrNow,
};
use libc::{
    c_int, EACCES, EEXIST, EINVAL, EIO, EISDIR, ENOENT, ENOSPC, ENOTDIR, ENOTEMPTY,
    EOPNOTSUPP, O_TRUNC,
};

/// Attribute/entry cache lifetime handed to the kernel. One second mirrors the
/// go-fuse defaults; the FS is authoritative so a short TTL is harmless.
const TTL: Duration = Duration::from_secs(1);

/// Kernel ino for a bloomfs id: root (id 0) maps to FUSE_ROOT_ID (1).
fn ino_of(id: u64) -> u64 {
    id + 1
}

/// Bloomfs id for a kernel ino (inverse of [`ino_of`]).
fn id_of(ino: u64) -> u64 {
    ino - 1
}

/// Map a bloomfs inode type byte to the FUSE file-type enum.
fn file_type(kind: u8) -> FileType {
    match kind {
        TYPE_DIR => FileType::Directory,
        TYPE_LINK => FileType::Symlink,
        _ => FileType::RegularFile,
    }
}

/// Nanoseconds-since-epoch to `SystemTime`.
fn to_systime(ns: u64) -> SystemTime {
    UNIX_EPOCH + Duration::from_nanos(ns)
}

/// `SystemTime` back to nanoseconds-since-epoch (saturating at the epoch).
fn from_systime(t: SystemTime) -> u64 {
    t.duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos() as u64)
        .unwrap_or(0)
}

/// Translate a bloomfs sentinel error to a POSIX errno; anything unexpected is
/// EIO. Mirrors the Go `errno()` table exactly.
fn errno(e: &Error) -> c_int {
    match e {
        Error::NotFound => ENOENT,
        Error::Exists => EEXIST,
        Error::NotDir => ENOTDIR,
        Error::IsDir => EISDIR,
        Error::NotEmpty => ENOTEMPTY,
        Error::NoInodes => ENOSPC,
        Error::NoSpace => ENOSPC,
        Error::Invalid => EINVAL,
        Error::NotFile => EINVAL,
        Error::Permission => EACCES,
        _ => EIO,
    }
}

/// Fill a FUSE `FileAttr` from a bloomfs `Stat`.
fn fill_attr(st: &Stat) -> FileAttr {
    FileAttr {
        ino: ino_of(st.ino),
        size: st.size,
        blocks: st.size.div_ceil(512),
        atime: to_systime(st.atime),
        mtime: to_systime(st.mtime),
        ctime: to_systime(st.ctime),
        crtime: to_systime(st.ctime),
        kind: file_type(st.kind),
        perm: st.mode & 0o7777,
        nlink: st.nlink,
        uid: st.uid,
        gid: st.gid,
        rdev: 0,
        blksize: bloomfs_block::SIZE as u32,
        flags: 0,
    }
}

/// One entry in a frozen directory snapshot taken at `opendir`. Holds the
/// kernel-facing ino, the FUSE file-type and the name — everything `readdir`
/// needs to emit a row without touching the live filesystem again.
struct DirEntrySnapshot {
    ino: u64,
    kind: FileType,
    name: String,
}

/// Take a point-in-time snapshot of directory `ino`'s entries, prefixed with the
/// mandatory `.` and `..` rows.
///
/// This is the heart of the readdir-consistency fix.  The kernel reads a large
/// directory in several `readdir` calls, each resuming at an opaque cookie (our
/// entry index).  If every call re-listed the *live* directory, a concurrent
/// `mkdir`/`unlink`/`rename` between two calls would shift indices and make the
/// paginated `ls` skip or duplicate names.  By freezing the listing once at
/// `opendir` and serving every `readdir` from that frozen `Vec`, the cookie is
/// stable for the life of the directory stream — exactly the POSIX guarantee
/// (entries present for the whole scan appear exactly once; concurrently
/// added/removed entries may or may not appear, but never corrupt the scan).
fn snapshot_dir<D: Device>(fs: &FS<D>, ino: u64) -> std::result::Result<Vec<DirEntrySnapshot>, Error> {
    let ents = fs.readdirents(id_of(ino))?;
    let mut snap = Vec::with_capacity(ents.len() + 2);
    // "." and ".." first, as the kernel expects.  We don't track the parent
    // ino, so ".." reports self — the kernel resolves it via its own dcache.
    snap.push(DirEntrySnapshot {
        ino,
        kind: FileType::Directory,
        name: ".".to_string(),
    });
    snap.push(DirEntrySnapshot {
        ino,
        kind: FileType::Directory,
        name: "..".to_string(),
    });
    for e in ents {
        snap.push(DirEntrySnapshot {
            ino: ino_of(e.ino),
            kind: file_type(e.kind),
            name: e.name,
        });
    }
    Ok(snap)
}

/// A FUSE filesystem backed by a bloomfs `FS`.
pub struct BloomFuse<D: Device> {
    fs: FS<D>,
    /// Open file handles, keyed by the fh we hand back to the kernel. Each pins
    /// its inode until release (POSIX unlink-of-open).
    handles: HashMap<u64, Handle>,
    /// Frozen directory snapshots, keyed by the dir fh from `opendir`. Freezing
    /// the listing at open time keeps the readdir cookie stable across a
    /// paginated scan even when the directory is concurrently modified (no
    /// skipped or duplicated entries — see [`snapshot_dir`]).
    dir_handles: HashMap<u64, Vec<DirEntrySnapshot>>,
    next_fh: u64,
}

impl<D: Device> BloomFuse<D> {
    /// Wrap a mounted/formatted `FS` as a FUSE filesystem.
    pub fn new(fs: FS<D>) -> Self {
        Self {
            fs,
            handles: HashMap::new(),
            dir_handles: HashMap::new(),
            next_fh: 1,
        }
    }

    /// Register an open handle and return its kernel fh.
    fn insert_handle(&mut self, h: Handle) -> u64 {
        let fh = self.next_fh;
        self.next_fh += 1;
        self.handles.insert(fh, h);
        fh
    }
}

impl<D: Device> Drop for BloomFuse<D> {
    /// Commit metadata when the session ends (clean unmount), mirroring the Go
    /// binding's post-`Wait()` fsync. A hard crash skips this and rolls back to
    /// the last commit, as designed.
    fn drop(&mut self) {
        let _ = self.fs.fsync();
    }
}

pub use fuser::BackgroundSession;

/// The mount options used by [`mount`] and [`spawn`]: `default_permissions` so
/// the kernel enforces the mode/uid/gid we report (chmod/chown actually gate
/// access), and a `bloomfs` source/subtype label in mtab.
fn mount_options() -> [MountOption; 3] {
    [
        MountOption::FSName("bloomfs".to_string()),
        MountOption::Subtype("bloomfs".to_string()),
        MountOption::DefaultPermissions,
    ]
}

/// Mount `fs` at `mountpoint` and block until the filesystem is unmounted.
pub fn mount<D, P>(fs: FS<D>, mountpoint: P) -> std::io::Result<()>
where
    D: Device,
    P: AsRef<Path>,
{
    fuser::mount2(BloomFuse::new(fs), mountpoint, &mount_options())
}

/// Mount `fs` at `mountpoint` in a background thread, returning immediately.
///
/// Dropping (or `join`-ing) the returned session unmounts the filesystem and,
/// via [`BloomFuse`]'s `Drop`, commits metadata — the clean-unmount path. This
/// lets a caller wait for a signal and then unmount on its own schedule.
pub fn spawn<D, P>(fs: FS<D>, mountpoint: P) -> std::io::Result<BackgroundSession>
where
    D: Device + Send + 'static,
    P: AsRef<Path>,
{
    fuser::spawn_mount2(BloomFuse::new(fs), mountpoint, &mount_options())
}

impl<D: Device> Filesystem for BloomFuse<D> {
    fn lookup(&mut self, _req: &Request<'_>, parent: u64, name: &OsStr, reply: ReplyEntry) {
        let name = match name.to_str() {
            Some(s) => s,
            None => {
                reply.error(EINVAL);
                return;
            }
        };
        match self.fs.lookup(id_of(parent), name) {
            Ok(Some(id)) => match self.fs.stat(id) {
                Ok(st) => reply.entry(&TTL, &fill_attr(&st), u64::from(st.generation)),
                Err(e) => reply.error(errno(&e)),
            },
            Ok(None) => reply.error(ENOENT),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn getattr(&mut self, _req: &Request<'_>, ino: u64, _fh: Option<u64>, reply: ReplyAttr) {
        match self.fs.stat(id_of(ino)) {
            Ok(st) => reply.attr(&TTL, &fill_attr(&st)),
            Err(e) => reply.error(errno(&e)),
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn setattr(
        &mut self,
        _req: &Request<'_>,
        ino: u64,
        mode: Option<u32>,
        uid: Option<u32>,
        gid: Option<u32>,
        size: Option<u64>,
        atime: Option<TimeOrNow>,
        mtime: Option<TimeOrNow>,
        _ctime: Option<SystemTime>,
        _fh: Option<u64>,
        _crtime: Option<SystemTime>,
        _chgtime: Option<SystemTime>,
        _bkuptime: Option<SystemTime>,
        _flags: Option<u32>,
        reply: ReplyAttr,
    ) {
        let id = id_of(ino);
        if let Some(sz) = size {
            if let Err(e) = self.fs.truncate(id, sz) {
                reply.error(errno(&e));
                return;
            }
        }
        if let Some(m) = mode {
            if let Err(e) = self.fs.chmod(id, (m & 0o7777) as u16) {
                reply.error(errno(&e));
                return;
            }
        }
        if uid.is_some() || gid.is_some() {
            let st = match self.fs.stat(id) {
                Ok(s) => s,
                Err(e) => {
                    reply.error(errno(&e));
                    return;
                }
            };
            let u = uid.unwrap_or(st.uid);
            let g = gid.unwrap_or(st.gid);
            if let Err(e) = self.fs.chown(id, u, g) {
                reply.error(errno(&e));
                return;
            }
        }
        if atime.is_some() || mtime.is_some() {
            let st = match self.fs.stat(id) {
                Ok(s) => s,
                Err(e) => {
                    reply.error(errno(&e));
                    return;
                }
            };
            let resolve = |t: Option<TimeOrNow>, cur: u64| match t {
                Some(TimeOrNow::SpecificTime(v)) => from_systime(v),
                Some(TimeOrNow::Now) => from_systime(SystemTime::now()),
                None => cur,
            };
            let a = resolve(atime, st.atime);
            let m = resolve(mtime, st.mtime);
            if let Err(e) = self.fs.utimes(id, a, m) {
                reply.error(errno(&e));
                return;
            }
        }
        match self.fs.stat(id) {
            Ok(st) => reply.attr(&TTL, &fill_attr(&st)),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn readlink(&mut self, _req: &Request<'_>, ino: u64, reply: ReplyData) {
        match self.fs.readlink(id_of(ino)) {
            Ok(target) => reply.data(target.as_bytes()),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn mkdir(
        &mut self,
        req: &Request<'_>,
        parent: u64,
        name: &OsStr,
        mode: u32,
        _umask: u32,
        reply: ReplyEntry,
    ) {
        let name = match name.to_str() {
            Some(s) => s,
            None => {
                reply.error(EINVAL);
                return;
            }
        };
        let id = match self.fs.mkdir(id_of(parent), name) {
            Ok(id) => id,
            Err(e) => {
                reply.error(errno(&e));
                return;
            }
        };
        if let Err(e) = self.fs.chmod(id, (mode & 0o7777) as u16) {
            reply.error(errno(&e));
            return;
        }
        if let Err(e) = self.fs.chown(id, req.uid(), req.gid()) {
            reply.error(errno(&e));
            return;
        }
        match self.fs.stat(id) {
            Ok(st) => reply.entry(&TTL, &fill_attr(&st), u64::from(st.generation)),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn unlink(&mut self, _req: &Request<'_>, parent: u64, name: &OsStr, reply: ReplyEmpty) {
        let name = match name.to_str() {
            Some(s) => s,
            None => {
                reply.error(EINVAL);
                return;
            }
        };
        match self.fs.unlink(id_of(parent), name) {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn rmdir(&mut self, _req: &Request<'_>, parent: u64, name: &OsStr, reply: ReplyEmpty) {
        let name = match name.to_str() {
            Some(s) => s,
            None => {
                reply.error(EINVAL);
                return;
            }
        };
        match self.fs.rmdir(id_of(parent), name) {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn symlink(
        &mut self,
        req: &Request<'_>,
        parent: u64,
        link_name: &OsStr,
        target: &Path,
        reply: ReplyEntry,
    ) {
        let name = match link_name.to_str() {
            Some(s) => s,
            None => {
                reply.error(EINVAL);
                return;
            }
        };
        let target = match target.to_str() {
            Some(s) => s,
            None => {
                reply.error(EINVAL);
                return;
            }
        };
        let id = match self.fs.symlink(id_of(parent), name, target) {
            Ok(id) => id,
            Err(e) => {
                reply.error(errno(&e));
                return;
            }
        };
        if let Err(e) = self.fs.chown(id, req.uid(), req.gid()) {
            reply.error(errno(&e));
            return;
        }
        match self.fs.stat(id) {
            Ok(st) => reply.entry(&TTL, &fill_attr(&st), u64::from(st.generation)),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn rename(
        &mut self,
        _req: &Request<'_>,
        parent: u64,
        name: &OsStr,
        newparent: u64,
        newname: &OsStr,
        _flags: u32,
        reply: ReplyEmpty,
    ) {
        let (name, newname) = match (name.to_str(), newname.to_str()) {
            (Some(a), Some(b)) => (a, b),
            _ => {
                reply.error(EINVAL);
                return;
            }
        };
        match self
            .fs
            .rename(id_of(parent), name, id_of(newparent), newname)
        {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn link(
        &mut self,
        _req: &Request<'_>,
        ino: u64,
        newparent: u64,
        newname: &OsStr,
        reply: ReplyEntry,
    ) {
        let newname = match newname.to_str() {
            Some(s) => s,
            None => {
                reply.error(EINVAL);
                return;
            }
        };
        let target = id_of(ino);
        if let Err(e) = self.fs.link(id_of(newparent), newname, target) {
            reply.error(errno(&e));
            return;
        }
        match self.fs.stat(target) {
            Ok(st) => reply.entry(&TTL, &fill_attr(&st), u64::from(st.generation)),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn create(
        &mut self,
        req: &Request<'_>,
        parent: u64,
        name: &OsStr,
        mode: u32,
        _umask: u32,
        _flags: i32,
        reply: ReplyCreate,
    ) {
        let name = match name.to_str() {
            Some(s) => s,
            None => {
                reply.error(EINVAL);
                return;
            }
        };
        let id = match self.fs.create(id_of(parent), name) {
            Ok(id) => id,
            Err(e) => {
                reply.error(errno(&e));
                return;
            }
        };
        if let Err(e) = self.fs.chmod(id, (mode & 0o7777) as u16) {
            reply.error(errno(&e));
            return;
        }
        if let Err(e) = self.fs.chown(id, req.uid(), req.gid()) {
            reply.error(errno(&e));
            return;
        }
        let st = match self.fs.stat(id) {
            Ok(s) => s,
            Err(e) => {
                reply.error(errno(&e));
                return;
            }
        };
        let h = match self.fs.open(id) {
            Ok(h) => h,
            Err(e) => {
                reply.error(errno(&e));
                return;
            }
        };
        let fh = self.insert_handle(h);
        reply.created(&TTL, &fill_attr(&st), u64::from(st.generation), fh, 0);
    }

    fn open(&mut self, _req: &Request<'_>, ino: u64, flags: i32, reply: ReplyOpen) {
        let id = id_of(ino);
        let h = match self.fs.open(id) {
            Ok(h) => h,
            Err(e) => {
                reply.error(errno(&e));
                return;
            }
        };
        if flags & O_TRUNC != 0 {
            if let Err(e) = self.fs.truncate(id, 0) {
                let mut h = h;
                let _ = self.fs.close(&mut h);
                reply.error(errno(&e));
                return;
            }
        }
        let fh = self.insert_handle(h);
        reply.opened(fh, 0);
    }

    fn read(
        &mut self,
        _req: &Request<'_>,
        ino: u64,
        _fh: u64,
        offset: i64,
        size: u32,
        _flags: i32,
        _lock_owner: Option<u64>,
        reply: ReplyData,
    ) {
        match self.fs.read_at(id_of(ino), offset as u64, size as usize) {
            Ok(data) => reply.data(&data),
            Err(e) => reply.error(errno(&e)),
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn write(
        &mut self,
        _req: &Request<'_>,
        ino: u64,
        _fh: u64,
        offset: i64,
        data: &[u8],
        _write_flags: u32,
        _flags: i32,
        _lock_owner: Option<u64>,
        reply: ReplyWrite,
    ) {
        match self.fs.write_at(id_of(ino), offset as u64, data) {
            Ok(()) => reply.written(data.len() as u32),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn flush(
        &mut self,
        _req: &Request<'_>,
        _ino: u64,
        _fh: u64,
        _lock_owner: u64,
        reply: ReplyEmpty,
    ) {
        // Like the Go binding, flush is a no-op: the FS has no per-handle dirty
        // buffer to push (writes go straight through write_at).
        reply.ok();
    }

    fn fsync(
        &mut self,
        _req: &Request<'_>,
        _ino: u64,
        _fh: u64,
        _datasync: bool,
        reply: ReplyEmpty,
    ) {
        match self.fs.fsync() {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(errno(&e)),
        }
    }

    /// Pre-flight space check for `fallocate(2)` / `posix_fallocate(3)`.
    ///
    /// BloomFS is content-addressed: actual cluster addresses are assigned at
    /// write time (after hashing + dedup), so it is impossible to truly
    /// pre-allocate clusters for data that does not exist yet.  Instead we
    /// perform a **worst-case estimate** (0 % dedup, all unique data) and
    /// return `ENOSPC` immediately if even that best-case scenario would
    /// exhaust the bitmap.  This gives callers a fast, cheap "will it fit?"
    /// answer before committing to a potentially multi-GB write.
    ///
    /// Modes not supported (punch-hole, zero-range, keep-size) return
    /// `EOPNOTSUPP`; the kernel or libc will then fall back transparently.
    fn fallocate(
        &mut self,
        _req: &Request<'_>,
        ino: u64,
        _fh: u64,
        _offset: i64,
        length: i64,
        mode: i32,
        reply: ReplyEmpty,
    ) {
        // We only support mode=0 (the standard "ensure space for [offset,
        // offset+length) and extend file size if needed" variant).  Any flag
        // (FALLOC_FL_KEEP_SIZE=1, FALLOC_FL_PUNCH_HOLE=2, etc.) means the
        // caller wants semantics we can't provide — return EOPNOTSUPP so the
        // kernel/libc falls back to emulation.
        if mode != 0 {
            reply.error(EOPNOTSUPP);
            return;
        }
        if length <= 0 {
            reply.ok();
            return;
        }
        match self.fs.fallocate(id_of(ino), length as u64) {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn release(
        &mut self,
        _req: &Request<'_>,
        _ino: u64,
        fh: u64,
        _flags: i32,
        _lock_owner: Option<u64>,
        _flush: bool,
        reply: ReplyEmpty,
    ) {
        match self.handles.remove(&fh) {
            Some(mut h) => match self.fs.close(&mut h) {
                Ok(()) => reply.ok(),
                Err(e) => reply.error(errno(&e)),
            },
            None => reply.ok(),
        }
    }

    /// Open a directory stream: freeze its entries into a per-fh snapshot so the
    /// subsequent paginated `readdir` calls are immune to concurrent mutation.
    fn opendir(&mut self, _req: &Request<'_>, ino: u64, _flags: i32, reply: ReplyOpen) {
        let snap = match snapshot_dir(&self.fs, ino) {
            Ok(s) => s,
            Err(e) => {
                reply.error(errno(&e));
                return;
            }
        };
        let fh = self.next_fh;
        self.next_fh += 1;
        self.dir_handles.insert(fh, snap);
        reply.opened(fh, 0);
    }

    fn readdir(
        &mut self,
        _req: &Request<'_>,
        ino: u64,
        fh: u64,
        offset: i64,
        mut reply: ReplyDirectory,
    ) {
        // Serve from the frozen snapshot taken at `opendir`. If the fh is unknown
        // (defensive — e.g. a kernel that skips opendir), build a one-shot
        // snapshot now so behaviour degrades to "consistent within this call".
        let owned;
        let entries: &[DirEntrySnapshot] = match self.dir_handles.get(&fh) {
            Some(s) => s,
            None => {
                owned = match snapshot_dir(&self.fs, ino) {
                    Ok(s) => s,
                    Err(e) => {
                        reply.error(errno(&e));
                        return;
                    }
                };
                &owned
            }
        };
        for (i, e) in entries.iter().enumerate().skip(offset as usize) {
            // Offset is the cookie to resume *after* this entry: a stable index
            // into the frozen snapshot, never shifted by concurrent mutation.
            if reply.add(e.ino, (i + 1) as i64, e.kind, &e.name) {
                break;
            }
        }
        reply.ok();
    }

    /// Release a directory stream: drop its snapshot.
    fn releasedir(
        &mut self,
        _req: &Request<'_>,
        _ino: u64,
        fh: u64,
        _flags: i32,
        reply: ReplyEmpty,
    ) {
        self.dir_handles.remove(&fh);
        reply.ok();
    }

    fn access(&mut self, req: &Request<'_>, ino: u64, mask: i32, reply: ReplyEmpty) {
        match self
            .fs
            .access(id_of(ino), mask as u32, req.uid(), req.gid(), &[])
        {
            Ok(()) => reply.ok(),
            Err(e) => reply.error(errno(&e)),
        }
    }

    fn statfs(&mut self, _req: &Request<'_>, _ino: u64, reply: ReplyStatfs) {
        let s = self.fs.statfs();
        reply.statfs(
            s.blocks,
            s.blocks_free,
            s.blocks_free,
            s.files,
            s.files_free,
            s.block_size as u32,
            255,
            s.block_size as u32,
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ino_id_roundtrip() {
        assert_eq!(ino_of(0), 1); // root -> FUSE_ROOT_ID
        assert_eq!(id_of(1), 0);
        for id in [0u64, 1, 2, 42, 4095, u64::MAX - 1] {
            assert_eq!(id_of(ino_of(id)), id);
        }
    }

    #[test]
    fn file_type_mapping() {
        assert_eq!(file_type(TYPE_DIR), FileType::Directory);
        assert_eq!(file_type(TYPE_LINK), FileType::Symlink);
        assert_eq!(file_type(0), FileType::RegularFile);
        assert_eq!(file_type(99), FileType::RegularFile);
    }

    #[test]
    fn time_roundtrip() {
        for ns in [0u64, 1, 1_000_000_000, 1_700_000_000_123_456_789] {
            assert_eq!(from_systime(to_systime(ns)), ns);
        }
        // Times before the epoch saturate to 0.
        assert_eq!(from_systime(UNIX_EPOCH - Duration::from_secs(1)), 0);
    }

    #[test]
    fn errno_table() {
        assert_eq!(errno(&Error::NotFound), ENOENT);
        assert_eq!(errno(&Error::Exists), EEXIST);
        assert_eq!(errno(&Error::NotDir), ENOTDIR);
        assert_eq!(errno(&Error::IsDir), EISDIR);
        assert_eq!(errno(&Error::NotEmpty), ENOTEMPTY);
        assert_eq!(errno(&Error::NoInodes), ENOSPC);
        assert_eq!(errno(&Error::Invalid), EINVAL);
        assert_eq!(errno(&Error::NotFile), EINVAL);
        assert_eq!(errno(&Error::Permission), EACCES);
        assert_eq!(errno(&Error::Corrupt), EIO);
    }

    #[test]
    fn fill_attr_maps_fields() {
        let st = Stat {
            ino: 41,
            size: 5000,
            nlink: 2,
            generation: 7,
            mode: 0o100644,
            kind: TYPE_DIR,
            uid: 1000,
            gid: 1001,
            atime: 10,
            mtime: 20,
            ctime: 30,
        };
        let a = fill_attr(&st);
        assert_eq!(a.ino, 42); // id+1
        assert_eq!(a.size, 5000);
        assert_eq!(a.blocks, 10); // ceil(5000/512)
        assert_eq!(a.kind, FileType::Directory);
        assert_eq!(a.perm, 0o644); // type bits stripped
        assert_eq!(a.nlink, 2);
        assert_eq!(a.uid, 1000);
        assert_eq!(a.gid, 1001);
        assert_eq!(a.atime, to_systime(10));
        assert_eq!(a.mtime, to_systime(20));
        assert_eq!(a.ctime, to_systime(30));
        assert_eq!(a.blksize, bloomfs_block::SIZE as u32);
    }

    /// The core readdir-consistency guarantee: a snapshot taken at `opendir` is
    /// frozen, so concurrent `mkdir`/`rmdir` after the stream is open cannot
    /// make a paginated scan skip or duplicate entries.
    #[test]
    fn readdir_snapshot_stable_across_concurrent_mutation() {
        use bloomfs_block::MemDevice;
        let mut fs = FS::format(MemDevice::new(512), None).unwrap();
        let root = fs.root();
        fs.mkdir(root, "alpha").unwrap();
        fs.mkdir(root, "beta").unwrap();

        // "opendir": freeze the listing.
        let snap = snapshot_dir(&fs, ino_of(root)).unwrap();
        let names_at_open: Vec<String> = snap.iter().map(|e| e.name.clone()).collect();

        // Concurrent mutation after the stream is open.
        fs.mkdir(root, "gamma").unwrap();
        fs.rmdir(root, "alpha").unwrap();

        // The snapshot is byte-for-byte unchanged.
        let names_now: Vec<String> = snap.iter().map(|e| e.name.clone()).collect();
        assert_eq!(names_at_open, names_now, "snapshot must be frozen at opendir");

        // `.` and `..` lead; pre-existing entries are present exactly once; the
        // post-open `gamma` is absent and the removed `alpha` still shows.
        assert_eq!(snap[0].name, ".");
        assert_eq!(snap[1].name, "..");
        assert!(snap.iter().any(|e| e.name == "alpha"));
        assert!(snap.iter().any(|e| e.name == "beta"));
        assert!(
            !snap.iter().any(|e| e.name == "gamma"),
            "an entry created after opendir must not appear in the frozen snapshot"
        );

        // No duplicate names — the classic skip/duplicate corruption.
        let mut names: Vec<&str> = snap.iter().map(|e| e.name.as_str()).collect();
        names.sort_unstable();
        let mut deduped = names.clone();
        deduped.dedup();
        assert_eq!(names, deduped, "snapshot must contain no duplicate names");
    }
}
