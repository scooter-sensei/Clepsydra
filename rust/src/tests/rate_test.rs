//! rate_test.go, floodexp_test.go, mass_exp_test.go.

use super::attack_test::attack_filter;
use super::check::*;
use super::gorand::Rand;
use crate::register::{BUCKET_BITS, BUCKET_SHIFT, N_BUCKETS, REG_MASK};
use crate::slots::*;
use crate::{Config, Filter};

/// TestSustainedRate: one key sent at a steady rate for three horizons of
/// 60 s (1 ms ticks; past 1,000/s, several events per tick), queried at every
/// touch of the last horizon for 1, 5, 15 and 60 s windows. Per rate and
/// window: Count / exact (min, max), the share of answers flagged Saturated,
/// and unflagged answers with Upper below the exact count (the fail-closed
/// contract: there must be none). With hot entries (the default) and without
/// (Hot: -1, the 48-bit register alone). Deterministic: the clock is the
/// caller's and nothing is random.
#[test]
fn TestSustainedRate() {
    let t = T::new("TestSustainedRate");
    const H_TICKS: u64 = 60_000;
    let rates = [10i64, 12, 13, 20, 50, 100, 200, 500, 1000, 2000, 5000];
    let windows = [1_000u64, 5_000, 15_000, 60_000];
    let mut low = 0i64;
    for hot in [0i64, -1] {
        for rate in rates {
            let every = 1.max(1000 / rate) as u64; // ticks between sends
            let per = 1.max(rate / 1000); // events per send
            let f = match Filter::new(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), hot, ..Config::default() }) {
                Ok(f) => f,
                Err(e) => fatalf!(t, "{e}"),
            };
            let h = mix64(rate as u64);
            #[derive(Clone, Copy)]
            struct Acc {
                lo: f64,
                hi: f64,
                sat: i64,
                n: i64,
                bad: i64,
            }
            let mut st = [Acc { lo: f64::INFINITY, hi: 0.0, sat: 0, n: 0, bad: 0 }; 4];
            let mut now = 1u64;
            while now < 3 * H_TICKS {
                for _ in 0..per {
                    f.touch_at(h, 0, now);
                }
                if now >= 2 * H_TICKS {
                    for (wi, &w) in windows.iter().enumerate() {
                        let exact = per * (w / every + 1) as i64; // sends in [now-w, now], as the filter counts
                        let r = f.count_at(h, w, now);
                        let a = &mut st[wi];
                        a.n += 1;
                        if r.saturated {
                            a.sat += 1;
                        } else if r.upper < exact {
                            a.bad += 1;
                        }
                        let q = r.count as f64 / exact as f64;
                        (a.lo, a.hi) = (a.lo.min(q), a.hi.max(q));
                    }
                }
                now += every;
            }
            let name = if hot < 0 { "register only" } else { "hot entries" };
            for (wi, &w) in windows.iter().enumerate() {
                let a = st[wi];
                low += a.bad;
                logf!(
                    t,
                    "{name:<13} rate={rate}/s window={}s count/exact min={:.3} max={:.3} saturated={:.1}% unflagged_upper_low={}",
                    w / 1000,
                    a.lo,
                    a.hi,
                    100.0 * a.sat as f64 / a.n as f64,
                    a.bad
                );
            }
            if hot == 0 {
                logf!(t, "{name:<13} rate={rate}/s promoted={} refused={}", f.promoted(), f.hot_refused());
            }
        }
    }
    if low > 0 {
        errorf!(t, "{low} unflagged answers read Upper below the exact count");
    }
}

/// TestFloodSizing (TF_EXP=1) sizes the ghost overflow table and the stash
/// against heavy floods of 1x-16x the table's slots (and a 16x flood of
/// one-event keys): for each ghost size (units = slots / div), placement (one
/// or two candidate buckets), floors per bucket and stash size, the share of
/// new keys a strict limit of k turns away, split by cause, the offenders'
/// answers, and the bytes per key the filter holds once the flood is in. Same
/// workload as TestAttackFloodCollateral.
#[test]
fn TestFloodSizing() {
    let t = T::new("TestFloodSizing");
    if !env_on("TF_EXP") {
        skip!(t, "set TF_EXP=1");
    }
    const K: i64 = 20;
    const OFFENDERS: usize = 200;
    const PROBES: i64 = 20_000;
    let window = 60_000u64;
    // name, div (units = slots / div), choices, sub (floors per bucket),
    // stash (stash entries: slots / stash)
    let mut cfgs: Vec<(String, f64, i64, i64, i64)> = [
        ("one /4   f1 s/8 (17c55c8)", 4.0, 1, 1, 8),
        ("one /4   f3 s/8", 4.0, 1, 3, 8),
        ("one /1.8 f1 s/8", 1.8, 1, 1, 8),
        ("two /4   f1 s/8", 4.0, 2, 1, 8),
        ("two /2   f1 s/8", 2.0, 2, 1, 8),
        ("two /2   f3 s/8", 2.0, 2, 3, 8),
        ("two /1.8 f1 s/8", 1.8, 2, 1, 8),
        ("two /1.8 f3 s/8", 1.8, 2, 3, 8),
        ("two /1.8 f3 s/16 (default)", 1.8, 2, 3, 16),
        ("two /1.9 f3 s/16", 1.9, 2, 3, 16),
        ("two /1.75 f3 s/16", 1.75, 2, 3, 16),
        ("two /1.8 f3 s/32", 1.8, 2, 3, 32),
    ]
    .into_iter()
    .map(|(n, d, c, s, st)| (n.to_string(), d, c, s, st))
    .collect();
    let s = getenv("TF_FLOOD_CFG");
    if !s.is_empty() {
        let p: Vec<&str> = s.split_whitespace().collect();
        let parsed = (|| Some((p.first()?.parse::<f64>().ok()?, p.get(1)?.parse().ok()?, p.get(2)?.parse().ok()?, p.get(3)?.parse().ok()?)))();
        let Some((div, choices, sub, stash)) = parsed else {
            fatalf!(t, "TF_FLOOD_CFG: want \"div choices floors stash\", got {s:?}");
        };
        cfgs = vec![(format!("env {s}"), div, choices, sub, stash)];
    }
    let floods: [(i64, i64); 8] = [(1, K + 1), (2, K + 1), (3, K + 1), (4, K + 1), (6, K + 1), (8, K + 1), (16, K + 1), (16, 1)];
    for (name, div, choices, sub, stash) in &cfgs {
        for (mult, weight) in floods {
            let (mut f, now) = attack_filter(4096, 4820 / stash, &[]); // 4,820 slots at 4,096 keys
            let slots = f.slots();
            f.tweak(|c| {
                c.sk_w = 64.max((slots as f64 / div) as i64) as u64;
                c.gh_choices = *choices;
                c.gh_sub = *sub;
            });
            let mut rng = Rand::new(11, (mult * 100 + weight) as u64);
            for _ in 0..f.slots() * 85 / 100 {
                f.touch_at(rng.uint64(), 0, now.add(1));
            }
            let mut offs = vec![0u64; OFFENDERS];
            for o in offs.iter_mut() {
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
                    now.add(1); // the flood spans at most 30 s of the 60 s horizon
                }
            }
            let at = now.add(1);
            let (mut below, mut rec) = (0, 0);
            for &h in &offs {
                let r = f.count_at(h, window, at);
                if r.upper < K {
                    below += 1;
                }
                if r.count >= K * 9 / 10 {
                    rec += 1;
                }
            }
            // Turned away: as the attack tests count it (Degraded or Upper
            // over k), and as the fail-closed recipe in
            // docs/paper/06-security.md does (Upper reaching k). Causes: a
            // fingerprint match (Count from the table) or floors only (Upper,
            // not Count).
            let (mut deg, mut test, mut recipe, mut by_match, mut by_floor) = (0i64, 0i64, 0i64, 0i64, 0i64);
            for _ in 0..PROBES {
                let r = f.count_at(rng.uint64(), window, at);
                if r.degraded {
                    deg += 1;
                }
                if r.degraded || r.upper > K {
                    test += 1;
                }
                if r.upper >= K || r.saturated {
                    recipe += 1;
                    if r.count > 0 {
                        by_match += 1;
                    } else {
                        by_floor += 1;
                    }
                }
            }
            let bytes = f.bytes();
            let pct = |n: i64| 100.0 * n as f64 / PROBES as f64;
            logf!(
                t,
                "{name:<27} flood {mult:2}x x {weight:2}: offenders Upper<true {below}, Count>=90% {rec:3}/{OFFENDERS} | new keys Degraded {:5.2}%, turned away (test) {:6.2}% (recipe) {:6.2}% = match {:5.2}% + floor {:5.2}% | floored {:6} folded {:6} | {:5.1} B/key",
                pct(deg),
                pct(test),
                pct(recipe),
                pct(by_match),
                pct(by_floor),
                f.floored(),
                f.folded(),
                bytes as f64 / 4096.0
            );
            if below > 0 {
                errorf!(t, "{name} flood {mult}x: {below} offenders read Upper below the truth");
            }
        }
    }
}

/// TestMassMatchesView (TF_EXP=1): mass, read from word1 in place, equals the
/// view's walk (viewOf, viewMass) on 20 M random registers biased to the
/// edges: empty registers, bucket 0 alone, full and one-event counters, every
/// epoch offset, clocks at, just past and behind the last touch, on unit
/// boundaries and at the horizon.
#[test]
fn TestMassMatchesView() {
    let t = T::new("TestMassMatchesView");
    if !env_on("TF_EXP") {
        skip!(t, "set TF_EXP=1");
    }
    let f = new_test(1000, MINUTE);
    let view_mass = |w1: u64, last: u64, now: u64| -> u64 {
        if age(now, last) > f.horizon || (w1 & REG_MASK) == 0 {
            return 0;
        }
        let (v, _) = f.view_of(0, w1, last);
        f.view_mass(&v, now)
    };
    let (u, h) = (f.unit, f.horizon);
    let mut x = 7u64;
    let mut next = || {
        x = mix64(x.wrapping_add(0x9e3779b97f4a7c15));
        x
    };
    for i in 0..20_000_000 {
        let r = next();
        let mut reg = 0u64;
        match r % 8 {
            0 => {}                 // empty
            1 => reg = next() % 32, // bucket 0 alone
            _ => {
                for j in 0..N_BUCKETS {
                    let max = (1u64 << BUCKET_BITS[j]) - 1;
                    let c = match next() % 4 {
                        1 => 1,
                        2 => max,
                        3 => next() & max,
                        _ => 0,
                    };
                    reg |= c << BUCKET_SHIFT[j];
                }
            }
        }
        let tg = next() % ((1 << TAG_BITS) - 1);
        let on = next() & ONSET_MASK;
        let w1 = (tg << TAG_SHIFT) | (on << ONSET_SHIFT) | reg;
        let mut last = 3 * h + next() % (10 * h);
        if next() % 4 == 0 {
            last = last / u * u + [0, 1, u - 1][(next() % 3) as usize]; // on, just past, just before a unit boundary
        }
        let now = match next() % 6 {
            0 => last,                      // an event at now
            1 => last - next() % (2 * u),   // a caller behind the last touch
            2 => last + h - 1 + next() % 3, // at the horizon
            3 => {
                // unit boundaries
                let a = next() % 30;
                (last / u + a) * u + [0, 1, u - 1][(next() % 3) as usize]
            }
            _ => last + next() % (h + 2 * u),
        };
        let (got, want) = (f.mass(0, w1, last, now), view_mass(w1, last, now));
        if got != want {
            fatalf!(t, "case {i}: w1 {w1:#x} last {last} now {now}: mass {got}, view {want}");
        }
    }
}
