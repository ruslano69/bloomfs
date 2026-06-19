// Package dir implements the BloomFS directory subsystem (§3 of the spec):
// a logical directory is a linked list of fixed-capacity "virtual segments",
// each guarded by a blocked Bloom filter so that a lookup for an absent file
// returns without touching the index. Names are keyed by XXH3.
//
// Stage A (in-memory only): no disk, no inodes on disk — InodeID is just an
// opaque handle. The goal is to validate the search architecture and reproduce
// the nanosecond-scale lookups from the design brief.
package dir

import (
	"unsafe"

	bloom "github.com/ruslano69/xxh3-bloom"
)

// InodeID is an opaque reference to a file's metadata (§4.2: the directory maps
// name -> InodeID; the inode itself never stores the name).
type InodeID uint64

const (
	// MaxFilesPerSegment caps one virtual segment (§3.1). Overflow opens a new
	// segment, keeping every Bloom filter small and accurate.
	MaxFilesPerSegment = 4000
	// BloomFP is the target false-positive rate per segment (§3.1).
	BloomFP = 0.01
)

// dirEntry is one name -> inode link. The name is kept for listing and for the
// final collision check after the Bloom filter and hash-index both admit a hit
// (§3.2).
type dirEntry struct {
	name  string
	inode InodeID
}

// segment is one virtual segment: a blocked Bloom filter in front of a hash
// index. The index is keyed by xxh3(name); the value is a (tiny) collision
// chain so that two distinct names sharing a 64-bit hash stay correct.
type segment struct {
	filter *bloom.BlockedFilter
	index  map[uint64][]dirEntry
	count  int
	cap    int     // capacity; default MaxFilesPerSegment, configurable for tuning
	fp     float64 // target false-positive rate for this segment's filter
}

func newSegment(capacity int, fp float64) *segment {
	return &segment{
		// NewBlockedTuned guarantees the *measured* FP rate is <= fp at the cost
		// of ~20-35% more bits — correctness first for the prototype. Swap to
		// NewBlocked for the smallest filter if memory ever matters more.
		filter: bloom.NewBlockedTuned(uint(capacity), fp),
		index:  make(map[uint64][]dirEntry, capacity),
		cap:    capacity,
		fp:     fp,
	}
}

// s2b returns a zero-copy, READ-ONLY view of s as bytes. The result must never
// be mutated or retained past the call. This keeps the hot Test path
// allocation-free (a plain []byte(s) would copy).
func s2b(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

func (s *segment) full() bool { return s.count >= s.cap }

// add links name -> inode. The caller guarantees name is not already present in
// this segment.
func (s *segment) add(h uint64, name string, inode InodeID) {
	s.filter.Add(s2b(name))
	s.index[h] = append(s.index[h], dirEntry{name: name, inode: inode})
	s.count++
}

// find resolves name within this segment. The Bloom filter answers "definitely
// absent" in one cache miss; on a maybe-hit we confirm against the index and
// verify the stored name (guards against the astronomically rare xxh3
// collision, §3.2).
func (s *segment) find(h uint64, name string) (InodeID, bool) {
	if !s.filter.Test(s2b(name)) {
		return 0, false // Bloom guarantees absence — no index access
	}
	for _, e := range s.index[h] {
		if e.name == name {
			return e.inode, true
		}
	}
	return 0, false // Bloom false positive (or hash-only collision)
}

// remove deletes name and rebuilds the filter from the surviving keys. A
// classic Bloom filter cannot clear a single key (shared bits), so on any
// removal we rebuild from scratch — microseconds for <=4000 keys (§3.3).
func (s *segment) remove(h uint64, name string) bool {
	chain := s.index[h]
	for i, e := range chain {
		if e.name == name {
			chain[i] = chain[len(chain)-1]
			if len(chain) == 1 {
				delete(s.index, h)
			} else {
				s.index[h] = chain[:len(chain)-1]
			}
			s.count--
			s.rebuildFilter()
			return true
		}
	}
	return false
}

// rebuildFilter discards the old filter and rebuilds it from the live keys
// (§3.3). In a concurrent design the new filter would be swapped in atomically;
// Stage A is single-threaded.
func (s *segment) rebuildFilter() {
	f := bloom.NewBlockedTuned(uint(s.cap), s.fp)
	for _, chain := range s.index {
		for _, e := range chain {
			f.Add(s2b(e.name))
		}
	}
	s.filter = f
}
