//! register_test.go.

use std::collections::BTreeMap;

use super::check::*;
use super::gorand::Rand;
use crate::hot::is_hot;
use crate::register::*;
use crate::slots::*;
use crate::{Config, Filter};

/// Reference: keep an exact per-unit histogram and bucket it directly at the
/// end. The register, driven like bump drives it (advance, then add), must
/// match whenever nothing saturates.
#[test]
fn TestAdvanceMatchesHistogram() {
    let t = T::new("TestAdvanceMatchesHistogram");
    let mut rng = Rand::new(3, 4);
    for _ in 0..3000 {
        let mut u = rng.int_n(1 << 20);
        let start = u;
        let mut r: Register = [0; N_BUCKETS];
        let mut hist: BTreeMap<i64, u64> = BTreeMap::new();
        for _ in 0..1 + rng.int_n(40) {
            let mut next = u + rng.int_n(12);
            if rng.int_n(8) == 0 {
                next += rng.int_n(3 * UNITS_PER_HORIZON as i64);
            }
            reg_advance(&mut r, u, next);
            u = next;
            reg_add(&mut r, 0, 1);
            *hist.entry(u).or_default() += 1;
        }
        let end = u + rng.int_n(2 * UNITS_PER_HORIZON as i64 + 5);
        reg_advance(&mut r, u, end);
        let mut want: Register = [0; N_BUCKETS];
        if end - u <= 2 * UNITS_PER_HORIZON as i64 {
            for (&ts, &n) in &hist {
                for j in 0..N_BUCKETS {
                    let mut hi = 1i64 << 62;
                    if j > 0 {
                        hi = bucket_start(end, j - 1);
                    }
                    if ts >= bucket_start(end, j) && ts < hi {
                        want[j] += n;
                    }
                }
            }
            // history older than the last reset gap was legitimately dropped
            if last_gap(&hist, end) {
                continue;
            }
        }
        if r != want {
            fatalf!(t, "start {start} end {end}: register {r:?}, histogram {want:?}");
        }
    }
}

/// lastGap reports whether some gap between consecutive events exceeded the
/// reset threshold (the register then forgets everything before it).
fn last_gap(hist: &BTreeMap<i64, u64>, end: i64) -> bool {
    let mut ts: Vec<i64> = hist.keys().copied().collect();
    ts.push(end);
    ts.windows(2).any(|w| w[1] - w[0] > 2 * UNITS_PER_HORIZON as i64)
}

#[test]
fn TestBucketsCoverHorizon() {
    let t = T::new("TestBucketsCoverHorizon");
    for u in 64i64..4096 {
        let span = u - bucket_start(u, N_BUCKETS - 1);
        if span < UNITS_PER_HORIZON as i64 {
            fatalf!(t, "u={u}: buckets cover only {span} units");
        }
    }
}

#[test]
fn TestPackRoundTripAndSpill() {
    let t = T::new("TestPackRoundTripAndSpill");
    let mut r: Register = [0; N_BUCKETS];
    reg_add(&mut r, 0, 100); // bucket 0 holds 31; the rest spills to older buckets
    let sum: u64 = r.iter().sum();
    if sum != 100 || r[0] != 31 {
        fatalf!(t, "spill: {r:?}");
    }
    let got = unpack(pack(&r));
    if got != r {
        fatalf!(t, "round trip {r:?} -> {got:?}");
    }
}

#[test]
fn TestUnitOfMatchesDivision() {
    let t = T::new("TestUnitOfMatchesDivision");
    let mut rng = Rand::new(5, 6);
    for h in [us(48), secs(1), MINUTE, secs(7 * 3600)] {
        let f = match Filter::new(Config { capacity: 10, horizon: h, ..Config::default() }) {
            Ok(f) => f,
            Err(e) => fatalf!(t, "{e}"),
        };
        for _ in 0..100_000 {
            let mut x = rng.uint64() & TS_MASK;
            if rng.int_n(2) == 0 {
                x = (x / f.unit * f.unit).wrapping_add(rng.int_n(3) as u64).wrapping_sub(1); // around boundaries
            }
            let (got, want) = (f.unit_of(x), (x / f.unit) as i64);
            if got != want {
                fatalf!(t, "unit {}: unitOf({x}) = {got}, want {want}", f.unit);
            }
        }
    }
}

/// TestCountsLastTouchWhole: the newest bucket's last event lies exactly at
/// the slot's lastTouch, so a window that reaches it counts it whole and
/// spreads only the bucket's other events. Eleven touches 200 ms apart in one
/// 2.5 s unit, the last at 27,000: spreading all eleven over [25,000, 27,000]
/// read 200 ms as 1.1 events (truth 2) and 400 ms as 2.2 (truth 3).
#[test]
fn TestCountsLastTouchWhole() {
    let t = T::new("TestCountsLastTouchWhole");
    let f = new_test(1000, MINUTE); // unit 2,500 ticks: [25,000, 27,500) is one unit
    let h = mix64(42);
    let mut ts = 25_000u64;
    while ts <= 27_000 {
        f.touch_at(h, 0, ts);
        ts += 200;
    }
    for (window, want) in [(200u64, 2i64), (400, 3), (2_000, 11)] {
        let got = f.count_at(h, window, 27_000);
        if got.count != want {
            errorf!(t, "window {window} ms: Count {}, want {want} ({got:?})", got.count);
        }
    }
}

/// TestRegisterLayoutLiterals: unpack, pack, hot and viewOf write the
/// register's layout out in literals; they must decode as bucketBits and
/// bucketShift say, on words with each bucket at its edges (0, 1,
/// satPerUnit-1, satPerUnit and its largest value, the others random) and on
/// 1,000,000 random registers.
#[test]
fn TestRegisterLayoutLiterals() {
    let t = T::new("TestRegisterLayoutLiterals");
    let f = match Filter::new(Config { capacity: 64, horizon: MINUTE, resolution: ms(1), ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    let reference = |w: u64| -> Register {
        let mut r = [0u64; N_BUCKETS];
        for j in 0..N_BUCKETS {
            r[j] = (w >> BUCKET_SHIFT[j]) & ((1u64 << BUCKET_BITS[j]) - 1);
        }
        r
    };
    let mut rng = Rand::new(9, 9);
    let mut words: Vec<u64> = Vec::new();
    for j in 0..N_BUCKETS {
        for c in [0, 1, SAT_PER_UNIT - 1, SAT_PER_UNIT, (1u64 << BUCKET_BITS[j]) - 1] {
            let mut w = 0u64;
            for k in 0..N_BUCKETS {
                if k != j {
                    w |= rng.uint64_n(1 << BUCKET_BITS[k]) << BUCKET_SHIFT[k];
                }
            }
            words.push(w | (c << BUCKET_SHIFT[j]));
        }
    }
    for _ in 0..1_000_000 {
        words.push(rng.uint64() & REG_MASK);
    }
    for &w in &words {
        let r = reference(w);
        let got = unpack(w);
        if got != r {
            fatalf!(t, "unpack({w:#x}) = {got:?}, want {r:?}");
        }
        let got = pack(&r);
        if got != (w & REG_MASK) {
            fatalf!(t, "pack({r:?}) = {got:#x}, want {:#x}", w & REG_MASK);
        }
        let want = r.iter().any(|&c| c >= SAT_PER_UNIT) && !is_hot(w);
        let got = hot(w);
        if got != want {
            fatalf!(t, "hot({w:#x}) = {got}, want {want}");
        }
        if !is_hot(w) {
            let (v, ok) = f.view_of(1, w, 1000);
            if !ok || v.c != r {
                fatalf!(t, "viewOf({w:#x}).c = {:?} (ok {ok}), want {r:?}", v.c);
            }
        }
    }
}
