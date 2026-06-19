package fs

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// benchFS formats an in-memory pool and fills the root with `files` empty files,
// returning the FS plus the names and ids in creation order. The device is sized
// generously so write benchmarks (which defer cluster frees until a commit) do
// not exhaust the bitmap between periodic commits.
func benchFS(b *testing.B, files int) (*FS, uint64, []string, []uint64) {
	b.Helper()
	return benchFSOn(b, block.NewMem(16384), files) // 64 MiB pool
}

// benchFSFile is benchFS backed by a real file image, so a Commit pays a true
// fsync (MemDevice.Sync is a no-op). Point TMPDIR at the medium under test
// (tmpfs vs. a real SSD/NVMe) to compare commit cost across storage.
func benchFSFile(b *testing.B, files int) (*FS, uint64, []string, []uint64) {
	b.Helper()
	dev, err := block.Create(filepath.Join(b.TempDir(), "disk.img"), 16384)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { dev.Close() })
	return benchFSOn(b, dev, files)
}

// benchFSOn formats dev and fills the root with `files` empty files, shared by
// the in-memory and file-image variants above.
func benchFSOn(b *testing.B, dev block.Device, files int) (*FS, uint64, []string, []uint64) {
	b.Helper()
	f, err := Format(dev, testKey())
	if err != nil {
		b.Fatal(err)
	}
	root := f.Root()
	names := make([]string, files)
	ids := make([]uint64, files)
	for i := range names {
		names[i] = fmt.Sprintf("file_%06d.dat", i)
		id, err := f.Create(root, names[i])
		if err != nil {
			b.Fatalf("create %d: %v", i, err)
		}
		ids[i] = id
		// Commit periodically: each Create rewrites the whole directory blob and
		// defers the old one's clusters until a commit, so building thousands of
		// files in one transaction would otherwise exhaust the bitmap.
		if i%256 == 255 {
			if err := f.Commit(); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := f.Commit(); err != nil {
		b.Fatal(err)
	}
	return f, root, names, ids
}

// pseudoRandom fills buf with an incompressible LCG stream, so the compress and
// encrypt stages do real work rather than collapsing a run of zeros.
func pseudoRandom(buf []byte) {
	x := uint64(0x9e3779b97f4a7c15)
	for i := range buf {
		x = x*6364136223846793005 + 1442695040888963407
		buf[i] = byte(x >> 56)
	}
}

// BenchmarkLookupHit measures the hot Lookup path through the full FS: RWMutex
// RLock + open-directory cache hit + Bloom-segmented index hit. This is the
// real-world cost of the project's headline feature (vs. the raw dir.Find micro).
func BenchmarkLookupHit(b *testing.B) {
	f, root, names, _ := benchFS(b, 4000) // 2 segments
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, _ := f.Lookup(root, names[i%len(names)]); !ok {
			b.Fatal("miss on present name")
		}
	}
}

// BenchmarkLookupMiss measures lookup of an absent name: the Bloom filter should
// reject without touching the index.
func BenchmarkLookupMiss(b *testing.B) {
	f, root, _, _ := benchFS(b, 4000)
	miss := make([]string, 4000)
	for i := range miss {
		miss[i] = fmt.Sprintf("absent_%06d.dat", i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, _ := f.Lookup(root, miss[i%len(miss)]); ok {
			b.Fatal("hit on absent name")
		}
	}
}

// BenchmarkStat measures the getattr path: RLock + inode-table fetch (a copy).
func BenchmarkStat(b *testing.B) {
	f, _, _, ids := benchFS(b, 4000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.Stat(ids[i%len(ids)]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParallelLookupHit shows read-side concurrency: many goroutines hold
// the RWMutex read lock at once, so the Bloom-segmented index serves parallel
// readers without contention on the writer lock.
func BenchmarkParallelLookupHit(b *testing.B) {
	f, root, names, _ := benchFS(b, 4000)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, ok, _ := f.Lookup(root, names[i%len(names)]); !ok {
				b.Fatal("miss on present name")
			}
			i++
		}
	})
}

// BenchmarkCreateUnlink measures one metadata-churn cycle (create + unlink) in a
// directory already holding ~4000 entries. Since the page-aligned directory
// layout + deferred Bloom-filter rebuild, each cycle rewrites only the one
// touched 4 KiB page and skips the filter rebuild, so this is the per-op CPU
// cost of the mutation itself. Commits run every batch to reclaim deferred-freed
// clusters, with the timer paused so commit cost does not pollute the per-op
// number (see BenchmarkCreateUnlinkCommit for the commit-inclusive figure).
func BenchmarkCreateUnlink(b *testing.B) {
	f, root, _, _ := benchFS(b, 4000)
	const name = "churn.tmp"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.Create(root, name); err != nil {
			b.Fatal(err)
		}
		if err := f.Unlink(root, name); err != nil {
			b.Fatal(err)
		}
		if i%512 == 511 {
			b.StopTimer()
			if err := f.Commit(); err != nil { // reclaim deferred frees
				b.Fatal(err)
			}
			b.StartTimer()
		}
	}
}

// BenchmarkWriteAt4K measures a single-record (4 KiB) overwrite with unique
// content each iteration: a dedup miss, so the full hash → compress → encrypt →
// write → inode-Put pipeline runs. Periodic commits reclaim the deferred frees
// the overwrite leaves behind.
func BenchmarkWriteAt4K(b *testing.B) {
	f, root, _, _ := benchFS(b, 1)
	id, err := f.Create(root, "w.dat")
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 4096)
	pseudoRandom(buf)
	if err := f.WriteFile(id, buf); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		binary.LittleEndian.PutUint64(buf, uint64(i)) // make this write unique
		if err := f.WriteAt(id, 0, buf); err != nil {
			b.Fatal(err)
		}
		if i%512 == 511 {
			b.StopTimer()
			if err := f.Commit(); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
	}
}

// BenchmarkReadAt4K measures reading a single 4 KiB record back: load ref +
// decrypt + decompress.
func BenchmarkReadAt4K(b *testing.B) {
	f, root, _, _ := benchFS(b, 1)
	id, err := f.Create(root, "r.dat")
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 4096)
	pseudoRandom(buf)
	if err := f.WriteFile(id, buf); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.ReadAt(id, 0, 4096); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteFileDedupHit writes identical 4 KiB content repeatedly: every
// write after the first is a dedup hit, so the pipeline short-circuits before
// compress/encrypt/write (only the hash + refcount bump runs). Contrast with
// BenchmarkWriteAt4K to see the value of the content-hash dedup.
func BenchmarkWriteFileDedupHit(b *testing.B) {
	f, root, _, _ := benchFS(b, 1)
	id, err := f.Create(root, "d.dat")
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 4096)
	pseudoRandom(buf)
	b.SetBytes(4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := f.WriteFile(id, buf); err != nil {
			b.Fatal(err)
		}
	}
}

// commitCycle times one durable metadata transaction: a create+unlink followed
// by a Commit, with the Commit INSIDE the timed region (unlike BenchmarkCreateUnlink,
// which pauses the timer around the commit). On a MemDevice the commit is the
// CoW snapshot serialization only; on a FileDevice it also pays a real fsync, so
// the mem-vs-file delta isolates the cost of durability on the target medium.
func commitCycle(b *testing.B, f *FS, root uint64) {
	b.Helper()
	const name = "churn.tmp"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.Create(root, name); err != nil {
			b.Fatal(err)
		}
		if err := f.Unlink(root, name); err != nil {
			b.Fatal(err)
		}
		if err := f.Commit(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCreateUnlinkCommit is the in-memory baseline: one create+unlink+commit
// per op with no real fsync (MemDevice.Sync is a no-op), so it measures the CoW
// snapshot cost alone.
func BenchmarkCreateUnlinkCommit(b *testing.B) {
	f, root, _, _ := benchFS(b, 4000)
	commitCycle(b, f, root)
}

// BenchmarkCreateUnlinkCommitFile is the file-image counterpart: identical work
// but the per-op Commit fsyncs a real image. Subtract the mem figure to read off
// the fsync/durability tax of the storage TMPDIR points at.
func BenchmarkCreateUnlinkCommitFile(b *testing.B) {
	f, root, _, _ := benchFSFile(b, 4000)
	commitCycle(b, f, root)
}
