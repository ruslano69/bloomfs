package store

import "encoding/binary"

// RefSize is the encoded size of a Ref: Hash(32) + Start(8) + Count(4) +
// Payload(4) + Logical(4) + Raw(1). It fits an inode's 64-byte inline block map,
// so a single-block file/directory references its data directly from the inode.
const RefSize = 32 + 8 + 4 + 4 + 4 + 1 // 53

// Marshal encodes the ref to RefSize bytes.
func (r Ref) Marshal() []byte {
	b := make([]byte, RefSize)
	copy(b[0:32], r.Hash[:])
	binary.LittleEndian.PutUint64(b[32:], r.Start)
	binary.LittleEndian.PutUint32(b[40:], r.Count)
	binary.LittleEndian.PutUint32(b[44:], r.Payload)
	binary.LittleEndian.PutUint32(b[48:], r.Logical)
	if r.Raw {
		b[52] = 1
	}
	return b
}

// UnmarshalRef decodes a ref from at least RefSize bytes.
func UnmarshalRef(b []byte) Ref {
	var r Ref
	copy(r.Hash[:], b[0:32])
	r.Start = binary.LittleEndian.Uint64(b[32:])
	r.Count = binary.LittleEndian.Uint32(b[40:])
	r.Payload = binary.LittleEndian.Uint32(b[44:])
	r.Logical = binary.LittleEndian.Uint32(b[48:])
	r.Raw = b[52] == 1
	return r
}
