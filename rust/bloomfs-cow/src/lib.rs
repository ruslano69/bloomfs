//! The Copy-on-Write durability layer (SPEC §B1, §D-1). The commit root is an
//! "uberblock" stored in two alternating slots. A transaction writes new metadata
//! — the allocator bitmap, the dedup table AND the inode table, as one
//! `[bitmap | ddt | inode]` snapshot — to the inactive metadata slot, then writes
//! a new uberblock (higher sequence + content checksum) to the inactive uberblock
//! slot. That final block write is the atomic flip: a crash before it leaves the
//! previous uberblock and the metadata it points to fully intact, so mount rolls
//! back to the last consistent commit. Live metadata is never overwritten in place.
//!
//! Port note: the Go prototype checksums the uberblock with BLAKE2b; this port
//! uses BLAKE3 (faster, no separate "256" variant). The image format is therefore
//! NOT compatible with a Go-formatted image — the port is fresh-format-only, as
//! the prototype carries no on-disk compatibility guarantee.

use std::fmt;

use bloomfs_alloc::Bitmap;
use bloomfs_block::Device;
use bloomfs_dedup::Table as DedupTable;
use bloomfs_inode::Table as InodeTable;

/// Block size as `u64`, for geometry arithmetic.
const BLK: u64 = bloomfs_block::SIZE as u64;

/// Identifies an uberblock (a fixed, arbitrary 64-bit tag).
const UBER_MAGIC: u64 = 0x00B1_00F5_C0FF_EE01;
/// Where the BLAKE3 checksum of the preceding bytes lives.
const UBER_CHECKSUM_OFF: usize = 104;
/// The two ping-pong uberblock blocks.
pub const UBER_SLOT0: u64 = 0;
pub const UBER_SLOT1: u64 = 1;

/// Errors returned by the CoW layer.
#[derive(Debug)]
pub enum Error {
    /// No valid uberblock (not formatted or fully corrupt).
    NotFormatted,
    /// An uberblock slot has bad magic or a failed checksum (torn write).
    BadUber,
    /// The metadata snapshot does not fit the metadata slot.
    MetaTooBig,
    /// The device cannot hold the computed geometry.
    DeviceTooSmall {
        /// Blocks the layout needs (data must start past this).
        need: u64,
        /// Blocks the device actually has.
        have: u64,
    },
    /// An error from the block layer.
    Block(bloomfs_block::Error),
    /// An error from the allocator.
    Alloc(bloomfs_alloc::Error),
    /// An error from the dedup table.
    Dedup(bloomfs_dedup::Error),
    /// An error from the inode table.
    Inode(bloomfs_inode::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::NotFormatted => {
                f.write_str("cow: no valid uberblock (not formatted or fully corrupt)")
            }
            Error::BadUber => f.write_str("cow: uberblock invalid (bad magic or checksum)"),
            Error::MetaTooBig => f.write_str("cow: metadata snapshot exceeds the metadata slot"),
            Error::DeviceTooSmall { need, have } => {
                write!(
                    f,
                    "cow: device too small: need > {need} blocks, have {have}"
                )
            }
            Error::Block(e) => write!(f, "cow: {e}"),
            Error::Alloc(e) => write!(f, "cow: load bitmap: {e}"),
            Error::Dedup(e) => write!(f, "cow: load dedup table: {e}"),
            Error::Inode(e) => write!(f, "cow: load inode table: {e}"),
        }
    }
}

impl std::error::Error for Error {}

impl From<bloomfs_block::Error> for Error {
    fn from(e: bloomfs_block::Error) -> Self {
        Error::Block(e)
    }
}
impl From<bloomfs_alloc::Error> for Error {
    fn from(e: bloomfs_alloc::Error) -> Self {
        Error::Alloc(e)
    }
}
impl From<bloomfs_dedup::Error> for Error {
    fn from(e: bloomfs_dedup::Error) -> Self {
        Error::Dedup(e)
    }
}
impl From<bloomfs_inode::Error> for Error {
    fn from(e: bloomfs_inode::Error) -> Self {
        Error::Inode(e)
    }
}

/// A `Result` specialized to the CoW layer's [`Error`].
pub type Result<T> = std::result::Result<T, Error>;

/// The self-describing commit root: geometry + which metadata slot holds this
/// commit's snapshot + a checksum that detects a torn write. The active metadata
/// slot is one CoW snapshot laid out as `[bitmap | dedup-table | inode-table]`,
/// whose three lengths are recorded here. The inode table living here (not in a
/// fixed in-place region) is what makes metadata mutations crash-atomic.
#[derive(Debug, Clone)]
pub struct Uberblock {
    pub magic: u64,
    /// Commit sequence; highest valid wins.
    pub seq: u64,
    pub block_size: u32,
    /// 0 => snapshot in `meta_a`, 1 => `meta_b`.
    pub active_meta: u8,
    pub total_blocks: u64,
    /// First block of metadata slot A.
    pub meta_a: u64,
    /// First block of metadata slot B.
    pub meta_b: u64,
    /// Size of each metadata slot, in blocks.
    pub meta_blocks: u64,
    /// Inode-table capacity (number of slots).
    pub inode_count: u64,
    pub data_start: u64,
    pub root_inode: u64,
    /// Next free inode id (bump-allocator high-water mark).
    pub next_inode: u64,
    /// Bytes of bitmap snapshot in the active metadata slot.
    pub bitmap_len: u32,
    /// Bytes of dedup-table snapshot, following the bitmap.
    pub ddt_len: u32,
    /// Bytes of inode-table snapshot, following the dedup table.
    pub inode_len: u32,
}

impl Uberblock {
    /// Encode into a full block with a trailing BLAKE3 checksum.
    pub fn marshal_binary(&self) -> [u8; bloomfs_block::SIZE] {
        let mut b = [0u8; bloomfs_block::SIZE];
        b[0..8].copy_from_slice(&self.magic.to_le_bytes());
        b[8..16].copy_from_slice(&self.seq.to_le_bytes());
        b[16..20].copy_from_slice(&self.block_size.to_le_bytes());
        b[20] = self.active_meta;
        b[24..32].copy_from_slice(&self.total_blocks.to_le_bytes());
        b[32..40].copy_from_slice(&self.meta_a.to_le_bytes());
        b[40..48].copy_from_slice(&self.meta_b.to_le_bytes());
        b[48..56].copy_from_slice(&self.meta_blocks.to_le_bytes());
        b[56..64].copy_from_slice(&self.inode_count.to_le_bytes());
        b[64..72].copy_from_slice(&self.data_start.to_le_bytes());
        b[72..80].copy_from_slice(&self.root_inode.to_le_bytes());
        b[80..88].copy_from_slice(&self.next_inode.to_le_bytes());
        b[88..92].copy_from_slice(&self.bitmap_len.to_le_bytes());
        b[92..96].copy_from_slice(&self.ddt_len.to_le_bytes());
        b[96..100].copy_from_slice(&self.inode_len.to_le_bytes());
        let sum = blake3::hash(&b[..UBER_CHECKSUM_OFF]);
        b[UBER_CHECKSUM_OFF..UBER_CHECKSUM_OFF + 32].copy_from_slice(sum.as_bytes());
        b
    }

    /// First block of the active metadata slot.
    fn meta_start(&self) -> u64 {
        if self.active_meta == 1 {
            self.meta_b
        } else {
            self.meta_a
        }
    }
}

/// Validate magic + checksum and decode an uberblock. A torn or corrupt slot
/// fails here, so mount ignores it and uses the other slot.
fn parse_uber(b: &[u8]) -> Result<Uberblock> {
    if b.len() < bloomfs_block::SIZE {
        return Err(Error::BadUber);
    }
    if u64::from_le_bytes(b[0..8].try_into().unwrap()) != UBER_MAGIC {
        return Err(Error::BadUber);
    }
    let sum = blake3::hash(&b[..UBER_CHECKSUM_OFF]);
    if sum.as_bytes()[..] != b[UBER_CHECKSUM_OFF..UBER_CHECKSUM_OFF + 32] {
        return Err(Error::BadUber);
    }
    Ok(Uberblock {
        magic: UBER_MAGIC,
        seq: u64::from_le_bytes(b[8..16].try_into().unwrap()),
        block_size: u32::from_le_bytes(b[16..20].try_into().unwrap()),
        active_meta: b[20],
        total_blocks: u64::from_le_bytes(b[24..32].try_into().unwrap()),
        meta_a: u64::from_le_bytes(b[32..40].try_into().unwrap()),
        meta_b: u64::from_le_bytes(b[40..48].try_into().unwrap()),
        meta_blocks: u64::from_le_bytes(b[48..56].try_into().unwrap()),
        inode_count: u64::from_le_bytes(b[56..64].try_into().unwrap()),
        data_start: u64::from_le_bytes(b[64..72].try_into().unwrap()),
        root_inode: u64::from_le_bytes(b[72..80].try_into().unwrap()),
        next_inode: u64::from_le_bytes(b[80..88].try_into().unwrap()),
        bitmap_len: u32::from_le_bytes(b[88..92].try_into().unwrap()),
        ddt_len: u32::from_le_bytes(b[92..96].try_into().unwrap()),
        inode_len: u32::from_le_bytes(b[96..100].try_into().unwrap()),
    })
}

/// Lay out a fresh image on `dev` with `inode_count` inode slots and `ddt_reserve`
/// bytes of headroom for the dedup-table snapshot, then write the first commit
/// (seq 1). The metadata slot is sized to hold the bitmap, the dedup-table reserve
/// and a full inode table at once.
pub fn format<D: Device>(dev: &D, inode_count: u64, ddt_reserve: u64) -> Result<Uberblock> {
    let total = dev.blocks();
    let inode_count = if inode_count == 0 {
        bloomfs_inode::PER_BLOCK as u64
    } else {
        inode_count
    };
    let bitmap_bytes = 8 + total.div_ceil(8);
    let inode_reserve = inode_count * bloomfs_inode::SIZE as u64;
    let meta_blocks = (64 + bitmap_bytes + ddt_reserve + inode_reserve).div_ceil(BLK);

    let meta_a = 2u64;
    let meta_b = meta_a + meta_blocks;
    let data_start = meta_b + meta_blocks;
    if total <= data_start {
        return Err(Error::DeviceTooSmall {
            need: data_start,
            have: total,
        });
    }

    let mut bm = Bitmap::new(total);
    bm.reserve(0, data_start); // uberblocks + both metadata slots are off-limits
    let ddt = DedupTable::new();
    let tbl = InodeTable::new(inode_count);

    let mut ub = Uberblock {
        magic: UBER_MAGIC,
        seq: 1,
        block_size: bloomfs_block::SIZE as u32,
        active_meta: 0,
        total_blocks: total,
        meta_a,
        meta_b,
        meta_blocks,
        inode_count,
        data_start,
        root_inode: 0,
        next_inode: 1, // inode 0 is the root directory
        bitmap_len: 0,
        ddt_len: 0,
        inode_len: 0,
    };

    write_snapshot(dev, &mut ub, &bm, &ddt, &tbl, None)?;
    write_uber(dev, &ub)?;
    Ok(ub)
}

/// Read both uberblock slots, pick the highest-sequence valid one, and load its
/// allocator bitmap, dedup table and inode table. A torn commit is invisible:
/// `parse_uber` rejects the corrupt slot, so the previous consistent commit wins.
pub fn mount<D: Device>(dev: &D) -> Result<(Uberblock, Bitmap, DedupTable, InodeTable)> {
    let mut best: Option<Uberblock> = None;
    for slot in [UBER_SLOT0, UBER_SLOT1] {
        let raw = dev.read_block(slot)?;
        if let Ok(ub) = parse_uber(&raw) {
            if best.as_ref().is_none_or(|b| ub.seq > b.seq) {
                best = Some(ub);
            }
        }
    }
    let best = best.ok_or(Error::NotFormatted)?;

    let blk = BLK as usize;
    let mut buf = vec![0u8; (best.meta_blocks * BLK) as usize];
    let start = best.meta_start();
    for i in 0..best.meta_blocks {
        let blkdata = dev.read_block(start + i)?;
        let off = i as usize * blk;
        buf[off..off + blk].copy_from_slice(&blkdata);
    }
    let bm_end = best.bitmap_len as usize;
    let dd_end = bm_end + best.ddt_len as usize;
    let in_end = dd_end + best.inode_len as usize;
    let bm = Bitmap::unmarshal(&buf[..bm_end])?;
    let ddt = DedupTable::unmarshal(&buf[bm_end..dd_end])?;
    let tbl = InodeTable::unmarshal_table(&buf[dd_end..in_end], best.inode_count)?;
    Ok((best, bm, ddt, tbl))
}

/// Atomically record a new consistent state: snapshot `bm`+`ddt`+`tbl` into the
/// inactive metadata slot (synced), then flip the uberblock (synced). On any
/// failure the previous commit remains valid; remounting rolls back to it.
///
/// `scratch` is an optional reusable serialization buffer (>= `meta_blocks*BLK`):
/// the hot path passes a persistent one so a commit allocates nothing.
#[allow(clippy::too_many_arguments)]
pub fn commit<D: Device>(
    dev: &D,
    prev: &Uberblock,
    bm: &mut Bitmap,
    ddt: &DedupTable,
    tbl: &InodeTable,
    root_inode: u64,
    next_inode: u64,
    scratch: Option<&mut [u8]>,
) -> Result<Uberblock> {
    let mut next = prev.clone(); // inherit geometry
    next.seq = prev.seq + 1;
    next.active_meta = 1 - prev.active_meta; // alternate metadata slot
    next.root_inode = root_inode;
    next.next_inode = next_inode;

    write_snapshot(dev, &mut next, bm, ddt, tbl, scratch)?;
    write_uber(dev, &next)?; // the atomic flip
                             // Only now that the commit is durable may clusters freed during this
                             // transaction be reused (§F1): a crash before the flip left them pinned.
    bm.apply_deferred();
    Ok(next)
}

/// Serialize `bm`+`ddt`+`tbl` into `ub`'s active metadata slot, recording the
/// three lengths on `ub`, and sync. The tail past their combined length is never
/// read on mount, so a reused scratch buffer with stale tail bytes is safe.
fn write_snapshot<D: Device>(
    dev: &D,
    ub: &mut Uberblock,
    bm: &Bitmap,
    ddt: &DedupTable,
    tbl: &InodeTable,
    scratch: Option<&mut [u8]>,
) -> Result<()> {
    let bm_len = bm.marshal_len();
    let dd_len = ddt.marshal_len();
    let in_len = tbl.marshal_len();
    let need = (ub.meta_blocks * BLK) as usize;
    if bm_len + dd_len + in_len > need {
        return Err(Error::MetaTooBig);
    }

    let mut owned: Vec<u8>;
    let buf: &mut [u8] = match scratch {
        Some(s) if s.len() >= need => s,
        _ => {
            owned = vec![0u8; need];
            &mut owned
        }
    };
    bm.marshal_into(&mut buf[..bm_len]);
    ddt.marshal_into(&mut buf[bm_len..bm_len + dd_len]);
    tbl.marshal_into(&mut buf[bm_len + dd_len..bm_len + dd_len + in_len]);
    ub.bitmap_len = bm_len as u32;
    ub.ddt_len = dd_len as u32;
    ub.inode_len = in_len as u32;

    let start = ub.meta_start();
    let blk = BLK as usize;
    for i in 0..ub.meta_blocks {
        let off = i as usize * blk;
        dev.write_block(start + i, &buf[off..off + blk])?;
    }
    dev.sync()?;
    Ok(())
}

/// Write `ub` to slot `seq % 2` and sync. Consecutive sequence numbers have
/// opposite parity, so a new commit never overwrites the slot holding the
/// previous one — that is what makes the flip safe.
fn write_uber<D: Device>(dev: &D, ub: &Uberblock) -> Result<()> {
    let b = ub.marshal_binary();
    dev.write_block(ub.seq % 2, &b)?;
    dev.sync()?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::Cell;

    use bloomfs_block::MemDevice;
    use bloomfs_dedup::{Entry, Key};

    /// Simulates power loss: the `fail_at`-th `write_block` corrupts its target's
    /// checksum region (a torn sector) and fails; every later write fails too.
    struct FlakyDevice<'a> {
        inner: &'a dyn Device,
        fail_at: usize,
        n: Cell<usize>,
    }

    impl Device for FlakyDevice<'_> {
        fn read_block(&self, num: u64) -> bloomfs_block::Result<Vec<u8>> {
            self.inner.read_block(num)
        }
        fn write_block(&self, num: u64, data: &[u8]) -> bloomfs_block::Result<()> {
            let n = self.n.get() + 1;
            self.n.set(n);
            if self.fail_at != 0 && n >= self.fail_at {
                if n == self.fail_at {
                    // Model a block that did not finish persisting: stored content
                    // and checksum no longer agree, so parse_uber rejects it.
                    let mut torn = data.to_vec();
                    let end = (UBER_CHECKSUM_OFF + 32).min(torn.len());
                    for byte in &mut torn[UBER_CHECKSUM_OFF..end] {
                        *byte ^= 0xFF;
                    }
                    let _ = self.inner.write_block(num, &torn);
                }
                return Err(bloomfs_block::Error::Io(std::io::Error::other(
                    "flaky: simulated power loss",
                )));
            }
            self.inner.write_block(num, data)
        }
        fn blocks(&self) -> u64 {
            self.inner.blocks()
        }
        fn sync(&self) -> bloomfs_block::Result<()> {
            self.inner.sync()
        }
    }

    fn dkey(b: u8) -> Key {
        let mut k = [0u8; 32];
        k[0] = b;
        k
    }

    /// One allocation + one dedup insert, mutating bm and ddt.
    fn activity(bm: &mut Bitmap, ddt: &mut DedupTable, id: u8, count: u64) {
        let start = bm.alloc(count).expect("alloc");
        ddt.add(
            dkey(id),
            Entry {
                start,
                count: count as u32,
                ..Default::default()
            },
        );
    }

    fn formatted() -> MemDevice {
        let dev = MemDevice::new(2048);
        format(&dev, 1000, 64 * 1024).expect("format");
        dev
    }

    #[test]
    fn format_mount() {
        let dev = formatted();
        let (ub, bm, ddt, _) = mount(&dev).expect("mount");
        assert_eq!(ub.seq, 1);
        assert_eq!(ddt.len(), 0, "fresh ddt");
        assert_eq!(bm.used(), ub.data_start, "reserved == data_start");
    }

    #[test]
    fn commit_persists() {
        let dev = formatted();
        let (ub, mut bm, mut ddt, tbl) = mount(&dev).unwrap();

        activity(&mut bm, &mut ddt, 1, 3);
        activity(&mut bm, &mut ddt, 2, 5);
        let (want_used, want_len) = (bm.used(), ddt.len());

        commit(&dev, &ub, &mut bm, &ddt, &tbl, 0, 1, None).expect("commit");

        let (ub2, bm2, ddt2, _) = mount(&dev).expect("remount");
        assert_eq!(ub2.seq, 2);
        assert_eq!(bm2.used(), want_used);
        assert_eq!(ddt2.len(), want_len);
    }

    /// Commit a seq-2 state and return its uberblock + recorded used/len so crash
    /// tests can assert rollback to it.
    fn commit_to_seq2(dev: &MemDevice) -> (Uberblock, u64, usize) {
        let (ub, mut bm, mut ddt, tbl) = mount(dev).unwrap();
        activity(&mut bm, &mut ddt, 1, 3);
        let (used, length) = (bm.used(), ddt.len());
        let ub2 = commit(dev, &ub, &mut bm, &ddt, &tbl, 0, 1, None).expect("seq2 commit");
        (ub2, used, length)
    }

    fn assert_rolled_back_to_seq2(dev: &MemDevice, want_used: u64, want_len: usize) {
        let (ub, bm, ddt, _) = mount(dev).expect("remount after crash");
        assert_eq!(ub.seq, 2, "rolled back to consistent seq 2");
        assert_eq!(bm.used(), want_used);
        assert_eq!(ddt.len(), want_len);
    }

    #[test]
    fn crash_during_metadata() {
        let dev = formatted();
        let (ub2, used2, len2) = commit_to_seq2(&dev);

        let (_, mut bm, mut ddt, tbl) = mount(&dev).unwrap();
        activity(&mut bm, &mut ddt, 1, 3);
        activity(&mut bm, &mut ddt, 2, 9);
        // Crash on the FIRST write of the next commit (mid-metadata, before flip).
        let flaky = FlakyDevice {
            inner: &dev,
            fail_at: 1,
            n: Cell::new(0),
        };
        assert!(
            commit(&flaky, &ub2, &mut bm, &ddt, &tbl, 0, 1, None).is_err(),
            "commit must fail mid-metadata"
        );

        assert_rolled_back_to_seq2(&dev, used2, len2);
    }

    #[test]
    fn crash_during_uberblock() {
        let dev = formatted();
        let (ub2, used2, len2) = commit_to_seq2(&dev);

        let (_, mut bm, mut ddt, tbl) = mount(&dev).unwrap();
        activity(&mut bm, &mut ddt, 2, 7);
        // Metadata writes succeed; tear the very next write — the uberblock flip.
        let flaky = FlakyDevice {
            inner: &dev,
            fail_at: ub2.meta_blocks as usize + 1,
            n: Cell::new(0),
        };
        assert!(
            commit(&flaky, &ub2, &mut bm, &ddt, &tbl, 0, 1, None).is_err(),
            "commit must fail during uberblock flip"
        );

        assert_rolled_back_to_seq2(&dev, used2, len2);
    }
}
