// Package store is the BloomFS data-path pipeline (§5): for each logical block,
//
//	plaintext -> BLAKE hash -> dedup lookup
//	   HIT  -> RefCount++ (no compress, no encrypt, no write)
//	   MISS -> compress (ZSTD) -> encrypt (AES-XTS, tweak = cluster addr) -> write
//
// and the reverse on read, with an end-to-end content-hash integrity check.
//
// Notes on Stage C choices vs the spec:
//   - Dedup hash is BLAKE2b-256, used here as a drop-in for BLAKE3 (§B7): same
//     cryptographic role, trusted without byte-verify (§D-2). Swap to BLAKE3 by
//     changing one function once the dependency is vendored.
//   - The hash is computed over the *uncompressed* block, enabling the
//     short-circuit and making dedup robust to compressor settings (§5.1).
//   - Incompressible blocks are stored raw to avoid expansion (§B9).
package store

import (
	"crypto/aes"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/xts"

	"github.com/ruslano69/bloomfs/alloc"
	"github.com/ruslano69/bloomfs/block"
	"github.com/ruslano69/bloomfs/dedup"
)

// Ref is what an inode stores to locate a logical block (it maps onto an extent,
// §4.4). It is returned by Write and consumed by Read/Release.
type Ref struct {
	Hash    dedup.Key
	Start   uint64
	Count   uint32
	Payload uint32 // bytes of stored payload (compressed or raw)
	Logical uint32 // uncompressed bytes
	Raw     bool
}

// BlockStore wires the device, allocator, dedup table, cipher and compressor.
type BlockStore struct {
	dev   block.Device
	alloc *alloc.Bitmap
	ddt   *dedup.Table
	xts   *xts.Cipher
	enc   *zstd.Encoder
	dec   *zstd.Decoder
}

// New builds a BlockStore sharing the caller's allocator and dedup table (so the
// durability layer can snapshot them, §B3). A nil key selects a plaintext pool
// (§5.5 opt-out); otherwise key must be a valid AES-XTS key (32 bytes for
// AES-128-XTS or 64 for AES-256-XTS). Key management (keyring/Argon2id) is §B4.
func New(dev block.Device, a *alloc.Bitmap, ddt *dedup.Table, key []byte) (*BlockStore, error) {
	var x *xts.Cipher
	if key != nil {
		var err error
		if x, err = xts.NewCipher(aes.NewCipher, key); err != nil {
			return nil, fmt.Errorf("store: xts cipher: %w", err)
		}
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, fmt.Errorf("store: zstd encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("store: zstd decoder: %w", err)
	}
	return &BlockStore{dev: dev, alloc: a, ddt: ddt, xts: x, enc: enc, dec: dec}, nil
}

// Close releases the compressor/decompressor resources.
func (s *BlockStore) Close() {
	s.enc.Close()
	s.dec.Close()
}

func clusters(n int) uint64 {
	c := (uint64(n) + block.Size - 1) / block.Size
	if c == 0 {
		c = 1
	}
	return c
}

// Write stores a logical block and returns a Ref to it. Identical content is
// stored once (dedup), bumping a reference count instead of writing.
func (s *BlockStore) Write(plaintext []byte) (Ref, error) {
	key := dedup.Key(blake2b.Sum256(plaintext))

	if e, ok := s.ddt.Lookup(key); ok { // dedup hit — short-circuit (§5.1)
		s.ddt.Incr(key)
		return refFrom(key, e), nil
	}

	// Compress; fall back to raw if compression does not shrink the block (§B9).
	payload := s.enc.EncodeAll(plaintext, nil)
	raw := false
	if len(payload) >= len(plaintext) {
		payload, raw = plaintext, true
	}

	count := clusters(len(payload))
	start, err := s.alloc.Alloc(count)
	if err != nil {
		return Ref{}, err
	}

	// Pad to whole clusters and encrypt each cluster with its address as the XTS
	// tweak (§5.1). Writing in place is fine — XTS allows full overlap.
	buf := make([]byte, count*block.Size)
	copy(buf, payload)
	for i := uint64(0); i < count; i++ {
		sec := buf[i*block.Size : (i+1)*block.Size]
		if s.xts != nil {
			s.xts.Encrypt(sec, sec, start+i)
		}
		if err := s.dev.WriteBlock(start+i, sec); err != nil {
			s.alloc.Free(start, count)
			return Ref{}, err
		}
	}

	e := dedup.Entry{
		Start:   start,
		Count:   uint32(count),
		Payload: uint32(len(payload)),
		Logical: uint32(len(plaintext)),
		Raw:     raw,
	}
	s.ddt.Add(key, e)
	return refFrom(key, e), nil
}

// Read reconstructs the plaintext for a Ref and verifies its content hash.
func (s *BlockStore) Read(r Ref) ([]byte, error) {
	buf := make([]byte, uint64(r.Count)*block.Size)
	for i := uint64(0); i < uint64(r.Count); i++ {
		blk, err := s.dev.ReadBlock(r.Start + i)
		if err != nil {
			return nil, err
		}
		if s.xts != nil {
			s.xts.Decrypt(blk, blk, r.Start+i)
		}
		copy(buf[i*block.Size:], blk)
	}
	payload := buf[:r.Payload]

	var plaintext []byte
	if r.Raw {
		plaintext = append([]byte(nil), payload...)
	} else {
		var err error
		plaintext, err = s.dec.DecodeAll(payload, make([]byte, 0, r.Logical))
		if err != nil {
			return nil, fmt.Errorf("store: decompress: %w", err)
		}
	}
	if uint32(len(plaintext)) != r.Logical {
		return nil, errors.New("store: logical length mismatch")
	}
	// The dedup hash doubles as an end-to-end integrity check (§B13): any bit-rot
	// in the clusters surfaces here instead of returning silent garbage.
	if dedup.Key(blake2b.Sum256(plaintext)) != r.Hash {
		return nil, errors.New("store: content hash mismatch (corruption)")
	}
	return plaintext, nil
}

// Release drops one reference to a block; the last reference frees its clusters
// (§5.4). The free is deferred until the next commit (§F1): the clusters may
// belong to the last committed state, so reusing them before this transaction
// commits could overwrite data a crash would need to roll back to. In production
// this also hands off to the GC worker (§B5).
func (s *BlockStore) Release(r Ref) {
	if e, freed := s.ddt.Decr(r.Hash); freed {
		s.alloc.Defer(e.Start, uint64(e.Count))
	}
}

// UniqueBlocks is the number of distinct stored blocks (dedup table size).
func (s *BlockStore) UniqueBlocks() int { return s.ddt.Len() }

func refFrom(k dedup.Key, e dedup.Entry) Ref {
	return Ref{Hash: k, Start: e.Start, Count: e.Count, Payload: e.Payload, Logical: e.Logical, Raw: e.Raw}
}
