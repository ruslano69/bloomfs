package inode

import "errors"

// ErrOutOfRange is returned when an inode id falls outside the table capacity.
var ErrOutOfRange = errors.New("inode: id out of range")

// Table is the in-RAM inode table — the Copy-on-Write unit for metadata (§B1).
// All Get/Put are pure RAM operations; the table is made durable only when the
// filesystem serializes it (Marshal) into a CoW metadata snapshot and flips the
// uberblock. Because nothing is written in place, an uncommitted change vanishes
// after a crash, and a committed change is atomic — the property the in-place
// inode store could not give (§E-критический блокер).
//
// Slots are dense and indexed by id. count is the high-water mark: ids in
// [0, count) have been touched (the bump allocator never reuses an id), so only
// those are serialized — an empty filesystem snapshots zero bytes, not the whole
// capacity.
type Table struct {
	inodes []Inode
	count  uint64
}

// NewTable returns an empty table with room for capacity inodes.
func NewTable(capacity uint64) *Table {
	return &Table{inodes: make([]Inode, capacity)}
}

// Cap reports the table capacity (number of inode slots).
func (t *Table) Cap() uint64 { return uint64(len(t.inodes)) }

// Count reports the high-water mark: the number of leading slots that have been
// written and are therefore serialized by Marshal.
func (t *Table) Count() uint64 { return t.count }

// Get returns a copy of inode id. A copy (not a pointer into the table) keeps
// the "mutate then Put" contract: a caller cannot change persisted state without
// going through Put, exactly as the old disk-backed Get returned a fresh decode.
func (t *Table) Get(id uint64) (*Inode, error) {
	if id >= uint64(len(t.inodes)) {
		return nil, ErrOutOfRange
	}
	cp := t.inodes[id]
	return &cp, nil
}

// Put stores inode id, extending the high-water mark if needed.
func (t *Table) Put(id uint64, in *Inode) error {
	if id >= uint64(len(t.inodes)) {
		return ErrOutOfRange
	}
	t.inodes[id] = *in
	if id >= t.count {
		t.count = id + 1
	}
	return nil
}

// MarshalLen reports how many bytes Marshal/MarshalInto will write: the leading
// count slots at Size bytes each.
func (t *Table) MarshalLen() int { return int(t.count * Size) }

// MarshalInto serializes slots [0, count) into dst (which must be at least
// MarshalLen bytes) and returns the number of bytes written. It allocates
// nothing — the commit path reuses one buffer across snapshots.
func (t *Table) MarshalInto(dst []byte) int {
	for i := uint64(0); i < t.count; i++ {
		t.inodes[i].marshalInto(dst[i*Size : (i+1)*Size])
	}
	return int(t.count * Size)
}

// Marshal serializes slots [0, count) as count*Size bytes (the explicit on-disk
// inode encoding, §B15). The snapshot lands in the CoW metadata slot.
func (t *Table) Marshal() []byte {
	buf := make([]byte, t.MarshalLen())
	t.MarshalInto(buf)
	return buf
}

// UnmarshalTable reconstructs a table of the given capacity from a snapshot
// produced by Marshal. The snapshot length must be a whole number of inodes and
// must not exceed capacity.
func UnmarshalTable(b []byte, capacity uint64) (*Table, error) {
	if len(b)%Size != 0 {
		return nil, ErrBadInodeSize
	}
	n := uint64(len(b)) / Size
	if n > capacity {
		return nil, ErrOutOfRange
	}
	t := NewTable(capacity)
	for i := uint64(0); i < n; i++ {
		if err := t.inodes[i].UnmarshalBinary(b[i*Size : (i+1)*Size]); err != nil {
			return nil, err
		}
	}
	t.count = n
	return t, nil
}
