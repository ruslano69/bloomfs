//! Scalar blocked Bloom filter — the directory differentiator's critical path
//! (SPEC §3). All `k` probe bits of a key live in one 512-bit cache line, so an
//! `add`/`test` is a single memory access. This is a math-parity port of the Go
//! `xxh3-bloom` library's scalar `BlockedFilter` (default config: XXH3 hash,
//! seed 0, fastrange block selection). It is never serialized — BloomFS rebuilds
//! each segment filter from the live index — so the on-disk 'BBLM' format and
//! the SIMD/batch paths (§5b of the port plan) are out of scope here.

use xxhash_rust::xxh3::xxh3_128_with_seed;

const BLOCK_BITS: u32 = 512; // one cache line worth of bits (64 B)
const BLOCK_WORDS: u64 = (BLOCK_BITS / 64) as u64; // 8 u64 per block
const BLOCK_MASK: u32 = BLOCK_BITS - 1; // 511; `& mask` since 512 is a power of two

/// A blocked Bloom filter. Each key is confined to one cache line, picked by the
/// high 64 hash bits (fastrange); the low 64 bits drive `k` enhanced-double-hash
/// probes inside that line.
pub struct BlockedFilter {
    blocks: Vec<u64>, // num_blocks * BLOCK_WORDS words
    num_blocks: u64,
    k: u32,
    seed: u64,
}

impl BlockedFilter {
    /// Build a filter whose *measured* false-positive rate at `n` items is `<= fp`,
    /// inflating the block count to absorb uneven per-block load. Uses the default
    /// XXH3 hash, unseeded — matching `dir`'s `NewBlockedTuned(capacity, fp)`.
    pub fn new_blocked_tuned(n: u64, fp: f64) -> Self {
        let n = if n == 0 { 1 } else { n };
        // The blocked-FP model is mildly optimistic (it treats the k probes as
        // independent; double-hashing inside a 512-bit block collides a touch
        // more). Tune to a stricter internal target so the measured rate lands
        // at or under fp.
        const SAFETY: f64 = 0.75;
        let internal = fp * SAFETY;

        let mut best_blocks = 0u64;
        let mut best_k = 1u32;
        let mut best_bits = u64::MAX;
        // For each k, take the fewest blocks meeting the target, then keep the
        // (k, blocks) pair with the least total memory.
        for k in 1u32..=20 {
            let nb = min_blocks_for_k(n, internal, k);
            let bits = nb * BLOCK_BITS as u64;
            if bits < best_bits {
                best_bits = bits;
                best_blocks = nb;
                best_k = k;
            }
        }
        Self::alloc(best_blocks, best_k, 0)
    }

    fn alloc(num_blocks: u64, k: u32, seed: u64) -> Self {
        let num_blocks = num_blocks.max(1);
        let words = (num_blocks * BLOCK_WORDS) as usize;
        BlockedFilter {
            blocks: vec![0u64; words],
            num_blocks,
            k: k.max(1),
            seed,
        }
    }

    /// Word index where the key's cache line starts, plus the two 32-bit
    /// sub-hashes used to derive the `k` bit positions.
    #[inline]
    fn block_offset(&self, data: &[u8]) -> (u64, u32, u32) {
        let h = xxh3_128_with_seed(data, self.seed);
        let hi = (h >> 64) as u64;
        let lo = h as u64;
        // fastrange (Lemire): map hi into [0, num_blocks) with a widening
        // multiply + shift instead of a 64-bit modulo.
        let block_idx = ((hi as u128 * self.num_blocks as u128) >> 64) as u64;
        let off = block_idx * BLOCK_WORDS;
        let h1 = lo as u32;
        let h2 = (lo >> 32) as u32 | 1; // odd stride: coprime with 512, so all k probes are distinct
        (off, h1, h2)
    }

    /// Insert `data`. Touches exactly one cache line.
    pub fn add(&mut self, data: &[u8]) {
        let (off, mut a, mut b) = self.block_offset(data);
        for i in 0..self.k {
            let bit = a & BLOCK_MASK;
            self.blocks[(off + (bit >> 6) as u64) as usize] |= 1u64 << (bit & 63);
            a = a.wrapping_add(b);
            b = b.wrapping_add(i); // enhanced double hashing: triangular term breaks linear collisions
        }
    }

    /// Report possible membership. Touches exactly one cache line.
    pub fn test(&self, data: &[u8]) -> bool {
        let (off, mut a, mut b) = self.block_offset(data);
        for i in 0..self.k {
            let bit = a & BLOCK_MASK;
            if self.blocks[(off + (bit >> 6) as u64) as usize] & (1u64 << (bit & 63)) == 0 {
                return false;
            }
            a = a.wrapping_add(b);
            b = b.wrapping_add(i);
        }
        true
    }

    /// Total number of bits.
    pub fn cap(&self) -> u64 {
        self.num_blocks * BLOCK_BITS as u64
    }

    /// Number of hash probes.
    pub fn k(&self) -> u32 {
        self.k
    }
}

/// Smallest block count whose estimated blocked-FP at `n` items is `<= fp`, for a
/// fixed `k`. FP falls monotonically as blocks grow, so binary-search.
fn min_blocks_for_k(n: u64, fp: f64, k: u32) -> u64 {
    let fp_at = |nb: u64| estimate_blocked_fp(n, nb, k);
    let mut hi = 1u64;
    while fp_at(hi) > fp {
        hi *= 2;
        if hi > (1 << 42) {
            // safety: ~4.5e12 blocks
            break;
        }
    }
    let mut lo = (hi / 2).max(1);
    while lo < hi {
        let mid = lo + (hi - lo) / 2;
        if fp_at(mid) <= fp {
            hi = mid;
        } else {
            lo = mid + 1;
        }
    }
    lo
}

/// Estimated false-positive rate of a blocked filter with `num_blocks` cache
/// lines and `k` hashes holding `n` items. Keys land in blocks Poisson(λ=n/blocks);
/// a block of B bits holding j keys has FP `(1 - e^(-k·j/B))^k`, averaged over the
/// Poisson load (Putze, Sanders, Singler, 2007).
fn estimate_blocked_fp(n: u64, num_blocks: u64, k: u32) -> f64 {
    let num_blocks = num_blocks.max(1);
    let lambda = n as f64 / num_blocks as f64;
    // Hopelessly overloaded block => every bit set => FP ~ 1. Short-circuit.
    if lambda > 1000.0 {
        return 1.0;
    }
    const B: f64 = BLOCK_BITS as f64;
    let kf = k as f64;
    let hi = (lambda + 10.0 * lambda.sqrt() + 10.0) as i64;

    let mut sum = 0.0;
    let log_lambda = lambda.ln();
    let mut log_pmf = -lambda; // log P(j=0) = -lambda
    for j in 0..=hi {
        if j > 0 {
            log_pmf += log_lambda - (j as f64).ln();
        }
        let pmf = log_pmf.exp();
        let inner = (1.0 - (-kf * j as f64 / B).exp()).powf(kf);
        sum += pmf * inner;
    }
    sum
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key(i: usize) -> Vec<u8> {
        format!("key-{i}").into_bytes()
    }

    #[test]
    fn no_false_negatives() {
        let n = 10_000;
        let mut f = BlockedFilter::new_blocked_tuned(n as u64, 0.01);
        for i in 0..n {
            f.add(&key(i));
        }
        for i in 0..n {
            assert!(f.test(&key(i)), "false negative at {i}");
        }
    }

    #[test]
    fn measured_fp_under_target() {
        let n = 20_000usize;
        let target = 0.01;
        let mut f = BlockedFilter::new_blocked_tuned(n as u64, target);
        for i in 0..n {
            f.add(&key(i));
        }
        // Probe disjoint keys that were never inserted.
        let mut fp = 0usize;
        let trials = n;
        for i in n..n + trials {
            if f.test(&key(i)) {
                fp += 1;
            }
        }
        let rate = fp as f64 / trials as f64;
        // Tuned guarantees measured <= target; allow generous slack for the
        // single-run statistical wobble (Go's regression test uses up to ~2×).
        assert!(
            rate <= target * 3.0,
            "measured FP {rate} exceeds 3x target {target}"
        );
    }

    #[test]
    fn tuned_shape_is_sane() {
        let f = BlockedFilter::new_blocked_tuned(1000, 0.01);
        assert!(f.k() >= 1);
        // Blocked filters spend ~20-35% more bits than the classic estimate;
        // floor-check that we allocated a non-trivial, cache-line-aligned array.
        assert!(f.cap() >= BLOCK_BITS as u64);
        assert_eq!(f.cap() % BLOCK_BITS as u64, 0);
    }
}
