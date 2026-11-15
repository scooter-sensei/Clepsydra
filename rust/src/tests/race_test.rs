//! gaprace_test.go, read_race_test.go, reloc_race_test.go,
//! stash_race_test.go.

use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicBool, AtomicI64};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use super::check::*;
use super::gorand::Rand;
use super::helpers::*;
use super::regress_test::find_any;
use crate::hooks;
use crate::slots::*;
use crate::{Config, Filter, Result};

/// TestTouchesRacingAfterGap: K's last touch is g units back when touch X
/// moves its last touch to the current unit; before X's register follows, a
/// second touch Y of K in the same unit runs. A register's 4-bit epoch is
/// resolved against the slot's last touch, so for g >= 8 Y read the old
/// register as the new unit's and X then aged Y's event with it by g units
/// (past the horizon, X's reset dropped it): the last two units read Upper 1
/// for two events. Y must count in the current unit. (With realign's 16-byte
/// CAS, X moves both words at once for g >= 8; since claims D29 the register
/// stores no unit, and X moves both for any g >= 1.)
#[test]
fn TestTouchesRacingAfterGap() {
    let t = T::new("TestTouchesRacingAfterGap");
    if force_ct() {
        skip!(t, "ConstantTime touches of a key run one at a time under its stripe");
    }
    realign_modes(|mode| touches_racing_after_gap(&t, mode));
}

fn touches_racing_after_gap(t: &T, mode: &str) {
    for g in 1..=30u64 {
        let clock = TestClock::new(1);
        let f = match Filter::new(Config {
            capacity: 1000,
            horizon: Duration::from_millis(48),
            resolution: ms(1),
            clock: clock.func(),
            hot: -1,
            ..Config::default()
        }) {
            Ok(f) => Arc::new(f),
            Err(e) => fatalf!(t, "{e}"),
        };
        let (k, u, t0) = (mix64(99), f.unit, 10_000u64);
        f.touch_at(k, 0, t0);
        let now_x = t0 + g * u;
        let done: Arc<Mutex<Option<Done>>> = Arc::new(Mutex::new(None));
        let fired = Arc::new(AtomicBool::new(false));
        let hook = set_hook(&hooks::STAMPED, {
            let (f, done, fired) = (f.clone(), done.clone(), fired.clone());
            move |()| {
                if fired.swap(true, SeqCst) {
                    return;
                }
                let f = f.clone();
                let d = go(move || {
                    f.touch_at(k, 0, now_x + 1); // Y
                });
                closed_within(&d, ms(10)); // Y runs whole, or waits for X
                *done.lock().unwrap() = Some(d);
            }
        });
        f.touch_at(k, 0, now_x); // X
        drop(hook);
        if !fired.load(SeqCst) {
            fatalf!(t, "{mode}: gap {g}: X's touch never moved the last touch on");
        }
        if let Some(d) = done.lock().unwrap().take() {
            wait_for(t, &d, "Y");
        }
        let r = f.count_at(k, 2 * u, now_x + 1);
        if r.upper < 2 && !r.saturated {
            fatalf!(t, "{mode}: gap {g} units: the last two units read {}, want Upper >= 2 (X and Y)", show(&r));
        }
        let want = if now_x + 1 - t0 <= f.horizon { 3 } else { 2 };
        let r = f.count_at(k, f.horizon, now_x + 1);
        if r.upper < want && !r.saturated {
            fatalf!(t, "{mode}: gap {g} units: the horizon reads {}, want Upper >= {want}", show(&r));
        }
    }
}

/// Keys that are never touched again are read continuously while a fill to
/// 98% slot load keeps relocating them. A read must find the key wherever it
/// is: a relocation that moves it from one of its lines to the other between
/// the lookup's two line scans must not make it read as absent (Count 0,
/// Upper below the truth), and in ConstantTime mode such a miss must not
/// insert it a second time.
#[test]
fn TestReadDuringRelocationFindsKey() {
    let t = T::new("TestReadDuringRelocationFindsKey");
    let rounds = if short() { 8 } else { 40 };
    const CAP_N: i64 = 4000;
    const KEYS: usize = 256;
    const K: i64 = 3;
    const READERS: usize = 6;
    const AT: u64 = 60_000;
    let (bad, reads) = (AtomicI64::new(0), AtomicI64::new(0));
    for round in 0..rounds as u64 {
        let f = new_test(CAP_N, MINUTE);
        let salt = round << 40;
        let hs: Vec<u64> = (0..KEYS as u64).map(|i| mix64(salt + i)).collect();
        for &h in &hs {
            for _ in 0..K {
                f.check_and_touch_at(h, 0, AT);
            }
        }
        let stop = AtomicBool::new(false);
        std::thread::scope(|s| {
            for w in 0..READERS {
                let (f, hs, stop, bad, reads, t) = (&f, &hs, &stop, &bad, &reads, &t);
                s.spawn(move || {
                    let mut i = w;
                    while !stop.load(SeqCst) {
                        let h = hs[i % KEYS];
                        let mut r = Result::default();
                        if (i & 1) == 0 {
                            r = f.count_at(h, 60_000, AT);
                        } else {
                            r.seen = f.contains_at(h, 60_000, AT);
                            r.upper = K;
                        }
                        reads.fetch_add(1, SeqCst);
                        if (!r.seen || r.upper < K) && bad.fetch_add(1, SeqCst) + 1 <= 5 {
                            logf!(t, "round {round} key {} read {}", i % KEYS, show(&r));
                        }
                        i += 1;
                    }
                });
            }
            let fill = (f.slots() as f64 * 0.98) as i64 - KEYS as i64;
            for c in 0..fill as u64 {
                f.check_and_touch_at(mix64(salt + (1 << 36) + c), 0, AT);
            }
            stop.store(true, SeqCst);
        });
        for (i, &h) in hs.iter().enumerate() {
            let n = f.velocity_count_at(h, 60_000, AT);
            if n != K {
                bad.fetch_add(1, SeqCst);
                logf!(t, "round {round} key {i} ends with count {n}");
            }
        }
    }
    logf!(t, "{} reads, {} missed a key or read it low", reads.load(SeqCst), bad.load(SeqCst));
    if bad.load(SeqCst) > 0 {
        fatalf!(t, "{} reads missed a key that was there", bad.load(SeqCst));
    }
}

/// Hot keys touched continuously while inserts at ~98% load keep relocating
/// and evicting entries: no hot key may end up in two slots or lose a count.
/// (Before registers were published with their key, a touch landing between
/// an install's two writes could reset a hot key's register.)
///
/// Each hot key starts with initTouches events so eviction, which takes the
/// lowest-count slot, never picks it over a one-event cold key: losses can
/// then only come from races. With a single initial touch the test failed
/// under CPU load when the workers lagged the fill: a hot key still at one
/// event could be evicted like any cold key and lose that event.
///
/// All touches happen at tick `at`, a multiple of 8 register units (8 ×
/// Horizon/24 = 20s), where a one-minute window covers every bucket whole, so
/// VelocityCount is exact up to 1,273 events per key; each worker stops after
/// 200 passes over the hot keys (1,203 events per key at most). At other
/// ticks the window's edge cuts the oldest bucket and VelocityCount
/// interpolates it, which the test used to misread as lost counts on fast
/// runs.
#[test]
fn TestTouchRacingRelocationKeepsCounts() {
    let t = T::new("TestTouchRacingRelocationKeepsCounts");
    const CAP_N: i64 = 20_000;
    let f = new_test(CAP_N, MINUTE);
    const HOT: u64 = 2000;
    const INIT_TOUCHES: i64 = 3;
    const PASSES: i64 = 200;
    const AT: u64 = 60_000;
    for k in 0..HOT {
        for _ in 0..INIT_TOUCHES {
            f.check_and_touch_at(mix64(k), 0, AT);
        }
    }
    let stop = AtomicBool::new(false);
    let touches = AtomicI64::new(0);
    std::thread::scope(|s| {
        for w in 0..6u64 {
            let (f, stop, touches) = (&f, &stop, &touches);
            s.spawn(move || {
                let mut n = 0i64;
                let mut i = w;
                while !stop.load(SeqCst) && n < PASSES * HOT as i64 {
                    f.check_and_touch_at(mix64(i % HOT), 0, AT);
                    n += 1;
                    i += 1;
                }
                touches.fetch_add(n, SeqCst);
            });
        }
        // fill to ~98% slot load with cold keys, forcing cuckoo moves
        let fill = (f.slots() as f64 * 0.98) as i64 - HOT as i64;
        for k in 0..fill as u64 {
            f.check_and_touch_at(mix64((1 << 40) + k), 0, AT);
        }
        stop.store(true, SeqCst);
    });
    let (mut dups, mut total) = (0, 0i64);
    for k in 0..HOT {
        let h = mix64(k);
        let (fp, tg) = (fingerprint(h), tag(h));
        let p = f.primary(h);
        let (a, b) = (find_any(&f, p, fp, tg), find_any(&f, f.alt(p, fp), fp, tg));
        if a >= 0 && b >= 0 {
            dups += 1;
        }
        total += f.velocity_count_at(h, f.ticks(MINUTE), AT);
    }
    let want = touches.load(SeqCst) + HOT as i64 * INIT_TOUCHES;
    logf!(t, "relocations {}, evictions {}, hot dups {dups}, counted {total} of {want} touches", f.relocations(), f.evictions());
    if dups > 0 {
        fatalf!(t, "{dups} hot keys in two slots");
    }
    if total != want {
        fatalf!(t, "counted {total} of {want} touches");
    }
}

/// TestStashConcurrentEvictions: 8 workers touch 32 hot keys in a table of
/// ~76 slots while flooding it with heavier keys, so hot keys are evicted to
/// the stash and restored over and over, concurrently with their own touches.
/// With a stash big enough to drop nothing, every hot key must end with its
/// exact count.
#[test]
fn TestStashConcurrentEvictions() {
    let t = T::new("TestStashConcurrentEvictions");
    const WORKERS: usize = 8;
    const HOT: usize = 32;
    const PER_KEY: usize = 20;
    let rounds = if short() { 8 } else { 40 };
    for round in 0..rounds as u64 {
        let clock = TestClock::new(1);
        let f = match Filter::new(Config {
            capacity: 64,
            horizon: MINUTE,
            resolution: ms(1),
            clock: clock.func(),
            stash: 1 << 12,
            ..Config::default()
        }) {
            Ok(f) => f,
            Err(e) => fatalf!(t, "{e}"),
        };
        let keys: Vec<u64> = (0..HOT as u64).map(|i| mix64((round << 32) | i)).collect();
        std::thread::scope(|s| {
            for w in 0..WORKERS {
                let (f, keys, clock) = (&f, &keys, &clock);
                s.spawn(move || {
                    let mut rng = Rand::new(round, w as u64);
                    for i in 0..HOT * PER_KEY / WORKERS {
                        let now = clock.add(25);
                        f.touch_at(keys[(i * WORKERS + w) % HOT], 0, now);
                        if i % 2 == 0 {
                            let h = rng.uint64();
                            for _ in 0..25 {
                                f.touch_at(h, 0, now);
                            }
                        }
                    }
                });
            }
        });
        let now = clock.get();
        for (i, &h) in keys.iter().enumerate() {
            let r = f.count_at(h, f.ticks(MINUTE), now);
            if r.count != PER_KEY as i64 || r.saturated {
                fatalf!(
                    t,
                    "round {round} key {i}: {}, want count {PER_KEY} (evictions {}, restored {}, lost {})",
                    show(&r),
                    f.evictions(),
                    f.restored(),
                    f.lost()
                );
            }
        }
        if round == 0 {
            logf!(t, "round 0: evictions {}, restored {}, lost {}", f.evictions(), f.restored(), f.lost());
        }
    }
}
