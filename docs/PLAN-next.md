# Implementation plan — next session

Two remaining "honest scope" items toward a POSIX-clean filesystem. These are
not bugs but deliberate scope edges left after the five durability/robustness
fixes that are now at Rust↔Go parity.

- **A.** `..` in readdir reports the directory itself instead of its real parent.
- **B.** `rename` ignores `renameat2` flags (`RENAME_NOREPLACE`, `RENAME_EXCHANGE`).

> Scope correction: "rename only within-device / EXDEV on cross" is **not** a
> bug — cross-filesystem rename must return `EXDEV` per POSIX, and the Go binding
> already does this. The real rename-completeness gap is the `renameat2` flags.

## Grounding fact (already checked)

The on-disk inode is exactly **128 bytes, fully packed** (64-byte header +
64-byte BlockMap). The only free space is **3 reserved bytes (offsets 53–55)**.
There is no room for an 8-byte `parent` pointer without a format migration.
→ Decision for A: track parent **in RAM at the FUSE binding layer**, not on disk.

---

## Phase 0 — recon (do first, ~0.5h)

1. Confirm parent does not fit on disk (done: only 3 reserved bytes). → RAM tracking.
2. Go: does go-fuse's `fs` package synthesize `.`/`..` itself, and is
   `fs.Inode.Parent()`/`Parents()` available? If yes, A is nearly free in Go
   (read parent ino from the node tree).
3. Rust: confirm the self-report site (`snapshot_dir` pushes `..` with `ino: ino`).
4. renameat2: confirm flag values arrive — fuser `rename` already has `flags: u32`,
   go-fuse `Rename(... flags uint32)`. `RENAME_NOREPLACE=1`, `RENAME_EXCHANGE=2`.
5. Run existing rename/readdir tests (Rust + Go) as a baseline.

---

## Phase A — real parent for `..`

**Design: track parent in RAM at the binding layer, not on disk.**
Rationale: (1) no 8 free bytes on disk without migrating the 128-byte format —
too costly/risky just for `..`; (2) the kernel always walks top-down
(`lookup(parent, child)` before `readdir(child)`), so by the time `..` is needed
the parent has been seen; (3) this is how go-fuse loopback filesystems do it.
Academically defensible and reversible.

### Rust (`bloomfs-fuse/src/lib.rs`) — flagship first
- Add `parents: HashMap<u64, u64>` (child ino → parent ino) to `BloomFuse`.
- Populate in `lookup` (when the child is a directory), `mkdir`, `rename` (a
  moved directory's parent changes), and `root → root` at startup.
- `..` row uses `*self.parents.get(&ino).unwrap_or(&ino)` instead of self.
  Snapshot is built in `opendir`; either pass the parent into `snapshot_dir` or
  substitute the `..` ino in `opendir`/`readdir`.
- Test: `mkdir a/b`, `opendir(b)`, assert `..` carries ino `a`; after
  `rename(b → c/b)`, `..` carries ino `c`.

### Go (`fusefs/fusefs.go`) — parity
- If Phase 0 confirms `fs.Inode.Parent()` — read the parent ino from the node
  tree; minimal change (or go-fuse is already correct → close it "for free" and
  record that in the commit).
- Otherwise mirror the RAM map as in Rust.

**Risk:** the map is incomplete right after mount until first traversal.
Irrelevant for `..` (kernel walks top-down). Document the assumption. Root is
always `parent = self`.

---

## Phase B — `renameat2` flags

Depends on A: `RENAME_EXCHANGE` on directories must update parent tracking and
run the cycle check in both directions.

### FS layer (Rust `bloomfs-fs`, Go `fs`)
- Thread `flags: u32` into `rename` (or add `rename2`; old one becomes a
  `flags=0` wrapper).
- `RENAME_NOREPLACE`: if dst exists → `EEXIST` (`Error::Exists` / `ErrExists`),
  no replace.
- `RENAME_EXCHANGE`: atomic swap of two entries. Both names must exist (else
  `ENOENT`). nlink untouched (swap, not link/unlink). Bump ctime on both. If both
  are directories in different parents: (1) descendant-cycle check **both ways**
  via the existing `dir_subtree_contains`; (2) update parent tracking for both
  (from Phase A).
- Conflicting flags (`NOREPLACE | EXCHANGE`) → `EINVAL`.

### Binding layer
- Rust `rename`: parse `flags`, unknown (besides 0/1/2) → `EINVAL`/`ENOSYS`.
- Go `Rename`: same; map to the new `FS.Rename2`.

**Tests (mirror each other):**
- NOREPLACE onto existing dst → EEXIST; onto free name → ok.
- EXCHANGE of two files → names swapped, inodes unchanged, nlink unchanged.
- EXCHANGE of two dirs in different branches → ok and parent updated for both.
- EXCHANGE that would create a cycle → EINVAL.
- `NOREPLACE|EXCHANGE` → EINVAL.

---

## Phase C — parity, runs, commits

- Mirror tests Rust↔Go.
- Rust: `cargo test`, `cargo clippy` clean.
- Go: `go test ./...`, `go vet`, `gofmt -l`, key tests under `-race`.
- Commit **per phase** (A separate, B separate), `fix:`/`feat:` with the "why",
  same pattern as the prior five fixes. Push.

---

## Order & estimate

| Step | What | Est. |
|------|------|------|
| 0 | Recon + baseline tests | 0.5 h |
| A | parent for `..` (Rust → Go) | 1.5–2 h |
| B | renameat2 flags (FS → binding, Rust → Go) | 2–3 h |
| C | parity, runs, 2 commits | 1 h |
| D | commit dispatcher (dirty threshold + last-writer-close + idle timer) | 3.5 h |

Do **A → B** (EXCHANGE relies on A's parent tracking). Rust first (flagship),
then port to Go in one sitting.

**Main risk to keep in mind:** the temptation to put parent on disk. Don't — that
is a 128-byte format migration just for `..`; the cost is disproportionate. RAM
tracking at the binding closes it cleanly.

---

## Phase D — commit dispatcher (durability without false promises)

**Problem:** currently a `Commit` happens only on explicit `fsync(2)` or clean
unmount. `close(fd)` is a no-op (`Flush` returns 0). A hard crash between the
last write and the next `fsync`/`umount` loses everything written since the last
commit — window is unbounded.

Time-based background commits (`commit=5s` à la ext4) are the wrong unit: one
file that is half the disk is one "change" regardless of elapsed time. Count of
mutations is equally meaningless. The right unit is **signals the filesystem can
actually observe**.

### Three signals, in priority order

**Signal 1 — dirty-bytes threshold (strongest)**
Track `dirtyBytes uint64` on the `FS` struct: increment by the number of bytes
written on every `WriteAt`/`Append`. When `dirtyBytes` exceeds a configurable
threshold (default 64 MiB), trigger a `Commit` synchronously before returning
from the write call. Reset to 0 after every `Commit`.
- No goroutine needed. No race — already inside the write-lock.
- Prevents unbounded RAM growth. Covers streaming writers that never `fsync`.

**Signal 2 — last writer closes (semantic)**
Track `writeOpens int` per inode (or globally as a count of handles opened with
write intent). Increment in `Open`/`Create` when `flags` include `O_WRONLY` or
`O_RDWR`. Decrement in `Handle.Close()`. When `writeOpens` reaches 0 (and at
least one write happened since the last commit), trigger `Commit`.
- Closest to "the application said I'm done" without requiring explicit `fsync`.
- Does not help long-lived writers (SQLite, databases keep the file open
  continuously) — but for those, the application is expected to call `fsync`.

**Signal 3 — write-inactivity timer (weakest, opt-in)**
A background goroutine/thread wakes every `T` (mount option, default disabled).
If `dirtyBytes > 0` and the last write was more than `T` ago, `Commit`. This
covers burst workloads that pause naturally but never close the file.
- Requires a goroutine and a timestamp (`lastWriteAt time.Time`).
- Off by default; enabled with `-commit-idle=500ms` or similar mount option.
- Least reliable of the three: a streaming writer has no idle window.

### What this does NOT claim to solve
A complex structure (database, VM image) spread across multiple files or with
internal consistency invariants — only the application knows when its data is
in a consistent state. The dispatcher solves the "forgot to fsync, power died"
case, not the "half-written B-tree" case. Document this explicitly.

### Implementation sketch

```
FS struct additions:
  dirtyBytes  uint64        // reset on every Commit
  writeOpens  int           // count of write-intent handles currently open
  lastWriteAt time.Time     // for idle timer (only if enabled)

DirtyThreshold = 64 << 20  // 64 MiB default, overridable via mount option

on WriteAt / Append (already under write-lock):
  dirtyBytes += bytesWritten
  lastWriteAt = now()
  if dirtyBytes >= DirtyThreshold { Commit(); dirtyBytes = 0 }

on Open/Create with O_WRONLY|O_RDWR:
  writeOpens++

on Handle.Close() (already under lock):
  if handle.writable { writeOpens-- }
  if writeOpens == 0 && dirtyBytes > 0 { Commit(); dirtyBytes = 0 }

background goroutine (opt-in):
  ticker := time.NewTicker(idleInterval)
  for range ticker.C {
      if dirtyBytes > 0 && time.Since(lastWriteAt) >= idleInterval {
          fs.mu.Lock(); Commit(); dirtyBytes = 0; fs.mu.Unlock()
      }
  }
```

Rust mirrors this exactly. Signal 1 and 2 are always on; Signal 3 is a
`--commit-idle=<duration>` CLI flag passed through mount options.

### Estimate

| Step | What | Est. |
|------|------|-------|
| D1 | dirty-bytes threshold (Go + Rust) | 1 h |
| D2 | last-writer-close (Go + Rust) | 1 h |
| D3 | idle timer, mount option, tests | 1.5 h |

Do D1 → D2 → D3. Each is independently shippable; stop after D2 if D3 feels
like over-engineering for now.

---

## Status going in

All five prior fixes are at Rust↔Go parity, tests green under `-race`, pushed:
statfs deferred-free accounting, auto-commit on ENOSPC, bounded LRU dir cache,
rename descendant guard, fallocate(2). The readdir cookie fix is done in Rust
(per-fh frozen snapshot); in Go it is provided by go-fuse's per-handle DirStream
caching (no code change needed).
