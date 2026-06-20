# BloomFS

An experimental high-throughput, crypto-deduplicating filesystem core, written
in Go (with a planned Rust port). The design goal: **instant directory lookups**,
**parallel async reads**, and resistance to fragmentation — with **transparent
compression, encryption, and block deduplication** layered on top.

The distinguishing idea is the directory subsystem: a directory is a chain of
fixed-capacity **segments**, each guarded by a [blocked Bloom
filter](https://github.com/ruslano69/xxh3-bloom) keyed with XXH3, so a lookup for
an absent file is rejected in a single cache miss. Everything else — Copy-on-Write
durability, dedup, AES-XTS encryption, ZSTD compression — is shared with mature
filesystems like ZFS; the Bloom-segmented lookup is what BloomFS adds.

> Status: **research prototype — now mountable.** The on-disk layers work end to
> end and are covered by tests (including crash-injection), and a FUSE binding
> (`fusefs` + the `bloomfs` CLI) mounts the filesystem on a real kernel:
> mkdir/read/write/append/hardlink/symlink/rename/rmdir, multi-record large files
> with verified data integrity, `df`/`df -i` capacity reporting, persistence
> across remount, POSIX permission enforcement (`chmod`/`chown` under
> `default_permissions`, including unlink-of-open survival), and encrypted pools
> have all been exercised through an actual mount.

## Architecture at a glance

| Layer | Package | What it does |
|-------|---------|--------------|
| Directory lookup | `dir` | Bloom-segmented index (XXH3); ~ns lookups, zero alloc |
| Block device | `block` | 4 KiB blocks over an in-memory or file image |
| Metadata | `inode` | 128-byte inodes, 32 per block; files, dirs and symlinks |
| Free space | `alloc` | First-fit contiguous cluster bitmap |
| Dedup | `dedup` | Content-hash table with reference counts |
| Data pipeline | `store` | hash → dedup → ZSTD → AES-XTS → write |
| Durability | `cow` | Copy-on-Write commits with crash recovery |
| Filesystem (VFS) | `fs` | Ties it all together: Mkdir/Create/Read/Write/Readdir/Unlink/Commit |
| Kernel binding | `fusefs` | go-fuse v2 shim: maps each kernel op onto the VFS API |
| CLI | `cmd/bloomfs` | `format` and `mount` an on-disk image |

## Implementation status

- [x] **Stage A** — directory subsystem (Bloom segments + XXH3 index) + tuning sweep
- [x] **Stage B** — block device (memory + file image) + inode serialization + superblock
- [x] **Stage C** — allocator + dedup + compress/encrypt data pipeline
- [x] **Stage D** — Copy-on-Write transactions + crash recovery
- [x] **VFS integration** — `dir`+`inode`+`store`+`cow` as one persistent
  filesystem (`fs` package); end-to-end build → commit → remount → read back,
  tested without a kernel mount. Concurrency-safe (RWMutex: parallel readers,
  exclusive writer — validated under `-race`) with an open-directory cache so a
  hot `Lookup` is a RAM hit, not a disk read + decrypt + decompress + rebuild.
- [x] **CoW inode table** — the inode table is now a RAM structure
  (`inode.Table`) snapshotted into the CoW metadata slot alongside the bitmap and
  dedup table, with one atomic uberblock flip. No more in-place inode writes, so
  an uncommitted file vanishes after a crash and `Fsync` is a strong guarantee
  (§E).
- [x] **Deferred-free** — clusters freed inside a transaction are pinned until
  the commit flips (ZFS/Btrfs discipline), so an uncommitted overwrite can't
  reuse and clobber a still-committed cluster. "No grey zone" now holds for
  overwrite-then-crash too (SPEC §F1).
- [x] **Recordsize + multi-extent + addressable IO (§F2–F4, E5)** — file
  contents are chunked into recordsize records (§4.5), derived from the file
  size and frozen once data exists; a single-record file stores its ref inline,
  a multi-record file behind an external ref-list block-map (§4.4). Each record
  is an independent dedup/compress/encrypt unit, so `ReadAt`/`WriteAt` touch only
  the records they overlap (untouched records carry over without disturbing dedup
  refcounts) and `Truncate` (incl. sparse grow past EOF) closes E5.
- [x] **Inode free-list (§F5)** — reclaimed inode ids are recycled through a
  reuse stack (rebuilt at mount by scanning the committed table), so churn no
  longer leaks ids; reuse bumps the inode generation and evicts the directory
  cache so a stale handle can't alias the new file.
- [x] **Timestamps (§F6)** — a/m/ctime are stamped on mutations through an
  injectable clock: content/size changes bump mtime+ctime, metadata-only changes
  (link count, rename) bump ctime; atime is set on create/write but not on read
  (the read path stays mutation-free under `RLock` — effectively `noatime`). Times
  are part of the CoW snapshot and survive a remount.
- [x] **Atomic append + concurrency stress (§F8, E8)** — `Append` reads the size
  and writes under one write lock, so concurrent appends never lose, duplicate or
  interleave; proven by a 32-goroutine stress test under `-race`.
- [x] **Readdir snapshot (§F7, E9)** — `OpenDir` freezes a snapshot of the
  directory's names and `DirHandle.Next` paginates over it, so concurrent
  create/unlink/rename can't add, drop or duplicate names mid-iteration — the
  stability the kernel's paginated `getdents` needs.
- [x] **Path to mount (§F) complete** — every pre-FUSE prerequisite (F1–F8) is
  done.
- [x] **Stage E — FUSE binding (`fusefs`) + `bloomfs` CLI** — each kernel-visible
  node is a thin shim onto the VFS API (no storage logic of its own); bloomfs
  sentinel errors map to POSIX errno; inode ids are reported as `id+1` so the
  root (id 0) doesn't collide with FUSE's reserved nodeid 1. Verified through a
  **real kernel mount**: mkdir/write/append, a 1 MiB multi-record file with
  `sha1sum` integrity, hardlink (link count 2), rename, rm, rmdir (non-empty
  refused), persistence after unmount+remount, and an **encrypted pool** (AES-XTS;
  plaintext absent from the raw image, survives remount).
- [x] **Symlinks + statfs** — `Symlink`/`Readlink` store the target path as the
  link inode's data (through the normal dedup/compress/encrypt record pipeline);
  `StatFS` reports capacity (the bitmap counts 4 KiB blocks directly) and inode
  usage. Both are wired through the FUSE binding and verified on a real mount:
  `ln -s` (relative and absolute), `readlink`, following a link, `df`/`df -i`,
  and inode reclaim on `rm`. Page cache and quotas are the remaining Stage E work.
- [x] **Positional file handles + permission enforcement** — `Create`/`Open` now
  return a real handle (`bfile`) over `fs.Handle`: it pins the inode for the life
  of the kernel's open fd, so a file unlinked while open survives until the last
  `Release` (POSIX unlink-of-open). Permissions are enforced by mounting with
  `default_permissions` (the kernel checks the mode/uid/gid we report) backed by
  `fs.Access` (POSIX owner/group/other DAC, root bypass); new objects are owned by
  their creator and `NullPermissions` keeps a literal `chmod 000` from being
  coerced back to 0644. Verified on a real mount: `chmod 000/600/755` hold,
  read-through-a-held-fd after `rm`, `O_TRUNC`-on-open, and `EACCES` writing into a
  `0500` directory.
- [x] **Directory write-path optimization** — a directory mutation no longer
  rewrites the whole directory blob. The blob is split into 4 KiB pages (one
  record each), so create/unlink rewrites only the touched page via addressable
  `WriteAt` (untouched pages carry over by ref, no re-hash/re-encrypt); and the
  per-segment Bloom filter is rebuilt lazily (amortized, on accumulated
  removals) instead of on every unlink. `BenchmarkCreateUnlink` on a 4000-entry
  directory dropped ~35× (5.99 ms → 170 µs/op) with lookup latency unchanged.
- [x] **Zero-alloc metadata commit** — `cow.Commit` now serializes the
  bitmap+dedup+inode snapshot straight into one reusable buffer (`MarshalInto`)
  instead of allocating a fresh slice per inode. A commit on a 4000-inode image
  went from 4047 allocs / 1.9 MB to 41 allocs / 94 KB (and the allocation cost is
  now flat regardless of inode count); the on-disk bytes are byte-identical.
- [ ] **Stage F** — Rust port (and real BLAKE3, see notes)

## Design pipeline (data path)

```
plaintext → BLAKE hash → dedup lookup
   HIT  → RefCount++            (no compress, no encrypt, no write)
   MISS → ZSTD → AES-XTS (tweak = cluster addr) → write → record checksum
```

Compression runs before encryption (ciphertext is incompressible). The dedup
hash is computed over the *uncompressed* block, which both enables the
short-circuit and makes dedup robust to compressor settings. The same hash
doubles as an end-to-end integrity check on read.

## Notes

- **Dedup hash:** currently **BLAKE2b-256** (via `golang.org/x/crypto`) as a
  drop-in for BLAKE3 — same cryptographic role; BLAKE3 lands with the Rust port.
- **CoW commits:** an "uberblock" in two alternating slots carries a sequence
  number and a checksum. A commit writes the metadata snapshot, then flips the
  uberblock; a torn write is rejected by the checksum and mount rolls back to the
  last consistent commit.
- The full design specification (Russian) is in [docs/SPEC.md](docs/SPEC.md) and
  drives the staged implementation — including the resolved decisions on
  Copy-on-Write, deduplication, recordsize, and compression/encryption modes.
- The architecture and on-disk format are now **frozen** (SPEC §E10); the Rust
  port is planned in [docs/PORTING.md](docs/PORTING.md) — layer-by-layer order,
  crate mapping, the bloom-library split, and the deferred-optimization backlog.

## Build & test

```sh
go build ./...
go test ./...
```

Requires Go 1.26+.

## Mounting

```sh
go build -o bloomfs ./cmd/bloomfs

./bloomfs format -size 64 disk.img          # 64 MiB plaintext image
./bloomfs mount disk.img /mnt/bloom         # mount until Ctrl-C / umount
```

For an encrypted pool pass a 32- or 64-byte key as hex to both commands
(`-key <hex>`); an empty key mounts a plaintext pool. Metadata is made durable on
`fsync` and on a clean unmount (CoW commit); a hard crash rolls back to the last
commit. Mounting needs a FUSE-capable host (`/dev/fuse`, `fusermount3`).

## License

MIT — see [LICENSE](LICENSE).
