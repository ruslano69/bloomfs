//! The BloomFS data-path pipeline (SPEC §5). For each logical block:
//!
//! ```text
//! plaintext -> BLAKE3 hash -> dedup lookup
//!    HIT  -> refcount++ (no compress, no encrypt, no write)
//!    MISS -> compress (zstd) -> encrypt (AES-XTS, tweak = cluster addr) -> write
//! ```
//!
//! and the reverse on read, with an end-to-end content-hash integrity check.
//!
//! Port notes vs the Go prototype:
//!   - The dedup hash is BLAKE3 (the Go prototype used BLAKE2b-256 as a drop-in
//!     stand-in, §B7); same cryptographic role, trusted without byte-verify (§D-2).
//!   - The hash is computed over the *uncompressed* block, enabling the dedup
//!     short-circuit and making it robust to compressor settings (§5.1).
//!   - Incompressible blocks are stored raw to avoid expansion (§B9).
//!   - Unlike the Go `BlockStore` (which holds pointers to the shared allocator
//!     and dedup table), this stores only the cipher. The device, allocator and
//!     dedup table are passed per call, so the single owner (the fs layer) keeps
//!     exclusive `&mut` access and the CoW layer can still snapshot them (§B3).

use std::fmt;

use aes::cipher::KeyInit;
use aes::{Aes128, Aes256};
use xts_mode::{get_tweak_default, Xts128};

use bloomfs_alloc::Bitmap;
use bloomfs_block::Device;
use bloomfs_dedup::{Entry, Key, Table as DedupTable};

/// Block size as `u64`, for cluster arithmetic.
const BLK: u64 = bloomfs_block::SIZE as u64;
/// zstd compression level (the library default; the Go prototype used zstd's
/// default `SpeedDefault`). Output bytes need not match Go — only round-trip.
const ZSTD_LEVEL: i32 = 3;

/// Errors returned by the store layer.
#[derive(Debug)]
pub enum Error {
    /// The AES-XTS key was not 32 bytes (AES-128-XTS) or 64 bytes (AES-256-XTS).
    BadKey,
    /// zstd failed to compress the block.
    Compress,
    /// zstd failed to decompress the stored payload.
    Decompress,
    /// The decompressed length did not match the recorded logical length.
    LogicalMismatch,
    /// The content hash did not match — bit-rot or corruption (§B13).
    ContentHashMismatch,
    /// An error from the block layer.
    Block(bloomfs_block::Error),
    /// An error from the allocator.
    Alloc(bloomfs_alloc::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::BadKey => f.write_str("store: key must be 32 (AES-128) or 64 (AES-256) bytes"),
            Error::Compress => f.write_str("store: compress"),
            Error::Decompress => f.write_str("store: decompress"),
            Error::LogicalMismatch => f.write_str("store: logical length mismatch"),
            Error::ContentHashMismatch => f.write_str("store: content hash mismatch (corruption)"),
            Error::Block(e) => write!(f, "store: {e}"),
            Error::Alloc(e) => write!(f, "store: {e}"),
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

/// A `Result` specialized to the store layer's [`Error`].
pub type Result<T> = std::result::Result<T, Error>;

/// What an inode stores to locate a logical block (it maps onto an extent, §4.4).
/// Returned by [`BlockStore::write`] and consumed by `read`/`release`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Ref {
    /// Content hash (the dedup key).
    pub hash: Key,
    /// First cluster.
    pub start: u64,
    /// Clusters occupied.
    pub count: u32,
    /// Bytes of stored payload (compressed or raw).
    pub payload: u32,
    /// Uncompressed bytes.
    pub logical: u32,
    /// Payload stored uncompressed (compression did not help, §B9).
    pub raw: bool,
}

/// Encoded size of a [`Ref`]: hash(32) + start(8) + count(4) + payload(4) +
/// logical(4) + raw(1). Fits an inode's 64-byte inline block map.
pub const REF_SIZE: usize = 32 + 8 + 4 + 4 + 4 + 1; // 53

impl Ref {
    /// Encode the ref to [`REF_SIZE`] bytes.
    pub fn marshal(&self) -> [u8; REF_SIZE] {
        let mut b = [0u8; REF_SIZE];
        b[0..32].copy_from_slice(&self.hash);
        b[32..40].copy_from_slice(&self.start.to_le_bytes());
        b[40..44].copy_from_slice(&self.count.to_le_bytes());
        b[44..48].copy_from_slice(&self.payload.to_le_bytes());
        b[48..52].copy_from_slice(&self.logical.to_le_bytes());
        b[52] = self.raw as u8;
        b
    }

    /// Decode a ref from at least [`REF_SIZE`] bytes.
    pub fn unmarshal(b: &[u8]) -> Ref {
        let mut hash = [0u8; 32];
        hash.copy_from_slice(&b[0..32]);
        Ref {
            hash,
            start: u64::from_le_bytes(b[32..40].try_into().unwrap()),
            count: u32::from_le_bytes(b[40..44].try_into().unwrap()),
            payload: u32::from_le_bytes(b[44..48].try_into().unwrap()),
            logical: u32::from_le_bytes(b[48..52].try_into().unwrap()),
            raw: b[52] == 1,
        }
    }
}

fn ref_from(k: Key, e: &Entry) -> Ref {
    Ref {
        hash: k,
        start: e.start,
        count: e.count,
        payload: e.payload,
        logical: e.logical,
        raw: e.raw,
    }
}

/// AES-XTS in one of the two supported key sizes. The cluster address is the XTS
/// tweak (§5.1), so each cluster decrypts independently and identical plaintext
/// at different addresses still yields different ciphertext.
enum Cipher {
    Aes128(Box<Xts128<Aes128>>),
    Aes256(Box<Xts128<Aes256>>),
}

impl Cipher {
    fn build(key: &[u8]) -> Result<Cipher> {
        match key.len() {
            32 => {
                let c1 = Aes128::new_from_slice(&key[..16]).map_err(|_| Error::BadKey)?;
                let c2 = Aes128::new_from_slice(&key[16..]).map_err(|_| Error::BadKey)?;
                Ok(Cipher::Aes128(Box::new(Xts128::new(c1, c2))))
            }
            64 => {
                let c1 = Aes256::new_from_slice(&key[..32]).map_err(|_| Error::BadKey)?;
                let c2 = Aes256::new_from_slice(&key[32..]).map_err(|_| Error::BadKey)?;
                Ok(Cipher::Aes256(Box::new(Xts128::new(c1, c2))))
            }
            _ => Err(Error::BadKey),
        }
    }

    fn encrypt(&self, sector: &mut [u8], addr: u64) {
        let tweak = get_tweak_default(addr as u128);
        match self {
            Cipher::Aes128(x) => x.encrypt_sector(sector, tweak),
            Cipher::Aes256(x) => x.encrypt_sector(sector, tweak),
        }
    }

    fn decrypt(&self, sector: &mut [u8], addr: u64) {
        let tweak = get_tweak_default(addr as u128);
        match self {
            Cipher::Aes128(x) => x.decrypt_sector(sector, tweak),
            Cipher::Aes256(x) => x.decrypt_sector(sector, tweak),
        }
    }
}

/// The data-path engine. Holds the optional cipher; the device, allocator and
/// dedup table are supplied per operation (see the port note above).
pub struct BlockStore {
    cipher: Option<Cipher>,
}

impl BlockStore {
    /// Build a store. `key` selects the encryption pool: `None` is a plaintext
    /// pool (§5.5 opt-out); otherwise it must be a valid AES-XTS key (32 bytes for
    /// AES-128-XTS or 64 for AES-256-XTS). Key management is §B4.
    pub fn new(key: Option<&[u8]>) -> Result<Self> {
        let cipher = match key {
            Some(k) => Some(Cipher::build(k)?),
            None => None,
        };
        Ok(BlockStore { cipher })
    }

    /// Store a logical block and return a [`Ref`] to it. Identical content is
    /// stored once (dedup), bumping a reference count instead of writing.
    pub fn write<D: Device>(
        &self,
        dev: &D,
        alloc: &mut Bitmap,
        ddt: &mut DedupTable,
        plaintext: &[u8],
    ) -> Result<Ref> {
        let key: Key = *blake3::hash(plaintext).as_bytes();

        if let Some(e) = ddt.lookup(&key) {
            // dedup hit — short-circuit (§5.1)
            ddt.incr(&key);
            return Ok(ref_from(key, &e));
        }

        // Compress; fall back to raw if compression does not shrink the block (§B9).
        let compressed = zstd::encode_all(plaintext, ZSTD_LEVEL).map_err(|_| Error::Compress)?;
        let (payload, raw): (&[u8], bool) = if compressed.len() >= plaintext.len() {
            (plaintext, true)
        } else {
            (&compressed, false)
        };

        let count = clusters(payload.len());
        let start = alloc.alloc(count)?;

        // Pad to whole clusters and encrypt each with its address as the XTS tweak.
        let blk = BLK as usize;
        let mut buf = vec![0u8; (count * BLK) as usize];
        buf[..payload.len()].copy_from_slice(payload);
        for i in 0..count {
            let off = i as usize * blk;
            let sec = &mut buf[off..off + blk];
            if let Some(c) = &self.cipher {
                c.encrypt(sec, start + i);
            }
            if let Err(e) = dev.write_block(start + i, sec) {
                alloc.free(start, count);
                return Err(e.into());
            }
        }

        let entry = Entry {
            start,
            count: count as u32,
            payload: payload.len() as u32,
            logical: plaintext.len() as u32,
            raw,
            ..Default::default()
        };
        ddt.add(key, entry);
        Ok(ref_from(key, &entry))
    }

    /// Reconstruct the plaintext for a [`Ref`] and verify its content hash.
    pub fn read<D: Device>(&self, dev: &D, r: &Ref) -> Result<Vec<u8>> {
        let blk = BLK as usize;
        let mut buf = vec![0u8; r.count as usize * blk];
        for i in 0..r.count as u64 {
            let mut sec = dev.read_block(r.start + i)?;
            if let Some(c) = &self.cipher {
                c.decrypt(&mut sec, r.start + i);
            }
            let off = i as usize * blk;
            buf[off..off + blk].copy_from_slice(&sec);
        }
        let payload = &buf[..r.payload as usize];

        let plaintext = if r.raw {
            payload.to_vec()
        } else {
            zstd::decode_all(payload).map_err(|_| Error::Decompress)?
        };
        if plaintext.len() as u32 != r.logical {
            return Err(Error::LogicalMismatch);
        }
        // The dedup hash doubles as an end-to-end integrity check (§B13): any
        // bit-rot surfaces here instead of returning silent garbage.
        let got: Key = *blake3::hash(&plaintext).as_bytes();
        if got != r.hash {
            return Err(Error::ContentHashMismatch);
        }
        Ok(plaintext)
    }

    /// Drop one reference to a block; the last reference frees its clusters (§5.4).
    /// The free is deferred until the next commit (§F1): the clusters may belong to
    /// the last committed state, so reusing them before this transaction commits
    /// could overwrite data a crash would need to roll back to.
    pub fn release(&self, alloc: &mut Bitmap, ddt: &mut DedupTable, r: &Ref) {
        if let Some(e) = ddt.decr(&r.hash) {
            alloc.defer(e.start, e.count as u64);
        }
    }
}

fn clusters(n: usize) -> u64 {
    (n as u64).div_ceil(BLK).max(1)
}

#[cfg(test)]
mod tests {
    use super::*;
    use bloomfs_block::MemDevice;

    /// A store over a fresh 512-block image, with the metadata region reserved and
    /// an AES-256-XTS key. Returns the store and its three shared pieces.
    fn new_store() -> (BlockStore, MemDevice, Bitmap, DedupTable) {
        let dev = MemDevice::new(512);
        let mut alloc = Bitmap::new(dev.blocks());
        alloc.reserve(0, 64); // stand in for the metadata region
        let key: Vec<u8> = (0..64u8).map(|i| i + 1).collect();
        let bs = BlockStore::new(Some(&key)).expect("new store");
        (bs, dev, alloc, DedupTable::new())
    }

    fn repeat(b: u8, n: usize) -> Vec<u8> {
        vec![b; n]
    }

    #[test]
    fn write_read_round_trip() {
        let (bs, dev, mut a, mut ddt) = new_store();
        let data = repeat(b'A', 32 * 1024); // compressible 32 KiB block
        let r = bs.write(&dev, &mut a, &mut ddt, &data).expect("write");
        assert!(!r.raw, "compressible data should be stored compressed");
        assert!(r.payload < r.logical, "compression did not shrink");
        let got = bs.read(&dev, &r).expect("read");
        assert_eq!(got, data, "round-trip mismatch");
    }

    #[test]
    fn encrypted_on_disk() {
        let (bs, dev, mut a, mut ddt) = new_store();
        let data = repeat(b'Z', 8 * 1024);
        let r = bs.write(&dev, &mut a, &mut ddt, &data).expect("write");
        let raw = dev.read_block(r.start).unwrap();
        assert_ne!(&raw[..64], &data[..64], "on-disk cluster equals plaintext");
    }

    #[test]
    fn dedup() {
        let (bs, dev, mut a, mut ddt) = new_store();
        let data = repeat(b'Q', 16 * 1024);
        let r1 = bs.write(&dev, &mut a, &mut ddt, &data).unwrap();
        let r2 = bs.write(&dev, &mut a, &mut ddt, &data).unwrap(); // identical
        assert_eq!(ddt.len(), 1, "dedup: one unique block");
        assert_eq!(r1.start, r2.start);
        assert_eq!(r1.hash, r2.hash);

        let other = repeat(b'W', 16 * 1024);
        let r3 = bs.write(&dev, &mut a, &mut ddt, &other).unwrap();
        assert_eq!(ddt.len(), 2);
        assert_ne!(r3.start, r1.start, "distinct block stored separately");

        // First release keeps the block (refs 2->1); second frees it.
        bs.release(&mut a, &mut ddt, &r1);
        assert!(bs.read(&dev, &r2).is_ok(), "read after one release");
        assert_eq!(ddt.len(), 2, "block freed too early");
        bs.release(&mut a, &mut ddt, &r2);
        assert_eq!(ddt.len(), 1, "block freed on last release");
    }

    #[test]
    fn incompressible_stored_raw() {
        let (bs, dev, mut a, mut ddt) = new_store();
        // Pseudo-random, incompressible payload (xorshift).
        let mut data = vec![0u8; 8 * 1024];
        let mut x = 2463534242u32;
        for d in &mut data {
            x ^= x << 13;
            x ^= x >> 17;
            x ^= x << 5;
            *d = x as u8;
        }
        let r = bs.write(&dev, &mut a, &mut ddt, &data).expect("write");
        assert!(r.raw, "incompressible data should be stored raw (§B9)");
        let got = bs.read(&dev, &r).expect("read");
        assert_eq!(got, data, "raw round-trip");
    }

    #[test]
    fn corruption_detected() {
        let (bs, dev, mut a, mut ddt) = new_store();
        let data = repeat(b'K', 8 * 1024);
        let r = bs.write(&dev, &mut a, &mut ddt, &data).expect("write");
        // Flip a byte in the stored cluster (simulated bit-rot, §B13).
        let mut blk = dev.read_block(r.start).unwrap();
        blk[10] ^= 0xFF;
        dev.write_block(r.start, &blk).unwrap();
        assert!(
            bs.read(&dev, &r).is_err(),
            "corruption must be caught by the content-hash check"
        );
    }

    #[test]
    fn ref_round_trip() {
        let r = Ref {
            hash: [7u8; 32],
            start: 1234,
            count: 3,
            payload: 5000,
            logical: 12288,
            raw: true,
        };
        assert_eq!(Ref::unmarshal(&r.marshal()), r);
    }
}
