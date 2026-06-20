//! The 128-byte BloomFS inode (SPEC §4.1) and its fixed little-endian on-disk
//! encoding, plus a RAM table that is the Copy-on-Write unit for metadata.
//!
//! The Rust struct is the in-memory form only. The on-disk form is the explicit
//! encoding produced by [`Inode::marshal_into`] — struct memory layout is NOT
//! used on disk (it is neither stable nor portable, §B15).

use std::fmt;

/// On-disk inode size in bytes (§4.1): 32 per 4 KiB block, and a 128-byte cache
/// line on Apple Silicon.
pub const SIZE: usize = 128;

/// How many inodes fit in one block.
pub const PER_BLOCK: usize = bloomfs_block::SIZE / SIZE; // 32

// Compile-time guarantee that inodes pack a block exactly (no slack, no
// overflow).
const _: () = assert!(PER_BLOCK * SIZE == bloomfs_block::SIZE);

/// File type codes (§4.1).
pub const TYPE_REGULAR: u8 = 0;
pub const TYPE_DIR: u8 = 1;
pub const TYPE_LINK: u8 = 2;

/// Block map is inline (else `block_map` holds a pointer).
pub const FLAG_INLINE_EXTENTS: u8 = 1 << 0;
pub const FLAG_COMPRESSED: u8 = 1 << 1;
pub const FLAG_ENCRYPTED: u8 = 1 << 2;

const BLOCK_MAP_BYTES: usize = 64;
const EXTENT_SIZE: usize = 16;

/// How many inline extents fit in the block-map area.
pub const INLINE_CAP: usize = BLOCK_MAP_BYTES / EXTENT_SIZE; // 4

/// Errors returned by the inode layer.
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// A buffer passed to decode was not exactly [`SIZE`] bytes (or a table
    /// snapshot was not a whole number of inodes).
    BadInodeSize,
    /// [`Inode::set_extents`] was given more than [`INLINE_CAP`] extents.
    TooManyExtents,
    /// An inode id (or snapshot inode count) outside the table capacity.
    OutOfRange,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::BadInodeSize => f.write_str("inode: encoded inode must be exactly 128 bytes"),
            Error::TooManyExtents => f.write_str("inode: too many inline extents"),
            Error::OutOfRange => f.write_str("inode: id out of range"),
        }
    }
}

impl std::error::Error for Error {}

/// A `Result` specialized to the inode layer's [`Error`].
pub type Result<T> = std::result::Result<T, Error>;

/// A run of contiguous clusters (§4.4/§4.5).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Extent {
    /// First cluster (block) index.
    pub start: u64,
    /// Number of clusters in the run.
    pub blocks: u32,
    /// Logical (uncompressed) bytes the run represents.
    pub logical: u32,
}

/// The in-memory metadata record. It never stores the file name (§4.2).
///
/// On-disk layout (little-endian, 128 bytes). UID/GID are u32 and Mode is u16
/// so real POSIX ids (>255) and full permission bits (>0o377) fit (§4.1):
///
/// ```text
/// 0   size        u64
/// 8   nlink       u32
/// 12  generation  u32
/// 16  uid         u32
/// 20  gid         u32
/// 24  atime       u64
/// 32  mtime       u64
/// 40  ctime       u64
/// 48  mode        u16   permission/setuid/sticky bits (type is in `kind`)
/// 50  kind        u8
/// 51  record_size_log2  u8
/// 52  flags       u8
/// 53  reserved    [3]
/// 56  checksum    [8]
/// 64  block_map   [64]  inline extents or external pointer
/// ```
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Inode {
    pub size: u64,
    pub nlink: u32,
    pub generation: u32,
    pub uid: u32,
    pub gid: u32,
    pub atime: u64,
    pub mtime: u64,
    pub ctime: u64,
    pub mode: u16,
    /// File type code (the on-disk Type byte at offset 50).
    pub kind: u8,
    pub record_size_log2: u8,
    pub flags: u8,
    pub checksum: [u8; 8],
    pub block_map: [u8; BLOCK_MAP_BYTES],
}

impl Default for Inode {
    fn default() -> Self {
        Inode {
            size: 0,
            nlink: 0,
            generation: 0,
            uid: 0,
            gid: 0,
            atime: 0,
            mtime: 0,
            ctime: 0,
            mode: 0,
            kind: 0,
            record_size_log2: 0,
            flags: 0,
            checksum: [0; 8],
            block_map: [0; BLOCK_MAP_BYTES],
        }
    }
}

impl Inode {
    /// Write this inode's [`SIZE`]-byte encoding into `dst[..SIZE]`. The caller
    /// owns `dst` (it must be at least [`SIZE`] bytes), so the hot commit path
    /// can serialize the whole table into one reusable buffer with no per-inode
    /// allocation.
    pub fn marshal_into(&self, dst: &mut [u8]) {
        dst[0..8].copy_from_slice(&self.size.to_le_bytes());
        dst[8..12].copy_from_slice(&self.nlink.to_le_bytes());
        dst[12..16].copy_from_slice(&self.generation.to_le_bytes());
        dst[16..20].copy_from_slice(&self.uid.to_le_bytes());
        dst[20..24].copy_from_slice(&self.gid.to_le_bytes());
        dst[24..32].copy_from_slice(&self.atime.to_le_bytes());
        dst[32..40].copy_from_slice(&self.mtime.to_le_bytes());
        dst[40..48].copy_from_slice(&self.ctime.to_le_bytes());
        dst[48..50].copy_from_slice(&self.mode.to_le_bytes());
        dst[50] = self.kind;
        dst[51] = self.record_size_log2;
        dst[52] = self.flags;
        // reserved — written explicitly so a reused buffer carries no stale bytes
        dst[53] = 0;
        dst[54] = 0;
        dst[55] = 0;
        dst[56..64].copy_from_slice(&self.checksum);
        dst[64..128].copy_from_slice(&self.block_map);
    }

    /// Encode the inode to exactly [`SIZE`] bytes.
    pub fn marshal_binary(&self) -> [u8; SIZE] {
        let mut b = [0u8; SIZE];
        self.marshal_into(&mut b);
        b
    }

    /// Decode exactly [`SIZE`] bytes into `self`.
    pub fn unmarshal_binary(&mut self, b: &[u8]) -> Result<()> {
        if b.len() != SIZE {
            return Err(Error::BadInodeSize);
        }
        self.size = u64::from_le_bytes(b[0..8].try_into().unwrap());
        self.nlink = u32::from_le_bytes(b[8..12].try_into().unwrap());
        self.generation = u32::from_le_bytes(b[12..16].try_into().unwrap());
        self.uid = u32::from_le_bytes(b[16..20].try_into().unwrap());
        self.gid = u32::from_le_bytes(b[20..24].try_into().unwrap());
        self.atime = u64::from_le_bytes(b[24..32].try_into().unwrap());
        self.mtime = u64::from_le_bytes(b[32..40].try_into().unwrap());
        self.ctime = u64::from_le_bytes(b[40..48].try_into().unwrap());
        self.mode = u16::from_le_bytes(b[48..50].try_into().unwrap());
        self.kind = b[50];
        self.record_size_log2 = b[51];
        self.flags = b[52];
        self.checksum.copy_from_slice(&b[56..64]);
        self.block_map.copy_from_slice(&b[64..128]);
        Ok(())
    }

    /// Decode a fresh inode from exactly [`SIZE`] bytes.
    pub fn from_bytes(b: &[u8]) -> Result<Inode> {
        let mut in_ = Inode::default();
        in_.unmarshal_binary(b)?;
        Ok(in_)
    }

    /// Encode up to [`INLINE_CAP`] extents into the inline block map and set
    /// [`FLAG_INLINE_EXTENTS`]. A zero-`blocks` extent terminates the list on
    /// decode, so do not pass extents with `blocks == 0`.
    pub fn set_extents(&mut self, exts: &[Extent]) -> Result<()> {
        if exts.len() > INLINE_CAP {
            return Err(Error::TooManyExtents);
        }
        let mut bm = [0u8; BLOCK_MAP_BYTES];
        for (i, e) in exts.iter().enumerate() {
            let off = i * EXTENT_SIZE;
            bm[off..off + 8].copy_from_slice(&e.start.to_le_bytes());
            bm[off + 8..off + 12].copy_from_slice(&e.blocks.to_le_bytes());
            bm[off + 12..off + 16].copy_from_slice(&e.logical.to_le_bytes());
        }
        self.block_map = bm;
        self.flags |= FLAG_INLINE_EXTENTS;
        Ok(())
    }

    /// Decode the inline extents (a zero-`blocks` entry terminates). Only
    /// meaningful when [`FLAG_INLINE_EXTENTS`] is set.
    pub fn extents(&self) -> Vec<Extent> {
        let mut out = Vec::new();
        for i in 0..INLINE_CAP {
            let off = i * EXTENT_SIZE;
            let blocks = u32::from_le_bytes(self.block_map[off + 8..off + 12].try_into().unwrap());
            if blocks == 0 {
                break;
            }
            out.push(Extent {
                start: u64::from_le_bytes(self.block_map[off..off + 8].try_into().unwrap()),
                blocks,
                logical: u32::from_le_bytes(self.block_map[off + 12..off + 16].try_into().unwrap()),
            });
        }
        out
    }
}

/// The in-RAM inode table — the Copy-on-Write unit for metadata (§B1). All
/// get/put are pure RAM operations; the table is made durable only when the
/// filesystem serializes it into a CoW metadata snapshot and flips the
/// uberblock. Because nothing is written in place, an uncommitted change
/// vanishes after a crash and a committed change is atomic (§E).
///
/// Slots are dense and indexed by id. `count` is the high-water mark: ids in
/// `[0, count)` have been touched (the bump allocator never reuses an id), so
/// only those are serialized — an empty filesystem snapshots zero bytes.
pub struct Table {
    inodes: Vec<Inode>,
    count: u64,
}

impl Table {
    /// Return an empty table with room for `capacity` inodes.
    pub fn new(capacity: u64) -> Self {
        Table {
            inodes: vec![Inode::default(); capacity as usize],
            count: 0,
        }
    }

    /// The table capacity (number of inode slots).
    pub fn cap(&self) -> u64 {
        self.inodes.len() as u64
    }

    /// The high-water mark: the number of leading slots that have been written
    /// and are therefore serialized by [`marshal`](Table::marshal).
    pub fn count(&self) -> u64 {
        self.count
    }

    /// Return a copy of inode `id`. A copy (not a reference into the table)
    /// keeps the "mutate then put" contract: a caller cannot change persisted
    /// state without going through [`put`](Table::put).
    pub fn get(&self, id: u64) -> Result<Inode> {
        if id >= self.inodes.len() as u64 {
            return Err(Error::OutOfRange);
        }
        Ok(self.inodes[id as usize].clone())
    }

    /// Store inode `id`, extending the high-water mark if needed.
    pub fn put(&mut self, id: u64, in_: &Inode) -> Result<()> {
        if id >= self.inodes.len() as u64 {
            return Err(Error::OutOfRange);
        }
        self.inodes[id as usize] = in_.clone();
        if id >= self.count {
            self.count = id + 1;
        }
        Ok(())
    }

    /// Report how many bytes [`marshal`](Table::marshal) /
    /// [`marshal_into`](Table::marshal_into) will write: the leading `count`
    /// slots at [`SIZE`] bytes each.
    pub fn marshal_len(&self) -> usize {
        self.count as usize * SIZE
    }

    /// Serialize slots `[0, count)` into `dst` (at least
    /// [`marshal_len`](Table::marshal_len) bytes) and return the bytes written.
    /// Allocates nothing — the commit path reuses one buffer across snapshots.
    pub fn marshal_into(&self, dst: &mut [u8]) -> usize {
        for (i, inode) in self.inodes[..self.count as usize].iter().enumerate() {
            inode.marshal_into(&mut dst[i * SIZE..(i + 1) * SIZE]);
        }
        self.count as usize * SIZE
    }

    /// Serialize slots `[0, count)` as `count * SIZE` bytes.
    pub fn marshal(&self) -> Vec<u8> {
        let mut buf = vec![0u8; self.marshal_len()];
        self.marshal_into(&mut buf);
        buf
    }

    /// Reconstruct a table of the given `capacity` from a snapshot produced by
    /// [`marshal`](Table::marshal). The snapshot length must be a whole number
    /// of inodes and must not exceed `capacity`.
    pub fn unmarshal_table(b: &[u8], capacity: u64) -> Result<Table> {
        if !b.len().is_multiple_of(SIZE) {
            return Err(Error::BadInodeSize);
        }
        let n = b.len() / SIZE;
        if n as u64 > capacity {
            return Err(Error::OutOfRange);
        }
        let mut t = Table::new(capacity);
        for (i, slot) in t.inodes[..n].iter_mut().enumerate() {
            slot.unmarshal_binary(&b[i * SIZE..(i + 1) * SIZE])?;
        }
        t.count = n as u64;
        Ok(t)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample() -> Inode {
        Inode {
            size: 123456,
            nlink: 2,
            generation: 7,
            uid: 1000,  // > 255: would not survive a u8 field (§4.1)
            gid: 65534, // full POSIX mode bits (> 0o377 needs > u8)
            kind: TYPE_REGULAR,
            mode: 0o644,
            record_size_log2: 15, // 32 KiB recordsize (§4.5)
            flags: FLAG_COMPRESSED | FLAG_ENCRYPTED,
            atime: 1000,
            mtime: 2000,
            ctime: 3000,
            checksum: [1, 2, 3, 4, 5, 6, 7, 8],
            ..Default::default()
        }
    }

    #[test]
    fn marshal_size() {
        assert_eq!(sample().marshal_binary().len(), SIZE);
    }

    #[test]
    fn round_trip() {
        let mut in_ = sample();
        in_.set_extents(&[
            Extent {
                start: 100,
                blocks: 8,
                logical: 32768,
            },
            Extent {
                start: 200,
                blocks: 3,
                logical: 12000,
            },
        ])
        .unwrap();

        let b = in_.marshal_binary();
        let got = Inode::from_bytes(&b).unwrap();
        assert_eq!(got, in_, "round-trip mismatch");

        let exts = got.extents();
        assert_eq!(exts.len(), 2);
        assert_eq!(exts[0].start, 100);
        assert_eq!(exts[1].blocks, 3);
        assert_ne!(got.flags & FLAG_INLINE_EXTENTS, 0, "FlagInlineExtents set");
    }

    #[test]
    fn unmarshal_bad_size() {
        let mut in_ = Inode::default();
        assert_eq!(
            in_.unmarshal_binary(&[0u8; SIZE - 1]),
            Err(Error::BadInodeSize)
        );
    }

    #[test]
    fn too_many_extents() {
        let mut in_ = sample();
        let too: Vec<Extent> = (0..=INLINE_CAP)
            .map(|i| Extent {
                start: (i + 1) as u64,
                blocks: 1,
                logical: 1,
            })
            .collect();
        assert_eq!(in_.set_extents(&too), Err(Error::TooManyExtents));
    }

    // Independent slots round-trip and out-of-range ids fail.
    #[test]
    fn table_put_get() {
        let mut tbl = Table::new(64);

        let ids = [0u64, 1, 31, 32, 63];
        for &id in &ids {
            let in_ = Inode {
                size: id * 1000,
                nlink: 1,
                uid: id as u32,
                ..Default::default()
            };
            tbl.put(id, &in_).unwrap();
        }
        for &id in &ids {
            let got = tbl.get(id).unwrap();
            assert_eq!(got.size, id * 1000);
            assert_eq!(got.uid, id as u32);
        }

        // Rewriting one slot leaves a neighbour untouched.
        tbl.put(
            0,
            &Inode {
                size: 999,
                uid: 200,
                ..Default::default()
            },
        )
        .unwrap();
        let mate = tbl.get(1).unwrap();
        assert_eq!(mate.size, 1000);
        assert_eq!(mate.uid, 1);

        assert_eq!(tbl.get(64), Err(Error::OutOfRange));
        assert_eq!(tbl.put(64, &Inode::default()), Err(Error::OutOfRange));
    }

    // Marshal only covers touched slots.
    #[test]
    fn table_high_water_mark() {
        let mut tbl = Table::new(1024);
        assert_eq!(tbl.marshal().len(), 0, "empty table marshals 0 bytes");
        tbl.put(
            4,
            &Inode {
                nlink: 1,
                ..Default::default()
            },
        )
        .unwrap();
        assert_eq!(tbl.count(), 5, "high-water mark id 4");
        assert_eq!(tbl.marshal().len(), 5 * SIZE);
    }

    // A table survives marshal -> unmarshal_table.
    #[test]
    fn table_marshal_round_trip() {
        let mut tbl = Table::new(256);
        for id in 0..100u64 {
            tbl.put(
                id,
                &Inode {
                    size: id,
                    nlink: id as u32 + 1,
                    kind: TYPE_REGULAR,
                    uid: id as u32,
                    ..Default::default()
                },
            )
            .unwrap();
        }
        let blob = tbl.marshal();

        let got = Table::unmarshal_table(&blob, 256).unwrap();
        assert_eq!(got.count(), tbl.count());
        assert_eq!(got.cap(), tbl.cap());
        assert_eq!(got.marshal(), blob, "round-tripped table matches snapshot");

        assert!(
            matches!(
                Table::unmarshal_table(&[0u8; SIZE + 1], 256),
                Err(Error::BadInodeSize)
            ),
            "ragged snapshot"
        );
        assert!(
            matches!(
                Table::unmarshal_table(&[0u8; 4 * SIZE], 2),
                Err(Error::OutOfRange)
            ),
            "oversized snapshot"
        );
    }
}
