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

> Status: **research prototype.** The on-disk layers work end to end and are
> covered by tests (including crash-injection), but this is not a mountable
> filesystem yet — FUSE integration is the next stage.

## Architecture at a glance

| Layer | Package | What it does |
|-------|---------|--------------|
| Directory lookup | `dir` | Bloom-segmented index (XXH3); ~ns lookups, zero alloc |
| Block device | `block` | 4 KiB blocks over an in-memory or file image |
| Metadata | `inode` | 128-byte inodes, 32 per block, inline extents |
| Free space | `alloc` | First-fit contiguous cluster bitmap |
| Dedup | `dedup` | Content-hash table with reference counts |
| Data pipeline | `store` | hash → dedup → ZSTD → AES-XTS → write |
| Durability | `cow` | Copy-on-Write commits with crash recovery |

## Implementation status

- [x] **Stage A** — directory subsystem (Bloom segments + XXH3 index) + tuning sweep
- [x] **Stage B** — block device (memory + file image) + inode serialization + superblock
- [x] **Stage C** — allocator + dedup + compress/encrypt data pipeline
- [x] **Stage D** — Copy-on-Write transactions + crash recovery
- [ ] **Stage E** — FUSE mount, permissions, page cache, quotas
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

## Build & test

```sh
go build ./...
go test ./...
```

Requires Go 1.26+.

## License

MIT — see [LICENSE](LICENSE).
