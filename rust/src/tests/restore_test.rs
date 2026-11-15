//! restore_test.go, restorewait_test.go, mirror_test.go, movecopy_test.go.

use std::sync::Mutex;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::Ordering::SeqCst;

use super::check::*;
use super::helpers::*;
use crate::hooks;
use crate::slots::*;
use crate::stash::SRC_TABLE;
use crate::{Config, Filter, Result};

/// The filter of the restore tests: two lines, a 64-entry stash (one waiting
/// key doesn't force placement: more than one per 16 entries does), a clock
/// that reads 1.
fn restore_filter(t: &T) -> Filter {
    let clock = TestClock::new(1);
    match Filter::new(Config { capacity: 4, horizon: secs(10), resolution: ms(1), clock: clock.func(), stash: 64, ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    }
}

/// TestRestoreBehindLastTouch: a key whose history comes back from the stash
/// in a call with a clock behind the history's last touch (callers of the *At
/// API pass their own clocks, and a concurrent caller's can be a few ticks
/// old) keeps that last touch. Before the fix the key's timestamp went back
/// to the caller's clock, so a later Count whose window began between the two
/// read nothing: Upper 0 for a key touched inside the window, unflagged.
/// Found by TestFuzzUpper. Two paths: an evicted key restored into the table,
/// and, in ConstantTime, a key waiting in the stash touched again there.
#[test]
fn TestRestoreBehindLastTouch() {
    let t = T::new("TestRestoreBehindLastTouch");
    for ct in [false, true] {
        let name = if ct { "ConstantTime waiting" } else { "evicted" };
        if (force_ct() || force_bg()) && !ct {
            logf!(t, "{name}: skipped: ConstantTime (TF_CT=1) and Background (TF_BG=1) evict no key here: inserts wait");
            continue;
        }
        let mut f = restore_filter(&t);
        f.tweak(|c| c.ct = ct);
        // Two lines, eight slots: touch keys at 1000 until one is in the
        // stash (evicted, or waiting for a slot in ConstantTime).
        let mut k = 0u64;
        while k < 64 {
            f.touch_at(mix64(k), 0, 1000);
            if f.evictions() + f.waited() > 0 {
                break;
            }
            k += 1;
        }
        let mut v = 0u64;
        for i in 0..k + 1 {
            let w = where_is(&f, mix64(i), 1000);
            if w.contains("stash") && !w.contains("slot") {
                v = mix64(i);
            }
        }
        if v == 0 {
            fatalf!(t, "{name}: no key in the stash after {} touches (evictions {}, waited {})", k + 1, f.evictions(), f.waited());
        }
        f.touch_at(v, 0, 990); // a caller whose clock is 10 ticks behind v's last touch
        // v's events: 1000 and 990. In [995, 1010] one of them.
        let r = f.count_at(v, 15, 1010);
        if r.upper < 1 || r.saturated {
            fatalf!(t, "{name}: Count after a touch behind the last one: {r:?}, want Upper >= 1 (events at 990 and 1000; {})", where_is(&f, v, 1010));
        }
        let r = f.count_at(v, 30, 1010);
        if r.upper < 2 {
            fatalf!(t, "{name}: Count over both events: {r:?}, want Upper >= 2");
        }
    }
}

/// TestEvictedKeyThatWaitsIsRestored: in Background and ConstantTime a first
/// touch that finds both lines full waits for a slot. An evicted key's touch
/// that does so still brought its history back from the stash: that answer is
/// Restored and Restored() counts it, once; the touch that later installs the
/// waiting key isn't (it recalls a waiting entry). Before, the waiting touch
/// cleared both, so such a key was never reported back.
#[test]
fn TestEvictedKeyThatWaitsIsRestored() {
    let t = T::new("TestEvictedKeyThatWaitsIsRestored");
    for mode in ["Background", "ConstantTime"] {
        let mut f = restore_filter(&t);
        // Evict on the call path first (TF_CT or TF_BG would make inserts wait).
        f.tweak(|c| (c.ct, c.bg) = (false, false));
        // Two lines, eight slots: touch keys at 1000 until one is evicted.
        let mut k = 0u64;
        while k < 64 && f.evictions() == 0 {
            f.touch_at(mix64(k), 0, 1000);
            k += 1;
        }
        let mut v = 0u64;
        for i in 0..k {
            let w = where_is(&f, mix64(i), 1000);
            if w.contains("stash") && !w.contains("slot") {
                v = mix64(i);
            }
        }
        if v == 0 {
            fatalf!(t, "{mode}: no evicted key in the stash after {k} touches (evictions {})", f.evictions());
        }
        if mode == "Background" {
            f.tweak(|c| (c.bg, c.bg_manual) = (true, true));
        } else {
            f.tweak(|c| c.ct = true);
        }
        // Both of v's lines are full and live: v waits, its history back.
        let r = f.touch_at(v, f.horizon, 1000);
        if f.waited() != 1 {
            fatalf!(t, "{mode}: v didn't wait for a slot (waited {}; {})", f.waited(), where_is(&f, v, 1000));
        }
        if !r.restored || r.count != 2 || !r.seen || f.restored() != 1 {
            fatalf!(t, "{mode}: waiting touch of an evicted key: {r:?}, Restored() {}; want count 2, seen, restored, Restored() 1", f.restored());
        }
        // Free a slot of v's primary line: v's next touch installs it.
        let base = (f.primary(v) * WORDS_PER_LINE) as isize;
        st(f.w(base), 0);
        st(f.w(base + 1), 0);
        let r = f.touch_at(v, f.horizon, 1000);
        if f.locate(v, fingerprint(v), tag(v)) < 0 {
            fatalf!(t, "{mode}: v not installed: {}", where_is(&f, v, 1000));
        }
        if r.restored || r.count != 3 || f.restored() != 1 {
            fatalf!(t, "{mode}: touch that installs the waiting key: {r:?}, Restored() {}; want count 3, not restored, Restored() 1", f.restored());
        }
    }
}

/// TestMirrorKeyInFoundSlotNotCounted: keys H and K share their fingerprint
/// and tag, and K's primary line is H's alternate, so K lives in H's primary
/// line displaced: only the displaced bit tells their entries apart (find
/// matches it). When K's entry takes the slot a lookup found H in (a
/// relocation moving H out and K in, between a batch's warm and its touch, or
/// between a lookup and its load), H's touch and read must not take K's slot
/// for H's. Before, they checked the fingerprint alone: H's touch counted in
/// K's register (K one high, H's event gone).
#[test]
fn TestMirrorKeyInFoundSlotNotCounted() {
    let t = T::new("TestMirrorKeyInFoundSlotNotCounted");
    let clock = TestClock::new(1);
    let f = match Filter::new(Config { capacity: 54, horizon: MINUTE, resolution: ms(1), clock: clock.func(), ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    const FP: u64 = 0x1234;
    const TG: u64 = 0x567;
    let mut p = 0u64;
    if f.alt(p, FP) == p {
        p = 1;
    }
    let q = f.alt(p, FP);
    // bits 0..35 that primary maps to line
    let with_primary = |line: u64| (FP << 48) | (TG << 36) | (((line << 36) + f.lines - 1) / f.lines);
    let (h, k) = (with_primary(p), with_primary(q));
    if f.primary(h) != p || f.primary(k) != q || fingerprint(h) != FP || tag(h) != TG || q == p {
        fatalf!(t, "keys not built as meant: lines {}, {} (want {p}, {q})", f.primary(h), f.primary(k));
    }
    const T0: u64 = 60_000;
    f.touch_at(h, 0, T0);
    let i0 = f.locate(h, FP, TG);
    if i0 < 0 || (i0 as u64) / WORDS_PER_LINE != p {
        fatalf!(t, "H not in its primary line (slot {i0})");
    }
    // K's entry, displaced, in the slot H was found in.
    let (kw1, _) = f.next_reg(0, TG, T0, T0, true);
    st(f.w(i0), (FP << 48) | DISP_BIT | T0);
    st(f.w(i0 + 1), kw1);
    if f.locate(k, FP, TG) != i0 {
        fatalf!(t, "K not found in H's old slot");
    }
    let (w0, _, src) = f.read(h, T0, i0);
    if src == SRC_TABLE && (w0 & DISP_BIT) != 0 {
        errorf!(t, "H's read from the found slot returned K's words");
    }
    if force_ct() {
        f.touch_at(h, 0, T0 + 1); // ConstantTime's touch takes no slot from warm: the lookup path only
    } else {
        f.c.touch(h, f.horizon, T0 + 1, true, i0); // as a batch does, from warm's slot
    }
    let r = f.count_at(k, f.horizon, T0 + 1);
    if r.count != 1 {
        fatalf!(t, "K reads {r:?} after H's touch, want count 1: H's touch counted in K's slot");
    }
    if f.locate(h, FP, TG) < 0 {
        fatalf!(t, "H's touch went to no slot");
    }
}

/// TestReadDuringMoveSeesEveryEvent drives one interleaving in moveTable. A
/// relocation copies X from A to its primary line P, which lookups reach
/// first; before the move hands off, a touch that found X at the source just
/// before the copy went in (a batch's warm, or any lookup a moment earlier)
/// counts there. X's next count, and its next touch, must see that event.
/// While copies were visible from their install (until claims A23) they found
/// the copy, without it: Upper 1 for 2 events, then 2 for 3.
#[test]
fn TestReadDuringMoveSeesEveryEvent() {
    let t = T::new("TestReadDuringMoveSeesEveryEvent");
    if force_ct() || force_bg() {
        skip!(t, "ConstantTime and Background inserts don't search on the call path");
    }
    let mut m = move_table(&t);
    let x = m.x;
    let t2 = 1 + m.f.horizon + 1; // P's oldest entry has expired: room for X's copy
    let src = m.f.find(MV_A, (fingerprint(x) << 1) | 1, tag(x));
    let k = m.key(MV_A, MV_B); // its first touch moves X to P
    let f = std::sync::Arc::new(m.f);
    let fired = std::sync::Arc::new(AtomicBool::new(false));
    let answers: std::sync::Arc<Mutex<(Result, Result)>> = Default::default();
    let _h = {
        let (f, fired, answers) = (f.clone(), fired.clone(), answers.clone());
        set_hook(&hooks::COPIED, move |()| {
            if fired.swap(true, SeqCst) {
                return;
            }
            f.c.touch(x, 0, t2, false, src); // starts from the slot it found X in: the source
            let count = f.count_at(x, f.horizon, t2);
            let touch = f.touch_at(x, f.horizon, t2);
            *answers.lock().unwrap() = (count, touch);
        })
    };
    f.touch_at(k, 0, t2);
    if !fired.load(SeqCst) {
        fatalf!(t, "k's insert did not relocate X");
    }
    let (count, touch) = *answers.lock().unwrap();
    if count.upper < 2 && !count.saturated {
        errorf!(t, "during the move, X's count read {count:?}, want Upper >= 2");
    }
    if touch.upper < 3 && !touch.saturated {
        errorf!(t, "during the move, X's touch read {touch:?}, want Upper >= 3");
    }
    let r = f.count_at(x, f.horizon, t2);
    if r.count != 3 {
        errorf!(t, "after the move, X reads {r:?}, want 3 events (relocations {})", f.relocations());
    }
}
