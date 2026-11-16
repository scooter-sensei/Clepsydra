//! filter_test.go.

use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use super::check::*;
use super::gorand::Rand;
use crate::filter::Clock;
use crate::slots::*;
use crate::stash::{STASH_WAYS, StashCell};
use crate::{Config, Filter, MAX_TICK};

#[test]
fn TestAlignment() {
    let t = T::new("TestAlignment");
    let f = new_test(1000, MINUTE);
    let p = f.words.as_ptr() as usize;
    if p % 64 != 0 {
        fatalf!(t, "table not cache-line aligned: {p:#x}");
    }
}

#[test]
fn TestCheckAndTouchWindows() {
    let t = T::new("TestCheckAndTouchWindows");
    let f = new_test(1000, MINUTE);
    let ms = |d: Duration| f.ticks(d);
    let h = mix64(42);
    let mut now = 1000u64;

    if f.check_and_touch_at(h, ms(secs(10)), now) {
        fatalf!(t, "first touch reported seen");
    }
    if !f.check_and_touch_at(h, ms(secs(10)), now + ms(secs(9))) {
        fatalf!(t, "touch 9s later not seen within 10s");
    }
    now += ms(secs(9));
    // 20s gap: outside a 10s window, inside a 30s one. The window is a query
    // parameter, not a table property.
    let k2 = mix64(43);
    f.check_and_touch_at(k2, 0, now);
    if f.check_and_touch_at(h, ms(secs(10)), now + ms(secs(20))) {
        fatalf!(t, "20s gap reported seen within 10s");
    }
    if !f.check_and_touch_at(k2, ms(secs(30)), now + ms(secs(20))) {
        fatalf!(t, "20s gap not seen within 30s");
    }
    // Past the horizon the key is gone regardless of window.
    if f.check_and_touch_at(k2, ms(secs(3600)), now + ms(secs(20)) + ms(secs(61))) {
        fatalf!(t, "key seen after horizon expired");
    }
}

#[test]
fn TestExactBoundary() {
    let t = T::new("TestExactBoundary");
    let f = new_test(1000, MINUTE);
    let h = mix64(7);
    f.check_and_touch_at(h, 0, 5000);
    if !f.check_and_touch_at(h, 3000, 8000) {
        fatalf!(t, "age == window must be seen");
    }
    if f.check_and_touch_at(h, 2999, 11000) {
        fatalf!(t, "age == window+1 must not be seen");
    }
}

/// Velocity estimate vs exact sliding count for Poisson and bursty traffic.
/// The interpolation error is bounded by the count in the one bucket the
/// window's trailing edge cuts; at age W that bucket lies within ages
/// (W/2 - unit, 2W + unit].
#[test]
fn TestVelocityAccuracy() {
    let t = T::new("TestVelocityAccuracy");
    let horizon = secs(60);
    let f = new_test(1000, horizon);
    let mut r = Rand::new(1, 2);
    let u = f.unit;

    for trial in 0..200u64 {
        let h = mix64(trial + 1000);
        let rate = 0.5 + r.float64() * 3.0; // events per second
        let mut events: Vec<u64> = Vec::new();
        let mut now = 10_000_000 + trial * 1_000_000;
        let end = now + f.ticks(horizon) * 2;
        while now < end {
            now += (r.exp_float64() / rate * 1000.0) as u64;
            if r.int_n(10) == 0 {
                // occasional burst
                for _ in 0..5 {
                    events.push(now);
                    f.check_and_touch_at(h, 0, now);
                }
                continue;
            }
            events.push(now);
            f.check_and_touch_at(h, 0, now);
        }
        for w in [secs(1), secs(5), secs(30), horizon] {
            let wt = f.ticks(w);
            let (mut exact, mut edge) = (0i64, 0i64);
            for &e in &events {
                if e + wt > now {
                    exact += 1;
                }
                let a = now - e;
                if a + 2 * u > wt / 2 && a <= 2 * wt + 2 * u {
                    edge += 1;
                }
            }
            let got = f.velocity_count_at(h, wt, now);
            let d = got - exact;
            if d > edge + 1 || -d > edge + 1 {
                fatalf!(t, "trial {trial} w={w:?}: est {got} exact {exact} (edge bound {edge})");
            }
        }
    }
}

#[test]
fn TestVelocityBurstAndDecay() {
    let t = T::new("TestVelocityBurstAndDecay");
    let f = new_test(1000, MINUTE);
    let h = mix64(99);
    for i in 0..10u64 {
        f.check_and_touch_at(h, 0, 100_000 + i * 100); // 10 hits in 1s
    }
    let now = 101_000u64;
    let got = f.velocity_count_at(h, f.ticks(MINUTE), now);
    if got != 10 {
        fatalf!(t, "60s count = {got}, want 10");
    }
    // Windows at or above one sub-window (Horizon/5 = 12s) resolve the burst.
    let got = f.velocity_count_at(h, f.ticks(secs(30)), now + f.ticks(secs(20)));
    if !(9..=10).contains(&got) {
        fatalf!(t, "30s count 20s later = {got}, want ~10");
    }
    // Below sub-window resolution the estimate is bounded, never zero while
    // the key is fresh, and exactly zero once the window passes the last
    // touch.
    let got = f.velocity_count_at(h, f.ticks(secs(1)), now);
    if !(1..=10).contains(&got) {
        fatalf!(t, "1s count = {got}, want within [1,10]");
    }
    let got = f.velocity_count_at(h, f.ticks(secs(1)), now + f.ticks(secs(2)));
    if got != 0 {
        fatalf!(t, "1s count 2s after last touch = {got}, want 0");
    }
    let got = f.velocity_count_at(h, f.ticks(MINUTE), now + f.ticks(secs(61)));
    if got != 0 {
        fatalf!(t, "count after horizon = {got}, want 0");
    }
    // A returning key starts a fresh run.
    let (_, c) = f.touch_and_count_at(h, f.ticks(MINUTE), now + f.ticks(secs(90)));
    if c != 1 {
        fatalf!(t, "count after return = {c}, want 1");
    }
}

#[test]
fn TestHeavyHitterSpill() {
    let t = T::new("TestHeavyHitterSpill");
    let f = new_test(100, MINUTE);
    let h = mix64(5);
    for _ in 0..1000 {
        f.check_and_touch_at(h, 0, 1000);
    }
    // A full bucket spills into older ones, so a burst keeps its count up to
    // 1273 events (all buckets full; tfbench -exact, heavy_hitter).
    let got = f.velocity_count_at(h, f.ticks(MINUTE), 1000);
    if !(950..=1000).contains(&got) {
        fatalf!(t, "1000 hits counted as {got}");
    }
}

#[test]
fn TestFalsePositiveRate() {
    let t = T::new("TestFalsePositiveRate");
    const N: u64 = 100_000;
    let f = new_test(N as i64, MINUTE);
    let now = 1000u64;
    for i in 0..N {
        f.check_and_touch_at(mix64(i), 0, now);
    }
    let mut fp = 0;
    const PROBES: u64 = 1_000_000;
    for i in 0..PROBES {
        if f.contains_at(mix64(N + i), f.ticks(MINUTE), now) {
            fp += 1;
        }
    }
    let rate = f64::from(fp) / PROBES as f64;
    logf!(t, "FPR at capacity: {} ({fp}/{PROBES}), slots={}", go_e(rate, 2), f.slots());
    if rate > 1e-5 {
        fatalf!(t, "FPR {} too high", go_e(rate, 2));
    }
}

#[test]
fn TestRetentionAtCapacity() {
    let t = T::new("TestRetentionAtCapacity");
    const N: u64 = 100_000;
    let f = new_test(N as i64, MINUTE);
    let now = 1000u64;
    for i in 0..N {
        f.check_and_touch_at(mix64(i), 0, now);
    }
    let mut lost = 0;
    for i in 0..N {
        if !f.check_and_touch_at(mix64(i), f.ticks(MINUTE), now + 1) {
            lost += 1;
        }
    }
    logf!(t, "false negatives at capacity: {lost}/{N}, evictions={}", f.evictions());
    if lost > N / 1000 {
        fatalf!(t, "lost {lost} of {N} live keys");
    }
}

#[test]
fn TestContinuousDecayReclaims() {
    let t = T::new("TestContinuousDecayReclaims");
    let clock = TestClock::new(1);
    let f = match Filter::new(Config { capacity: 10_000, horizon: secs(1), resolution: ms(1), clock: clock.func(), ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    for i in 0..10_000u64 {
        f.check_and_touch_at(mix64(i), 0, clock.get());
    }
    if f.occupancy(clock.get()) == 0 {
        fatalf!(t, "nothing inserted");
    }
    settle(&f); // with Background, keys placed after the last pass would stay
    // Advance the clock in small steps, as a live system would; each step
    // sweeps only the lines its elapsed ticks pay for.
    let mut max_step = 0;
    while clock.get() < 3000 {
        clock.add(1);
        loop {
            let n = f.continuous_decay();
            max_step = max_step.max(n);
            if n == 0 {
                break;
            }
        }
    }
    let mut live = 0;
    let mut i = 0;
    while i < f.words.len() {
        if ld(&f.words[i]) != 0 {
            live += 1;
        }
        i += 2;
    }
    if live != 0 {
        fatalf!(t, "{live} expired slots not reclaimed");
    }
    if max_step > 4 {
        fatalf!(t, "decay step swept {max_step} lines; must stay bounded");
    }
}

#[test]
fn TestConcurrentTouchNoLostUpdates() {
    let t = T::new("TestConcurrentTouchNoLostUpdates");
    let f = new_test(1000, MINUTE);
    let h = mix64(1);
    f.check_and_touch_at(h, 0, 1000);
    const WORKERS: i64 = 8;
    const PER: i64 = 25;
    std::thread::scope(|s| {
        for _ in 0..WORKERS {
            s.spawn(|| {
                for _ in 0..PER {
                    f.check_and_touch_at(h, 0, 1000);
                }
            });
        }
    });
    let got = f.velocity_count_at(h, f.ticks(MINUTE), 1000);
    if got != 1 + WORKERS * PER {
        fatalf!(t, "count = {got}, want {}", 1 + WORKERS * PER);
    }
}

#[test]
fn TestConcurrentMixedRace() {
    let _t = T::new("TestConcurrentMixedRace");
    let f = new_test(1000, ms(100));
    std::thread::scope(|s| {
        for w in 0..8u64 {
            let f = &f;
            s.spawn(move || {
                let mut r = Rand::new(w, 9);
                for _ in 0..20_000 {
                    let h = mix64(r.int_n(3000) as u64);
                    let now = f.now();
                    match r.int_n(3) {
                        0 => {
                            f.check_and_touch_at(h, f.ticks(ms(50)), now);
                        }
                        1 => {
                            let _ = f.velocity_count_at(h, f.ticks(ms(50)), now);
                            f.check_and_touch_batch(&[h, h ^ 1, h ^ 2], f.ticks(ms(50)), now, &mut [false; 3]);
                        }
                        _ => {
                            f.continuous_decay();
                        }
                    }
                }
            });
        }
    });
}

#[test]
fn TestStringAPI() {
    let t = T::new("TestStringAPI");
    let f = must(Config { capacity: 100, horizon: MINUTE, ..Config::default() });
    if f.check_and_touch("10.0.0.1", MINUTE) {
        fatalf!(t, "first seen");
    }
    if !f.check_and_touch("10.0.0.1", MINUTE) {
        fatalf!(t, "second not seen");
    }
    let (seen, c) = f.touch_and_count("10.0.0.1", MINUTE);
    if !seen || c != 3 {
        fatalf!(t, "TouchAndCount = {seen},{c}");
    }
    let c = f.velocity_count("10.0.0.1", MINUTE);
    if c != 3 {
        fatalf!(t, "VelocityCount = {c}");
    }
}

/// An attacker who sprays fresh keys to overflow the table must not be able
/// to evict their own repeat key: eviction takes the lowest-velocity slot.
#[test]
fn TestSprayCannotEvictRepeatOffender() {
    let t = T::new("TestSprayCannotEvictRepeatOffender");
    const N: u64 = 10_000;
    let f = new_test(N as i64, MINUTE);
    let mut now = 1000u64;
    let mut offenders = [0u64; 50];
    for (i, o) in offenders.iter_mut().enumerate() {
        *o = mix64((1 << 40) + i as u64);
        for _ in 0..20 {
            f.check_and_touch_at(*o, 0, now);
        }
    }
    for i in 0..20 * N {
        // 20x capacity of one-off keys, all inside the window
        f.check_and_touch_at(mix64(i), 0, now + i % 30_000);
    }
    now += 30_000;
    for (i, &h) in offenders.iter().enumerate() {
        // The spray's spilled histories share the ghost table, so a
        // fingerprint collision can add a few events, but only flagged.
        let r = f.count_at(h, f.ticks(MINUTE), now);
        if r.count < 20 || (r.count > 20 && !r.degraded) {
            fatalf!(t, "offender {i}: count {} (degraded {}) after spray, want 20", r.count, r.degraded);
        }
    }
    logf!(t, "evictions during spray: {}", f.evictions());
}

#[test]
fn TestBatchMatchesSingle() {
    let t = T::new("TestBatchMatchesSingle");
    let (a, b) = (new_test(10_000, MINUTE), new_test(10_000, MINUTE));
    let hs: Vec<u64> = (0..5000u64).map(|i| mix64(i % 3000)).collect(); // repeats inside one batch too
    let mut out = vec![false; hs.len()];
    for round in 0..3u64 {
        let now = 1000 + round * 2000;
        a.check_and_touch_batch(&hs, 3000, now, &mut out);
        for (i, &h) in hs.iter().enumerate() {
            let want = b.check_and_touch_at(h, 3000, now);
            if out[i] != want {
                fatalf!(t, "round {round} key {i}: batch {} single {want}", out[i]);
            }
        }
    }
}

#[test]
fn TestCountBatchesMatchSingle() {
    let t = T::new("TestCountBatchesMatchSingle");
    let (a, b) = (new_test(10_000, MINUTE), new_test(10_000, MINUTE));
    let hs: Vec<u64> = (0..5000u64).map(|i| mix64(i % 3000)).collect(); // repeats inside one batch too
    let (mut seen, mut counts, mut vel) = (vec![false; hs.len()], vec![0i64; hs.len()], vec![0i64; hs.len()]);
    for round in 0..3u64 {
        let now = 1000 + round * 2000;
        a.touch_and_count_batch(&hs, 3000, now, &mut seen, &mut counts);
        for (i, &h) in hs.iter().enumerate() {
            let (s, c) = b.touch_and_count_at(h, 3000, now);
            if seen[i] != s || counts[i] != c {
                fatalf!(t, "round {round} key {i}: batch {}/{} single {s}/{c}", seen[i], counts[i]);
            }
        }
        a.velocity_count_batch(&hs, 5000, now + 500, &mut vel);
        for (i, &h) in hs.iter().enumerate() {
            let want = b.velocity_count_at(h, 5000, now + 500);
            if vel[i] != want {
                fatalf!(t, "round {round} key {i}: velocity batch {} single {want}", vel[i]);
            }
        }
    }
    a.check_and_touch_batch(&[], 1, 1, &mut []); // empty batches are fine
    a.touch_and_count_batch(&[], 1, 1, &mut [], &mut []);
    a.velocity_count_batch(&[], 1, 1, &mut []);
}

#[test]
fn TestCoarseClock() {
    let t = T::new("TestCoarseClock");
    let f = match Filter::new(Config { capacity: 100, horizon: MINUTE, coarse_clock: ms(1), ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    let t0 = f.now();
    std::thread::sleep(ms(20));
    let d = f.now() - t0;
    if d < f.ticks(ms(10)) || d > f.ticks(ms(200)) {
        fatalf!(t, "coarse clock advanced {d} ticks over 20ms");
    }
    if f.check_and_touch("k", secs(1)) || !f.check_and_touch("k", secs(1)) {
        fatalf!(t, "string API with coarse clock");
    }
    f.close();
    f.close(); // idempotent
}

/// TestMemoryOnFirstUse: a filter that never evicts holds only its table;
/// the stash comes with the first eviction, the overflow table with the
/// first history the stash drops.
#[test]
fn TestMemoryOnFirstUse() {
    let t = T::new("TestMemoryOnFirstUse");
    if force_ct() || force_bg() {
        skip!(t, "ConstantTime and Background allocate the stash on the first insert that waits for a slot");
    }
    let f = new_test(4096, MINUTE);
    let table = 16 * f.slots();
    for i in 0..(f.slots() * 85 / 100) as u64 {
        f.touch_at(mix64(i), 0, 1);
    }
    if f.evictions() != 0 || f.bytes() != table {
        fatalf!(t, "at 85% load: {} evictions, {} bytes, want 0 and the table's {table}", f.evictions(), f.bytes());
    }
    let stash = (f.stash_n * STASH_WAYS) as i64 * size_of::<StashCell>() as i64 + 8 * f.stash_n.div_ceil(64) as i64;
    let mut i = 1u64 << 40;
    while f.evictions() == 0 {
        f.touch_at(mix64(i), 0, 1);
        i += 1;
    }
    if f.bytes() != table + stash {
        fatalf!(t, "after the first eviction: {} bytes, want table {table} + stash {stash}", f.bytes());
    }
    let mut i = 1u64 << 41;
    while f.folded() == 0 {
        for _ in 0..4 {
            // SpillMin: lighter histories are shed, not kept
            f.touch_at(mix64(i), 0, 1);
        }
        i += 1;
    }
    match f.gh.get() {
        Some(g) if f.bytes() == table + stash + 8 * g.words.len() as i64 => {}
        _ => fatalf!(t, "after the first fold: {} bytes, want the overflow table's too", f.bytes()),
    }
}

/// TestTickRange: inserts, realigns and the decay step record ticks up to
/// MaxTick and panic past it, before taking any lock, so a caller that
/// recovers keeps a usable filter; New rejects a Clock already past it (one
/// counting from an epoch) and a horizon of MaxTick ticks.
#[test]
fn TestTickRange() {
    let t = T::new("TestTickRange");
    const W: u64 = 1000;
    let h = 0x9e3779b97f4a7c15u64; // h&decayEvery != 0: no inline decay step
    type Call = fn(&Filter, u64, u64);
    let inserts: [(&str, Call); 5] = [
        ("TouchAt", |f, h, now| {
            f.touch_at(h, W, now);
        }),
        ("CheckAndTouchAt", |f, h, now| {
            f.check_and_touch_at(h, W, now);
        }),
        ("TouchAndCountAt", |f, h, now| {
            f.touch_and_count_at(h, W, now);
        }),
        ("CheckAndTouchBatch", |f, h, now| f.check_and_touch_batch(&[h], W, now, &mut [false])),
        ("TouchAndCountBatch", |f, h, now| f.touch_and_count_batch(&[h], W, now, &mut [false], &mut [0])),
    ];
    for (name, call) in inserts {
        let f = new_test(1000, MINUTE);
        if !panics(|| call(&f, h, MAX_TICK + 1)) {
            errorf!(t, "{name} inserting past MaxTick did not panic");
        }
        if panics(|| call(&f, h, MAX_TICK)) {
            errorf!(t, "{name} inserting at MaxTick (after a recovered panic) panicked");
        }
        let r = f.count_at(h, W, MAX_TICK);
        if !r.seen || r.count != 1 {
            errorf!(t, "{name}: the key inserted at MaxTick reads {r:?}");
        }
    }
    let f = new_test(1000, MINUTE);
    f.touch_at(h, W, MAX_TICK - 10 * f.unit);
    if !panics(|| {
        f.touch_at(h, W, MAX_TICK + 1);
    }) {
        // 10 units on: realign
        errorf!(t, "a realigning touch past MaxTick did not panic");
    }
    f.touch_at(h, W, MAX_TICK);
    let r = f.count_at(h, f.horizon, MAX_TICK);
    if !r.seen || r.count != 2 {
        errorf!(t, "touches at MaxTick-10 units and MaxTick (after a recovered panic) read {r:?}");
    }
    for now in [MAX_TICK, MAX_TICK + 1] {
        let f = Filter::with(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), ..Config::default() }, |c| {
            c.clock = Clock::User(Arc::new(move || now));
        })
        .unwrap();
        let p = panics(|| {
            f.continuous_decay();
        });
        if p != (now > MAX_TICK) {
            errorf!(t, "ContinuousDecay at {now}: panicked {p}");
        }
    }
    let unix_micro: crate::ClockFn = Arc::new(|| SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_micros() as u64);
    if Filter::new(Config { capacity: 1000, horizon: MINUTE, clock: Some(unix_micro), ..Config::default() }).is_ok() {
        errorf!(t, "New accepted a clock in Unix microseconds");
    }
    if Filter::new(Config { capacity: 1000, horizon: secs(24 * 3600), resolution: Duration::from_nanos(1), ..Config::default() }).is_ok() {
        errorf!(t, "New accepted a horizon of a day in nanoseconds (8.6e13 ticks)");
    }
}
