//! regress_test.go, lost_test.go, dup_test.go, disp_test.go: regression
//! tests for fixed bugs.

use std::sync::Barrier;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering::SeqCst;
use std::time::Duration;

use super::check::*;
use crate::slots::*;
use crate::{Config, Filter};

/// Fixed in a01e3dc: a negative window wrapped to a huge tick count, so
/// every key read as seen. A `Duration` has no negative values: the test
/// holds the zero window to zero ticks and the longest one to a count that
/// did not wrap.
#[test]
fn TestRegressNegativeTicks() {
    let t = T::new("TestRegressNegativeTicks");
    let f = new_test(10, MINUTE);
    let got = f.ticks(Duration::ZERO);
    if got != 0 {
        fatalf!(t, "Ticks(0) = {got}, want 0");
    }
    let got = f.ticks(Duration::MAX);
    if got != (i64::MAX as u64) / 1_000_000 {
        fatalf!(t, "Ticks(the longest duration) = {got}, want 2^63-1 ns in ms");
    }
}

/// Fixed in d1db914: CheckAndTouchBatch indexed out[len(hs)-1] and panicked
/// on an empty batch.
#[test]
fn TestRegressEmptyBatch() {
    let _t = T::new("TestRegressEmptyBatch");
    let f = new_test(10, MINUTE);
    f.check_and_touch_batch(&[], 1, 1, &mut []);
}

/// A tick past the 46 bits of a slot's last touch ran into its saturated,
/// displaced and fingerprint bits: a caller passing Unix microseconds as the
/// *At methods' clock got every key back unseen with no count (a rate limit
/// failed open), and its slots were lost to lookups. Such a tick now panics.
#[test]
fn TestRegressTickPastRange() {
    let t = T::new("TestRegressTickPastRange");
    let f = new_test(1000, MINUTE);
    const H: u64 = 0x9e3779b97f4a7c15;
    let now = 1_793_232_000_000_000u64; // 2026-10-29 00:00 UTC in Unix microseconds: ~1.8e15 > 2^46
    let mut seen = false;
    let panicked = panics(|| {
        f.check_and_touch_at(H, 60_000_000, now);
        seen = f.check_and_touch_at(H, 60_000_000, now + 1);
    });
    if !panicked {
        fatalf!(
            t,
            "touches at Unix microseconds ({now}) returned; the second read seen {seen} and the key then counts {}, want true and 2",
            f.velocity_count_at(H, 60_000_000, now + 2)
        );
    }
}

/// TestLostAtNewestEvent: without an overflow table a history the stash
/// drops is lost, and unseen keys read Degraded while its events may still be
/// in a window: until a horizon after its newest event. A dropper whose clock
/// is behind that event (a lagging caller, or Background's placer, which
/// places a key at the clock of its last touch) must not end that early.
#[test]
fn TestLostAtNewestEvent() {
    let t = T::new("TestLostAtNewestEvent");
    let f = match Filter::new(Config { capacity: 64, horizon: secs(1), resolution: ms(1), overflow: -1, ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    let h = mix64(1);
    let (fp, tg) = (fingerprint(h), tag(h));
    let (w1, _) = f.next_reg(0, tg, 1000, 1000, true);
    f.spill(f.stash_id(h, fp, tg), (fp << 48) | 1000, w1, 990); // dropped by a clock 10 ticks behind its last touch
    if f.lost() != 1 {
        fatalf!(t, "Lost() {}, want 1", f.lost());
    }
    let r = f.count_at(h, 1000, 1995);
    if !r.degraded {
        // its event at 1000 is in this window
        fatalf!(t, "at 1995, a horizon after the dropper's clock but not the event: {r:?}, want Degraded");
    }
    let r = f.count_at(h, 1000, 2001);
    if r.degraded {
        fatalf!(t, "at 2001, past a horizon after the event: {r:?}, want not Degraded");
    }
}

/// Concurrent first touches of the same keys must not leave duplicate slots
/// (which would split a key's count).
#[test]
fn TestConcurrentFirstInsertNoDuplicates() {
    let t = T::new("TestConcurrentFirstInsertNoDuplicates");
    const KEYS: u64 = 20_000;
    const WORKERS: usize = 8;
    let f = new_test(KEYS as i64, MINUTE);
    let start = Barrier::new(WORKERS);
    std::thread::scope(|s| {
        for _ in 0..WORKERS {
            s.spawn(|| {
                start.wait();
                for k in 0..KEYS {
                    f.check_and_touch_at(mix64(k), 0, 1000);
                }
            });
        }
    });
    // Keys waiting for a slot: ConstantTime's stay in the stash, and
    // Background's placer thread drains its wait room (moving entries as it
    // goes), so first wait until the count is 0 or has held still for 50 ms,
    // then count slots between two equal reads of it.
    let (mut n, mut still) = (f.waiting(), 0);
    while n > 0 && still < 50 {
        std::thread::sleep(ms(1));
        let m = f.waiting();
        if m != n {
            (n, still) = (m, 0);
        }
        still += 1;
    }
    let mut occ;
    loop {
        let n = f.waiting();
        occ = f.occupancy(1000);
        if f.waiting() == n {
            occ += n;
            break;
        }
    }
    // Keys in the stash: Background's placer races the workers' touches for
    // its moves and, under contention, now and then runs out of search and
    // evicts one; its history waits in the stash.
    occ += (f.evictions() - f.restored()) as i64;
    let mut total = 0i64;
    for k in 0..KEYS {
        total += f.velocity_count_at(mix64(k), f.ticks(MINUTE), 1000);
    }
    logf!(t, "slots used {occ} for {KEYS} keys; counted {total} of {} touches", KEYS * WORKERS as u64);
    if occ != KEYS as i64 {
        fatalf!(t, "{} duplicate slots", occ - KEYS as i64);
    }
    if total != (KEYS * WORKERS as u64) as i64 {
        fatalf!(t, "counts split or lost: {total} of {}", KEYS * WORKERS as u64);
    }
}

/// findAny is find ignoring the displaced bit.
pub(crate) fn find_any(f: &Filter, line: u64, fp: u64, tg: u64) -> isize {
    let i = f.find(line, fp << 1, tg);
    if i >= 0 {
        return i;
    }
    f.find(line, (fp << 1) | 1, tg)
}

/// Under concurrent churn with relocations, evictions and sweeps, every
/// entry's displaced bit must say which of its two lines it is in; otherwise
/// locate would miss it.
#[test]
fn TestDisplacedBitTracksLine() {
    let t = T::new("TestDisplacedBitTracksLine");
    const CAP_N: u64 = 20_000;
    const WORKERS: u64 = 8;
    let f = new_test(CAP_N as i64, secs(1));
    let horizon = f.ticks(secs(1));
    let clock = AtomicU64::new(1000);
    let key = |k: u64, now: u64| mix64(k + now / horizon * CAP_N);
    std::thread::scope(|s| {
        for w in 0..WORKERS {
            let (f, clock) = (&f, &clock);
            s.spawn(move || {
                for i in 0..300_000u64 {
                    let now = (clock.fetch_add(1, SeqCst) + 1) * horizon / 100_000; // ~24 horizons overall
                    let k = (w * 7919 + i) % (CAP_N * 6 / 5); // 1.2x capacity live
                    f.check_and_touch_at(key(k, now), horizon, now);
                    if i % 97 == 0 {
                        f.decay_step(now, 4);
                    }
                }
            });
        }
    });
    logf!(t, "relocations {}, evictions {}", f.relocations(), f.evictions());
    let now = clock.load(SeqCst) * horizon / 100_000;
    let mut found = 0;
    for k in 0..CAP_N * 6 / 5 {
        let h = key(k, now);
        let (fp, tg) = (fingerprint(h), tag(h));
        let p = f.primary(h);
        let (in_p, in_alt) = (find_any(&f, p, fp, tg), find_any(&f, f.alt(p, fp), fp, tg));
        if in_p >= 0 && (ld(f.w(in_p)) & DISP_BIT) != 0 {
            fatalf!(t, "key {k} in its primary line marked displaced");
        }
        if in_p < 0 && in_alt >= 0 && (ld(f.w(in_alt)) & DISP_BIT) == 0 {
            fatalf!(t, "key {k} in its alternate line not marked displaced");
        }
        if (f.locate(h, fp, tg) >= 0) != (in_p >= 0 || in_alt >= 0) {
            fatalf!(t, "key {k}: locate disagrees with a two-line search");
        }
        if in_p >= 0 || in_alt >= 0 {
            found += 1;
        }
    }
    if found == 0 {
        fatalf!(t, "no keys survived; test exercises nothing");
    }
}
