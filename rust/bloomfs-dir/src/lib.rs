//! The BloomFS directory subsystem (SPEC §3): a logical directory is a linked
//! list of fixed-capacity "virtual segments", each guarded by a blocked Bloom
//! filter so a lookup for an absent name returns without touching the index.
//! Names are keyed by XXH3-64 for the index; the filter hashes the raw name
//! bytes internally.
//!
//! Stage A is in-memory only: no disk, no on-disk inodes — [`InodeId`] is an
//! opaque handle. The goal is to validate the search architecture.

use std::collections::HashMap;

use bloomfs_bloom::BlockedFilter;
use xxhash_rust::xxh3::xxh3_64;

/// An opaque reference to a file's metadata (§4.2: the directory maps
/// name -> `InodeId`; the inode itself never stores the name).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub struct InodeId(pub u64);

/// Caps one virtual segment (§3.1). Overflow opens a new segment, keeping every
/// Bloom filter small and accurate.
pub const MAX_FILES_PER_SEGMENT: usize = 4000;
/// Target false-positive rate per segment (§3.1).
pub const BLOOM_FP: f64 = 0.01;

/// One name -> inode link. The name is kept for listing and for the final
/// collision check after the filter and hash-index both admit a hit (§3.2).
struct DirEntry {
    name: String,
    inode: InodeId,
}

/// One virtual segment: a blocked Bloom filter in front of a hash index keyed by
/// xxh3-64(name); the value is a (tiny) collision chain so two distinct names
/// sharing a 64-bit hash stay correct.
struct Segment {
    filter: BlockedFilter,
    index: HashMap<u64, Vec<DirEntry>>,
    count: usize,
    cap: usize,
    fp: f64,
    // Removals leave their bits set in the filter (a blocked filter can't clear a
    // single key's shared bits). Harmless — find() confirms every hit against the
    // index — but it nudges the FP rate up, so we rebuild once `tomb` crosses a
    // threshold, amortizing the costly re-tuning to O(1) per remove (§3.3).
    tomb: usize,
}

impl Segment {
    fn new(capacity: usize, fp: f64) -> Self {
        Segment {
            filter: BlockedFilter::new_blocked_tuned(capacity as u64, fp),
            index: HashMap::with_capacity(capacity),
            count: 0,
            cap: capacity,
            fp,
            tomb: 0,
        }
    }

    fn full(&self) -> bool {
        self.count >= self.cap
    }

    /// Link name -> inode. The caller guarantees name is absent from this segment.
    fn add(&mut self, h: u64, name: String, inode: InodeId) {
        self.filter.add(name.as_bytes());
        self.index
            .entry(h)
            .or_default()
            .push(DirEntry { name, inode });
        self.count += 1;
    }

    /// Resolve name within this segment. The filter answers "definitely absent"
    /// in one cache miss; on a maybe-hit we confirm against the index and verify
    /// the stored name (guards against the rare xxh3 collision, §3.2).
    fn find(&self, h: u64, name: &str) -> Option<InodeId> {
        if !self.filter.test(name.as_bytes()) {
            return None; // Bloom guarantees absence — no index access
        }
        if let Some(chain) = self.index.get(&h) {
            for e in chain {
                if e.name == name {
                    return Some(e.inode);
                }
            }
        }
        None // Bloom false positive (or hash-only collision)
    }

    /// Delete name, leaving its now-stale bits in the filter. Deferred rebuild
    /// (§3.3) keeps the FP drift bounded while amortizing the tuning cost away.
    fn remove(&mut self, h: u64, name: &str) -> bool {
        let chain = match self.index.get_mut(&h) {
            Some(c) => c,
            None => return false,
        };
        let pos = match chain.iter().position(|e| e.name == name) {
            Some(p) => p,
            None => return false,
        };
        chain.swap_remove(pos);
        if chain.is_empty() {
            self.index.remove(&h);
        }
        self.count -= 1;
        self.tomb += 1;
        if self.tomb >= self.rebuild_threshold() {
            self.rebuild_filter();
        }
        true
    }

    /// Stale-key tolerance before a rebuild. cap/4 bounds over-population to
    /// ~1.25x capacity at rebuild — a modest, bounded FP increase — while making
    /// rebuilds rare enough that their tuning cost amortizes away.
    fn rebuild_threshold(&self) -> usize {
        (self.cap / 4).max(1)
    }

    /// Discard the old filter and rebuild from the live keys, dropping all
    /// accumulated stale bits (§3.3).
    fn rebuild_filter(&mut self) {
        self.tomb = 0;
        let mut f = BlockedFilter::new_blocked_tuned(self.cap as u64, self.fp);
        for chain in self.index.values() {
            for e in chain {
                f.add(e.name.as_bytes());
            }
        }
        self.filter = f;
    }
}

/// A name → inode pair, for serializing or listing a directory's contents.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Entry {
    /// File name.
    pub name: String,
    /// Inode handle.
    pub inode: InodeId,
}

/// A logical directory: a linked list of virtual segments (§3.1). A lookup walks
/// the segments, each segment's Bloom filter rejecting non-members before any
/// index access. Not safe for concurrent use in Stage A.
pub struct Directory {
    segments: Vec<Segment>,
    cap: usize,
    fp: f64,
}

impl Default for Directory {
    fn default() -> Self {
        Self::new()
    }
}

/// The directory's single source of name hashing (XXH3-64, §3.4).
fn name_hash(name: &str) -> u64 {
    xxh3_64(name.as_bytes())
}

impl Directory {
    /// An empty directory with one segment ready, using the default per-segment
    /// capacity and FP.
    pub fn new() -> Self {
        Self::with_cap(MAX_FILES_PER_SEGMENT, BLOOM_FP)
    }

    /// A directory with a custom per-segment capacity and target FP (for tuning).
    pub fn with_cap(capacity: usize, fp: f64) -> Self {
        Directory {
            segments: vec![Segment::new(capacity, fp)],
            cap: capacity,
            fp,
        }
    }

    /// Link name -> inode. Returns false if name already exists anywhere.
    pub fn add(&mut self, name: &str, inode: InodeId) -> bool {
        let h = name_hash(name);
        for s in &self.segments {
            if s.find(h, name).is_some() {
                return false;
            }
        }
        // Reuse space freed by deletions: fill the first non-full segment, else
        // open a new continuation segment.
        for s in &mut self.segments {
            if !s.full() {
                s.add(h, name.to_string(), inode);
                return true;
            }
        }
        let mut s = Segment::new(self.cap, self.fp);
        s.add(h, name.to_string(), inode);
        self.segments.push(s);
        true
    }

    /// Link a name already known to be absent (e.g. when reloading a directory
    /// from disk, where the persisted entries are guaranteed unique). Unlike
    /// [`add`](Self::add) this skips the cross-segment duplicate scan and takes
    /// the name by value, so a bulk rebuild costs one hash + one insert per
    /// entry instead of an O(entries x segments) membership check.
    pub fn push_unique(&mut self, name: String, inode: InodeId) {
        let h = name_hash(&name);
        for s in &mut self.segments {
            if !s.full() {
                s.add(h, name, inode);
                return;
            }
        }
        let mut s = Segment::new(self.cap, self.fp);
        s.add(h, name, inode);
        self.segments.push(s);
    }

    /// Resolve name -> inode, scanning segments until a filter admits a hit.
    pub fn find(&self, name: &str) -> Option<InodeId> {
        let h = name_hash(name);
        for s in &self.segments {
            if let Some(inode) = s.find(h, name) {
                return Some(inode);
            }
        }
        None
    }

    /// Unlink name. Returns false if it was not present.
    pub fn delete(&mut self, name: &str) -> bool {
        let h = name_hash(name);
        for s in &mut self.segments {
            if s.remove(h, name) {
                return true;
            }
        }
        false
    }

    /// Total number of entries across all segments.
    pub fn len(&self) -> usize {
        self.segments.iter().map(|s| s.count).sum()
    }

    /// Whether the directory holds no entries.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// How many virtual segments back this directory.
    pub fn segments(&self) -> usize {
        self.segments.len()
    }

    /// Every name in the directory (readdir, §B12). Order is unspecified.
    pub fn list(&self) -> Vec<String> {
        self.entries().into_iter().map(|e| e.name).collect()
    }

    /// Every (name, inode) pair. Order is unspecified.
    pub fn entries(&self) -> Vec<Entry> {
        let mut out = Vec::with_capacity(self.len());
        for s in &self.segments {
            for chain in s.index.values() {
                for e in chain {
                    out.push(Entry {
                        name: e.name.clone(),
                        inode: e.inode,
                    });
                }
            }
        }
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn names(n: usize) -> Vec<String> {
        (0..n).map(|i| format!("file_{i:06}.dat")).collect()
    }

    #[test]
    fn add_find_delete() {
        let mut d = Directory::new();
        let all = names(10_000); // > 2*MAX_FILES_PER_SEGMENT forces multiple segments

        for (i, n) in all.iter().enumerate() {
            assert!(d.add(n, InodeId(i as u64 + 1)), "Add({n}) returned false");
        }
        assert_eq!(d.len(), all.len());
        assert!(
            d.segments() >= 3,
            "expected >=3 segments, got {}",
            d.segments()
        );

        // Every present file resolves to its inode.
        for (i, n) in all.iter().enumerate() {
            assert_eq!(d.find(n), Some(InodeId(i as u64 + 1)), "Find({n})");
        }

        // Absent files are reported absent — no false negatives.
        for i in 0..all.len() {
            let n = format!("absent_{i:06}.dat");
            assert!(d.find(&n).is_none(), "Find({n}) reported present");
        }

        // Delete the even-indexed files; the rest must survive (filter rebuilt).
        for i in (0..all.len()).step_by(2) {
            assert!(d.delete(&all[i]), "Delete({}) returned false", all[i]);
        }
        for (i, n) in all.iter().enumerate() {
            let want = i % 2 == 1;
            assert_eq!(d.find(n).is_some(), want, "after delete Find({n})");
        }
        assert_eq!(d.len(), all.len() / 2);

        // Duplicate Add is rejected.
        assert!(!d.add(&all[1], InodeId(99)), "duplicate Add accepted");
    }

    #[test]
    fn push_unique_matches_add() {
        // push_unique (the dup-scan-skipping bulk loader) must produce a
        // directory indistinguishable from one built with add: same length,
        // same segment growth, every name resolving to its inode.
        let mut d = Directory::new();
        let all = names(10_000); // forces multiple segments
        for (i, n) in all.iter().enumerate() {
            d.push_unique(n.clone(), InodeId(i as u64 + 1));
        }
        assert_eq!(d.len(), all.len());
        assert!(d.segments() >= 3, "got {} segments", d.segments());
        for (i, n) in all.iter().enumerate() {
            assert_eq!(d.find(n), Some(InodeId(i as u64 + 1)), "Find({n})");
        }
        for i in 0..all.len() {
            let n = format!("absent_{i:06}.dat");
            assert!(d.find(&n).is_none(), "Find({n}) reported present");
        }
        // The result stays mutable through the normal API.
        assert!(d.delete(&all[0]), "delete after push_unique");
        assert!(!d.add(&all[1], InodeId(99)), "duplicate add accepted");
    }

    #[test]
    fn list_and_entries() {
        let mut d = Directory::new();
        for (i, n) in names(50).iter().enumerate() {
            d.add(n, InodeId(i as u64 + 1));
        }
        let mut listed = d.list();
        listed.sort();
        let mut want = names(50);
        want.sort();
        assert_eq!(listed, want);
        assert_eq!(d.entries().len(), 50);
        assert!(!d.is_empty());
    }
}
