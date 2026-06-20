//! The `bloomfs` command-line tool (Stage E): format and mount a BloomFS image.
//!
//! ```text
//! bloomfs format --size 64 disk.img          # 64 MiB image, formatted
//! bloomfs mount [--key HEX] disk.img /mnt     # mount until Ctrl-C / umount
//! ```
//!
//! A `--key` of 64 hex chars (32 bytes) or 128 hex chars (64 bytes) selects
//! AES-XTS; an empty key uses a plaintext pool. Metadata is made durable on a
//! clean unmount (CoW commit); a hard crash rolls back to the last commit.

use std::path::PathBuf;
use std::process::ExitCode;
use std::sync::mpsc;

use bloomfs_block::{FileDevice, SIZE};
use bloomfs_fs::FS;
use clap::{Parser, Subcommand};

#[derive(Parser)]
#[command(name = "bloomfs", about = "Format and mount a BloomFS image", version)]
struct Cli {
    #[command(subcommand)]
    cmd: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Create and format a new image.
    Format {
        /// Image size in MiB.
        #[arg(long, default_value_t = 64)]
        size: u64,
        /// AES-XTS key in hex (empty = plaintext).
        #[arg(long, default_value = "")]
        key: String,
        /// Path of the image file to create.
        image: PathBuf,
    },
    /// Mount an image until unmounted (Ctrl-C or `umount`).
    Mount {
        /// AES-XTS key in hex (empty = plaintext).
        #[arg(long, default_value = "")]
        key: String,
        /// Disable the resident open-directory cache (slower lookups, less RAM).
        #[arg(long)]
        no_dir_cache: bool,
        /// Path of the image file to mount.
        image: PathBuf,
        /// Directory to mount the filesystem on.
        mountpoint: PathBuf,
    },
}

/// Decode a hex string into bytes (lowercase or uppercase, even length).
fn decode_hex(s: &str) -> Result<Vec<u8>, String> {
    if !s.len().is_multiple_of(2) {
        return Err("odd number of hex digits".to_string());
    }
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&s[i..i + 2], 16).map_err(|_| "invalid hex digit".to_string()))
        .collect()
}

/// Parse a `--key` argument: empty selects plaintext, otherwise hex of exactly
/// 32 or 64 bytes (AES-128 or AES-256 XTS).
fn parse_key(h: &str) -> Result<Option<Vec<u8>>, String> {
    if h.is_empty() {
        return Ok(None);
    }
    let key = decode_hex(h).map_err(|e| format!("bad --key (must be hex): {e}"))?;
    if key.len() != 32 && key.len() != 64 {
        return Err(format!(
            "bad --key: need 32 or 64 bytes (got {})",
            key.len()
        ));
    }
    Ok(Some(key))
}

fn do_format(size_mib: u64, key_hex: &str, image: &PathBuf) -> Result<(), String> {
    let key = parse_key(key_hex)?;
    let blocks = size_mib * 1024 * 1024 / SIZE as u64;
    let dev = FileDevice::create(image, blocks).map_err(|e| format!("create image: {e}"))?;
    let mut fs = FS::format(dev, key.as_deref()).map_err(|e| format!("format: {e}"))?;
    // The fresh root is owned by uid 0; hand it to the formatting user so they
    // can write to their own pool under default_permissions.
    // SAFETY: getuid/getgid are always-successful syscalls with no preconditions.
    let (uid, gid) = unsafe { (libc::getuid(), libc::getgid()) };
    fs.chown(fs.root(), uid, gid)
        .map_err(|e| format!("chown root: {e}"))?;
    fs.fsync().map_err(|e| format!("commit: {e}"))?;
    drop(fs); // closes the device on drop
    println!(
        "formatted {}: {} blocks ({} MiB)",
        image.display(),
        blocks,
        size_mib
    );
    Ok(())
}

fn do_mount(
    key_hex: &str,
    no_dir_cache: bool,
    image: &PathBuf,
    mountpoint: &PathBuf,
) -> Result<(), String> {
    let key = parse_key(key_hex)?;
    let dev = FileDevice::open(image).map_err(|e| format!("open image: {e}"))?;
    let mut fs = FS::mount(dev, key.as_deref()).map_err(|e| format!("mount fs: {e}"))?;
    if no_dir_cache {
        fs.set_dir_cache(false); // opt out of the default resident dir cache
    }

    let session = bloomfs_fuse::spawn(fs, mountpoint).map_err(|e| format!("fuse mount: {e}"))?;
    println!(
        "mounted {} at {} (Ctrl-C to unmount)",
        image.display(),
        mountpoint.display()
    );

    // Block until a signal, then unmount cleanly so the kernel detaches before
    // we commit+close (BloomFuse::drop commits on session teardown).
    let (tx, rx) = mpsc::channel();
    ctrlc::set_handler(move || {
        let _ = tx.send(());
    })
    .map_err(|e| format!("install signal handler: {e}"))?;
    let _ = rx.recv();

    session.join();
    println!("unmounted");
    Ok(())
}

fn run() -> Result<(), String> {
    match Cli::parse().cmd {
        Command::Format { size, key, image } => do_format(size, &key, &image),
        Command::Mount {
            key,
            no_dir_cache,
            image,
            mountpoint,
        } => do_mount(&key, no_dir_cache, &image, &mountpoint),
    }
}

fn main() -> ExitCode {
    match run() {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("bloomfs: {e}");
            ExitCode::FAILURE
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_key_empty_is_plaintext() {
        assert_eq!(parse_key("").unwrap(), None);
    }

    #[test]
    fn parse_key_32_and_64_bytes() {
        let k32 = "00".repeat(32);
        let k64 = "ab".repeat(64);
        assert_eq!(parse_key(&k32).unwrap(), Some(vec![0u8; 32]));
        assert_eq!(parse_key(&k64).unwrap(), Some(vec![0xabu8; 64]));
    }

    #[test]
    fn parse_key_rejects_bad_length() {
        let k16 = "00".repeat(16); // 16 bytes, not 32/64
        assert!(parse_key(&k16).is_err());
    }

    #[test]
    fn parse_key_rejects_non_hex() {
        assert!(parse_key("zz").is_err());
        assert!(parse_key("abc").is_err()); // odd length
    }

    #[test]
    fn decode_hex_basic() {
        assert_eq!(decode_hex("00ff10").unwrap(), vec![0x00, 0xff, 0x10]);
        assert_eq!(
            decode_hex("DEADbeef").unwrap(),
            vec![0xde, 0xad, 0xbe, 0xef]
        );
    }
}
