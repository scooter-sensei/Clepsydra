//! background_test.go.

use super::check::*;
use super::gorand::Rand;
use super::helpers::*;
use crate::slots::*;
use crate::{Config, Filter};

/// TestBackgroundWaitsAndPlaces: with Background, a first touch whose lines
/// are full and that needs a move waits in the wait room instead of
/// searching. Its touches and counts find it there, the placer moves it into
/// the table with its history, and a filter that never evicts holds no stash.
/// The placer runs by hand here (bgManual), so the test is deterministic.
#[test]
fn TestBackgroundWaitsAndPlaces() {
    let t = T::new("TestBackgroundWaitsAndPlaces");
    if force_ct() {
        skip!(t, "Background has no effect with ConstantTime");
    }
    let clock = TestClock::new(1);
    let f = match Filter::with(
        Config { capacity: 4096, horizon: MINUTE, resolution: ms(1), clock: clock.func(), background: true, ..Config::default() },
        |c| c.bg_manual = true,
    ) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    let mut key = 0u64;
    let mut i = 1u64;
    while key == 0 {
        let w = f.waited();
        f.touch_at(mix64(i), 0, 1);
        if f.waited() != w {
            key = mix64(i);
        }
        i += 1;
    }
    let (fp, tg) = (fingerprint(key), tag(key));
    if f.locate(key, fp, tg) >= 0 || f.waiting() != 1 {
        fatalf!(t, "waiting key in the table, or {} waiting", f.waiting());
    }
    for n in 2..=5 {
        let r = f.touch_at(key, 60_000, 1);
        if r.count != n || !r.seen || r.restored {
            fatalf!(t, "touch {n} of a waiting key: {}", show(&r));
        }
    }
    let r = f.count_at(key, 60_000, 1);
    if r.count != 5 || r.upper != 5 {
        fatalf!(t, "count of a waiting key: {}", show(&r));
    }
    while f.waiting() > 0 {
        f.place_next(f.room.get(), 0);
    }
    if f.locate(key, fp, tg) < 0 {
        fatalf!(t, "placed key not in the table");
    }
    let r = f.count_at(key, 60_000, 1);
    if r.count != 5 || r.upper != 5 {
        fatalf!(t, "count of the placed key: {}", show(&r));
    }
    if f.stash.get().is_some() || f.evictions() != 0 {
        fatalf!(t, "stash allocated ({}) or {} evictions without a full table", f.stash.get().is_some(), f.evictions());
    }
    logf!(t, "waited {}, relocations {}, bytes {} (table {})", f.waited(), f.relocations(), f.bytes(), 16 * f.slots());
}

/// TestBackgroundConcurrentCounts: 8 workers touch 32 keys 20 times each in a
/// table of ~76 slots while flooding it with heavier keys, as
/// TestStashConcurrentEvictions does, with Background on: keys wait in the
/// wait room, are evicted to the stash and come back, and the placer moves
/// waiting keys into the table without their claim stripe, all concurrently
/// with the keys' own touches. With a stash and SpillMin that drop nothing,
/// every key must end with its exact count, in at most one slot.
#[test]
fn TestBackgroundConcurrentCounts() {
    let t = T::new("TestBackgroundConcurrentCounts");
    if force_ct() {
        skip!(t, "Background has no effect with ConstantTime");
    }
    const WORKERS: usize = 8;
    const HOT: usize = 32;
    const PER_KEY: usize = 20;
    let rounds = if short() { 40 } else { 200 };
    let mut waited = 0u64;
    for round in 0..rounds as u64 {
        let clock = TestClock::new(1);
        let f = match Filter::new(Config {
            capacity: 64,
            horizon: MINUTE,
            resolution: ms(1),
            clock: clock.func(),
            stash: 1 << 12,
            spill_min: 1,
            background: true,
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
                    "round {round} key {i}: {}, want count {PER_KEY} (waited {}, evictions {}, restored {}, lost {})",
                    show(&r),
                    f.waited(),
                    f.evictions(),
                    f.restored(),
                    f.lost()
                );
            }
        }
        settle(&f);
        for (i, &h) in keys.iter().enumerate() {
            let n = slots_of(&f, h).len();
            if n > 1 {
                fatalf!(t, "round {round} key {i}: in {n} slots");
            }
        }
        waited += f.waited();
    }
    logf!(t, "{rounds} rounds: {waited} waits");
}
