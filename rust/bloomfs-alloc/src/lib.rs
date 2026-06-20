//! The free-space allocator: a bitmap of clusters (SPEC §B2). One bit per
//! cluster, set = used. Allocation is first-fit contiguous so that, where
//! possible, a logical block's clusters land in one run (extents, §4.4). The
//! bitmap lives in RAM and (de)serializes for persistence.
//!
//! Port note: unlike the block device (the shared backing, accessed via a
//! `&self` trait), the bitmap is a plain owned structure that the upper layers
//! mutate under their own write lock. So mutating methods take `&mut self`
//! here — no interior mutability — which is both idiomatic and matches the Go
//! prototype's single-writer discipline (§B6).

use std::fmt;

/// Errors returned by the allocator.
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// No contiguous run of the requested size exists.
    NoSpace,
    /// `alloc` was called with a zero count.
    ZeroCount,
    /// A serialized bitmap buffer is truncated.
    Short,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::NoSpace => f.write_str("alloc: no contiguous run of the requested size"),
            Error::ZeroCount => f.write_str("alloc: zero count"),
            Error::Short => f.write_str("alloc: serialized bitmap too short"),
        }
    }
}

impl std::error::Error for Error {}

/// A `Result` specialized to the allocator's [`Error`].
pub type Result<T> = std::result::Result<T, Error>;

/// A contiguous run pending deferred free.
struct FreeRange {
    start: u64,
    count: u64,
}

/// Tracks which clusters are used.
///
/// `deferred` holds clusters freed during the current (uncommitted) transaction.
/// Their bits stay SET so they cannot be reallocated and overwrite data the last
/// commit still references; the durability layer applies them
/// ([`apply_deferred`](Bitmap::apply_deferred)) only after a commit's uberblock
/// flip, so they become reusable one commit later. This is the
/// deferred-free / pinned-extent discipline of ZFS/Btrfs and is what keeps "no
/// grey zone" true for overwrite-then-crash (§B2, §E, §F1). It is transient RAM
/// state: a fresh mount starts with none pending.
pub struct Bitmap {
    bits: Vec<u64>,
    total: u64,
    used: u64,
    deferred: Vec<FreeRange>,
}

impl Bitmap {
    /// Return a bitmap for `total` clusters, all free.
    pub fn new(total: u64) -> Self {
        Bitmap {
            bits: vec![0u64; total.div_ceil(64) as usize],
            total,
            used: 0,
            deferred: Vec::new(),
        }
    }

    #[inline]
    fn get(&self, i: u64) -> bool {
        self.bits[(i >> 6) as usize] & (1u64 << (i & 63)) != 0
    }

    #[inline]
    fn set(&mut self, i: u64) {
        self.bits[(i >> 6) as usize] |= 1u64 << (i & 63);
    }

    #[inline]
    fn clr(&mut self, i: u64) {
        self.bits[(i >> 6) as usize] &= !(1u64 << (i & 63));
    }

    /// Mark `[start, start+count)` used — e.g. the superblock and inode table
    /// that the allocator must never hand out.
    pub fn reserve(&mut self, start: u64, count: u64) {
        let mut i = start;
        while i < start + count && i < self.total {
            if !self.get(i) {
                self.set(i);
                self.used += 1;
            }
            i += 1;
        }
    }

    /// Find the first contiguous free run of `count` clusters, mark it used and
    /// return its start. Returns [`Error::NoSpace`] if no such run exists.
    pub fn alloc(&mut self, count: u64) -> Result<u64> {
        if count == 0 {
            return Err(Error::ZeroCount);
        }
        let mut run: u64 = 0;
        let mut start: u64 = 0;
        for i in 0..self.total {
            if self.get(i) {
                run = 0;
                continue;
            }
            if run == 0 {
                start = i;
            }
            run += 1;
            if run == count {
                for j in start..start + count {
                    self.set(j);
                }
                self.used += count;
                return Ok(start);
            }
        }
        Err(Error::NoSpace)
    }

    /// Mark `[start, start+count)` free immediately. Use this only for clusters
    /// allocated within the current transaction that were never committed (e.g.
    /// the rollback of a failed write) — reusing them at once is safe. For
    /// releasing clusters that may belong to a committed state, use
    /// [`defer`](Bitmap::defer).
    pub fn free(&mut self, start: u64, count: u64) {
        let mut i = start;
        while i < start + count && i < self.total {
            if self.get(i) {
                self.clr(i);
                self.used -= 1;
            }
            i += 1;
        }
    }

    /// Record `[start, start+count)` for release at the next commit, leaving the
    /// bits SET so the run cannot be reallocated before then (§F1). The clusters
    /// keep counting as used until [`apply_deferred`](Bitmap::apply_deferred).
    pub fn defer(&mut self, start: u64, count: u64) {
        self.deferred.push(FreeRange { start, count });
    }

    /// Actually free every range recorded by [`defer`](Bitmap::defer) and clear
    /// the pending list. The durability layer calls this after a commit's
    /// uberblock flip succeeds, so the just-committed snapshot still marks these
    /// clusters used (reclaimed one commit later) while a crash before the flip
    /// leaves them pinned — never reused, never lost.
    pub fn apply_deferred(&mut self) {
        let ranges = std::mem::take(&mut self.deferred);
        for r in &ranges {
            self.free(r.start, r.count);
        }
    }

    /// Report how many deferred-free ranges await
    /// [`apply_deferred`](Bitmap::apply_deferred).
    pub fn pending(&self) -> usize {
        self.deferred.len()
    }

    /// Total number of clusters queued in deferred-free ranges.
    ///
    /// These clusters are still counted as *used* in the bitmap (they cannot be
    /// reallocated until after the next commit), but from the user's perspective
    /// they are "about to be free" — `statfs` should add this to `available()`
    /// so that `df` does not falsely report the filesystem as nearly full during
    /// a long write or unlink loop.
    pub fn deferred_count(&self) -> u64 {
        self.deferred.iter().map(|r| r.count).sum()
    }

    /// Number of clusters currently marked used.
    pub fn used(&self) -> u64 {
        self.used
    }

    /// Number of clusters currently free.
    pub fn available(&self) -> u64 {
        self.total - self.used
    }

    /// Total number of clusters.
    pub fn total(&self) -> u64 {
        self.total
    }

    /// Report how many bytes [`marshal`](Bitmap::marshal) /
    /// [`marshal_into`](Bitmap::marshal_into) will write.
    pub fn marshal_len(&self) -> usize {
        8 + self.bits.len() * 8
    }

    /// Serialize the bitmap into `dst` (at least [`marshal_len`](Bitmap::marshal_len)
    /// bytes) and return the number of bytes written, allocating nothing.
    pub fn marshal_into(&self, dst: &mut [u8]) -> usize {
        dst[0..8].copy_from_slice(&self.total.to_le_bytes());
        for (i, w) in self.bits.iter().enumerate() {
            let off = 8 + i * 8;
            dst[off..off + 8].copy_from_slice(&w.to_le_bytes());
        }
        8 + self.bits.len() * 8
    }

    /// Serialize the bitmap (total header + raw words).
    pub fn marshal(&self) -> Vec<u8> {
        let mut out = vec![0u8; self.marshal_len()];
        self.marshal_into(&mut out);
        out
    }

    /// Reconstruct a bitmap from [`marshal`](Bitmap::marshal) output
    /// (recomputing the used count).
    pub fn unmarshal(data: &[u8]) -> Result<Bitmap> {
        if data.len() < 8 {
            return Err(Error::Short);
        }
        let total = u64::from_le_bytes(data[0..8].try_into().unwrap());
        let mut b = Bitmap::new(total);
        let body = &data[8..];
        if body.len() < b.bits.len() * 8 {
            return Err(Error::Short);
        }
        for i in 0..b.bits.len() {
            let off = i * 8;
            b.bits[i] = u64::from_le_bytes(body[off..off + 8].try_into().unwrap());
        }
        for i in 0..total {
            if b.get(i) {
                b.used += 1;
            }
        }
        Ok(b)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // A deferred range stays pinned (cannot be reallocated) until
    // apply_deferred, then becomes reusable — the §F1 discipline.
    #[test]
    fn deferred_free() {
        let mut b = Bitmap::new(8);
        assert_eq!(b.alloc(8).unwrap(), 0, "Alloc(8) should fill from 0");

        b.defer(2, 3); // release [2,5) but keep it pinned
        assert_eq!(b.pending(), 1);
        assert_eq!(b.used(), 8, "pinned, still used");
        // The pinned run must not be handed out: the device is logically full.
        assert_eq!(b.alloc(1), Err(Error::NoSpace), "Alloc over pinned range");

        b.apply_deferred();
        assert_eq!(b.pending(), 0);
        assert_eq!(b.used(), 5);
        assert_eq!(b.alloc(3).unwrap(), 2, "released run now reusable");
    }

    #[test]
    fn alloc_free_contiguous() {
        let mut b = Bitmap::new(64);
        b.reserve(0, 5); // superblock + inode table
        assert_eq!(b.used(), 5);

        assert_eq!(b.alloc(3).unwrap(), 5);
        assert_eq!(b.alloc(2).unwrap(), 8);
        assert_eq!(b.used(), 10);

        // Free the first run; the next Alloc(3) should reuse the freed hole at 5.
        b.free(5, 3);
        assert_eq!(b.alloc(3).unwrap(), 5);
    }

    #[test]
    fn alloc_no_space() {
        let mut b = Bitmap::new(10);
        assert_eq!(b.alloc(20), Err(Error::NoSpace));

        // Fragmentation: reserved at 1 and 3 leaves no run of 2.
        let mut b2 = Bitmap::new(4);
        b2.reserve(1, 1);
        b2.reserve(3, 1);
        assert_eq!(b2.alloc(2), Err(Error::NoSpace), "fragmented map");
    }

    #[test]
    fn bitmap_round_trip() {
        let mut b = Bitmap::new(200);
        b.reserve(0, 5);
        b.alloc(7).unwrap();

        let mut got = Bitmap::unmarshal(&b.marshal()).unwrap();
        assert_eq!(got.total(), b.total());
        assert_eq!(got.used(), b.used());
        // A reloaded bitmap must keep allocating consistently.
        // 0..4 reserved, 5..11 allocated, next free = 12.
        assert_eq!(got.alloc(1).unwrap(), 12);
    }

    #[test]
    fn zero_count() {
        let mut b = Bitmap::new(8);
        assert_eq!(b.alloc(0), Err(Error::ZeroCount));
    }
}
