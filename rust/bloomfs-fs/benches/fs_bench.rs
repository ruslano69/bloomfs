//! Benchmarks for the BloomFS VFS layer, ported from the Go `fs/bench_test.go`.
//!
//! These exercise the hot paths through the full FS — Lookup (hit/miss), Stat,
//! the create/unlink metadata churn, the 4 KiB read/write data pipeline
//! (hash -> dedup -> compress -> encrypt), the dedup-hit short-circuit, and the
//! CoW commit cost (in-memory vs. a real file image, isolating the fsync tax).
//!
//! Run with `cargo bench -p bloomfs-fs`. Point `TMPDIR` at the medium under test
//! (tmpfs vs. SSD/NVMe) to compare commit cost across storage.

use std::path::PathBuf;
use std::time::{Duration, Instant};

use bloomfs_block::{Device, FileDevice, MemDevice};
use bloomfs_fs::FS;
use criterion::{black_box, criterion_group, criterion_main, Criterion, Throughput};

/// The 64-byte AES-256-XTS key used by the Go tests, so the crypto path runs.
fn test_key() -> Vec<u8> {
    (0u32..64).map(|i| (i.wrapping_mul(7) + 1) as u8).collect()
}

/// Format `dev` and fill the root with `files` empty files, committing every 256
/// so the deferred-free clusters are reclaimed before the bitmap is exhausted.
/// Returns the FS, the root id, the file names and their ids (creation order).
fn bench_fs_on<D: Device>(dev: D, files: usize) -> (FS<D>, u64, Vec<String>, Vec<u64>) {
    let key = test_key();
    let mut fs = FS::format(dev, Some(&key)).expect("format");
    let root = fs.root();
    let mut names = Vec::with_capacity(files);
    let mut ids = Vec::with_capacity(files);
    for i in 0..files {
        let name = format!("file_{i:06}.dat");
        let id = fs.create(root, &name).expect("create");
        names.push(name);
        ids.push(id);
        if i % 256 == 255 {
            fs.commit().expect("commit");
        }
    }
    fs.commit().expect("commit");
    (fs, root, names, ids)
}

/// In-memory pool of 64 MiB (16384 blocks), sized generously so the write
/// benchmarks (which defer frees until a commit) do not exhaust the bitmap.
fn bench_fs(files: usize) -> (FS<MemDevice>, u64, Vec<String>, Vec<u64>) {
    bench_fs_on(MemDevice::new(16384), files)
}

/// Fill `buf` with an incompressible LCG stream, so compress/encrypt do real
/// work rather than collapsing a run of zeros.
fn pseudo_random(buf: &mut [u8]) {
    let mut x: u64 = 0x9e37_79b9_7f4a_7c15;
    for b in buf.iter_mut() {
        x = x
            .wrapping_mul(6364136223846793005)
            .wrapping_add(1442695040888963407);
        *b = (x >> 56) as u8;
    }
}

fn bench_lookup_hit(c: &mut Criterion) {
    let (mut fs, root, names, _) = bench_fs(4000); // 2 segments
    fs.set_dir_cache(false); // measure the cache-free reload baseline
    let mut i = 0usize;
    c.bench_function("lookup_hit", |b| {
        b.iter(|| {
            let r = fs.lookup(root, &names[i % names.len()]).unwrap();
            assert!(r.is_some(), "miss on present name");
            black_box(r);
            i += 1;
        })
    });
}

fn bench_lookup_miss(c: &mut Criterion) {
    let (mut fs, root, _, _) = bench_fs(4000);
    fs.set_dir_cache(false); // measure the cache-free reload baseline
    let miss: Vec<String> = (0..4000).map(|i| format!("absent_{i:06}.dat")).collect();
    let mut i = 0usize;
    c.bench_function("lookup_miss", |b| {
        b.iter(|| {
            let r = fs.lookup(root, &miss[i % miss.len()]).unwrap();
            assert!(r.is_none(), "hit on absent name");
            black_box(r);
            i += 1;
        })
    });
}

fn bench_lookup_hit_cached(c: &mut Criterion) {
    let (mut fs, root, names, _) = bench_fs(4000);
    fs.set_dir_cache(true);
    fs.lookup(root, &names[0]).unwrap(); // prime the resident directory
    let mut i = 0usize;
    c.bench_function("lookup_hit_cached", |b| {
        b.iter(|| {
            let r = fs.lookup(root, &names[i % names.len()]).unwrap();
            assert!(r.is_some(), "miss on present name");
            black_box(r);
            i += 1;
        })
    });
}

fn bench_lookup_miss_cached(c: &mut Criterion) {
    let (mut fs, root, _, _) = bench_fs(4000);
    fs.set_dir_cache(true);
    fs.lookup(root, "absent_000000.dat").unwrap(); // prime the resident directory
    let miss: Vec<String> = (0..4000).map(|i| format!("absent_{i:06}.dat")).collect();
    let mut i = 0usize;
    c.bench_function("lookup_miss_cached", |b| {
        b.iter(|| {
            let r = fs.lookup(root, &miss[i % miss.len()]).unwrap();
            assert!(r.is_none(), "hit on absent name");
            black_box(r);
            i += 1;
        })
    });
}

fn bench_stat(c: &mut Criterion) {
    let (fs, _, _, ids) = bench_fs(4000);
    let mut i = 0usize;
    c.bench_function("stat", |b| {
        b.iter(|| {
            black_box(fs.stat(ids[i % ids.len()]).unwrap());
            i += 1;
        })
    });
}

fn bench_create_unlink(c: &mut Criterion) {
    let (mut fs, root, _, _) = bench_fs(4000);
    const NAME: &str = "churn.tmp";
    c.bench_function("create_unlink", |b| {
        // iter_custom so the periodic reclaim commit stays outside the timed
        // region, mirroring the Go StopTimer/StartTimer around Commit.
        b.iter_custom(|iters| {
            let mut total = Duration::ZERO;
            for i in 0..iters {
                let start = Instant::now();
                fs.create(root, NAME).unwrap();
                fs.unlink(root, NAME).unwrap();
                total += start.elapsed();
                if i % 512 == 511 {
                    fs.commit().unwrap(); // reclaim deferred frees, untimed
                }
            }
            total
        })
    });
}

fn bench_write_at_4k(c: &mut Criterion) {
    let (mut fs, root, _, _) = bench_fs(1);
    let id = fs.create(root, "w.dat").unwrap();
    let mut buf = vec![0u8; 4096];
    pseudo_random(&mut buf);
    fs.write_file(id, &buf).unwrap();
    let mut group = c.benchmark_group("data");
    group.throughput(Throughput::Bytes(4096));
    group.bench_function("write_at_4k", |b| {
        b.iter_custom(|iters| {
            let mut total = Duration::ZERO;
            for i in 0..iters {
                buf[..8].copy_from_slice(&i.to_le_bytes()); // unique write
                let start = Instant::now();
                fs.write_at(id, 0, &buf).unwrap();
                total += start.elapsed();
                if i % 512 == 511 {
                    fs.commit().unwrap();
                }
            }
            total
        })
    });
    group.finish();
}

fn bench_read_at_4k(c: &mut Criterion) {
    let (mut fs, root, _, _) = bench_fs(1);
    let id = fs.create(root, "r.dat").unwrap();
    let mut buf = vec![0u8; 4096];
    pseudo_random(&mut buf);
    fs.write_file(id, &buf).unwrap();
    let mut group = c.benchmark_group("data");
    group.throughput(Throughput::Bytes(4096));
    group.bench_function("read_at_4k", |b| {
        b.iter(|| {
            black_box(fs.read_at(id, 0, 4096).unwrap());
        })
    });
    group.finish();
}

fn bench_write_file_dedup_hit(c: &mut Criterion) {
    let (mut fs, root, _, _) = bench_fs(1);
    let id = fs.create(root, "d.dat").unwrap();
    let mut buf = vec![0u8; 4096];
    pseudo_random(&mut buf);
    let mut group = c.benchmark_group("data");
    group.throughput(Throughput::Bytes(4096));
    group.bench_function("write_file_dedup_hit", |b| {
        b.iter(|| {
            fs.write_file(id, &buf).unwrap();
        })
    });
    group.finish();
}

/// One durable metadata transaction (create + unlink + commit) with the commit
/// INSIDE the timed region. On a MemDevice this is the CoW snapshot cost alone;
/// on a FileDevice it also pays a real fsync, so the mem-vs-file delta isolates
/// the cost of durability on the target medium.
fn commit_cycle<D: Device>(fs: &mut FS<D>, root: u64, c: &mut Criterion, name: &str) {
    const CHURN: &str = "churn.tmp";
    c.bench_function(name, |b| {
        b.iter(|| {
            fs.create(root, CHURN).unwrap();
            fs.unlink(root, CHURN).unwrap();
            fs.commit().unwrap();
        })
    });
}

fn bench_create_unlink_commit(c: &mut Criterion) {
    let (mut fs, root, _, _) = bench_fs(4000);
    commit_cycle(&mut fs, root, c, "create_unlink_commit");
}

fn bench_create_unlink_commit_file(c: &mut Criterion) {
    let path: PathBuf =
        std::env::temp_dir().join(format!("bloomfs_bench_{}.img", std::process::id()));
    let dev = FileDevice::create(&path, 16384).expect("create image");
    let (mut fs, root, _, _) = bench_fs_on(dev, 4000);
    commit_cycle(&mut fs, root, c, "create_unlink_commit_file");
    drop(fs);
    let _ = std::fs::remove_file(&path);
}

criterion_group!(
    benches,
    bench_lookup_hit,
    bench_lookup_miss,
    bench_lookup_hit_cached,
    bench_lookup_miss_cached,
    bench_stat,
    bench_create_unlink,
    bench_write_at_4k,
    bench_read_at_4k,
    bench_write_file_dedup_hit,
    bench_create_unlink_commit,
    bench_create_unlink_commit_file,
);
criterion_main!(benches);
