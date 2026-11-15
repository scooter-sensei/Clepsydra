//! overflowread_test.go.

use std::sync::Arc;
use std::sync::atomic::AtomicI64;
use std::sync::atomic::Ordering::SeqCst;
use std::time::Duration;

use super::check::*;
use super::gorand::Rand;
use super::helpers::*;
use crate::hooks;
use crate::slots::*;
use crate::{Config, Filter};

/// TestCheckAndTouchMatchesTouchSeen: CheckAndTouch's answer is Touch's Seen.
/// Two filters with one Config are fed one seeded sequence, a flood of one-off
/// keys (8x the slots per horizon) against 6,000 repeat keys coming back at
/// random gaps up to the horizon, with SpillMin 1 so every dropped history
/// goes to the overflow table. Filter A calls CheckAndTouchAt, B TouchAt; they
/// differ only in reads, so their states stay equal and every answer must
/// too. Run with the ghost table and the count-min overflow, each in the
/// default mode and in Background, the placer run by hand at seeded points.
#[test]
fn TestCheckAndTouchMatchesTouchSeen() {
    let t = T::new("TestCheckAndTouchMatchesTouchSeen");
    const HORIZON: u64 = 60_000;
    const SPAN: u64 = 3 * 60_000; // ticks: the sequence runs three horizons
    let key: [u8; 16] = [9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 0, 0, 0, 0, 0, 0];
    let wins = [1_000u64, 10_000, 60_000];
    for cms in [false, true] {
        for bg in [false, true] {
            let clock = TestClock::new(1);
            let mut now = 1u64;
            let cfg = Config {
                capacity: 2048,
                horizon: MINUTE,
                resolution: ms(1),
                spill_min: 1,
                key: Some(key),
                clock: clock.func(),
                background: bg,
                ..Config::default()
            };
            let mut fs: Vec<Filter> = Vec::new();
            for _ in 0..2 {
                let f = match Filter::with(cfg.clone(), |c| {
                    c.bg_manual = true; // also under TF_BG=1: no placer thread, so the twins stay in step
                    c.cms = cms;
                }) {
                    Ok(f) => f,
                    Err(e) => fatalf!(t, "{e}"),
                };
                fs.push(f);
            }
            let (a, b) = (&fs[0], &fs[1]);
            let mut rng = Rand::new(5, 1);
            let mut evs: Vec<(u64, u64)> = Vec::new(); // at, h
            for i in 0..6_000u64 {
                let h = a.hash_uint64(i + (1 << 40));
                let mut at = rng.uint64_n(HORIZON);
                while at < SPAN {
                    evs.push((1 + at, h));
                    at += 1 + rng.uint64_n(HORIZON);
                }
            }
            for i in 0..8 * a.slots() as u64 * SPAN / HORIZON {
                evs.push((1 + rng.uint64_n(SPAN), a.hash_uint64(i)));
            }
            evs.sort_by_key(|x| x.0); // stable, as slices.SortStableFunc
            let mut seen_degraded = 0;
            for (i, &(at, h)) in evs.iter().enumerate() {
                now = at;
                clock.set(now);
                let w = wins[rng.int_n(wins.len() as i64) as usize];
                let ra = a.check_and_touch_at(h, w, now);
                let rb = b.touch_at(h, w, now);
                if ra != rb.seen {
                    errorf!(t, "cms {cms}, bg {bg}: call {i} (key {h:x}, window {w}, at {now}): CheckAndTouch {ra}, Touch {}", show(&rb));
                    break;
                }
                if rb.seen && rb.degraded {
                    seen_degraded += 1;
                }
                if a.bg && rng.int_n(50) == 0 {
                    for f in &fs {
                        while f.room.get().is_some() && f.place_next(f.room.get(), now) {}
                    }
                }
            }
            if a.evictions() != b.evictions() || a.folded() != b.folded() || a.lost() != b.lost() || a.occupancy(now) != b.occupancy(now) {
                errorf!(
                    t,
                    "cms {cms}, bg {bg}: the twins' states differ: evictions {}/{}, folded {}/{}, lost {}/{}, occupancy {}/{}",
                    a.evictions(),
                    b.evictions(),
                    a.folded(),
                    b.folded(),
                    a.lost(),
                    b.lost(),
                    a.occupancy(now),
                    b.occupancy(now)
                );
            }
            logf!(
                t,
                "cms {cms}, bg {bg}: {} calls, {seen_degraded} Seen with a dropped history behind it, folded {}, evictions {}",
                evs.len(),
                b.folded(),
                b.evictions()
            );
            if seen_degraded == 0 {
                errorf!(
                    t,
                    "cms {cms}, bg {bg}: Touch never answered Seen with a dropped history behind it (folded {}): the overflow table never answered",
                    b.folded()
                );
            }
            a.close();
            b.close();
        }
    }
}

/// TestConstantTimeReadsOverflowOnEveryHit: a ConstantTime filter reads the
/// overflow table on every CheckAndTouch, Seen or not, so its timing does not
/// depend on the answer. With the table flooded and the overflow table live,
/// 1,000 present keys that answer Seen each read it.
#[test]
fn TestConstantTimeReadsOverflowOnEveryHit() {
    let t = T::new("TestConstantTimeReadsOverflowOnEveryHit");
    let clock = TestClock::new(1);
    let mut now = 1u64;
    let f = match Filter::new(Config {
        capacity: 4096,
        horizon: MINUTE,
        resolution: ms(1),
        constant_time: Duration::from_nanos(200),
        clock: clock.func(),
        ..Config::default()
    }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    let n = 8 * f.slots();
    for i in 0..n {
        let h = f.hash_uint64(i as u64);
        for _ in 0..4 {
            f.touch_at(h, 60_000, now);
        }
        if i % 256 == 0 {
            now += 1;
            clock.set(now);
        }
    }
    let live = f.gh.get().is_some_and(|g| now <= g.until.load(SeqCst));
    if !f.sketch_live(now) || !live || f.folded() == 0 {
        fatalf!(t, "overflow table not live at {now} (folded {})", f.folded());
    }
    let mut present: Vec<u64> = Vec::new();
    let mut i = n - 1;
    while i >= 0 && present.len() < 1_000 {
        let h = f.hash_uint64(i as u64);
        if f.locate(h, fingerprint(h), tag(h)) >= 0 {
            present.push(h);
        }
        i -= 1;
    }
    if present.len() < 1_000 {
        fatalf!(t, "only {} flood keys left in the table", present.len());
    }
    let reads = Arc::new(AtomicI64::new(0));
    let hook = set_hook(&hooks::GHOST_READ, {
        let reads = reads.clone();
        move |()| {
            reads.fetch_add(1, SeqCst);
        }
    });
    for &h in &present {
        if !f.check_and_touch_at(h, 60_000, now) {
            fatalf!(t, "present key {h:x} answered not Seen");
        }
    }
    drop(hook);
    if reads.load(SeqCst) < present.len() as i64 {
        fatalf!(t, "{} overflow reads for {} Seen hits: ConstantTime skipped the overflow table", reads.load(SeqCst), present.len());
    }
    f.close();
}
