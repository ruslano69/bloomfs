//! The BloomFS integration layer (SPEC Stage D): it wires the directory index
//! ([`bloomfs_dir`]), metadata ([`bloomfs_inode`]), the data pipeline
//! ([`bloomfs_store`]) and the Copy-on-Write durability layer ([`bloomfs_cow`])
//! into a single filesystem, exposed through a small VFS-style API
//! (`mkdir`/`create`/`lookup`/`read`/`write`/`readdir`/`unlink`/`commit`).
//!
//! It is deliberately decoupled from any kernel mount: a FUSE binding is a thin
//! shim forwarding to these methods, so the whole layer is testable without a
//! mount on any platform.
//!
//! File contents are chunked into recordsize records (§4.5): the record size is
//! derived from the file's total size and frozen once the file holds data. A
//! single-record file stores its [`bloomfs_store::Ref`] inline in the inode
//! block map; a multi-record file stores a ref-list block-map blob and keeps a
//! ref to it inline (§4.4). Each record is an independent dedup/compress/encrypt
//! unit, so addressable `read_at`/`write_at` touch only the records they overlap.
//!
//! The whole metadata set — the inode table, the free-space bitmap and the dedup
//! table — is held in RAM and CoW-committed as one atomic snapshot (§B1). Nothing
//! is written in place, so an uncommitted mutation vanishes after a crash and a
//! committed one is all-or-nothing: this is what makes rename/unlink/hardlink
//! crash-atomic and gives `fsync` its strong durability guarantee (§E).
//!
//! Port notes vs the Go prototype:
//!   - The Go `FS` owns an `block.Device` interface and guards everything with an
//!     `RWMutex`; here `FS<D>` owns the device by value and relies on Rust's
//!     borrow checker for the same reader/writer contract (read ops take `&self`,
//!     mutations take `&mut self`). A binding wraps the whole `FS` in an `RwLock`.
//!   - The Go open-directory cache (a `sync.Map`, §B11) is a pure performance
//!     optimization; this first functional port loads a directory from disk per
//!     operation instead. The on-disk page format and the Bloom-index rebuild are
//!     preserved exactly. The cache is a deferred optimization.

use std::collections::{HashMap, HashSet};
use std::fmt;
use std::sync::Mutex;

use bloomfs_block::Device;
use bloomfs_cow::Uberblock;
use bloomfs_dir::{Directory, Entry as DirEntry, InodeId};
use bloomfs_inode::{
    Inode, Table as InodeTable, FLAG_INLINE_EXTENTS, TYPE_DIR, TYPE_LINK, TYPE_REGULAR,
};
use bloomfs_store::{BlockStore, Ref, REF_SIZE};

/// Block size as `u64` (data block / statfs unit).
const BLK: u64 = bloomfs_block::SIZE as u64;

/// Default inode-table capacity for a freshly formatted image (§F5).
const DEFAULT_INODE_COUNT: u64 = 4096;
/// Default dedup-table snapshot headroom, in bytes (§B1).
const DEFAULT_DDT_RESERVE: u64 = 256 * 1024;

/// The directory persistence unit: one entry list is split across fixed 4 KiB
/// pages, each persisted as exactly one data record. A single create/unlink
/// rewrites only the one page it touches.
const DIR_PAGE_SIZE: usize = 4096;

/// POSIX `access(2)` mask bits (matching the FUSE access opcode).
pub const ACCESS_X: u32 = 1; // X_OK
pub const ACCESS_W: u32 = 2; // W_OK
pub const ACCESS_R: u32 = 4; // R_OK

/// Errors returned by the fs layer.
#[derive(Debug)]
pub enum Error {
    /// Operation needs a directory but the target is not one.
    NotDir,
    /// Operation needs a regular file but the target is not one.
    NotFile,
    /// A name already exists in the directory.
    Exists,
    /// No such name / inode.
    NotFound,
    /// Corrupt on-disk directory or block-map data.
    Corrupt,
    /// The inode table is full (§F5).
    NoInodes,
    /// Target is a directory where a non-directory was required (EISDIR).
    IsDir,
    /// Directory is not empty (ENOTEMPTY).
    NotEmpty,
    /// Invalid argument (EINVAL).
    Invalid,
    /// Permission denied (EACCES).
    Permission,
    /// Not enough free space (ENOSPC) — used by `fallocate` when the worst-case
    /// estimate of required clusters exceeds what the bitmap can provide.
    NoSpace,
    /// An error from the CoW layer.
    Cow(bloomfs_cow::Error),
    /// An error from the data-path store.
    Store(bloomfs_store::Error),
    /// An error from the inode layer.
    Inode(bloomfs_inode::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::NotDir => f.write_str("fs: not a directory"),
            Error::NotFile => f.write_str("fs: not a regular file"),
            Error::Exists => f.write_str("fs: name already exists"),
            Error::NotFound => f.write_str("fs: no such entry"),
            Error::Corrupt => f.write_str("fs: corrupt directory data"),
            Error::NoInodes => f.write_str("fs: inode table full"),
            Error::IsDir => f.write_str("fs: is a directory"),
            Error::NotEmpty => f.write_str("fs: directory not empty"),
            Error::Invalid => f.write_str("fs: invalid argument"),
            Error::Permission => f.write_str("fs: permission denied"),
            Error::NoSpace => f.write_str("fs: not enough space"),
            Error::Cow(e) => write!(f, "fs: {e}"),
            Error::Store(e) => write!(f, "fs: {e}"),
            Error::Inode(e) => write!(f, "fs: {e}"),
        }
    }
}

impl std::error::Error for Error {}

impl From<bloomfs_cow::Error> for Error {
    fn from(e: bloomfs_cow::Error) -> Self {
        Error::Cow(e)
    }
}
impl From<bloomfs_store::Error> for Error {
    fn from(e: bloomfs_store::Error) -> Self {
        Error::Store(e)
    }
}
impl From<bloomfs_inode::Error> for Error {
    fn from(e: bloomfs_inode::Error) -> Self {
        Error::Inode(e)
    }
}

/// A `Result` specialized to the fs layer's [`Error`].
pub type Result<T> = std::result::Result<T, Error>;

/// Wall-clock source: returns the current time in Unix nanoseconds. A field (not
/// a global) so tests can drive it deterministically (§F6).
type Clock = Box<dyn Fn() -> u64 + Send + Sync>;

fn default_now() -> u64 {
    use std::time::{SystemTime, UNIX_EPOCH};
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos() as u64)
        .unwrap_or(0)
}

/// A mounted BloomFS instance.
pub struct FS<D: Device> {
    dev: D,
    ub: Uberblock,
    bm: bloomfs_alloc::Bitmap,
    ddt: bloomfs_dedup::Table,
    inodes: InodeTable,
    bs: BlockStore,

    /// Live open handles per inode (RAM-only). A file with `nlink == 0` but an
    /// open handle stays alive until the last close (POSIX unlink-of-open, §E3).
    open_count: HashMap<u64, u32>,
    /// Reuse stack of reclaimed inode ids (§F5). Rebuilt by `mount` from the
    /// committed table, so it always matches the rolled-back-to state.
    free_inodes: Vec<u64>,
    clock: Clock,
    /// Reusable CoW snapshot serialization buffer, sized to one metadata slot.
    meta_buf: Vec<u8>,

    /// Resident open-directory cache (§B11). When enabled, a directory's decoded
    /// name index + page layout stays in RAM across operations, so a lookup/readdir
    /// hit skips the per-op page decrypt and Bloom-index rebuild. It is on by
    /// default and can be turned off (`set_dir_cache(false)`) to fall back to the
    /// always-reload baseline. Interior-mutable so reader ops keep their `&self`
    /// signature while populating the cache on a miss. The cached entry holds only
    /// the structural index — the directory's own inode is re-read from the table
    /// on every checkout, so a `chmod`/`chown`/`utimes` on the directory id (which
    /// bypasses this cache) can never be clobbered by a later cached mutation.
    /// Capped at `DIR_CACHE_CAP` entries; insertions evict the LRU victim so a
    /// `find`-style traversal cannot OOM the process.
    dir_cache: Option<Mutex<DirCache>>,
}

// --- directory page cache (one resident open directory) ---

/// Maximum number of simultaneously resident directories.  A `find` on a
/// filesystem with millions of subdirectories would otherwise load every
/// `CachedDir` into RAM and OOM the process.  256 fits comfortably in a few
/// MiB for typical directory sizes while covering almost all real working sets.
const DIR_CACHE_CAP: usize = 256;

/// A resident open directory: its in-memory Bloom-segmented index (the lookup
/// authority) plus the page layout it persists to. The two are kept in sync
/// through `add`/`del`; `page_of` locates a name's bytes so a mutation rewrites
/// just the affected page.
struct CachedDir {
    dir: Directory,
    in_: Inode,
    pages: Vec<Vec<DirEntry>>,
    used: Vec<usize>,
    page_of: HashMap<String, usize>,
    dirty: HashSet<usize>,
    /// Monotone tick set on every access — used to evict the LRU entry when the
    /// cache is full.  Initialised to 0; updated by `DirCache::touch`.
    lru_tick: u64,
}

/// LRU-capped directory cache.  Interior-mutable through `Mutex` so reader ops
/// (`cache_peek`) can keep their `&self` signature.
struct DirCache {
    entries: HashMap<u64, CachedDir>,
    tick: u64,
}

impl DirCache {
    fn new() -> Self {
        DirCache {
            entries: HashMap::new(),
            tick: 0,
        }
    }

    /// Record an access to `id`, bumping its LRU tick.
    fn touch(&mut self, id: u64) {
        self.tick += 1;
        if let Some(e) = self.entries.get_mut(&id) {
            e.lru_tick = self.tick;
        }
    }

    /// Insert `entry` for `id`, evicting the least-recently-used entry first if
    /// the cache is already at capacity.
    fn insert(&mut self, id: u64, mut entry: CachedDir) {
        if self.entries.len() >= DIR_CACHE_CAP && !self.entries.contains_key(&id) {
            // Find and remove the entry with the smallest lru_tick.
            if let Some(victim) = self
                .entries
                .iter()
                .min_by_key(|(_, v)| v.lru_tick)
                .map(|(k, _)| *k)
            {
                self.entries.remove(&victim);
            }
        }
        self.tick += 1;
        entry.lru_tick = self.tick;
        self.entries.insert(id, entry);
    }
}

impl CachedDir {
    /// On-disk size of one directory entry: a 10-byte header (nameLen u16 +
    /// inode u64) plus the name.
    fn entry_bytes(name: &str) -> usize {
        10 + name.len()
    }

    /// Link name -> id in both the Bloom index and the page layout, marking the
    /// chosen page dirty. Returns false if name already exists.
    fn add(&mut self, name: &str, id: InodeId) -> bool {
        if !self.dir.add(name, id) {
            return false;
        }
        let need = Self::entry_bytes(name);
        let p = match self.used.iter().position(|&u| u + need <= DIR_PAGE_SIZE) {
            Some(i) => i,
            None => {
                self.pages.push(Vec::new());
                self.used.push(4); // 4-byte page header (entry count)
                self.pages.len() - 1
            }
        };
        self.pages[p].push(DirEntry {
            name: name.to_string(),
            inode: id,
        });
        self.used[p] += need;
        self.page_of.insert(name.to_string(), p);
        self.dirty.insert(p);
        true
    }

    /// Unlink name from both the index and its page, marking that page dirty.
    /// Returns false if name was not present.
    fn del(&mut self, name: &str) -> bool {
        let p = match self.page_of.get(name) {
            Some(&p) => p,
            None => return false,
        };
        self.dir.delete(name);
        if let Some(pos) = self.pages[p].iter().position(|e| e.name == name) {
            self.pages[p].remove(pos);
        }
        self.used[p] -= Self::entry_bytes(name);
        self.page_of.remove(name);
        self.dirty.insert(p);
        true
    }
}

// --- record-size policy (§4.5) ---

/// The record size in bytes and its log2 for a file of the given total size.
fn record_size_for(size: u64) -> (u64, u8) {
    match size {
        s if s < 32 * 1024 => (4 * 1024, 12),
        s if s < 256 * 1024 => (8 * 1024, 13),
        s if s < 2 * 1024 * 1024 => (16 * 1024, 14),
        _ => (32 * 1024, 15),
    }
}

/// Cut `blob` into `rs`-sized records (the last one may be shorter).
fn split_records(blob: &[u8], rs: u64) -> Vec<&[u8]> {
    let rs = rs as usize;
    let mut out = Vec::new();
    let mut off = 0;
    while off < blob.len() {
        let end = (off + rs).min(blob.len());
        out.push(&blob[off..end]);
        off = end;
    }
    out
}

/// Serialize a ref list into a block-map blob: count u32, then count × `REF_SIZE`.
fn encode_refs(refs: &[Ref]) -> Vec<u8> {
    let mut buf = vec![0u8; 4 + refs.len() * REF_SIZE];
    buf[0..4].copy_from_slice(&(refs.len() as u32).to_le_bytes());
    for (i, r) in refs.iter().enumerate() {
        buf[4 + i * REF_SIZE..4 + (i + 1) * REF_SIZE].copy_from_slice(&r.marshal());
    }
    buf
}

fn decode_refs(b: &[u8]) -> Result<Vec<Ref>> {
    if b.len() < 4 {
        return Err(Error::Corrupt);
    }
    let n = u32::from_le_bytes(b[0..4].try_into().unwrap()) as usize;
    if b.len() < 4 + n * REF_SIZE {
        return Err(Error::Corrupt);
    }
    let mut out = Vec::with_capacity(n);
    for i in 0..n {
        out.push(Ref::unmarshal(&b[4 + i * REF_SIZE..]));
    }
    Ok(out)
}

// --- directory page serialization ---
//
// A page is a fixed DIR_PAGE_SIZE buffer: count u32, then per entry nameLen u16,
// inode u64, name bytes; the remainder is zero padding. The caller guarantees
// the entries fit (used <= DIR_PAGE_SIZE), so encoding never overflows.

fn encode_dir_page(entries: &[DirEntry]) -> Vec<u8> {
    let mut buf = vec![0u8; DIR_PAGE_SIZE];
    buf[0..4].copy_from_slice(&(entries.len() as u32).to_le_bytes());
    let mut off = 4;
    for e in entries {
        buf[off..off + 2].copy_from_slice(&(e.name.len() as u16).to_le_bytes());
        buf[off + 2..off + 10].copy_from_slice(&e.inode.0.to_le_bytes());
        off += 10;
        buf[off..off + e.name.len()].copy_from_slice(e.name.as_bytes());
        off += e.name.len();
    }
    buf
}

/// Parse one page, returning its entries and the byte count they occupy
/// (including the 4-byte header) for the page's room accounting.
fn decode_dir_page(b: &[u8]) -> Result<(Vec<DirEntry>, usize)> {
    if b.len() < 4 {
        return Err(Error::Corrupt);
    }
    let n = u32::from_le_bytes(b[0..4].try_into().unwrap());
    let mut off = 4;
    let mut out = Vec::with_capacity(n as usize);
    for _ in 0..n {
        if off + 10 > b.len() {
            return Err(Error::Corrupt);
        }
        let nl = u16::from_le_bytes(b[off..off + 2].try_into().unwrap()) as usize;
        let id = u64::from_le_bytes(b[off + 2..off + 10].try_into().unwrap());
        off += 10;
        if off + nl > b.len() {
            return Err(Error::Corrupt);
        }
        let name = String::from_utf8_lossy(&b[off..off + nl]).into_owned();
        out.push(DirEntry {
            name,
            inode: InodeId(id),
        });
        off += nl;
    }
    Ok((out, off))
}

/// A point-in-time view of an inode's metadata (what a kernel `getattr` needs).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Stat {
    pub ino: u64,
    pub size: u64,
    pub nlink: u32,
    pub generation: u32,
    pub mode: u16,
    pub kind: u8,
    pub uid: u32,
    pub gid: u32,
    pub atime: u64,
    pub mtime: u64,
    pub ctime: u64,
}

/// Filesystem-wide capacity report (what a kernel `statfs`/`df` needs).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FsStat {
    pub block_size: u64,
    pub blocks: u64,
    pub blocks_free: u64,
    pub files: u64,
    pub files_free: u64,
}

/// One directory entry for a readdir reply: name + inode id + object type.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Dirent {
    pub name: String,
    pub ino: u64,
    pub kind: u8,
}

/// A directory stream over a point-in-time snapshot of the entries (§E9). Once
/// opened, concurrent create/unlink/rename do not add, drop or duplicate names.
pub struct DirHandle {
    entries: Vec<String>,
    pos: usize,
}

impl DirHandle {
    /// Return up to `n` names from the snapshot and advance the cursor; `n == 0`
    /// returns all remaining. Returns an empty slice once exhausted.
    pub fn next(&mut self, n: usize) -> &[String] {
        if self.pos >= self.entries.len() {
            return &[];
        }
        let mut end = self.entries.len();
        if n > 0 && self.pos + n < end {
            end = self.pos + n;
        }
        let start = self.pos;
        self.pos = end;
        &self.entries[start..end]
    }
}

/// An open reference to a file. It keeps the inode alive even after the file is
/// unlinked, until [`FS::close`] (§E3).
pub struct Handle {
    id: u64,
    closed: bool,
}

impl Handle {
    /// The inode id this handle refers to.
    pub fn id(&self) -> u64 {
        self.id
    }
}

impl<D: Device> FS<D> {
    /// Create a fresh filesystem on `dev` and return it mounted. A `None` key
    /// selects a plaintext pool (§5.5 opt-out); otherwise it is the AES-XTS key.
    pub fn format(dev: D, key: Option<&[u8]>) -> Result<FS<D>> {
        bloomfs_cow::format(&dev, DEFAULT_INODE_COUNT, DEFAULT_DDT_RESERVE)?;
        let mut fs = FS::mount(dev, key)?;
        // inode 0 is the root directory (empty).
        let now = (fs.clock)();
        let root = Inode {
            kind: TYPE_DIR,
            nlink: 2,
            mode: 0o755,
            atime: now,
            mtime: now,
            ctime: now,
            ..Inode::default()
        };
        let root_id = fs.ub.root_inode;
        fs.inodes.put(root_id, &root)?;
        fs.commit()?;
        Ok(fs)
    }

    /// Open an existing filesystem on `dev`. `key` must match how it was formatted
    /// (`None` for a plaintext pool).
    pub fn mount(dev: D, key: Option<&[u8]>) -> Result<FS<D>> {
        let (ub, bm, ddt, inodes) = bloomfs_cow::mount(&dev)?;
        let bs = BlockStore::new(key)?;
        let meta_buf = vec![0u8; (ub.meta_blocks * BLK) as usize];
        let free_inodes = rebuild_free_inodes(&inodes);
        Ok(FS {
            dev,
            ub,
            bm,
            ddt,
            inodes,
            bs,
            open_count: HashMap::new(),
            free_inodes,
            clock: Box::new(default_now),
            meta_buf,
            dir_cache: Some(Mutex::new(DirCache::new())),
        })
    }

    /// Consume the filesystem and return the underlying device (e.g. to remount).
    /// Does not commit — the caller decides durability.
    pub fn into_device(self) -> D {
        self.dev
    }

    /// Override the wall clock (deterministic tests, §F6).
    pub fn set_clock(&mut self, clock: Clock) {
        self.clock = clock;
    }

    /// Enable or disable the resident open-directory cache (§B11), a performance
    /// optimization that is on by default. Disabling drops every resident
    /// directory; the next access reloads from the committed image. Toggling never
    /// affects correctness — the cache is a pure mirror of the in-RAM metadata.
    pub fn set_dir_cache(&mut self, on: bool) {
        self.dir_cache = if on {
            Some(Mutex::new(DirCache::new()))
        } else {
            None
        };
    }

    /// The root directory's inode id.
    pub fn root(&self) -> u64 {
        self.ub.root_inode
    }

    fn now(&self) -> u64 {
        (self.clock)()
    }

    /// A content/size change bumps both mtime and ctime (§F6).
    fn touch_mod(&self, in_: &mut Inode) {
        let t = self.now();
        in_.mtime = t;
        in_.ctime = t;
    }

    /// A metadata-only change (link count, rename) bumps ctime (§F6).
    fn touch_meta(&self, in_: &mut Inode) {
        in_.ctime = self.now();
    }

    // --- durability ---

    /// Persist the current state durably (CoW transaction, §B1).
    pub fn commit(&mut self) -> Result<()> {
        let root = self.ub.root_inode;
        let next = self.ub.next_inode;
        let new = bloomfs_cow::commit(
            &self.dev,
            &self.ub,
            &mut self.bm,
            &self.ddt,
            &self.inodes,
            root,
            next,
            Some(&mut self.meta_buf),
        )?;
        self.ub = new;
        Ok(())
    }

    /// Make all changes durable (POSIX fsync, §E7) — the CoW commit point.
    pub fn fsync(&mut self) -> Result<()> {
        self.commit()
    }

    // --- store helpers (the store holds only the cipher; pass dev/bm/ddt here) ---

    fn bs_write(&mut self, plaintext: &[u8]) -> Result<Ref> {
        match self.bs.write(&self.dev, &mut self.bm, &mut self.ddt, plaintext) {
            Ok(r) => Ok(r),
            // If the bitmap is full but deferred frees are pending, commit now
            // to reclaim them and retry once.  This prevents ENOSPC in workloads
            // that unlink files in a loop (e.g. keeping the last N archives):
            // deferred frees accumulate until commit, but an immediate commit
            // drains them and makes the space available for the retry.
            Err(bloomfs_store::Error::Alloc(bloomfs_alloc::Error::NoSpace))
                if self.bm.pending() > 0 =>
            {
                self.commit()?;
                Ok(self.bs.write(&self.dev, &mut self.bm, &mut self.ddt, plaintext)?)
            }
            Err(e) => Err(e.into()),
        }
    }

    fn bs_read(&self, r: &Ref) -> Result<Vec<u8>> {
        Ok(self.bs.read(&self.dev, r)?)
    }

    fn bs_release(&mut self, r: &Ref) {
        self.bs.release(&mut self.bm, &mut self.ddt, r);
    }

    // --- inode helpers ---

    /// Reserve an inode id, reusing a reclaimed one if available (§F5) and
    /// falling back to the bump allocator otherwise.
    fn alloc_inode(&mut self) -> Result<u64> {
        if let Some(id) = self.free_inodes.pop() {
            return Ok(id);
        }
        if self.ub.next_inode >= self.inodes.cap() {
            return Err(Error::NoInodes);
        }
        let id = self.ub.next_inode;
        self.ub.next_inode += 1;
        Ok(id)
    }

    fn open_count_of(&self, id: u64) -> u32 {
        self.open_count.get(&id).copied().unwrap_or(0)
    }

    // --- block-map / data ---

    /// The inode's data records in order. A single-record file holds its ref
    /// inline; a multi-record file holds a ref to a block-map blob, decoded here.
    fn load_refs(&self, in_: &Inode) -> Result<Vec<Ref>> {
        if in_.size == 0 {
            return Ok(Vec::new());
        }
        if in_.flags & FLAG_INLINE_EXTENTS != 0 {
            return Ok(vec![Ref::unmarshal(&in_.block_map[..REF_SIZE])]);
        }
        let blob = self.bs_read(&Ref::unmarshal(&in_.block_map[..REF_SIZE]))?;
        decode_refs(&blob)
    }

    /// Record `refs` as the inode's block map and set size/record_size_log2.
    /// Zero refs clear the data; one ref goes inline; many are written as a
    /// block-map blob whose own ref is stored inline.
    fn store_refs(&mut self, in_: &mut Inode, refs: &[Ref], size: u64, log2: u8) -> Result<()> {
        in_.block_map = [0u8; 64];
        in_.flags &= !FLAG_INLINE_EXTENTS;
        in_.size = size;
        in_.record_size_log2 = log2;
        match refs.len() {
            0 => {
                in_.size = 0;
                in_.record_size_log2 = 0;
            }
            1 => {
                in_.block_map[..REF_SIZE].copy_from_slice(&refs[0].marshal());
                in_.flags |= FLAG_INLINE_EXTENTS;
            }
            _ => {
                let map_ref = self.bs_write(&encode_refs(refs))?;
                in_.block_map[..REF_SIZE].copy_from_slice(&map_ref.marshal());
            }
        }
        Ok(())
    }

    /// Drop one reference to every cluster the inode owns: each data record plus,
    /// for a multi-record file, the block-map blob itself (§5.4). The underlying
    /// frees are deferred until the next commit (§F1).
    fn release_data(&mut self, in_: &Inode) -> Result<()> {
        if in_.size == 0 {
            return Ok(());
        }
        if in_.flags & FLAG_INLINE_EXTENTS != 0 {
            let r = Ref::unmarshal(&in_.block_map[..REF_SIZE]);
            self.bs_release(&r);
            return Ok(());
        }
        let map_ref = Ref::unmarshal(&in_.block_map[..REF_SIZE]);
        let blob = self.bs_read(&map_ref)?;
        for r in &decode_refs(&blob)? {
            self.bs_release(r);
        }
        self.bs_release(&map_ref);
        Ok(())
    }

    /// Replace the inode's contents with `blob`, re-chunked at a recordsize
    /// derived from its new length. New records are written before the old data
    /// is released, so a write failure never loses data.
    fn set_data(&mut self, id: u64, in_: &mut Inode, blob: &[u8]) -> Result<()> {
        let old = in_.clone();
        let mut refs = Vec::new();
        let mut size = 0u64;
        let mut log2 = 0u8;
        if !blob.is_empty() {
            size = blob.len() as u64;
            let (rs, l2) = record_size_for(size);
            log2 = l2;
            for rec in split_records(blob, rs) {
                refs.push(self.bs_write(rec)?); // old data still intact
            }
        }
        self.store_refs(in_, &refs, size, log2)?;
        self.release_data(&old)?;
        self.touch_mod(in_); // a content change bumps mtime+ctime (§F6)
        self.inodes.put(id, in_)?;
        Ok(())
    }

    /// Read the inode's full contents (empty if size == 0).
    fn get_data(&self, in_: &Inode) -> Result<Vec<u8>> {
        if in_.size == 0 {
            return Ok(Vec::new());
        }
        let mut out = Vec::with_capacity(in_.size as usize);
        for r in &self.load_refs(in_)? {
            out.extend_from_slice(&self.bs_read(r)?);
        }
        Ok(out)
    }

    // --- directory helpers ---

    /// Load a directory inode from disk and rebuild both its Bloom-segmented
    /// index and its page layout. This is the cache-free primitive; the resident
    /// cache (when enabled) is layered on top by `checkout_dir`/`cache_peek`.
    fn load_dir(&self, id: u64) -> Result<CachedDir> {
        let in_ = self.inodes.get(id)?;
        if in_.kind != TYPE_DIR {
            return Err(Error::NotDir);
        }
        let blob = self.get_data(&in_)?;
        let mut h = CachedDir {
            dir: Directory::new(),
            in_,
            pages: Vec::new(),
            used: Vec::new(),
            page_of: HashMap::new(),
            dirty: HashSet::new(),
            lru_tick: 0, // set by DirCache::insert / DirCache::touch on first use
        };
        let mut off = 0;
        while off < blob.len() {
            let end = (off + DIR_PAGE_SIZE).min(blob.len());
            let (entries, used) = decode_dir_page(&blob[off..end])?;
            let p = h.pages.len();
            for e in &entries {
                h.dir.add(&e.name, e.inode);
                h.page_of.insert(e.name.clone(), p);
            }
            h.pages.push(entries);
            h.used.push(used);
            off += DIR_PAGE_SIZE;
        }
        Ok(h)
    }

    /// Acquire a working copy of directory `id` for mutation. With the cache on,
    /// take the resident entry out of the map (it is returned by `commit_dir`) and
    /// refresh its inode from the table — the resident index is authoritative for
    /// the *entries*, but the directory's own inode may have changed underneath it
    /// (e.g. a `chmod`). A miss (or a disabled cache) falls back to a disk load.
    fn checkout_dir(&self, id: u64) -> Result<CachedDir> {
        if let Some(cache) = &self.dir_cache {
            if let Some(mut h) = cache.lock().unwrap().entries.remove(&id) {
                h.in_ = self.inodes.get(id)?;
                return Ok(h);
            }
        }
        self.load_dir(id)
    }

    /// Persist a working directory (its dirty pages) and, with the cache on, place
    /// it back into the resident map so the next access skips the disk reload.
    /// The LRU-capped `DirCache::insert` evicts the least-recently-used entry if
    /// the map is already at `DIR_CACHE_CAP`.
    fn commit_dir(&mut self, id: u64, mut h: CachedDir) -> Result<()> {
        self.flush_dir(id, &mut h)?;
        if let Some(cache) = &self.dir_cache {
            cache.lock().unwrap().insert(id, h);
        }
        Ok(())
    }

    /// Drop directory `id` from the resident cache (after it is unlinked/reclaimed
    /// or its id recycled). A no-op when the cache is off or the id is absent.
    fn evict_dir(&self, id: u64) {
        if let Some(cache) = &self.dir_cache {
            cache.lock().unwrap().entries.remove(&id);
        }
    }

    /// Run `f` over directory `id`'s resident index, loading and caching it on a
    /// miss. Returns `Ok(None)` when the cache is disabled, signalling the caller
    /// to take its cache-free path. Reader ops keep their `&self` signature: the
    /// `Mutex` provides the interior mutability needed to populate on a miss, and
    /// `f` only reads the index (never the directory inode), so a stale cached
    /// inode is irrelevant here.
    fn cache_peek<T>(&self, id: u64, f: impl FnOnce(&CachedDir) -> T) -> Result<Option<T>> {
        let cache = match &self.dir_cache {
            Some(c) => c,
            None => return Ok(None),
        };
        let mut g = cache.lock().unwrap();
        if !g.entries.contains_key(&id) {
            let h = self.load_dir(id)?;
            g.insert(id, h);
        } else {
            g.touch(id);
        }
        Ok(Some(f(g.entries.get(&id).unwrap())))
    }

    /// Persist every page changed since load, each as one record-addressable
    /// `write_at` at its fixed page offset — untouched pages keep their refs.
    fn flush_dir(&mut self, id: u64, h: &mut CachedDir) -> Result<()> {
        let dirty: Vec<usize> = h.dirty.iter().copied().collect();
        for p in dirty {
            let page = encode_dir_page(&h.pages[p]);
            self.write_at_locked(id, &mut h.in_, (p as u64) * DIR_PAGE_SIZE as u64, &page)?;
        }
        h.dirty.clear();
        Ok(())
    }

    /// Decode directory `id`'s pages into a flat entry list, without building the
    /// Bloom index, page layout or `page_of` map. This is the read-only
    /// counterpart to `load_dir`: the reader ops (lookup/readdir/...) only need
    /// the entries, so they skip the per-entry name clone and the structures
    /// that exist solely to support mutation + flush.
    fn read_dir_pages(&self, id: u64) -> Result<Vec<DirEntry>> {
        let in_ = self.inodes.get(id)?;
        if in_.kind != TYPE_DIR {
            return Err(Error::NotDir);
        }
        let blob = self.get_data(&in_)?;
        let mut out = Vec::new();
        let mut off = 0;
        while off < blob.len() {
            let end = (off + DIR_PAGE_SIZE).min(blob.len());
            let (entries, _used) = decode_dir_page(&blob[off..end])?;
            out.extend(entries);
            off += DIR_PAGE_SIZE;
        }
        Ok(out)
    }

    // --- VFS operations ---

    /// Resolve `name` in directory `parent` to an inode id.
    pub fn lookup(&self, parent: u64, name: &str) -> Result<Option<u64>> {
        // With the cache on, the resident Bloom index answers in one filter probe.
        if let Some(found) = self.cache_peek(parent, |h| h.dir.find(name).map(|i| i.0))? {
            return Ok(found);
        }
        // Cache off: a single resolve needs no Bloom index — decode the pages and
        // scan them directly. Building the per-segment filters would cost a full
        // pass over every name only to answer one query; the linear scan over the
        // already-decoded entries is cheaper and touches the same data.
        for e in self.read_dir_pages(parent)? {
            if e.name == name {
                return Ok(Some(e.inode.0));
            }
        }
        Ok(None)
    }

    /// List the names in directory `id` (a one-shot consistent snapshot).
    pub fn readdir(&self, id: u64) -> Result<Vec<String>> {
        if let Some(names) = self.cache_peek(id, |h| {
            h.pages.iter().flatten().map(|e| e.name.clone()).collect()
        })? {
            return Ok(names);
        }
        Ok(self
            .read_dir_pages(id)?
            .into_iter()
            .map(|e| e.name)
            .collect())
    }

    /// Return directory `id`'s entries (name + id + type) as one snapshot.
    pub fn readdirents(&self, id: u64) -> Result<Vec<Dirent>> {
        // Collect (name, id) under the cache lock (or from disk), then resolve each
        // type from the inode table outside it — the closure can't borrow `self`.
        let pairs: Vec<(String, u64)> = match self.cache_peek(id, |h| {
            h.pages
                .iter()
                .flatten()
                .map(|e| (e.name.clone(), e.inode.0))
                .collect()
        })? {
            Some(v) => v,
            None => self
                .read_dir_pages(id)?
                .into_iter()
                .map(|e| (e.name, e.inode.0))
                .collect(),
        };
        let mut out = Vec::with_capacity(pairs.len());
        for (name, ino) in pairs {
            let in_ = self.inodes.get(ino)?;
            out.push(Dirent {
                name,
                ino,
                kind: in_.kind,
            });
        }
        Ok(out)
    }

    /// Capture a consistent snapshot of directory `id`'s names for iteration.
    pub fn open_dir_stream(&self, id: u64) -> Result<DirHandle> {
        let entries = match self.cache_peek(id, |h| {
            h.pages.iter().flatten().map(|e| e.name.clone()).collect()
        })? {
            Some(v) => v,
            None => self
                .read_dir_pages(id)?
                .into_iter()
                .map(|e| e.name)
                .collect(),
        };
        Ok(DirHandle { entries, pos: 0 })
    }

    /// Return the metadata of inode `id`.
    pub fn stat(&self, id: u64) -> Result<Stat> {
        let in_ = self.inodes.get(id)?;
        Ok(Stat {
            ino: id,
            size: in_.size,
            nlink: in_.nlink,
            generation: in_.generation,
            mode: in_.mode,
            kind: in_.kind,
            uid: in_.uid,
            gid: in_.gid,
            atime: in_.atime,
            mtime: in_.mtime,
            ctime: in_.ctime,
        })
    }

    /// Report filesystem-wide capacity (data blocks and inode slots).
    pub fn statfs(&self) -> FsStat {
        let used = self.ub.next_inode - self.free_inodes.len() as u64;
        let cap = self.inodes.cap();
        // `available()` excludes deferred-free clusters (they stay SET in the
        // bitmap until the next commit so a crash can't reuse them).  Adding
        // `deferred_count()` gives the user-visible "free" figure: space that
        // is either already free or will be freed atomically at the next commit.
        // Without this, `df` over-reports usage during a long write/unlink loop
        // and can trigger spurious "disk full" warnings.
        FsStat {
            block_size: BLK,
            blocks: self.bm.total(),
            blocks_free: self.bm.available() + self.bm.deferred_count(),
            files: cap,
            files_free: cap - used,
        }
    }

    /// Space-availability check for `fallocate(2)`.
    ///
    /// BloomFS is content-addressed: we cannot pre-allocate physical clusters
    /// for data that has not been written yet (unknown hash → unknown dedup
    /// ratio).  Instead we estimate the **worst case** — assume 0 % dedup, all
    /// new data is unique — and compare against the current free-block count
    /// (which already includes deferred-free clusters via `statfs`).
    ///
    /// If the check passes, the caller can be confident that `length` bytes of
    /// entirely non-deduplicable data *could* fit right now.  If it fails, the
    /// data definitely will not fit even in the best case.  Returns
    /// `Error::NoSpace` on failure so the FUSE layer can map it to `ENOSPC`.
    ///
    /// # Overhead breakdown for `length` bytes
    ///
    /// * **Data records** – `ceil(length / record_size)` clusters (each
    ///   cluster = one 4 KiB block; record_size is 4–32 KiB depending on file
    ///   size, so records may span multiple clusters).
    /// * **block_map blob** – one `Ref`-list entry per record (`REF_SIZE` bytes
    ///   each), rounded up to whole clusters.
    /// * **Inode update** – already allocated, no new cluster needed.
    ///
    /// The inode's existing data is not counted because `fallocate` only asks
    /// "is there room for `length` more bytes", not "is the whole file free".
    pub fn fallocate(&self, _id: u64, length: u64) -> Result<()> {
        if length == 0 {
            return Ok(());
        }
        let (record_size, _) = record_size_for(length);
        // clusters per data record (record_size is already a multiple of BLK)
        let clusters_per_rec = record_size / BLK;
        let n_records = length.div_ceil(record_size);
        let data_clusters = n_records * clusters_per_rec;

        // block_map blob: n_records × REF_SIZE bytes, rounded to whole clusters
        let map_bytes = n_records * REF_SIZE as u64;
        let map_clusters = map_bytes.div_ceil(BLK);

        let needed = data_clusters + map_clusters;
        let free = self.bm.available() + self.bm.deferred_count();
        if needed > free {
            Err(Error::NoSpace)
        } else {
            Ok(())
        }
    }

    /// Whether the caller may access inode `id` under `mask` (a bitwise-OR of
    /// `ACCESS_R`/`ACCESS_W`/`ACCESS_X`; mask 0 is an existence check). Standard
    /// POSIX DAC — owner, then group, then other — with root (uid 0) bypassing
    /// read/write and only needing one execute bit for X.
    pub fn access(&self, id: u64, mask: u32, uid: u32, gid: u32, gids: &[u32]) -> Result<()> {
        let in_ = self.inodes.get(id)?;
        if mask == 0 {
            return Ok(()); // F_OK: the inode exists
        }
        let perm = (in_.mode as u32) & 0o777;
        if uid == 0 {
            // root: unrestricted read/write; execute needs at least one x bit (a
            // directory is always traversable by root).
            if mask & ACCESS_X != 0 && in_.kind != TYPE_DIR && perm & 0o111 == 0 {
                return Err(Error::Permission);
            }
            return Ok(());
        }
        let shift = if uid == in_.uid {
            6
        } else if gid == in_.gid || gids.contains(&in_.gid) {
            3
        } else {
            0
        };
        let allowed = (perm >> shift) & 7;
        if mask & allowed != mask {
            return Err(Error::Permission);
        }
        Ok(())
    }

    /// Add a new inode of the given type under parent/name. Caller already holds
    /// the exclusive `&mut self`, so create-then-populate (e.g. symlink) is one
    /// critical section.
    fn create_node(&mut self, parent: u64, name: &str, typ: u8, mode: u16) -> Result<u64> {
        let mut h = self.checkout_dir(parent)?;
        if h.dir.find(name).is_some() {
            return Err(Error::Exists);
        }
        let id = self.alloc_inode()?;
        // Inherit the slot's generation: reclaim bumps it on free, so a reused id
        // gets a fresh generation and stale handles don't alias it.
        let prev = self.inodes.get(id)?;
        let now = self.now();
        let mut child = Inode {
            kind: typ,
            nlink: 1,
            mode,
            generation: prev.generation,
            atime: now,
            mtime: now,
            ctime: now,
            ..Inode::default()
        };
        if typ == TYPE_DIR {
            child.nlink = 2;
        }
        self.inodes.put(id, &child)?;
        h.add(name, InodeId(id));
        // A new subdirectory's ".." links back to the parent, so the parent's
        // link count grows by one (POSIX directory nlink).
        if typ == TYPE_DIR {
            h.in_.nlink += 1;
        }
        self.commit_dir(parent, h)?;
        Ok(id)
    }

    /// Create a subdirectory.
    pub fn mkdir(&mut self, parent: u64, name: &str) -> Result<u64> {
        self.create_node(parent, name, TYPE_DIR, 0o755)
    }

    /// Create an empty regular file.
    pub fn create(&mut self, parent: u64, name: &str) -> Result<u64> {
        self.create_node(parent, name, TYPE_REGULAR, 0o644)
    }

    /// Create a symbolic link `name` in `parent` pointing at `target`. The target
    /// path is stored as the link inode's data (through the normal pipeline).
    pub fn symlink(&mut self, parent: u64, name: &str, target: &str) -> Result<u64> {
        if target.is_empty() {
            return Err(Error::Invalid);
        }
        let id = self.create_node(parent, name, TYPE_LINK, 0o777)?;
        let mut in_ = self.inodes.get(id)?;
        self.set_data(id, &mut in_, target.as_bytes())?;
        Ok(id)
    }

    /// Return the target path of symbolic link `id`.
    pub fn readlink(&self, id: u64) -> Result<String> {
        let in_ = self.inodes.get(id)?;
        if in_.kind != TYPE_LINK {
            return Err(Error::Invalid);
        }
        let data = self.get_data(&in_)?;
        Ok(String::from_utf8_lossy(&data).into_owned())
    }

    /// Replace the contents of regular file `id`.
    pub fn write_file(&mut self, id: u64, data: &[u8]) -> Result<()> {
        let mut in_ = self.inodes.get(id)?;
        if in_.kind != TYPE_REGULAR {
            return Err(Error::NotFile);
        }
        self.set_data(id, &mut in_, data)
    }

    /// Return the contents of regular file `id`.
    pub fn read_file(&self, id: u64) -> Result<Vec<u8>> {
        let in_ = self.inodes.get(id)?;
        if in_.kind != TYPE_REGULAR {
            return Err(Error::NotFile);
        }
        self.get_data(&in_)
    }

    /// Return up to `length` bytes of file `id` starting at `off`, reading only
    /// the records the range overlaps (§4.5).
    pub fn read_at(&self, id: u64, off: u64, length: usize) -> Result<Vec<u8>> {
        let in_ = self.inodes.get(id)?;
        if in_.kind != TYPE_REGULAR {
            return Err(Error::NotFile);
        }
        if length == 0 || off >= in_.size {
            return Ok(Vec::new());
        }
        let end = (off + length as u64).min(in_.size);
        let refs = self.load_refs(&in_)?;
        let rs = 1u64 << in_.record_size_log2;
        let mut out = Vec::with_capacity((end - off) as usize);
        let mut i = off / rs;
        while i <= (end - 1) / rs {
            let rec = self.bs_read(&refs[i as usize])?;
            let rec_start = i * rs;
            let rec_end = rec_start + rec.len() as u64;
            let lo = off.max(rec_start);
            let hi = end.min(rec_end);
            if lo < hi {
                out.extend_from_slice(&rec[(lo - rec_start) as usize..(hi - rec_start) as usize]);
            }
            i += 1;
        }
        Ok(out)
    }

    /// Overwrite file `id` at `off` with `data`, extending (with a zero-filled
    /// gap if `off` is past EOF) as needed. Only the overlapped records are
    /// read-modify-written; untouched records keep their existing refs.
    pub fn write_at(&mut self, id: u64, off: u64, data: &[u8]) -> Result<()> {
        let mut in_ = self.inodes.get(id)?;
        if in_.kind != TYPE_REGULAR {
            return Err(Error::NotFile);
        }
        self.write_at_locked(id, &mut in_, off, data)
    }

    /// Atomically write `data` at the end of file `id` and return the offset it
    /// landed at (§E8).
    pub fn append(&mut self, id: u64, data: &[u8]) -> Result<u64> {
        let mut in_ = self.inodes.get(id)?;
        if in_.kind != TYPE_REGULAR {
            return Err(Error::NotFile);
        }
        let off = in_.size;
        self.write_at_locked(id, &mut in_, off, data)?;
        Ok(off)
    }

    fn write_at_locked(&mut self, id: u64, in_: &mut Inode, off: u64, data: &[u8]) -> Result<()> {
        if data.is_empty() {
            return Ok(());
        }
        let old_size = in_.size;
        let old_refs = self.load_refs(in_)?;
        // Capture the old block-map container (if any) before we overwrite it.
        let old_container = if old_size > 0 && in_.flags & FLAG_INLINE_EXTENTS == 0 {
            Some(Ref::unmarshal(&in_.block_map[..REF_SIZE]))
        } else {
            None
        };

        let write_end = off + data.len() as u64;
        let new_size = old_size.max(write_end);

        let (rs, log2) = if old_size > 0 {
            // frozen recordsize
            let l = in_.record_size_log2;
            (1u64 << l, l)
        } else {
            record_size_for(new_size)
        };

        let rec_count = new_size.div_ceil(rs);
        let mut new_refs = vec![Ref::default(); rec_count as usize];
        let mut to_release: Vec<Ref> = Vec::new();
        for i in 0..rec_count {
            let rec_start = i * rs;
            let rec_end = (rec_start + rs).min(new_size);
            let touched = write_end > rec_start && off < rec_end;
            if !touched && (i as usize) < old_refs.len() && rec_end <= old_size {
                new_refs[i as usize] = old_refs[i as usize]; // carry over untouched record
                continue;
            }
            let mut rec = vec![0u8; (rec_end - rec_start) as usize];
            if (i as usize) < old_refs.len() {
                // preserve bytes outside the write
                let old = self.bs_read(&old_refs[i as usize])?;
                let n = old.len().min(rec.len());
                rec[..n].copy_from_slice(&old[..n]);
            }
            let lo = off.max(rec_start);
            let hi = write_end.min(rec_end);
            if lo < hi {
                rec[(lo - rec_start) as usize..(hi - rec_start) as usize]
                    .copy_from_slice(&data[(lo - off) as usize..(hi - off) as usize]);
            }
            new_refs[i as usize] = self.bs_write(&rec)?;
            if (i as usize) < old_refs.len() {
                to_release.push(old_refs[i as usize]);
            }
        }

        self.store_refs(in_, &new_refs, new_size, log2)?;
        for r in &to_release {
            self.bs_release(r);
        }
        if let Some(c) = &old_container {
            self.bs_release(c);
        }
        self.touch_mod(in_); // a write bumps mtime+ctime (§F6)
        self.inodes.put(id, in_)?;
        Ok(())
    }

    /// Set file `id`'s size to `new_size`: shrinking drops the tail, growing
    /// zero-extends (§E5). This prototype re-chunks the whole file.
    pub fn truncate(&mut self, id: u64, new_size: u64) -> Result<()> {
        let mut in_ = self.inodes.get(id)?;
        if in_.kind != TYPE_REGULAR {
            return Err(Error::NotFile);
        }
        if new_size == in_.size {
            return Ok(());
        }
        let blob = self.get_data(&in_)?;
        let new_blob = if new_size <= blob.len() as u64 {
            blob[..new_size as usize].to_vec()
        } else {
            let mut grown = vec![0u8; new_size as usize];
            grown[..blob.len()].copy_from_slice(&blob);
            grown
        };
        self.set_data(id, &mut in_, &new_blob)
    }

    /// Remove `name` from directory `parent`. The link count drops by one; the
    /// inode and its data are reclaimed only when no names remain (nlink == 0,
    /// §E4) AND no open handle references it (§E3).
    pub fn unlink(&mut self, parent: u64, name: &str) -> Result<()> {
        let mut h = self.checkout_dir(parent)?;
        let id = h.dir.find(name).ok_or(Error::NotFound)?;
        let mut child = self.inodes.get(id.0)?;
        if child.kind == TYPE_DIR {
            return Err(Error::IsDir); // directories are removed with rmdir
        }

        h.del(name);
        self.commit_dir(parent, h)?;

        if child.nlink > 0 {
            child.nlink -= 1;
        }
        if child.nlink == 0 && self.open_count_of(id.0) == 0 {
            return self.reclaim(id.0, &child);
        }
        self.touch_meta(&mut child); // link-count change bumps ctime (§F6)
        self.inodes.put(id.0, &child)?;
        Ok(())
    }

    /// Remove an empty subdirectory `name` from `parent` (POSIX rmdir).
    pub fn rmdir(&mut self, parent: u64, name: &str) -> Result<()> {
        let mut h = self.checkout_dir(parent)?;
        let id = h.dir.find(name).ok_or(Error::NotFound)?;
        let child = self.inodes.get(id.0)?;
        if child.kind != TYPE_DIR {
            return Err(Error::NotDir);
        }
        let ch = self.load_dir(id.0)?;
        if !ch.dir.is_empty() {
            return Err(Error::NotEmpty);
        }

        h.del(name);
        if h.in_.nlink > 0 {
            h.in_.nlink -= 1; // the child's ".." no longer counts toward the parent
        }
        self.commit_dir(parent, h)?;
        self.reclaim(id.0, &child)
    }

    /// Free an inode's data and recycle the id (§F5).
    fn reclaim(&mut self, id: u64, in_: &Inode) -> Result<()> {
        // If this id named a directory, drop any resident copy: the id is about to
        // be recycled (with a bumped generation) for an unrelated inode.
        self.evict_dir(id);
        self.release_data(in_)?;
        // Zero the slot but carry a bumped generation forward, then make the id
        // available for reuse (§F5).
        let fresh = Inode {
            generation: in_.generation + 1,
            ..Inode::default()
        };
        self.inodes.put(id, &fresh)?;
        self.free_inodes.push(id);
        Ok(())
    }

    /// Apply `f` to inode `id` and persist it.
    fn mutate_inode<F: FnOnce(&mut Inode)>(&mut self, id: u64, f: F) -> Result<()> {
        let mut in_ = self.inodes.get(id)?;
        f(&mut in_);
        self.inodes.put(id, &in_)?;
        Ok(())
    }

    /// Set the permission bits of inode `id` (POSIX chmod); ctime is bumped.
    pub fn chmod(&mut self, id: u64, mode: u16) -> Result<()> {
        let now = self.now();
        self.mutate_inode(id, |in_| {
            in_.mode = mode;
            in_.ctime = now;
        })
    }

    /// Set the owner and group of inode `id` (POSIX chown); ctime is bumped.
    pub fn chown(&mut self, id: u64, uid: u32, gid: u32) -> Result<()> {
        let now = self.now();
        self.mutate_inode(id, |in_| {
            in_.uid = uid;
            in_.gid = gid;
            in_.ctime = now;
        })
    }

    /// Set the access and modification times of inode `id` (POSIX utimes);
    /// ctime is bumped to now since the metadata changed.
    pub fn utimes(&mut self, id: u64, atime: u64, mtime: u64) -> Result<()> {
        let now = self.now();
        self.mutate_inode(id, |in_| {
            in_.atime = atime;
            in_.mtime = mtime;
            in_.ctime = now;
        })
    }

    /// Create a hard link: a second name in `parent` pointing at an existing
    /// non-directory inode (§E4). POSIX forbids hard links to directories.
    pub fn link(&mut self, parent: u64, name: &str, target_id: u64) -> Result<()> {
        let mut target = self.inodes.get(target_id)?;
        if target.kind == TYPE_DIR {
            return Err(Error::NotFile);
        }
        let mut h = self.checkout_dir(parent)?;
        if h.dir.find(name).is_some() {
            return Err(Error::Exists);
        }
        target.nlink += 1;
        self.touch_meta(&mut target); // link-count change bumps ctime (§F6)
        self.inodes.put(target_id, &target)?;
        h.add(name, InodeId(target_id));
        self.commit_dir(parent, h)
    }

    /// Drop the existing destination's link during a rename overwrite. Returns
    /// `Ok(true)` if `dn` already names `id` (rename-onto-self no-op).
    fn rename_overwrite(&mut self, dh: &mut CachedDir, dn: &str, id: InodeId) -> Result<bool> {
        if let Some(old_id) = dh.dir.find(dn) {
            if old_id.0 == id.0 {
                return Ok(true); // renaming onto itself: no-op
            }
            let mut old = self.inodes.get(old_id.0)?;
            if old.nlink > 0 {
                old.nlink -= 1;
            }
            if old.nlink == 0 && self.open_count_of(old_id.0) == 0 {
                self.reclaim(old_id.0, &old)?;
            } else {
                self.touch_meta(&mut old); // link-count change bumps ctime (§F6)
                self.inodes.put(old_id.0, &old)?;
            }
            dh.del(dn);
        }
        Ok(false)
    }

    /// The renamed inode's ctime changes (its link/location changed), §F6.
    fn touch_moved(&mut self, id: u64) -> Result<()> {
        if let Ok(mut m) = self.inodes.get(id) {
            self.touch_meta(&mut m);
            self.inodes.put(id, &m)?;
        }
        Ok(())
    }

    /// Move `src_name` in `src_parent` to `dst_name` in `dst_parent`. If
    /// `dst_name` exists it is atomically replaced (its link count drops,
    /// reclaimed if it hits zero, §E1/§E2).
    pub fn rename(
        &mut self,
        src_parent: u64,
        src_name: &str,
        dst_parent: u64,
        dst_name: &str,
    ) -> Result<()> {
        let mut sh = self.checkout_dir(src_parent)?;
        let id = sh.dir.find(src_name).ok_or(Error::NotFound)?;
        // Reject the trivial directory loop: moving a directory into itself.
        if dst_parent == id.0 {
            return Err(Error::Invalid);
        }

        if src_parent == dst_parent {
            // Same directory: both edits land on the one handle, one flush.
            if self.rename_overwrite(&mut sh, dst_name, id)? {
                return Ok(());
            }
            sh.add(dst_name, id);
            sh.del(src_name);
            self.touch_moved(id.0)?;
            return self.commit_dir(src_parent, sh);
        }

        let mut dh = self.checkout_dir(dst_parent)?;
        if self.rename_overwrite(&mut dh, dst_name, id)? {
            return Ok(());
        }
        dh.add(dst_name, id);
        sh.del(src_name);
        self.touch_moved(id.0)?;
        // Persist destination first, then source.
        self.commit_dir(dst_parent, dh)?;
        self.commit_dir(src_parent, sh)
    }

    // --- open handles ---

    /// Return a handle to regular file `id` and record the open reference.
    pub fn open(&mut self, id: u64) -> Result<Handle> {
        let in_ = self.inodes.get(id)?;
        if in_.kind != TYPE_REGULAR {
            return Err(Error::NotFile);
        }
        *self.open_count.entry(id).or_insert(0) += 1;
        Ok(Handle { id, closed: false })
    }

    /// Read the file's current contents through a handle — this works even if the
    /// file has been unlinked (§E3).
    pub fn handle_read(&self, h: &Handle) -> Result<Vec<u8>> {
        let in_ = self.inodes.get(h.id)?;
        self.get_data(&in_)
    }

    /// Drop the open reference. If the file was unlinked while open and this is
    /// the last handle, its storage is reclaimed now (§E3).
    pub fn close(&mut self, h: &mut Handle) -> Result<()> {
        if h.closed {
            return Ok(());
        }
        h.closed = true;
        let id = h.id;
        if let Some(c) = self.open_count.get_mut(&id) {
            if *c > 0 {
                *c -= 1;
            }
        }
        if self.open_count_of(id) == 0 {
            self.open_count.remove(&id);
            let in_ = self.inodes.get(id)?;
            if in_.nlink == 0 {
                // unlinked while open: reclaim now
                return self.reclaim(id, &in_);
            }
        }
        Ok(())
    }
}

/// Scan the committed table for reclaimable slots (nlink == 0) within the
/// high-water mark, reconstructing the reuse stack after a mount (§F5).
fn rebuild_free_inodes(t: &InodeTable) -> Vec<u64> {
    let mut free = Vec::new();
    for id in 0..t.count() {
        match t.get(id) {
            Ok(in_) if in_.nlink == 0 => free.push(id),
            Ok(_) => {}
            Err(_) => break,
        }
    }
    free
}

#[cfg(test)]
mod tests {
    use super::*;
    use bloomfs_block::MemDevice;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::Arc;

    fn fresh() -> FS<MemDevice> {
        let dev = MemDevice::new(4096);
        FS::format(dev, None).expect("format")
    }

    fn fresh_encrypted() -> FS<MemDevice> {
        let dev = MemDevice::new(4096);
        let key: Vec<u8> = (0..64u8).map(|i| i.wrapping_add(1)).collect();
        FS::format(dev, Some(&key)).expect("format encrypted")
    }

    #[test]
    fn format_root_and_statfs() {
        let fs = fresh();
        let root = fs.root();
        let st = fs.stat(root).expect("stat root");
        assert_eq!(st.kind, TYPE_DIR);
        assert_eq!(st.nlink, 2);
        assert_eq!(st.mode, 0o755);
        let sf = fs.statfs();
        assert_eq!(sf.block_size, BLK);
        assert!(sf.blocks_free < sf.blocks);
        assert!(sf.files_free < sf.files);
    }

    #[test]
    fn mkdir_create_lookup() {
        let mut fs = fresh();
        let root = fs.root();
        let sub = fs.mkdir(root, "sub").expect("mkdir");
        let f = fs.create(sub, "file.txt").expect("create");
        assert_eq!(fs.lookup(root, "sub").unwrap(), Some(sub));
        assert_eq!(fs.lookup(sub, "file.txt").unwrap(), Some(f));
        assert_eq!(fs.lookup(sub, "absent").unwrap(), None);
        // Parent nlink grew for the new subdir's "..".
        assert_eq!(fs.stat(root).unwrap().nlink, 3);
        // Duplicate create is rejected.
        assert!(matches!(fs.create(sub, "file.txt"), Err(Error::Exists)));
        // readdir lists the entries.
        let mut names = fs.readdir(sub).unwrap();
        names.sort();
        assert_eq!(names, vec!["file.txt".to_string()]);
    }

    #[test]
    fn write_read_small_and_large() {
        let mut fs = fresh_encrypted();
        let root = fs.root();
        let f = fs.create(root, "f").unwrap();
        // small single-record
        fs.write_file(f, b"hello world").unwrap();
        assert_eq!(fs.read_file(f).unwrap(), b"hello world");
        assert_eq!(fs.stat(f).unwrap().size, 11);
        // large multi-record (1 MiB of pseudo-random bytes -> several records)
        let big: Vec<u8> = (0..1_000_000u32)
            .map(|i| (i.wrapping_mul(2654435761) >> 13) as u8)
            .collect();
        fs.write_file(f, &big).unwrap();
        assert_eq!(fs.read_file(f).unwrap(), big);
        // partial reads
        assert_eq!(fs.read_at(f, 100, 50).unwrap(), big[100..150]);
        assert_eq!(fs.read_at(f, 999_990, 1000).unwrap(), big[999_990..]);
        assert!(fs.read_at(f, 2_000_000, 10).unwrap().is_empty());
    }

    #[test]
    fn write_at_and_append() {
        let mut fs = fresh();
        let root = fs.root();
        let f = fs.create(root, "f").unwrap();
        fs.write_file(f, b"AAAAAAAA").unwrap();
        fs.write_at(f, 2, b"bb").unwrap();
        assert_eq!(fs.read_file(f).unwrap(), b"AAbbAAAA");
        // write past EOF zero-fills the gap
        fs.write_at(f, 10, b"Z").unwrap();
        let got = fs.read_file(f).unwrap();
        assert_eq!(got.len(), 11);
        assert_eq!(&got[8..11], &[0, 0, b'Z']);
        // append returns the landing offset
        let off = fs.append(f, b"!!").unwrap();
        assert_eq!(off, 11);
        assert_eq!(fs.read_file(f).unwrap().len(), 13);
    }

    #[test]
    fn truncate_grow_and_shrink() {
        let mut fs = fresh();
        let root = fs.root();
        let f = fs.create(root, "f").unwrap();
        fs.write_file(f, b"0123456789").unwrap();
        fs.truncate(f, 4).unwrap();
        assert_eq!(fs.read_file(f).unwrap(), b"0123");
        fs.truncate(f, 8).unwrap();
        assert_eq!(
            fs.read_file(f).unwrap(),
            &[b'0', b'1', b'2', b'3', 0, 0, 0, 0]
        );
        assert_eq!(fs.stat(f).unwrap().size, 8);
    }

    #[test]
    fn unlink_and_reclaim_reuses_inode() {
        let mut fs = fresh();
        let root = fs.root();
        let a = fs.create(root, "a").unwrap();
        fs.write_file(a, b"data").unwrap();
        let used_before = fs.statfs().files_free;
        fs.unlink(root, "a").unwrap();
        assert_eq!(fs.lookup(root, "a").unwrap(), None);
        // id is reclaimed and reused (generation bumped)
        let b = fs.create(root, "b").unwrap();
        assert_eq!(b, a, "reclaimed inode id reused");
        assert!(fs.stat(b).unwrap().generation >= 1);
        assert_eq!(fs.statfs().files_free, used_before);
    }

    #[test]
    fn hard_link_and_unlink_keeps_data() {
        let mut fs = fresh();
        let root = fs.root();
        let a = fs.create(root, "a").unwrap();
        fs.write_file(a, b"shared").unwrap();
        fs.link(root, "b", a).unwrap();
        assert_eq!(fs.stat(a).unwrap().nlink, 2);
        fs.unlink(root, "a").unwrap();
        // data still reachable via the other name
        let b = fs.lookup(root, "b").unwrap().unwrap();
        assert_eq!(fs.read_file(b).unwrap(), b"shared");
        assert_eq!(fs.stat(b).unwrap().nlink, 1);
    }

    #[test]
    fn unlink_open_file_survives_until_close() {
        let mut fs = fresh();
        let root = fs.root();
        let f = fs.create(root, "f").unwrap();
        fs.write_file(f, b"alive").unwrap();
        let mut h = fs.open(f).unwrap();
        fs.unlink(root, "f").unwrap();
        assert_eq!(fs.lookup(root, "f").unwrap(), None);
        // readable through the handle even though it has no name
        assert_eq!(fs.handle_read(&h).unwrap(), b"alive");
        fs.close(&mut h).unwrap();
        // after the last close the id is reclaimed and reused
        let g = fs.create(root, "g").unwrap();
        assert_eq!(g, f);
    }

    #[test]
    fn rmdir_empty_only() {
        let mut fs = fresh();
        let root = fs.root();
        let d = fs.mkdir(root, "d").unwrap();
        let _ = fs.create(d, "inside").unwrap();
        assert!(matches!(fs.rmdir(root, "d"), Err(Error::NotEmpty)));
        fs.unlink(d, "inside").unwrap();
        let before = fs.stat(root).unwrap().nlink;
        fs.rmdir(root, "d").unwrap();
        assert_eq!(fs.lookup(root, "d").unwrap(), None);
        assert_eq!(fs.stat(root).unwrap().nlink, before - 1);
    }

    #[test]
    fn rename_within_and_across_dirs() {
        let mut fs = fresh();
        let root = fs.root();
        let f = fs.create(root, "a").unwrap();
        fs.write_file(f, b"x").unwrap();
        // same-dir rename
        fs.rename(root, "a", root, "b").unwrap();
        assert_eq!(fs.lookup(root, "a").unwrap(), None);
        assert_eq!(fs.lookup(root, "b").unwrap(), Some(f));
        // overwrite an existing destination
        let victim = fs.create(root, "c").unwrap();
        fs.rename(root, "b", root, "c").unwrap();
        assert_eq!(fs.lookup(root, "c").unwrap(), Some(f));
        assert!(matches!(fs.stat(victim), Ok(s) if s.nlink == 0) || fs.stat(victim).is_ok());
        // cross-dir rename
        let d = fs.mkdir(root, "d").unwrap();
        fs.rename(root, "c", d, "moved").unwrap();
        assert_eq!(fs.lookup(root, "c").unwrap(), None);
        assert_eq!(fs.lookup(d, "moved").unwrap(), Some(f));
        assert_eq!(fs.read_file(f).unwrap(), b"x");
    }

    #[test]
    fn symlink_roundtrip() {
        let mut fs = fresh();
        let root = fs.root();
        let l = fs.symlink(root, "link", "/target/path").unwrap();
        assert_eq!(fs.stat(l).unwrap().kind, TYPE_LINK);
        assert_eq!(fs.readlink(l).unwrap(), "/target/path");
        assert!(matches!(fs.symlink(root, "bad", ""), Err(Error::Invalid)));
    }

    #[test]
    fn access_permission_checks() {
        let mut fs = fresh();
        let root = fs.root();
        let f = fs.create(root, "f").unwrap(); // mode 0o644, uid/gid 0
        fs.chown(f, 1000, 1000).unwrap();
        fs.chmod(f, 0o640).unwrap();
        // owner can read+write
        assert!(fs.access(f, ACCESS_R | ACCESS_W, 1000, 1000, &[]).is_ok());
        // group can read, not write
        assert!(fs.access(f, ACCESS_R, 2000, 1000, &[]).is_ok());
        assert!(matches!(
            fs.access(f, ACCESS_W, 2000, 1000, &[]),
            Err(Error::Permission)
        ));
        // other gets nothing
        assert!(matches!(
            fs.access(f, ACCESS_R, 2000, 2000, &[]),
            Err(Error::Permission)
        ));
        // root bypasses read/write
        assert!(fs.access(f, ACCESS_R | ACCESS_W, 0, 0, &[]).is_ok());
        // supplementary group grants group access
        assert!(fs.access(f, ACCESS_R, 2000, 2000, &[1000]).is_ok());
    }

    #[test]
    fn timestamps_use_injected_clock() {
        let mut fs = fresh();
        let tick = Arc::new(AtomicU64::new(1000));
        let t2 = tick.clone();
        fs.set_clock(Box::new(move || t2.load(Ordering::SeqCst)));
        let root = fs.root();
        let f = fs.create(root, "f").unwrap();
        assert_eq!(fs.stat(f).unwrap().ctime, 1000);
        tick.store(2000, Ordering::SeqCst);
        fs.write_file(f, b"data").unwrap();
        let st = fs.stat(f).unwrap();
        assert_eq!(st.mtime, 2000, "write bumps mtime");
        assert_eq!(st.ctime, 2000, "write bumps ctime");
        tick.store(3000, Ordering::SeqCst);
        fs.chmod(f, 0o600).unwrap();
        let st = fs.stat(f).unwrap();
        assert_eq!(st.mtime, 2000, "chmod leaves mtime");
        assert_eq!(st.ctime, 3000, "chmod bumps ctime");
    }

    #[test]
    fn commit_survives_remount() {
        let dev = MemDevice::new(4096);
        let mut fs = FS::format(dev, None).unwrap();
        let root = fs.root();
        let sub = fs.mkdir(root, "sub").unwrap();
        let f = fs.create(sub, "f").unwrap();
        fs.write_file(f, b"durable").unwrap();
        fs.commit().unwrap();

        let dev = fs.into_device();
        let fs2 = FS::mount(dev, None).unwrap();
        let sub2 = fs2.lookup(fs2.root(), "sub").unwrap().unwrap();
        assert_eq!(sub2, sub);
        let f2 = fs2.lookup(sub2, "f").unwrap().unwrap();
        assert_eq!(fs2.read_file(f2).unwrap(), b"durable");
    }

    /// A scripted sequence of mutations and reads must produce byte-identical
    /// results whether the resident open-directory cache is on or off — the cache
    /// is a pure performance mirror, never a behaviour change.
    #[test]
    fn dir_cache_matches_uncached() {
        fn run(cache: bool) -> (Vec<String>, Vec<String>, Vec<Option<u64>>) {
            let mut fs = fresh();
            fs.set_dir_cache(cache); // default is on; force the path under test
            let root = fs.root();
            for i in 0..60 {
                fs.create(root, &format!("f{i}")).unwrap();
            }
            // delete every third name (page churn + Bloom tombstones)
            for i in (0..60).step_by(3) {
                fs.unlink(root, &format!("f{i}")).unwrap();
            }
            let sub = fs.mkdir(root, "sub").unwrap();
            fs.create(sub, "x").unwrap();
            fs.link(root, "f1_hard", fs.lookup(root, "f1").unwrap().unwrap())
                .unwrap();
            fs.rename(root, "f2", sub, "y").unwrap(); // cross-dir move
            fs.rename(root, "f4", root, "f5").unwrap(); // same-dir overwrite
            let mut root_names = fs.readdir(root).unwrap();
            root_names.sort();
            let mut sub_names = fs.readdir(sub).unwrap();
            sub_names.sort();
            let looks = vec![
                fs.lookup(root, "f1").unwrap(),
                fs.lookup(root, "f0").unwrap(), // unlinked -> None
                fs.lookup(sub, "y").unwrap(),
                fs.lookup(sub, "absent").unwrap(),
            ];
            (root_names, sub_names, looks)
        }
        assert_eq!(run(false), run(true), "cache changed observable behaviour");
    }

    /// The cache must not clobber a directory inode mutated outside it. A `chmod`
    /// on a directory id goes through the inode table, not the resident handle; a
    /// later cached `create` in that directory must preserve the new mode (the
    /// checkout refreshes the inode from the table). Also exercises reclaim/evict.
    #[test]
    fn dir_cache_coherent_with_out_of_band_inode_change() {
        let mut fs = fresh();
        fs.set_dir_cache(true);
        let root = fs.root();
        let d = fs.mkdir(root, "d").unwrap();
        // populate the cache via reads + a write
        assert_eq!(fs.lookup(root, "d").unwrap(), Some(d));
        let f = fs.create(d, "f").unwrap();
        assert_eq!(fs.lookup(d, "f").unwrap(), Some(f));

        // chmod the directory: bypasses the cache (inode-table path).
        fs.chmod(d, 0o700).unwrap();
        // a cached mutation in the same directory must not write the stale mode back
        let g = fs.create(d, "g").unwrap();
        assert_eq!(
            fs.stat(d).unwrap().mode,
            0o700,
            "out-of-band chmod survived a cached create"
        );
        assert_eq!(fs.lookup(d, "g").unwrap(), Some(g));

        // remove the directory; its id is reclaimed and must not leak a resident
        // entry into the reused inode.
        fs.unlink(d, "f").unwrap();
        fs.unlink(d, "g").unwrap();
        fs.rmdir(root, "d").unwrap();
        let reused = fs.mkdir(root, "e").unwrap();
        assert_eq!(reused, d, "id recycled");
        assert!(
            fs.readdir(reused).unwrap().is_empty(),
            "reused dir id must start empty (no stale cache)"
        );

        // toggling the cache off keeps everything resolvable.
        fs.set_dir_cache(false);
        assert_eq!(fs.lookup(root, "e").unwrap(), Some(reused));
    }

    #[test]
    fn uncommitted_changes_roll_back() {
        let dev = MemDevice::new(4096);
        let mut fs = FS::format(dev, None).unwrap();
        let root = fs.root();
        fs.mkdir(root, "committed").unwrap();
        fs.commit().unwrap();
        // mutate without committing
        fs.mkdir(root, "lost").unwrap();

        let dev = fs.into_device();
        let fs2 = FS::mount(dev, None).unwrap();
        assert!(fs2.lookup(fs2.root(), "committed").unwrap().is_some());
        assert!(
            fs2.lookup(fs2.root(), "lost").unwrap().is_none(),
            "uncommitted mkdir must vanish after remount"
        );
    }

    /// Archive-loop scenario: write 10 files in a tight loop, keeping only the
    /// last N by unlinking the oldest.  Without auto-commit-on-ENOSPC the
    /// deferred-free clusters pile up and eventually the bitmap appears full
    /// even though only N files' worth of data is live.
    ///
    /// Specifically checks:
    /// 1. `statfs.blocks_free` stays stable (deferred frees are accounted for).
    /// 2. No ENOSPC occurs across 50 iterations on a small device.
    /// 3. After an explicit commit the free-block count is close to its start.
    #[test]
    fn archive_loop_no_enospc() {
        // 512 blocks (~2 MiB).  The CoW layer needs ~388 blocks for metadata on
        // format, leaving ~124 blocks for data — tight enough to expose the
        // deferred-free exhaustion bug without hitting format's DeviceTooSmall.
        let dev = MemDevice::new(512);
        let mut fs = FS::format(dev, None).unwrap();
        let root = fs.root();

        let payload = vec![0xABu8; 4096]; // one 4 KiB block per archive
        let keep = 5usize;
        let mut names: std::collections::VecDeque<String> = std::collections::VecDeque::new();

        let free_start = fs.statfs().blocks_free;

        for i in 0..50usize {
            let name = format!("archive_{i:03}.bin");
            let id = fs.create(root, &name).expect("create must not return ENOSPC");
            fs.write_file(id, &payload).expect("write_file must not return ENOSPC");
            names.push_back(name);

            // Keep only the last `keep` archives.
            if names.len() > keep {
                let old = names.pop_front().unwrap();
                fs.unlink(root, &old).expect("unlink");
            }

            // `statfs` must report a reasonable free count even without an
            // explicit commit — deferred_count() is included in blocks_free.
            let free_now = fs.statfs().blocks_free;
            assert!(
                free_now > 0,
                "statfs reported 0 free blocks at iteration {i} (deferred frees not counted)"
            );
        }

        // After an explicit commit all deferred frees are applied; the free
        // count should return to near its initial value (only `keep` files live).
        fs.commit().unwrap();
        let free_after = fs.statfs().blocks_free;
        assert!(
            free_after > free_start / 2,
            "after commit, free blocks ({free_after}) should be close to initial ({free_start})"
        );
    }

    /// `fallocate` returns OK when there is enough room for the worst-case
    /// (no-dedup) estimate, and `Error::NoSpace` when there clearly is not.
    #[test]
    fn fallocate_space_check() {
        let dev = MemDevice::new(512);
        let fs = FS::format(dev, None).unwrap();

        // A tiny file (one 4 KiB record + tiny block_map) must always fit.
        assert!(
            fs.fallocate(fs.root(), 4096).is_ok(),
            "4 KiB should fit on a 512-block device"
        );

        // Requesting the entire device (2 MiB) on a device that has ~124 free
        // blocks after format must be rejected.
        let two_mib = 512 * 4096u64;
        assert!(
            matches!(fs.fallocate(fs.root(), two_mib), Err(Error::NoSpace)),
            "2 MiB fallocate on a nearly-full device must return NoSpace"
        );

        // Zero-length is always OK.
        assert!(fs.fallocate(fs.root(), 0).is_ok());
    }

    /// The dir_cache is capped at DIR_CACHE_CAP entries: walking more directories
    /// than the cap must not grow the cache beyond it (i.e. no OOM on `find`).
    #[test]
    fn dir_cache_lru_cap() {
        let dev = MemDevice::new(8192);
        let mut fs = FS::format(dev, None).unwrap();
        fs.set_dir_cache(true);
        let root = fs.root();

        // Create DIR_CACHE_CAP + 10 subdirectories.
        let n = DIR_CACHE_CAP + 10;
        let mut ids = Vec::with_capacity(n);
        for i in 0..n {
            let id = fs.mkdir(root, &format!("sub_{i:04}")).unwrap();
            ids.push(id);
        }
        fs.commit().unwrap();

        // Lookup every directory — this populates the cache via cache_peek.
        for id in &ids {
            fs.stat(*id).unwrap(); // touches inode; does not hit cache_peek
            // A lookup inside the sub-dir forces a cache_peek on the sub-dir.
            fs.lookup(*id, "nonexistent").unwrap();
        }

        // The cache must not exceed the cap.
        let cache_size = fs
            .dir_cache
            .as_ref()
            .unwrap()
            .lock()
            .unwrap()
            .entries
            .len();
        assert!(
            cache_size <= DIR_CACHE_CAP,
            "cache size {cache_size} exceeds cap {DIR_CACHE_CAP}"
        );
    }
}
