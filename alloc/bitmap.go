// Package alloc implements the free-space allocator: a bitmap of clusters
// (§B2). One bit per cluster, set = used. Allocation is first-fit contiguous so
// that, where possible, a logical block's clusters land in one run (extents,
// §4.4). The bitmap lives in RAM and (de)serializes for persistence; on-disk
// flushing and crash recovery are wired in later stages (§B2, §B3).
package alloc

import (
	"encoding/binary"
	"errors"
)

var (
	ErrNoSpace   = errors.New("alloc: no contiguous run of the requested size")
	ErrZeroCount = errors.New("alloc: zero count")
	ErrShort     = errors.New("alloc: serialized bitmap too short")
)

// Bitmap tracks which clusters are used.
//
// deferred holds clusters freed during the current (uncommitted) transaction.
// Their bits stay SET so they cannot be reallocated and overwrite data the last
// commit still references; the durability layer applies them (ApplyDeferred)
// only after a commit's uberblock flip, so they become reusable one commit
// later. This is the deferred-free / pinned-extent discipline of ZFS/Btrfs and
// is what keeps "no grey zone" true for overwrite-then-crash (§B2, §E, §F1).
// It is transient RAM state: a fresh Mount starts with none pending.
type Bitmap struct {
	bits     []uint64
	total    uint64
	used     uint64
	deferred []freeRange
}

// freeRange is a contiguous run pending deferred free.
type freeRange struct{ start, count uint64 }

// New returns a bitmap for total clusters, all free.
func New(total uint64) *Bitmap {
	return &Bitmap{bits: make([]uint64, (total+63)/64), total: total}
}

func (b *Bitmap) get(i uint64) bool { return b.bits[i>>6]&(1<<(i&63)) != 0 }
func (b *Bitmap) set(i uint64)      { b.bits[i>>6] |= 1 << (i & 63) }
func (b *Bitmap) clr(i uint64)      { b.bits[i>>6] &^= 1 << (i & 63) }

// Reserve marks [start, start+count) used — e.g. the superblock and inode table
// that the allocator must never hand out.
func (b *Bitmap) Reserve(start, count uint64) {
	for i := start; i < start+count && i < b.total; i++ {
		if !b.get(i) {
			b.set(i)
			b.used++
		}
	}
}

// Alloc finds the first contiguous free run of count clusters, marks it used and
// returns its start. Returns ErrNoSpace if no such run exists.
func (b *Bitmap) Alloc(count uint64) (uint64, error) {
	if count == 0 {
		return 0, ErrZeroCount
	}
	var run, start uint64
	for i := uint64(0); i < b.total; i++ {
		if b.get(i) {
			run = 0
			continue
		}
		if run == 0 {
			start = i
		}
		run++
		if run == count {
			for j := start; j < start+count; j++ {
				b.set(j)
			}
			b.used += count
			return start, nil
		}
	}
	return 0, ErrNoSpace
}

// Free marks [start, start+count) free immediately. Use this only for clusters
// allocated within the current transaction that were never committed (e.g. the
// rollback of a failed Write) — reusing them at once is safe. For releasing
// clusters that may belong to a committed state, use Defer.
func (b *Bitmap) Free(start, count uint64) {
	for i := start; i < start+count && i < b.total; i++ {
		if b.get(i) {
			b.clr(i)
			b.used--
		}
	}
}

// Defer records [start, start+count) for release at the next commit, leaving the
// bits SET so the run cannot be reallocated before then (§F1). The clusters keep
// counting as used until ApplyDeferred runs.
func (b *Bitmap) Defer(start, count uint64) {
	b.deferred = append(b.deferred, freeRange{start, count})
}

// ApplyDeferred actually frees every range recorded by Defer and clears the
// pending list. The durability layer calls this after a commit's uberblock flip
// succeeds, so the just-committed snapshot still marks these clusters used (they
// are reclaimed one commit later) while a crash before the flip leaves them
// pinned — never reused, never lost.
func (b *Bitmap) ApplyDeferred() {
	for _, r := range b.deferred {
		b.Free(r.start, r.count)
	}
	b.deferred = b.deferred[:0]
}

// Pending reports how many deferred-free ranges await ApplyDeferred.
func (b *Bitmap) Pending() int { return len(b.deferred) }

func (b *Bitmap) Used() uint64      { return b.used }
func (b *Bitmap) Available() uint64 { return b.total - b.used }
func (b *Bitmap) Total() uint64     { return b.total }

// MarshalLen reports how many bytes Marshal/MarshalInto will write.
func (b *Bitmap) MarshalLen() int { return 8 + len(b.bits)*8 }

// MarshalInto serializes the bitmap into dst (at least MarshalLen bytes) and
// returns the number of bytes written, allocating nothing.
func (b *Bitmap) MarshalInto(dst []byte) int {
	binary.LittleEndian.PutUint64(dst, b.total)
	for i, w := range b.bits {
		binary.LittleEndian.PutUint64(dst[8+i*8:], w)
	}
	return 8 + len(b.bits)*8
}

// Marshal serializes the bitmap (total header + raw words).
func (b *Bitmap) Marshal() []byte {
	out := make([]byte, b.MarshalLen())
	b.MarshalInto(out)
	return out
}

// Unmarshal reconstructs a bitmap (recomputing the used count).
func Unmarshal(data []byte) (*Bitmap, error) {
	if len(data) < 8 {
		return nil, ErrShort
	}
	total := binary.LittleEndian.Uint64(data)
	b := New(total)
	body := data[8:]
	if len(body) < len(b.bits)*8 {
		return nil, ErrShort
	}
	for i := range b.bits {
		b.bits[i] = binary.LittleEndian.Uint64(body[i*8:])
	}
	for i := uint64(0); i < total; i++ {
		if b.get(i) {
			b.used++
		}
	}
	return b, nil
}
