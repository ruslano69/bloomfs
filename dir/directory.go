package dir

import "github.com/zeebo/xxh3"

// Directory is a logical directory: a linked list of virtual segments (§3.1).
// A lookup walks the segments, and each segment's blocked Bloom filter rejects
// non-members before any index access.
//
// Not safe for concurrent use in Stage A. Granular per-segment locking and
// atomic filter swap come later (§B6).
type Directory struct {
	segments []*segment
	cap      int     // per-segment capacity
	fp       float64 // per-segment target false-positive rate
}

// New returns an empty directory with one segment ready, using the default
// per-segment capacity and FP.
func New() *Directory { return newWithCap(MaxFilesPerSegment, BloomFP) }

// newWithCap builds a directory with a custom per-segment capacity and target
// FP. Exposed (package-internal) for tuning sweeps; production uses New().
func newWithCap(capacity int, fp float64) *Directory {
	return &Directory{
		segments: []*segment{newSegment(capacity, fp)},
		cap:      capacity,
		fp:       fp,
	}
}

// nameHash is the directory's single source of name hashing (XXH3, §3.4).
func nameHash(name string) uint64 { return xxh3.HashString(name) }

// Add links name -> inode. It returns false if name already exists anywhere in
// the directory (no duplicate names).
func (d *Directory) Add(name string, inode InodeID) bool {
	h := nameHash(name)
	for _, s := range d.segments {
		if _, ok := s.find(h, name); ok {
			return false
		}
	}
	// Reuse space freed by deletions: fill the first non-full segment, else open
	// a new "continuation" segment.
	for _, s := range d.segments {
		if !s.full() {
			s.add(h, name, inode)
			return true
		}
	}
	s := newSegment(d.cap, d.fp)
	d.segments = append(d.segments, s)
	s.add(h, name, inode)
	return true
}

// Find resolves name -> inode, scanning segments until a Bloom filter admits a
// hit. Returns (0, false) if the file is absent.
func (d *Directory) Find(name string) (InodeID, bool) {
	h := nameHash(name)
	for _, s := range d.segments {
		if inode, ok := s.find(h, name); ok {
			return inode, true
		}
	}
	return 0, false
}

// Delete unlinks name. Returns false if it was not present.
func (d *Directory) Delete(name string) bool {
	h := nameHash(name)
	for _, s := range d.segments {
		if s.remove(h, name) {
			return true
		}
	}
	return false
}

// Len is the total number of entries across all segments.
func (d *Directory) Len() int {
	n := 0
	for _, s := range d.segments {
		n += s.count
	}
	return n
}

// Segments reports how many virtual segments back this directory.
func (d *Directory) Segments() int { return len(d.segments) }

// List returns every name in the directory (readdir, §B12). The Bloom filter is
// useless for listing, so this walks all segments. Order is unspecified.
func (d *Directory) List() []string {
	names := make([]string, 0, d.Len())
	for _, s := range d.segments {
		for _, chain := range s.index {
			for _, e := range chain {
				names = append(names, e.name)
			}
		}
	}
	return names
}
