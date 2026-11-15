//! ctwait_test.go.

use std::sync::atomic::Ordering::SeqCst;
use std::time::Instant;

use super::check::*;
use super::helpers::*;
use crate::slots::*;
use crate::stash::STASH_WAYS;
use crate::{Config, Filter};

/// ctFilter is a ConstantTime filter with a pad long enough to leave room for
/// placing waiting keys, and a stash big enough never to drop a history (so a
/// short count is a bug, not the stash's policy).
#[track_caller]
fn ct_filter(capacity: i64) -> Filter {
    must(Config { capacity, horizon: MINUTE, resolution: ms(1), constant_time: us(4), stash: capacity, ..Config::default() })
}

/// whereKey counts h's entries: in the table (as lookups match) and in the
/// stash.
fn where_key(f: &Filter, h: u64) -> (i64, i64) {
    let (fp, tg) = (fingerprint(h), tag(h));
    let p = f.primary(h);
    let (mut table, mut stash) = (0, 0);
    if f.find(p, fp << 1, tg) >= 0 {
        table += 1;
    }
    if f.find(f.alt(p, fp), (fp << 1) | 1, tg) >= 0 {
        table += 1;
    }
    let id = f.stash_id(h, fp, tg);
    if let Some(st) = f.stash.get() {
        let (base, _) = st.set(id);
        for k in base..base + STASH_WAYS as usize {
            if st.get(k).id == id {
                stash += 1;
            }
        }
    }
    (table, stash)
}

/// TestCTWaitingKeyNotRestored: a key that waited for a slot and is touched
/// again once one is free was never evicted: its answer isn't Restored, and
/// Restored() doesn't count it. The table is filled at tick 1; the key waits
/// at half a horizon, and its next touch comes when the fill has expired.
#[test]
fn TestCTWaitingKeyNotRestored() {
    let t = T::new("TestCTWaitingKeyNotRestored");
    let f = must(Config { capacity: 256, horizon: MINUTE, resolution: ms(1), constant_time: us(1), stash: 256, ..Config::default() });
    for i in 0..(f.slots() * 85 / 100) as u64 {
        f.touch_at(mix64(i), 0, 1);
    }
    let mut key = 0u64;
    let mut i = 1u64 << 40;
    while key == 0 {
        let w = f.waited();
        f.touch_at(mix64(i), 0, 30_000);
        if f.waited() != w {
            key = mix64(i);
        }
        i += 1;
    }
    let (tb, st) = where_key(&f, key);
    if tb != 0 || st != 1 {
        fatalf!(t, "key not waiting: {tb} table and {st} stash entries");
    }
    let r = f.touch_at(key, 60_000, 60_002);
    let (tb, _) = where_key(&f, key);
    if tb != 1 {
        fatalf!(t, "key not placed by its touch: {tb} table entries");
    }
    if r.count != 2 || !r.seen || r.restored || f.restored() != 0 {
        fatalf!(t, "touch of a key that waited: {}, Restored() {}; want count 2, seen, not restored", show(&r), f.restored());
    }
}

/// TestCTWaitingKeysKeepCounts: in ConstantTime an insert that finds its two
/// lines full waits in the stash instead of searching, and the pads of later
/// calls place it (by path or eviction, as a plain insert would). Filled to
/// 97% of the slots, every key must keep its exact count, in one place,
/// before and after placement has drained the stash.
#[test]
fn TestCTWaitingKeysKeepCounts() {
    let t = T::new("TestCTWaitingKeysKeepCounts");
    const CAPACITY: i64 = 4096;
    const PER: i64 = 3;
    let f = ct_filter(CAPACITY);
    let n = (f.slots() * 97 / 100) as u64;
    for round in 0..PER as u64 {
        for i in 0..n {
            f.touch_at(mix64(i), 0, 1000 + round);
        }
    }
    let check = |when: &str| {
        for i in 0..n {
            let h = mix64(i);
            let r = f.count_at(h, 60_000, 2000);
            if r.count != PER || r.saturated {
                fatalf!(t, "{when}: key {i} reads {}, want {PER} events", show(&r));
            }
            let (tb, st) = where_key(&f, h);
            if tb + st != 1 {
                fatalf!(t, "{when}: key {i} has {tb} table and {st} stash entries");
            }
        }
    };
    check("after the inserts");
    let waiting = f.waiting();
    for _ in 0..1000 {
        // pads of any length (a sanitizer slows a step past 4 µs)
        if f.waiting() == 0 {
            break;
        }
        f.place(Instant::now(), 1_000_000, 2000);
    }
    logf!(
        t,
        "{n} keys at 97% of {} slots: {} inserts waited, {waiting} waiting after the inserts and Counts, {} after placing; evictions {}, folded {}",
        f.slots(),
        f.waited(),
        f.waiting(),
        f.evictions(),
        f.folded()
    );
    if f.waited() == 0 {
        fatalf!(t, "no insert waited: the test doesn't exercise placement");
    }
    if f.waiting() != 0 {
        fatalf!(t, "{} keys still wait", f.waiting());
    }
    check("after placing");
}

/// TestCTWaitingKeysUnderRaces: workers touch shared keys at 98% load in
/// ConstantTime, so inserts wait in the stash and pads place them while other
/// workers touch the same keys. Every key must end in one place with its
/// exact count.
#[test]
fn TestCTWaitingKeysUnderRaces() {
    let t = T::new("TestCTWaitingKeysUnderRaces");
    let rounds = if short() { 3 } else { 10 };
    const CAPACITY: i64 = 4096;
    const WORKERS: usize = 8;
    const PER: usize = 4;
    for round in 0..rounds as u64 {
        let f = ct_filter(CAPACITY);
        let n = (f.slots() * 98 / 100) as usize;
        let keys: Vec<u64> = (0..n as u64).map(|i| mix64((round << 40) | i)).collect();
        std::thread::scope(|s| {
            for w in 0..WORKERS {
                let (f, keys) = (&f, &keys);
                s.spawn(move || {
                    for p in 0..PER * n / WORKERS {
                        f.touch_at(keys[(p * WORKERS + w) % n], 0, 1000);
                    }
                });
            }
        });
        let mut total = 0i64;
        for (i, &h) in keys.iter().enumerate() {
            let r = f.count_at(h, 60_000, 1000);
            total += r.count;
            let (tb, st) = where_key(&f, h);
            if tb + st != 1 {
                fatalf!(t, "round {round} key {i} has {tb} table and {st} stash entries");
            }
        }
        let want = (PER * n / WORKERS * WORKERS) as i64;
        if total != want {
            fatalf!(
                t,
                "round {round}: counted {total} of {want} touches (waiting {}, evictions {}, folded {})",
                f.deferred.load(SeqCst),
                f.evictions(),
                f.folded()
            );
        }
    }
}
