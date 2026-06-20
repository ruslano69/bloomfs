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
use std::os::unix::io::AsRawFd;
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

/// `BLKGETSIZE64`: Linux ioctl that returns the device size in bytes (u64).
/// `_IOR(0x12, 114, u64)` = 0x80081272 on 64-bit Linux.
#[cfg(target_os = "linux")]
const BLKGETSIZE64: libc::c_ulong = 0x80081272;

/// Query the size of a block device file descriptor via `BLKGETSIZE64`.
#[cfg(target_os = "linux")]
fn blk_size_bytes(f: &File) -> Result<u64> {
    let mut size: u64 = 0;
    // SAFETY: fd is valid; ioctl writes exactly u64 into `size` on success.
    let ret = unsafe { libc::ioctl(f.as_raw_fd(), BLKGETSIZE64, &mut size) };
    if ret == -1 {
        return Err(Error::Io(std::io::Error::last_os_error()));
    }
    Ok(size)
}

/// A [`Device`] backed by a raw Linux block device node (e.g. `/dev/sda`,
/// `/dev/nvme0n1`, `/dev/ram0`, or a loop device created with `losetup`).
///
/// The device must be at least one block (4 KiB) in size and the caller needs
/// read + write permission on the node (typically root or the `disk` group).
/// No partition table awareness — the whole device is treated as a flat block
/// array, exactly like a file image.
///
/// ```text
/// # losetup /dev/loop0 disk.img          # wire a file as a block device
/// $ bloomfs format /dev/loop0             # format it
/// $ bloomfs mount /dev/loop0 /mnt         # mount it
/// ```
#[cfg(target_os = "linux")]
pub struct RawDevice {
    f: File,
    blocks: u64,
}

#[cfg(target_os = "linux")]
impl RawDevice {
    /// Open a raw block device node. Fails with an I/O error if `path` is not
    /// a block device or the kernel rejects `BLKGETSIZE64` (e.g. `ENOTTY` for
    /// a regular file, `EACCES` for insufficient permissions).
    pub fn open(path: impl AsRef<Path>) -> Result<Self> {
        let f = OpenOptions::new()
            .read(true)
            .write(true)
            .open(path.as_ref())?;
        let size = blk_size_bytes(&f)?;
        Ok(RawDevice {
            f,
            blocks: size / SIZE as u64,
        })
    }
}

#[cfg(target_os = "linux")]
impl Device for RawDevice {
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

    /// `RawDevice::open` on a plain file must fail — `BLKGETSIZE64` is not
    /// valid for regular files and the kernel returns `ENOTTY`.
    #[cfg(target_os = "linux")]
    #[test]
    fn raw_device_rejects_regular_file() {
        let path =
            std::env::temp_dir().join(format!("bloomfs_raw_test_{}.tmp", std::process::id()));
        std::fs::write(&path, vec![0u8; SIZE * 4]).unwrap();
        let result = RawDevice::open(&path);
        let _ = std::fs::remove_file(&path);
        assert!(
            result.is_err(),
            "RawDevice::open must fail on a regular file (ENOTTY)"
        );
    }

    /// Full round-trip on a real block device.  Skipped unless the environment
    /// provides one:  `BLOOMFS_TEST_DEV=/dev/loop0 cargo test raw_device`.
    ///
    /// Quick setup:
    /// ```text
    /// truncate -s 8M /tmp/test.img
    /// sudo losetup /dev/loop0 /tmp/test.img
    /// BLOOMFS_TEST_DEV=/dev/loop0 cargo test -p bloomfs-block raw_device_round_trip -- --ignored
    /// sudo losetup -d /dev/loop0
    /// ```
    #[cfg(target_os = "linux")]
    #[test]
    #[ignore = "needs a block device; set BLOOMFS_TEST_DEV=/dev/loopX after losetup"]
    fn raw_device_round_trip() {
        let path = std::env::var("BLOOMFS_TEST_DEV").unwrap_or("/dev/loop0".into());
        let d = RawDevice::open(&path).expect("open block device");
        assert!(d.blocks() >= 2, "device too small");
        round_trip(&d);
        d.sync().expect("sync");
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
