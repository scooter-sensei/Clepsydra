//! secexp_test.go.

use super::attack_test::attack_filter;
use super::check::*;
use super::gorand::Rand;
use crate::Filter;
use crate::overflow::SketchShape;

const K: i64 = 20;
const OFFENDERS: usize = 200;
const PROBES: i64 = 20_000;
const WINDOW: u64 = 60_000;

/// histStash is the stash these experiments ran with (slots/8 at 4,096 keys,
/// the default until 3603076); with the ghost table's one bucket per history
/// and one floor, it keeps their recorded results reproducible.
const HIST_STASH: i64 = 4820 / 8;

const fn shape(rows: i64, bits: i64, parts: i64) -> SketchShape {
    SketchShape { rows, bits, parts }
}

/// The experiments' flood (the body both tests of secexp_test.go repeat):
/// the table filled to its design load, the offenders with k events each,
/// then mult x slots keys of weight events each within 30 s. It returns the
/// offenders, the generator (the probes draw from it next) and the clock.
fn flood(f: &Filter, now: &TestClock, mult: i64, weight: i64) -> (Vec<u64>, Rand, u64) {
    let mut rng = Rand::new(11, (mult * 100 + weight) as u64);
    for _ in 0..f.slots() * 85 / 100 {
        f.touch_at(rng.uint64(), 0, now.add(1));
    }
    let mut offs = vec![0u64; OFFENDERS];
    for o in &mut offs {
        *o = rng.uint64();
        for _ in 0..K {
            f.touch_at(*o, 0, now.get());
        }
        now.add(1);
    }
    let per = 1.max(f.slots() * mult / 30_000);
    for i in 0..f.slots() * mult {
        let h = rng.uint64();
        for _ in 0..weight {
            f.touch_at(h, 0, now.get());
        }
        if i % per == 0 {
            now.add(1); // the whole flood spans at most 30 s of the 60 s horizon
        }
    }
    let at = now.add(1);
    (offs, rng, at)
}

/// TestSketchShapes compares overflow-sketch geometries, and the ghost table
/// in the same bytes, under floods (set TF_EXP=1). Each sketch config is a
/// counter width, row count, slices per horizon and columns per slot ("ghost
/// /4": a ghost table of the bytes of a sketch of one column per 4 slots);
/// floods are heavy (keys sent k+1 times, so they evict the offenders) or
/// light (one event per key, the cheapest flood).
#[test]
fn TestSketchShapes() {
    let t = T::new("TestSketchShapes");
    if !env_on("TF_EXP") {
        skip!(t, "set TF_EXP=1");
    }
    // name, shape, div (columns = slots / div), ghost
    let cfgs: [(&str, SketchShape, i64, bool); 14] = [
        ("16b r4 s4 /8 (dd30cbc)", shape(4, 16, 4), 8, false),
        ("16b r4 s4 /16", shape(4, 16, 4), 16, false),
        ("16b r4 s4 /4", shape(4, 16, 4), 4, false),
        ("8b  r4 s4 /8", shape(4, 8, 4), 8, false),
        ("8b  r4 s4 /4", shape(4, 8, 4), 4, false),
        ("8b  r4 s4 /2", shape(4, 8, 4), 2, false),
        ("16b r3 s4 /8", shape(3, 16, 4), 8, false),
        ("8b  r3 s4 /4", shape(3, 8, 4), 4, false),
        ("16b r4 s2 /8", shape(4, 16, 2), 8, false),
        ("8b  r4 s2 /4", shape(4, 8, 2), 4, false),
        ("8b  r4 s2 /2", shape(4, 8, 2), 2, false),
        ("ghost /8", shape(4, 8, 2), 8, true),
        ("ghost /4", shape(4, 8, 2), 4, true),
        ("ghost /2", shape(4, 8, 2), 2, true),
    ];
    let floods: [(&str, i64, i64); 6] =
        [("heavy 1x", 1, K + 1), ("heavy 2x", 2, K + 1), ("heavy 3x", 3, K + 1), ("heavy 4x", 4, K + 1), ("light 16x", 16, 1), ("light 64x", 64, 1)];
    for (name, sh, div, gh) in cfgs {
        let mut line = String::new();
        let mut bpk = 0.0f64;
        for (fl_name, mult, weight) in floods {
            let (mut f, now) = attack_filter(4096, HIST_STASH, &[]);
            let sk_w = 64.max(f.slots() / div) as u64;
            f.tweak(|c| {
                c.sk_w = sk_w;
                (c.sk_shape, c.gh_choices, c.gh_sub) = (sh, 1, 1);
                c.spill_min = 1; // shapes alone; TestSpillThreshold varies this
                c.cms = !gh;
            });
            bpk = sh.bytes(sk_w) as f64 / 4096.0;
            let (offs, mut rng, at) = flood(&f, &now, mult, weight);
            let (mut below, mut rec) = (0, 0);
            for &h in &offs {
                let r = f.count_at(h, WINDOW, at);
                if r.upper < K {
                    below += 1;
                }
                if r.count >= K * 9 / 10 {
                    rec += 1;
                }
            }
            let (mut deg, mut strict) = (0i64, 0i64);
            for _ in 0..PROBES {
                let r = f.count_at(rng.uint64(), WINDOW, at);
                if r.degraded {
                    deg += 1;
                }
                if r.degraded || r.upper > K {
                    strict += 1;
                }
            }
            if below > 0 {
                errorf!(t, "{name} {fl_name}: {below} offenders Upper below the truth");
            }
            line += &format!(" | {rec:3} {:5.1}% {:5.1}%", 100.0 * deg as f64 / PROBES as f64, 100.0 * strict as f64 / PROBES as f64);
        }
        logf!(t, "{name:<24} {bpk:5.1} B/key{line}");
    }
    logf!(
        t,
        "columns per flood (heavy 1x, 2x, 3x, 4x, light 16x, 64x): offenders with Count >= 90% of truth (of {OFFENDERS}) | new keys Degraded | new keys turned away by a strict limit of {K}"
    );
}

/// TestSpillThreshold: histories lighter than spillMin events are shed
/// instead of spilled, so cheap one-event floods add no sketch noise
/// (TF_EXP=1).
#[test]
fn TestSpillThreshold() {
    let t = T::new("TestSpillThreshold");
    if !env_on("TF_EXP") {
        skip!(t, "set TF_EXP=1");
    }
    let floods: [(&str, i64, i64); 6] =
        [("heavy 1x", 1, K + 1), ("heavy 2x", 2, K + 1), ("w1 16x", 16, 1), ("w2 16x", 16, 2), ("w4 16x", 16, 4), ("w8 8x", 8, 8)];
    for (name, sh, div, gh) in
        [("16b r4 s4 /8", shape(4, 16, 4), 8i64, false), ("8b r4 s2 /4", shape(4, 8, 2), 4, false), ("ghost /4", shape(4, 8, 2), 4, true)]
    {
        for min in [1u64, 2, 4, 8] {
            let mut line = String::new();
            for (fl_name, mult, weight) in floods {
                let (mut f, now) = attack_filter(4096, HIST_STASH, &[]);
                let sk_w = 64.max(f.slots() / div) as u64;
                f.tweak(|c| {
                    c.sk_w = sk_w;
                    (c.sk_shape, c.gh_choices, c.gh_sub) = (sh, 1, 1);
                    c.spill_min = min;
                    c.cms = !gh;
                });
                let (offs, mut rng, at) = flood(&f, &now, mult, weight);
                let (mut below, mut rec) = (0, 0);
                for &h in &offs {
                    let r = f.count_at(h, WINDOW, at);
                    if r.upper < K {
                        below += 1;
                    }
                    if r.count >= K * 9 / 10 {
                        rec += 1;
                    }
                }
                let mut strict = 0i64;
                for _ in 0..PROBES {
                    let r = f.count_at(rng.uint64(), WINDOW, at);
                    if r.degraded || r.upper > K {
                        strict += 1;
                    }
                }
                if below > 0 {
                    errorf!(t, "{name} min {min} {fl_name}: {below} offenders Upper below the truth");
                }
                line += &format!(" | {rec:3} {:5.1}%", 100.0 * strict as f64 / PROBES as f64);
            }
            logf!(t, "{name:<14} spillMin {min}{line}");
        }
    }
    logf!(
        t,
        "floods: heavy 1x, heavy 2x, 16x slots of 1-, 2-, 4-event keys, 8x of 8-event keys; per flood: offenders with Count >= 90% (of {OFFENDERS}) | new keys turned away by a strict limit of {K}"
    );
}
