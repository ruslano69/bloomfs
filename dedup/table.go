// Package dedup is the in-RAM deduplication table (§5.2): a content hash maps to
// where the one physical copy of a block lives plus a reference count. On-disk
// DDT persistence and crash recovery are §B3.
package dedup

// Key is the content hash of an uncompressed logical block (§5.1). 256 bits is
// trusted without byte-verify (§B7, §D-2).
type Key [32]byte

// Entry locates the single stored copy of a block and counts references to it.
type Entry struct {
	Start   uint64 // first cluster
	Count   uint32 // clusters occupied
	Payload uint32 // bytes of stored payload (compressed, or raw if Raw)
	Logical uint32 // uncompressed bytes
	Raw     bool   // payload stored uncompressed (compression did not help, §B9)
	Refs    uint32 // reference count
}

// Table maps content hashes to entries.
type Table struct {
	m map[Key]*Entry
}

// New returns an empty table.
func New() *Table { return &Table{m: make(map[Key]*Entry)} }

// Lookup returns the entry for k (a copy) and whether it exists.
func (t *Table) Lookup(k Key) (Entry, bool) {
	if e, ok := t.m[k]; ok {
		return *e, true
	}
	return Entry{}, false
}

// Add inserts a new unique block with reference count 1.
func (t *Table) Add(k Key, e Entry) {
	e.Refs = 1
	cp := e
	t.m[k] = &cp
}

// Incr bumps the reference count of an existing block. Returns false if absent.
func (t *Table) Incr(k Key) bool {
	if e, ok := t.m[k]; ok {
		e.Refs++
		return true
	}
	return false
}

// Decr drops one reference. When it reaches zero the entry is removed and
// (entry, true) is returned so the caller can free the clusters (§5.4).
func (t *Table) Decr(k Key) (Entry, bool) {
	e, ok := t.m[k]
	if !ok {
		return Entry{}, false
	}
	e.Refs--
	if e.Refs == 0 {
		delete(t.m, k)
		return *e, true
	}
	return Entry{}, false
}

// Len is the number of unique blocks tracked.
func (t *Table) Len() int { return len(t.m) }
