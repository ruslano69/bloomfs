//! The in-RAM deduplication table (SPEC §5.2): a content hash maps to where the
//! one physical copy of a block lives plus a reference count. On-disk DDT
//! persistence and crash recovery are §B3.

use std::collections::HashMap;
use std::fmt;

/// Content hash of an uncompressed logical block (§5.1). 256 bits is trusted
/// without a byte-verify (§B7, §D-2).
pub type Key = [u8; 32];

/// Per-entry encoded size: key(32) + start(8) + count(4) + payload(4) +
/// logical(4) + raw(1) + refs(4).
const REC_SIZE: usize = 32 + 8 + 4 + 4 + 4 + 1 + 4;

/// Errors returned by the dedup layer.
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// A serialized table buffer is truncated.
    Short,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Short => f.write_str("dedup: serialized table too short"),
        }
    }
}

impl std::error::Error for Error {}

/// A `Result` specialized to the dedup layer's [`Error`].
pub type Result<T> = std::result::Result<T, Error>;

/// Locates the single stored copy of a block and counts references to it.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Entry {
    /// First cluster.
    pub start: u64,
    /// Clusters occupied.
    pub count: u32,
    /// Bytes of stored payload (compressed, or raw if `raw`).
    pub payload: u32,
    /// Uncompressed bytes.
    pub logical: u32,
    /// Payload stored uncompressed (compression did not help, §B9).
    pub raw: bool,
    /// Reference count.
    pub refs: u32,
}

/// Maps content hashes to entries.
#[derive(Default)]
pub struct Table {
    m: HashMap<Key, Entry>,
}

impl Table {
    /// Return an empty table.
    pub fn new() -> Self {
        Table { m: HashMap::new() }
    }

    /// Return a copy of the entry for `k`, if present.
    pub fn lookup(&self, k: &Key) -> Option<Entry> {
        self.m.get(k).copied()
    }

    /// Insert a new unique block with reference count 1.
    pub fn add(&mut self, k: Key, mut e: Entry) {
        e.refs = 1;
        self.m.insert(k, e);
    }

    /// Bump the reference count of an existing block. Returns false if absent.
    pub fn incr(&mut self, k: &Key) -> bool {
        match self.m.get_mut(k) {
            Some(e) => {
                e.refs += 1;
                true
            }
            None => false,
        }
    }

    /// Drop one reference. When it reaches zero the entry is removed and the
    /// freed entry is returned so the caller can free the clusters (§5.4);
    /// otherwise (absent, or refs still > 0) returns `None`.
    pub fn decr(&mut self, k: &Key) -> Option<Entry> {
        let freed = {
            let e = self.m.get_mut(k)?;
            e.refs -= 1;
            e.refs == 0
        };
        if freed {
            self.m.remove(k)
        } else {
            None
        }
    }

    /// Number of unique blocks tracked.
    pub fn len(&self) -> usize {
        self.m.len()
    }

    /// Whether the table tracks no blocks.
    pub fn is_empty(&self) -> bool {
        self.m.is_empty()
    }

    /// Report how many bytes [`marshal`](Table::marshal) /
    /// [`marshal_into`](Table::marshal_into) will write.
    pub fn marshal_len(&self) -> usize {
        8 + self.m.len() * REC_SIZE
    }

    /// Serialize the table into `dst` (at least [`marshal_len`](Table::marshal_len)
    /// bytes) and return the bytes written, allocating nothing. Iteration order
    /// is unspecified — the table is a set, so order does not matter.
    pub fn marshal_into(&self, dst: &mut [u8]) -> usize {
        dst[0..8].copy_from_slice(&(self.m.len() as u64).to_le_bytes());
        let mut off = 8;
        for (k, e) in &self.m {
            dst[off..off + 32].copy_from_slice(k);
            off += 32;
            dst[off..off + 8].copy_from_slice(&e.start.to_le_bytes());
            off += 8;
            dst[off..off + 4].copy_from_slice(&e.count.to_le_bytes());
            off += 4;
            dst[off..off + 4].copy_from_slice(&e.payload.to_le_bytes());
            off += 4;
            dst[off..off + 4].copy_from_slice(&e.logical.to_le_bytes());
            off += 4;
            dst[off] = if e.raw { 1 } else { 0 };
            off += 1;
            dst[off..off + 4].copy_from_slice(&e.refs.to_le_bytes());
            off += 4;
        }
        off
    }

    /// Serialize the table (count header + fixed-size records).
    pub fn marshal(&self) -> Vec<u8> {
        let mut out = vec![0u8; self.marshal_len()];
        self.marshal_into(&mut out);
        out
    }

    /// Reconstruct a table from [`marshal`](Table::marshal) output.
    pub fn unmarshal(data: &[u8]) -> Result<Table> {
        if data.len() < 8 {
            return Err(Error::Short);
        }
        let n = u64::from_le_bytes(data[0..8].try_into().unwrap());
        let mut t = Table::new();
        let mut off = 8usize;
        for _ in 0..n {
            if off + REC_SIZE > data.len() {
                return Err(Error::Short);
            }
            let mut k = [0u8; 32];
            k.copy_from_slice(&data[off..off + 32]);
            off += 32;
            let start = u64::from_le_bytes(data[off..off + 8].try_into().unwrap());
            off += 8;
            let count = u32::from_le_bytes(data[off..off + 4].try_into().unwrap());
            off += 4;
            let payload = u32::from_le_bytes(data[off..off + 4].try_into().unwrap());
            off += 4;
            let logical = u32::from_le_bytes(data[off..off + 4].try_into().unwrap());
            off += 4;
            let raw = data[off] == 1;
            off += 1;
            let refs = u32::from_le_bytes(data[off..off + 4].try_into().unwrap());
            off += 4;
            t.m.insert(
                k,
                Entry {
                    start,
                    count,
                    payload,
                    logical,
                    raw,
                    refs,
                },
            );
        }
        Ok(t)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key(b: u8) -> Key {
        let mut k = [0u8; 32];
        k[0] = b;
        k
    }

    #[test]
    fn ref_counting() {
        let mut tab = Table::new();
        let k = key(1);

        tab.add(
            k,
            Entry {
                start: 100,
                count: 2,
                ..Default::default()
            },
        );
        assert_eq!(tab.len(), 1);
        let e = tab.lookup(&k).unwrap();
        assert_eq!(e.refs, 1);
        assert_eq!(e.start, 100);

        // Two more references (e.g. two more identical files).
        tab.incr(&k);
        tab.incr(&k);

        // Three releases; only the last frees the block.
        assert!(tab.decr(&k).is_none(), "freed too early at refs 3->2");
        assert!(tab.decr(&k).is_none(), "freed too early at refs 2->1");
        let freed = tab.decr(&k).expect("expected free");
        assert_eq!(freed.start, 100);
        assert_eq!(tab.len(), 0);
    }

    #[test]
    fn decr_absent() {
        let mut tab = Table::new();
        assert!(tab.decr(&key(9)).is_none(), "Decr on absent key");
        assert!(!tab.incr(&key(9)), "Incr on absent key");
    }

    #[test]
    fn serialize_round_trip() {
        let mut t = Table::new();
        t.add(
            key(1),
            Entry {
                start: 100,
                count: 2,
                payload: 4096,
                logical: 8192,
                raw: false,
                ..Default::default()
            },
        );
        t.add(
            key(2),
            Entry {
                start: 200,
                count: 1,
                payload: 500,
                logical: 4096,
                raw: true,
                ..Default::default()
            },
        );
        t.incr(&key(1)); // refs = 2

        let blob = t.marshal();
        assert_eq!(blob.len(), t.marshal_len());

        let got = Table::unmarshal(&blob).unwrap();
        assert_eq!(got.len(), 2);
        let e1 = got.lookup(&key(1)).unwrap();
        assert_eq!(e1.refs, 2);
        assert_eq!(e1.start, 100);
        assert_eq!(e1.logical, 8192);
        let e2 = got.lookup(&key(2)).unwrap();
        assert!(e2.raw, "raw flag must survive");
        assert_eq!(e2.payload, 500);

        // Truncation is detected.
        assert!(matches!(
            Table::unmarshal(&blob[..blob.len() - 1]),
            Err(Error::Short)
        ));
        assert!(matches!(Table::unmarshal(&[]), Err(Error::Short)));
    }
}
