//! The BloomFS block-device abstraction (SPEC §1.2): the whole filesystem sees
//! the disk as an array of fixed-size blocks. This crate ships an in-memory
//! device (for tests / oracle cross-checks) and a file-image device.
//!
//! O_DIRECT and an own buffer cache (§B11) are deliberately out of scope — like
//! the Go prototype, this uses ordinary buffered I/O plus an explicit `sync`.
//!
//! Port note: the Go `Device` interface has pointer-receiver methods that
//! mutate. Here the trait takes `&self` and the in-memory device uses interior
//! mutability (`Mutex`) so it is `Sync`; the file device needs no lock because
//! positional `read_at`/`write_at` already take `&self`. Higher layers do their
//! own locking (the §B6 RWMutex discipline), exactly as in Go.

use std::fmt;
use std::fs::{File, OpenOptions};
use std::os::unix::fs::{FileExt, OpenOptionsExt};
use std::path::Path;
use std::sync::Mutex;

/// Fixed block size in bytes (SPEC §2). 4 KiB matches common drives and holds
/// exactly 32 inodes (§4.1).
pub const SIZE: usize = 4096;

/// Errors returned by the block layer.
#[derive(Debug)]
pub enum Error {
    /// A block index past the device end.
    OutOfRange,
    /// A write payload that is not exactly one block.
    ShortData,
    /// An underlying I/O failure (file device only).
    Io(std::io::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::OutOfRange => f.write_str("block: block number out of range"),
            Error::ShortData => f.write_str("block: data must be exactly one block"),
            Error::Io(e) => write!(f, "block: io: {e}"),
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Error::Io(e) => Some(e),
            _ => None,
        }
    }
}

impl From<std::io::Error> for Error {
    fn from(e: std::io::Error) -> Self {
        Error::Io(e)
    }
}

/// A `Result` specialized to the block layer's [`Error`].
pub type Result<T> = std::result::Result<T, Error>;

/// The disk abstraction: read and write fixed-size blocks by index.
///
/// `read_block` returns a fresh owned buffer the caller may retain and mutate.
pub trait Device {
    /// Read block `num` into a fresh `SIZE`-byte buffer.
    fn read_block(&self, num: u64) -> Result<Vec<u8>>;
    /// Overwrite block `num` with `data`, which must be exactly one block.
    fn write_block(&self, num: u64, data: &[u8]) -> Result<()>;
    /// Total number of blocks.
    fn blocks(&self) -> u64;
    /// Flush to durable storage.
    fn sync(&self) -> Result<()>;
}

/// An in-memory [`Device`] backed by a flat byte buffer.
pub struct MemDevice {
    data: Mutex<Vec<u8>>,
    blocks: u64,
}

impl MemDevice {
    /// Create an in-memory device of `blocks` blocks (zero-filled).
    pub fn new(blocks: u64) -> Self {
        MemDevice {
            data: Mutex::new(vec![0u8; blocks as usize * SIZE]),
            blocks,
        }
    }
}

impl Device for MemDevice {
    fn read_block(&self, num: u64) -> Result<Vec<u8>> {
        if num >= self.blocks {
            return Err(Error::OutOfRange);
        }
        let data = self.data.lock().unwrap();
        let off = num as usize * SIZE;
        Ok(data[off..off + SIZE].to_vec())
    }

    fn write_block(&self, num: u64, data: &[u8]) -> Result<()> {
        if num >= self.blocks {
            return Err(Error::OutOfRange);
        }
        if data.len() != SIZE {
            return Err(Error::ShortData);
        }
        let mut buf = self.data.lock().unwrap();
        let off = num as usize * SIZE;
        buf[off..off + SIZE].copy_from_slice(data);
        Ok(())
    }

    fn blocks(&self) -> u64 {
        self.blocks
    }

    fn sync(&self) -> Result<()> {
        Ok(())
    }
}

/// A [`Device`] backed by a fixed-size file image (the emulated disk of §1.2).
/// Blocks are addressed by `offset = num * SIZE`. The file is closed on drop.
pub struct FileDevice {
    f: File,
    blocks: u64,
}

impl FileDevice {
    /// Create (truncating any existing file) an image of `blocks` blocks.
    pub fn create(path: impl AsRef<Path>, blocks: u64) -> Result<Self> {
        let f = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(path)?;
        f.set_len(blocks * SIZE as u64)?;
        Ok(FileDevice { f, blocks })
    }

    /// Open an existing image, inferring the block count from its size.
    pub fn open(path: impl AsRef<Path>) -> Result<Self> {
        let f = OpenOptions::new().read(true).write(true).open(path)?;
        let len = f.metadata()?.len();
        Ok(FileDevice {
            f,
            blocks: len / SIZE as u64,
        })
    }
}

impl Device for FileDevice {
    fn read_block(&self, num: u64) -> Result<Vec<u8>> {
        if num >= self.blocks {
            return Err(Error::OutOfRange);
        }
        let mut out = vec![0u8; SIZE];
        self.f.read_exact_at(&mut out, num * SIZE as u64)?;
        Ok(out)
    }

    fn write_block(&self, num: u64, data: &[u8]) -> Result<()> {
        if num >= self.blocks {
            return Err(Error::OutOfRange);
        }
        if data.len() != SIZE {
            return Err(Error::ShortData);
        }
        self.f.write_all_at(data, num * SIZE as u64)?;
        Ok(())
    }

    fn blocks(&self) -> u64 {
        self.blocks
    }

    fn sync(&self) -> Result<()> {
        self.f.sync_all().map_err(Error::Io)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fill(b: u8) -> Vec<u8> {
        vec![b; SIZE]
    }

    fn round_trip(d: &dyn Device) {
        d.write_block(0, &fill(0xAA)).expect("write block 0");
        d.write_block(d.blocks() - 1, &fill(0x55))
            .expect("write last block");

        assert_eq!(d.read_block(0).unwrap(), fill(0xAA), "read block 0");
        assert_eq!(
            d.read_block(d.blocks() - 1).unwrap(),
            fill(0x55),
            "read last block"
        );

        assert!(
            matches!(d.read_block(d.blocks()), Err(Error::OutOfRange)),
            "expected OutOfRange past the end"
        );
        assert!(
            matches!(d.write_block(0, &[1, 2, 3]), Err(Error::ShortData)),
            "expected ShortData for a short write"
        );
    }

    #[test]
    fn mem_device() {
        round_trip(&MemDevice::new(8));
    }

    #[test]
    fn file_device() {
        let path = std::env::temp_dir().join(format!("bloomfs-block-{}.img", std::process::id()));
        let _ = std::fs::remove_file(&path);

        let d = FileDevice::create(&path, 8).expect("create");
        round_trip(&d);
        d.sync().expect("sync");
        drop(d); // close

        // Reopen and confirm the bytes persisted across close.
        let d2 = FileDevice::open(&path).expect("open");
        assert_eq!(d2.blocks(), 8, "blocks after reopen");
        assert_eq!(
            d2.read_block(0).unwrap(),
            fill(0xAA),
            "persisted block 0 mismatch"
        );
        drop(d2);
        let _ = std::fs::remove_file(&path);
    }
}
