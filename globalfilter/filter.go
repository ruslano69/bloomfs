// Package globalfilter implements Phase P0 of the BloomFS plan: one Bloom filter
// for the WHOLE filesystem (keyed by the (directory, name) pair) plus an exact
// tombstone set, replacing the per-directory Bloom filter.
//
// Why (BenchmarkFlatVsSegmented, docs/PLAN-next.md): a per-directory filter is
// sized for one directory's population, so for the common case (small dirs) it
// sits mostly empty and still costs an extra indirection — a plain map beats it
// at every size up to 10 000 entries. A filter only pays off when it is dense
// and compact relative to the map's bucket array, which happens at *filesystem*
// scale, not directory scale. So: move the filter up one level.
//
//	key = xxh3(dir_inode_id ‖ name)   — scoped per directory, globally unique.
//	                                    realized as xxh3 seeded by dir inode id
//	                                    (zero-alloc; no concat buffer).
//
// Directories themselves become plain maps (no segments, no per-dir filter).
// This component owns them, so a Lookup that the filter rejects never touches a
// directory map at all — the win the design is after.
package globalfilter

import (
	"sync"

	bloom "github.com/ruslano69/xxh3-bloom"
	"github.com/zeebo/xxh3"
)

// InodeID is an opaque reference to a file's metadata (matches dir.InodeID;
// kept local so this package has no dependency on dir).
type InodeID = uint64

// DefaultRebuildThreshold is how many tombstones accumulate before the filter is
// rebuilt from the live set. 16 000 × ~16 B ≈ 256 KB — negligible; rebuild walks
// the in-RAM directories and is amortized O(1) per delete (docs/PLAN-next.md).
const DefaultRebuildThreshold = 16_000

// targetFP is the filter's false-positive rate. At 1 % and ~10 bits/entry, 1 M
// files cost ~1.25 MB — fits in L2/L3.
const targetFP = 0.01

type entry struct {
	name  string
	inode InodeID
}

// dirState is one directory's resident map plus a hotness flag. The flag lives
// INSIDE the directory record on purpose: a single `dirs[dirIno]` probe yields
// both "is this directory resident?" and its map, so the residency check costs
// nothing beyond the map fetch the hit path needs anyway. (A separate resident
// set would be a second map touch on every lookup — measurable at scale.) In the
// real FS this record is the dir-cache entry (§B11); hot mirrors cache presence.
type dirState struct {
	index map[uint64][]entry // hash-low-word -> collision chain
	hot   bool               // map is loaded/authoritative -> Lookup goes map-first
}

// GlobalFilter is the filesystem-wide name index: a single Bloom filter and an
// exact tombstone set in front of plain-map directories.
//
// Concurrency: its own RWMutex makes it self-contained and -race-safe. Lookups
// take the read lock (parallel); Create/Unlink take the write lock. The write
// lock spanning all of Create (clear-tombstone → filter.Add → make-visible)
// is what guarantees a concurrent Lookup never observes a half-applied create.
type GlobalFilter struct {
	mu      sync.RWMutex
	filter  *bloom.BlockedFilter
	deleted map[uint64]struct{}  // tombstones, keyed by the hash low word; also a negative cache
	dirs    map[uint64]*dirState // dirIno -> resident map + hotness
	live    int                  // live entry count (filter sizing)

	rebuildThreshold int
}

// New returns an empty GlobalFilter sized for a small filesystem; it grows on
// rebuild as the live count climbs.
func New() *GlobalFilter { return NewSized(1024) }

// NewSized pre-sizes the filter for an expected live-entry count (e.g. taken
// from the inode table at mount).
func NewSized(expected int) *GlobalFilter {
	if expected < 1 {
		expected = 1
	}
	return &GlobalFilter{
		filter:           bloom.NewBlockedTuned(uint(expected), targetFP),
		deleted:          make(map[uint64]struct{}),
		dirs:             make(map[uint64]*dirState),
		rebuildThreshold: DefaultRebuildThreshold,
	}
}

// key scopes a name to its directory and returns its full 128-bit xxh3 hash.
// Seeding by the directory inode id is the zero-alloc realization of
// xxh3(dir_inode_id ‖ name): the same name in two directories yields two
// independent hashes. The 128-bit form feeds the filter's hashed-input fast path
// (AddHash/TestHash) directly — no []byte key, no second hash pass — while the
// low word doubles as the directory-map bucket and tombstone key.
func key(dirIno uint64, name string) (hi, lo uint64) {
	h := xxh3.HashString128Seed(name, dirIno)
	return h.Hi, h.Lo
}

// Lookup resolves (dirIno, name) -> inode.
//
// One `dirs[dirIno]` probe drives the branch: if the directory is resident its
// map is authoritative, so we go map-first and skip the filter and the tombstone
// set entirely — the filter is purely a cold-directory optimization. If it is
// cold, the filter rejects a true miss in one cache miss without touching (in the
// real FS, without loading) the map, and the tombstone set masks bits left stale
// by deletions since the last rebuild.
func (g *GlobalFilter) Lookup(dirIno uint64, name string) (InodeID, bool) {
	hi, lo := key(dirIno, name)
	g.mu.RLock()
	defer g.mu.RUnlock()

	d := g.dirs[dirIno] // single probe: yields residency AND the map
	if d == nil || !d.hot {
		if !g.filter.TestHash(hi, lo) {
			return 0, false // Bloom guarantees absence — no map access / load
		}
		if _, dead := g.deleted[lo]; dead {
			return 0, false // precise: deleted, filter bits not yet rebuilt
		}
	}
	if d == nil {
		return 0, false // cold maybe-hit into an empty directory
	}
	for _, e := range d.index[lo] {
		if e.name == name {
			return e.inode, true
		}
	}
	return 0, false // resident miss, or Bloom false positive
}

// MarkResident records that a directory's map is loaded/hot, so Lookup serves it
// map-first and skips the filter. The fs layer calls this when it loads a
// directory into its cache.
func (g *GlobalFilter) MarkResident(dirIno uint64) {
	g.mu.Lock()
	g.dirEntry(dirIno).hot = true
	g.mu.Unlock()
}

// Evict marks a directory cold again (its map left the cache). Subsequent
// lookups for it go back through the filter.
func (g *GlobalFilter) Evict(dirIno uint64) {
	g.mu.Lock()
	if d := g.dirs[dirIno]; d != nil {
		d.hot = false
	}
	g.mu.Unlock()
}

// dirEntry returns the directory record, creating an empty one if needed.
// Caller holds g.mu.
func (g *GlobalFilter) dirEntry(dirIno uint64) *dirState {
	d := g.dirs[dirIno]
	if d == nil {
		d = &dirState{index: make(map[uint64][]entry)}
		g.dirs[dirIno] = d
	}
	return d
}

// Create links (dirIno, name) -> inode. Returns false if the name already exists
// in that directory. Step order is load-bearing under concurrent Lookup:
//
//  1. clear the tombstone   — so a rebuilt-away key is no longer "deleted"
//  2. add to the filter     — so the key is admitted
//  3. make the entry visible — only now can a reader resolve it
//
// If step 3 ran before step 2, a Lookup interleaving between them could find the
// entry in the map yet see filter.Test == false (post-rebuild) and wrongly
// return ENOENT. The write lock already serializes this; the order documents the
// invariant so correctness does not rest on lock reasoning alone.
func (g *GlobalFilter) Create(dirIno uint64, name string, inode InodeID) bool {
	hi, lo := key(dirIno, name)
	g.mu.Lock()
	defer g.mu.Unlock()

	d := g.dirEntry(dirIno)
	for _, e := range d.index[lo] {
		if e.name == name {
			return false // no duplicate names
		}
	}
	delete(g.deleted, lo)                                              // 1
	g.filter.AddHash(hi, lo)                                           // 2
	d.index[lo] = append(d.index[lo], entry{name: name, inode: inode}) // 3
	g.live++
	return true
}

// Unlink removes (dirIno, name). Returns false if absent. Symmetric order:
// remove from the directory FIRST (no reader can find it), THEN drop a tombstone
// to mask the now-stale filter bits.
func (g *GlobalFilter) Unlink(dirIno uint64, name string) bool {
	_, lo := key(dirIno, name)
	g.mu.Lock()
	defer g.mu.Unlock()

	d := g.dirs[dirIno]
	if d == nil {
		return false
	}
	chain := d.index[lo]
	for i, e := range chain {
		if e.name == name {
			chain[i] = chain[len(chain)-1] // 1: remove from directory
			if len(chain) == 1 {
				delete(d.index, lo)
			} else {
				d.index[lo] = chain[:len(chain)-1]
			}
			g.live--
			g.deleted[lo] = struct{}{} // 2: tombstone masks stale filter bits
			if len(g.deleted) >= g.rebuildThreshold {
				g.rebuildLocked()
			}
			return true
		}
	}
	return false
}

// ForceRebuild rebuilds the filter from the live set and clears all tombstones.
// Exposed for tests; production triggers it from Unlink at RebuildThreshold.
func (g *GlobalFilter) ForceRebuild() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rebuildLocked()
}

// rebuildLocked rebuilds the filter sized for the current live count, re-adding
// every live key, then resets the tombstone set. Caller holds g.mu.
func (g *GlobalFilter) rebuildLocked() {
	size := g.live
	if size < 1 {
		size = 1
	}
	nf := bloom.NewBlockedTuned(uint(size), targetFP)
	for dirIno, d := range g.dirs {
		for _, chain := range d.index {
			for _, e := range chain {
				hi, lo := key(dirIno, e.name)
				nf.AddHash(hi, lo)
			}
		}
	}
	g.filter = nf
	g.deleted = make(map[uint64]struct{})
}

// Len returns the number of live entries.
func (g *GlobalFilter) Len() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.live
}

// Tombstones returns the current tombstone count (for tests/telemetry).
func (g *GlobalFilter) Tombstones() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.deleted)
}
