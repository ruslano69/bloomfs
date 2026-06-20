package dedup

import (
	"encoding/binary"
	"errors"
)

// ErrShort is returned when a serialized table buffer is truncated.
var ErrShort = errors.New("dedup: serialized table too short")

// recSize is the per-entry encoded size: Key(32) + Start(8) + Count(4) +
// Payload(4) + Logical(4) + Raw(1) + Refs(4).
const recSize = 32 + 8 + 4 + 4 + 4 + 1 + 4

// MarshalLen reports how many bytes Marshal/MarshalInto will write.
func (t *Table) MarshalLen() int { return 8 + len(t.m)*recSize }

// MarshalInto serializes the table into dst (at least MarshalLen bytes) and
// returns the number of bytes written, allocating nothing. Iteration order is
// unspecified — the table is a set, so order does not matter.
func (t *Table) MarshalInto(dst []byte) int {
	binary.LittleEndian.PutUint64(dst, uint64(len(t.m)))
	off := 8
	for k, e := range t.m {
		copy(dst[off:], k[:])
		off += 32
		binary.LittleEndian.PutUint64(dst[off:], e.Start)
		off += 8
		binary.LittleEndian.PutUint32(dst[off:], e.Count)
		off += 4
		binary.LittleEndian.PutUint32(dst[off:], e.Payload)
		off += 4
		binary.LittleEndian.PutUint32(dst[off:], e.Logical)
		off += 4
		if e.Raw {
			dst[off] = 1
		} else {
			dst[off] = 0
		}
		off++
		binary.LittleEndian.PutUint32(dst[off:], e.Refs)
		off += 4
	}
	return off
}

// Marshal serializes the table (count header + fixed-size records).
func (t *Table) Marshal() []byte {
	out := make([]byte, t.MarshalLen())
	t.MarshalInto(out)
	return out
}

// Unmarshal reconstructs a table from Marshal output.
func Unmarshal(data []byte) (*Table, error) {
	if len(data) < 8 {
		return nil, ErrShort
	}
	n := binary.LittleEndian.Uint64(data)
	t := New()
	off := 8
	for i := uint64(0); i < n; i++ {
		if off+recSize > len(data) {
			return nil, ErrShort
		}
		var k Key
		copy(k[:], data[off:])
		off += 32
		e := &Entry{}
		e.Start = binary.LittleEndian.Uint64(data[off:])
		off += 8
		e.Count = binary.LittleEndian.Uint32(data[off:])
		off += 4
		e.Payload = binary.LittleEndian.Uint32(data[off:])
		off += 4
		e.Logical = binary.LittleEndian.Uint32(data[off:])
		off += 4
		e.Raw = data[off] == 1
		off++
		e.Refs = binary.LittleEndian.Uint32(data[off:])
		off += 4
		t.m[k] = e
	}
	return t, nil
}
