//! Go's `math/rand/v2` Rand over its PCG source, bit for bit: the Go suite's
//! seeded streams (the fuzzer's cases, the conformance vectors' scenarios,
//! the scorecard's workloads) replay here exactly as there.

// The ziggurat's constants, digit for digit as Go's source spells them.
#![allow(clippy::excessive_precision)]

use super::goexp::{EXP_FE, EXP_KE, EXP_WE};

/// `bits.Mul64`: (hi, lo) of a * b. (Here, not from the library: tools/tfbench.rs
/// includes this file too.)
fn mul64(a: u64, b: u64) -> (u64, u64) {
    let p = a as u128 * b as u128;
    ((p >> 64) as u64, p as u64)
}

/// `rand.NewPCG`: a 128-bit LCG with the DXSM output function.
#[derive(Clone)]
pub(crate) struct Pcg {
    hi: u64,
    lo: u64,
}

impl Pcg {
    pub(crate) fn new(seed1: u64, seed2: u64) -> Pcg {
        Pcg { hi: seed1, lo: seed2 }
    }

    fn next(&mut self) -> (u64, u64) {
        const MUL_HI: u64 = 2549297995355413924;
        const MUL_LO: u64 = 4865540595714422341;
        const INC_HI: u64 = 6364136223846793005;
        const INC_LO: u64 = 1442695040888963407;
        let (mut h, l) = mul64(self.lo, MUL_LO);
        h = h.wrapping_add(self.hi.wrapping_mul(MUL_LO)).wrapping_add(self.lo.wrapping_mul(MUL_HI));
        let (nl, c) = l.overflowing_add(INC_LO);
        h = h.wrapping_add(INC_HI).wrapping_add(c as u64);
        (self.lo, self.hi) = (nl, h);
        (h, nl)
    }

    pub(crate) fn uint64(&mut self) -> u64 {
        let (mut h, l) = self.next();
        const CHEAP_MUL: u64 = 0xda942042e4dd58b5;
        h ^= h >> 32;
        h = h.wrapping_mul(CHEAP_MUL);
        h ^= h >> 48;
        h.wrapping_mul(l | 1)
    }
}

/// `rand.New(rand.NewPCG(seed1, seed2))`.
#[derive(Clone)]
pub(crate) struct Rand {
    src: Pcg,
}

#[allow(dead_code)]
impl Rand {
    pub(crate) fn new(seed1: u64, seed2: u64) -> Rand {
        Rand { src: Pcg::new(seed1, seed2) }
    }

    pub(crate) fn uint64(&mut self) -> u64 {
        self.src.uint64()
    }

    pub(crate) fn int64(&mut self) -> i64 {
        (self.src.uint64() & !(1u64 << 63)) as i64
    }

    pub(crate) fn uint32(&mut self) -> u32 {
        (self.src.uint64() >> 32) as u32
    }

    /// uint64n is Lemire's bounded draw, as Go's.
    fn uint64n(&mut self, n: u64) -> u64 {
        if (n & n.wrapping_sub(1)) == 0 {
            return self.uint64() & n.wrapping_sub(1); // a power of two: mask
        }
        let (mut hi, mut lo) = mul64(self.uint64(), n);
        if lo < n {
            let thresh = n.wrapping_neg() % n;
            while lo < thresh {
                (hi, lo) = mul64(self.uint64(), n);
            }
        }
        hi
    }

    /// `r.Uint64N(n)`.
    pub(crate) fn uint64_n(&mut self, n: u64) -> u64 {
        assert!(n != 0, "invalid argument to Uint64N");
        self.uint64n(n)
    }

    /// `r.IntN(n)`.
    pub(crate) fn int_n(&mut self, n: i64) -> i64 {
        assert!(n > 0, "invalid argument to IntN");
        self.uint64n(n as u64) as i64
    }

    /// `r.Float64()`.
    pub(crate) fn float64(&mut self) -> f64 {
        ((self.uint64() << 11) >> 11) as f64 / 9007199254740992.0
    }

    /// ExpFloat64 is Go's ziggurat draw (Marsaglia & Tsang) over Go's own
    /// tables, so a seeded stream of exponential gaps replays as in Go. The
    /// tail calls log: Go's own here (go_log); the wedge test calls exp, std's
    /// here, compared as a float32. TestGoRand checks a million draws against
    /// Go's values.
    pub(crate) fn exp_float64(&mut self) -> f64 {
        const RE: f64 = 7.69711747013104972;
        loop {
            let u = self.uint64();
            let j = u as u32;
            let i = ((u >> 32) as u8) as usize;
            let x = j as f64 * EXP_WE[i] as f64;
            if j < EXP_KE[i] {
                return x;
            }
            if i == 0 {
                return RE - go_log(self.float64());
            }
            if EXP_FE[i] + self.float64() as f32 * (EXP_FE[i - 1] - EXP_FE[i]) < (-x).exp() as f32 {
                return x;
            }
        }
    }

    /// Shuffle is Go's Fisher-Yates shuffle.
    pub(crate) fn shuffle(&mut self, n: i64, mut swap: impl FnMut(usize, usize)) {
        let mut i = n - 1;
        while i > 0 {
            let j = self.uint64n((i + 1) as u64);
            swap(i as usize, j as usize);
            i -= 1;
        }
    }

    /// Perm is Go's rand.Perm: a permutation of [0, n).
    pub(crate) fn perm(&mut self, n: i64) -> Vec<i64> {
        let mut p: Vec<i64> = (0..n).collect();
        self.shuffle(n, |a, b| p.swap(a, b));
        p
    }
}

/// Go's `math.Log` (its translation of FreeBSD's e_log.c, which Go's amd64
/// assembly follows step for step), for ExpFloat64's tail: a libm's log
/// differs from it in the last bit of some results, and a seeded stream
/// would then differ from Go's there. x is positive and normal, or 0.
pub(crate) fn go_log(x: f64) -> f64 {
    const LN2_HI: f64 = 6.93147180369123816490e-01; /* 3fe62e42 fee00000 */
    const LN2_LO: f64 = 1.90821492927058770002e-10; /* 3dea39ef 35793c76 */
    const L1: f64 = 6.666666666666735130e-01; /* 3FE55555 55555593 */
    const L2: f64 = 3.999999999940941908e-01; /* 3FD99999 9997FA04 */
    const L3: f64 = 2.857142874366239149e-01; /* 3FD24924 94229359 */
    const L4: f64 = 2.222219843214978396e-01; /* 3FCC71C5 1D8E78AF */
    const L5: f64 = 1.818357216161805012e-01; /* 3FC74664 96CB03DE */
    const L6: f64 = 1.531383769920937332e-01; /* 3FC39A09 D078C69F */
    const L7: f64 = 1.479819860511658591e-01; /* 3FC2F112 DF3E5244 */
    if x == 0.0 {
        return f64::NEG_INFINITY;
    }
    // reduce: Frexp, for a normal x
    let bits = x.to_bits();
    let mut ki = ((bits >> 52) & 0x7ff) as i64 - 1022;
    let mut f1 = f64::from_bits((bits & !(0x7ffu64 << 52)) | (1022u64 << 52));
    if f1 < std::f64::consts::SQRT_2 / 2.0 {
        f1 *= 2.0;
        ki -= 1;
    }
    let f = f1 - 1.0;
    let k = ki as f64;
    // compute
    let s = f / (2.0 + f);
    let s2 = s * s;
    let s4 = s2 * s2;
    let t1 = s2 * (L1 + s4 * (L3 + s4 * (L5 + s4 * L7)));
    let t2 = s4 * (L2 + s4 * (L4 + s4 * L6));
    let r = t1 + t2;
    let hfsq = 0.5 * f * f;
    k * LN2_HI - ((hfsq - (s * (hfsq + r) + k * LN2_LO)) - f)
}
