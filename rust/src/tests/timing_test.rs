//! timing_test.go: what one request's response time tells about a key
//! (TestTimingLeak, TF_TIMING=1), and ConstantTime's dithered pad end.

use std::time::{Duration, Instant};

use super::check::*;
use super::gorand::Rand;
use crate::ct::OP_TOUCH;
use crate::{Config, Filter};

/// TestTimingLeak measures how well one request's response time tells a seen
/// key from an unseen one, without ConstantTime and with a 3 µs pad. A table
/// at design load (85%) larger than the last-level cache; seen and unseen
/// probes in random order, each timed alone. It reports each side's median
/// and the Kolmogorov-Smirnov distance D between the two latency
/// distributions: an observer who classifies by a threshold on one timing is
/// right at most (1+D)/2 of the time. Set TF_TIMING=1 to run. (In Rust the
/// compiler, not Go's, decides what the pad hides; this is the port's reading
/// of S41's measurement.)
#[test]
fn TestTimingLeak() {
    let t = T::new("TestTimingLeak");
    if !env_on("TF_TIMING") {
        skip!(t, "set TF_TIMING=1");
    }
    const CAPACITY: i64 = 1 << 21;
    const PROBES: usize = 40_000;
    for ct in [Duration::ZERO, us(3)] {
        for op in ["Count", "Touch"] {
            let clock = TestClock::new(1);
            let f = match Filter::new(Config {
                capacity: CAPACITY,
                horizon: MINUTE,
                resolution: ms(1),
                clock: clock.func(),
                constant_time: ct,
                ..Config::default()
            }) {
                Ok(f) => f,
                Err(e) => fatalf!(t, "{e}"),
            };
            let mut rng = Rand::new(3, 4);
            let mut seen = vec![0u64; CAPACITY as usize];
            for s in &mut seen {
                *s = f.hash_uint64(rng.uint64());
                f.touch_at(*s, 0, clock.get());
            }
            let now = clock.add(1000);
            // Probes are drawn up front and read in order, so fetching one
            // doesn't add a cache miss to the timed call.
            let mut kind = vec![0usize; PROBES];
            let mut hs = vec![0u64; PROBES];
            for i in 0..PROBES {
                kind[i] = rng.int_n(2) as usize;
                hs[i] = f.hash_uint64(rng.uint64());
                if kind[i] == 1 {
                    hs[i] = seen[rng.int_n(seen.len() as i64) as usize];
                }
            }
            let mut ts: [Vec<f64>; 2] = [Vec::new(), Vec::new()]; // [unseen, seen]
            for (i, &h) in hs.iter().enumerate() {
                let t0 = Instant::now();
                if op == "Count" {
                    std::hint::black_box(f.count_at(h, 60_000, now));
                } else {
                    std::hint::black_box(f.touch_at(h, 60_000, now));
                }
                ts[kind[i]].push(t0.elapsed().as_nanos() as f64);
            }
            ts[0].sort_by(f64::total_cmp);
            ts[1].sort_by(f64::total_cmp);
            let q = |x: &[f64], p: f64| x[(p * (x.len() - 1) as f64) as usize];
            let d = ks(&ts[0], &ts[1]);
            logf!(
                t,
                "ConstantTime {:<5} {:<5}: unseen p50/p99/p99.9 {:4.0}/{:5.0}/{:5.0} ns, seen {:4.0}/{:5.0}/{:5.0} ns; KS D = {d:.3} (best single-timing guess right {:.1}%)",
                go_duration(ct.as_nanos() as i64),
                op,
                q(&ts[0], 0.5),
                q(&ts[0], 0.99),
                q(&ts[0], 0.999),
                q(&ts[1], 0.5),
                q(&ts[1], 0.99),
                q(&ts[1], 0.999),
                50.0 + 50.0 * d
            );
        }
    }
}

/// TestCTPadEndIsDithered: a ConstantTime pad ends at a random point over a
/// span of several clock reads. A spin that ends at a fixed time ends at a
/// phase of the clock read that follows the call's own work (one read is ~37
/// ns on the Ryzen host), and keys whose work time is narrow show it: Count
/// at a fixed 1 µs pad leaked D 0.019-0.041 (claims S41). Calls with no work
/// of their own, spread end to end: their middle 80% must span more than one
/// clock read. Noise only widens the spread.
#[test]
fn TestCTPadEndIsDithered() {
    let t = T::new("TestCTPadEndIsDithered");
    let f = match Filter::new(Config { capacity: 1024, horizon: MINUTE, constant_time: us(1), ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    const N: usize = 20_000;
    let mut ds = vec![0f64; N];
    for d in &mut ds {
        let (t0, pad) = f.ct_begin(OP_TOUCH, 1);
        f.ct_end(OP_TOUCH, 0, t0, t0, pad);
        *d = t0.elapsed().as_nanos() as f64;
    }
    ds.sort_by(f64::total_cmp);
    let spread = ds[N * 9 / 10] - ds[N / 10];
    if spread < 80.0 {
        fatalf!(
            t,
            "padded calls end within {spread:.0} ns (p10 {:.0}, p90 {:.0}): the pad's end follows the clock's phase",
            ds[N / 10],
            ds[N * 9 / 10]
        );
    }
}

/// ks is the Kolmogorov-Smirnov distance between two sorted samples.
fn ks(a: &[f64], b: &[f64]) -> f64 {
    let mut d = 0f64;
    let (mut i, mut j) = (0, 0);
    while i < a.len() && j < b.len() {
        let x = a[i].min(b[j]);
        while i < a.len() && a[i] <= x {
            i += 1;
        }
        while j < b.len() && b[j] <= x {
            j += 1;
        }
        d = d.max((i as f64 / a.len() as f64 - j as f64 / b.len() as f64).abs());
    }
    d
}
