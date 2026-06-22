package globalfilter

import (
	"encoding/binary"
	"errors"
	"sync"

	bloom "github.com/ruslano69/xxh3-bloom"
)

// Gate is the filesystem-wide membership oracle: the global Bloom filter plus the
// exact tombstone set, WITHOUT owning the directory maps. The fs layer owns those
// (its dir cache), so the gate answers one question — "is (dir, name) possibly
// present?" — letting the fs skip LOADING a cold directory from disk on a
// negative lookup. That avoided read (decrypt + decompress + index rebuild) is
// the filter's real payoff; the in-RAM hit cost it adds is negligible by
// comparison (see globalfilter benchmarks). Residency lives in the fs dir cache:
// the gate is consulted only when the directory is NOT cached.
//
// Completeness contract: the gate must hold EVERY live name before it gates cold
// lookups, or it would yield a false negative (a real file reported absent). The
// fs populates it fully at Mount (tree walk) and keeps it current via Add/Remove
// on every name mutation.
type Gate struct {
	mu       sync.RWMutex
	filter   *bloom.BlockedFilter
	deleted  map[uint64]struct{} // tombstones keyed by the hash low word
	live     int
	capacity int // what the current filter was tuned for; rebuild when live outgrows it
}

// minGateCapacity floors the filter size so a small or freshly-formatted
// filesystem does not start with a saturated filter (a size-1 filter crammed with
// hundreds of keys tests positive for almost everything, defeating the gate). A
// 1024-key blocked filter at 1% FP is a few KiB — free.
const minGateCapacity = 1024

// tombstoneFraction triggers a rebuild once tombstones exceed capacity/this. A
// blocked filter can't clear one key, so deletions leak set bits that the
// tombstone set masks; past this many, the masking set (and the FP drift) is
// worth paying a rebuild to reset.
const tombstoneFraction = 4

// NewGate sizes the filter for an expected live-name count with 2x headroom (so
// the next doubling, not the next insert, triggers a rebuild — geometric growth,
// amortized O(1) per insert), floored at minGateCapacity. The caller adds the
// keys afterwards; live starts at 0.
func NewGate(expectedLive int) *Gate {
	capacity := expectedLive * 2
	if capacity < minGateCapacity {
		capacity = minGateCapacity
	}
	return &Gate{
		filter:   bloom.NewBlockedTuned(uint(capacity), targetFP),
		deleted:  make(map[uint64]struct{}),
		capacity: capacity,
	}
}

// NeedsRebuild reports whether the filter has outgrown its tuning — either the
// live set passed the headroom (FP now degraded) or tombstones piled up. The fs
// checks this at commit (a consistent, write-locked point) and rebuilds by
// re-walking the tree; doing it mid-mutation could re-read an evicted directory
// from disk before its change was flushed and miss a live name.
func (g *Gate) NeedsRebuild() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.live > g.capacity || len(g.deleted)*tombstoneFraction > g.capacity
}

// Capacity and Live expose the filter's sizing for tests and telemetry.
func (g *Gate) Capacity() int { g.mu.RLock(); defer g.mu.RUnlock(); return g.capacity }
func (g *Gate) Live() int     { g.mu.RLock(); defer g.mu.RUnlock(); return g.live }

// Add records (dirIno, name) as present: clear any tombstone first, then admit
// the key to the filter (same ordering rationale as GlobalFilter.Create).
func (g *Gate) Add(dirIno uint64, name string) {
	hi, lo := key(dirIno, name)
	g.mu.Lock()
	delete(g.deleted, lo)
	g.filter.AddHash(hi, lo)
	g.live++
	g.mu.Unlock()
}

// Remove tombstones (dirIno, name). A blocked filter cannot clear one key (shared
// bits), so the tombstone masks the now-stale bits until a Rebuild.
func (g *Gate) Remove(dirIno uint64, name string) {
	_, lo := key(dirIno, name)
	g.mu.Lock()
	g.deleted[lo] = struct{}{}
	g.live--
	g.mu.Unlock()
}

// Test reports whether (dirIno, name) MAY be present. false ⇒ definitely absent,
// so the caller may skip loading the directory. true ⇒ maybe; the caller must
// load the directory and resolve authoritatively.
func (g *Gate) Test(dirIno uint64, name string) bool {
	hi, lo := key(dirIno, name)
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.filter.TestHash(hi, lo) {
		return false
	}
	_, dead := g.deleted[lo]
	return !dead
}

// Tombstones reports the current tombstone count; the fs uses it to decide when a
// rebuild is worthwhile.
func (g *Gate) Tombstones() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.deleted)
}

// gateSnapMagic tags a serialized gate so a stray blob is rejected instead of
// misread.
const gateSnapMagic = 0x47415445_00000001 // "GATE" + v1

// MarshalBinary serializes the gate (filter + tombstones + sizing) so a mount can
// load it instead of re-walking the tree. The fs wraps this with a validity stamp
// (the uberblock seq) and persists it; on a stale or absent snapshot it falls back
// to the tree walk, so this is a cache, never the source of truth.
func (g *Gate) MarshalBinary() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	fb, err := g.filter.MarshalBinary()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 32, 32+len(g.deleted)*8+len(fb))
	binary.LittleEndian.PutUint64(out[0:], gateSnapMagic)
	binary.LittleEndian.PutUint64(out[8:], uint64(g.capacity))
	binary.LittleEndian.PutUint64(out[16:], uint64(g.live))
	binary.LittleEndian.PutUint64(out[24:], uint64(len(g.deleted)))
	var kb [8]byte
	for k := range g.deleted {
		binary.LittleEndian.PutUint64(kb[:], k)
		out = append(out, kb[:]...)
	}
	return append(out, fb...), nil
}

// UnmarshalGate reconstructs a Gate from MarshalBinary output.
func UnmarshalGate(data []byte) (*Gate, error) {
	if len(data) < 32 || binary.LittleEndian.Uint64(data) != gateSnapMagic {
		return nil, errors.New("globalfilter: not a gate snapshot")
	}
	capacity := int(binary.LittleEndian.Uint64(data[8:]))
	live := int(binary.LittleEndian.Uint64(data[16:]))
	tombN := int(binary.LittleEndian.Uint64(data[24:]))
	off := 32
	deleted := make(map[uint64]struct{}, tombN)
	for i := 0; i < tombN; i++ {
		if off+8 > len(data) {
			return nil, errors.New("globalfilter: truncated gate snapshot")
		}
		deleted[binary.LittleEndian.Uint64(data[off:])] = struct{}{}
		off += 8
	}
	f := &bloom.BlockedFilter{}
	if err := f.UnmarshalBinary(data[off:]); err != nil {
		return nil, err
	}
	return &Gate{filter: f, deleted: deleted, live: live, capacity: capacity}, nil
}
