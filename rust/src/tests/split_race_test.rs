//! split_race_test.go, sweep_race_test.go, install_race_test.go: moves,
//! evictions, the decay sweep and installs racing a key's touches (claims
//! A23, A26, A27).

use std::sync::atomic::AtomicBool;
use std::sync::atomic::Ordering::SeqCst;
use std::sync::{Arc, Mutex};

use super::check::*;
use super::helpers::*;
use crate::decay::DECAY_EVERY;
use crate::hooks;
use crate::slots::*;
use crate::{Config, Filter};

/// TestEvictionDuringMoveKeepsHistory drives one interleaving in moveTable.
/// A relocation copies X from A to its primary line P; before the move hands
/// off, X is touched, another key's first touch evicts the lightest entry of
/// its two lines, and X is touched again. While copies were visible from
/// their install (until claims A23), the first touch counted in the copy,
/// which lookups reach first. If that evictor took the copy, the second touch
/// found the source, the hand-off failed, and X's history was split: the
/// copy's events went to the stash, the source's stayed in the table, and
/// neither saw the other again. If it took the source, the hand-off failed
/// and abandoned the copy, whose events then had no entry to go back to. Now
/// the copy is hidden until the hand-off: both touches count in the source.
/// X must end with all its events.
#[test]
fn TestEvictionDuringMoveKeepsHistory() {
    let t = T::new("TestEvictionDuringMoveKeepsHistory");
    if force_ct() || force_bg() {
        skip!(t, "ConstantTime and Background inserts don't search on the call path");
    }
    evict_during_move(&t, false); // copy
    evict_during_move(&t, true); // source
}

fn evict_during_move(t: &T, source: bool) {
    let name = if source { "source" } else { "copy" };
    let mut m = move_table(t);
    let x = m.x;
    let t2 = 1 + m.f.horizon + 1; // P's oldest entry has expired: room for X's copy
    // k's first touch moves X to P; y's evicts X's copy, the lightest in P and Q.
    let k = m.key(MV_A, MV_B);
    let mut y = m.key(MV_P, MV_Q);
    if source {
        y = m.key(MV_A, MV_Q); // or X's source, the lightest in A and Q
    }
    let f = Arc::new(m.f);
    let fired = Arc::new(AtomicBool::new(false));
    let hook = set_hook(&hooks::COPIED, {
        let (f, fired) = (f.clone(), fired.clone());
        move |()| {
            if fired.swap(true, SeqCst) {
                return;
            }
            f.touch_at(x, 0, t2); // counts in the source (in the copy while copies were visible)
            f.touch_at(y, 0, t2); // evicts X's copy or source, unless its move is left alone
            f.touch_at(x, 0, t2);
        }
    });
    f.touch_at(k, 0, t2);
    drop(hook);
    if !fired.load(SeqCst) {
        fatalf!(t, "{name}: k's insert did not relocate X");
    }
    let r = f.count_at(x, f.horizon, t2);
    if r.count != 3 {
        fatalf!(t, "{name}: X reads {}, want 3 events (relocations {}, evictions {})", show(&r), f.relocations(), f.evictions());
    }
}

/// TestEvictionRacingMoveStart: an insert picks X's source as its eviction
/// victim before any move of X has begun; a relocation then copies X to its
/// primary line and X is touched (in the copy while copies were visible,
/// claims A23; now in the source); then the evictor goes on, and backs off
/// from a key whose move is under way. X must end with both its events.
#[test]
fn TestEvictionRacingMoveStart() {
    let t = T::new("TestEvictionRacingMoveStart");
    if force_ct() || force_bg() {
        skip!(t, "ConstantTime and Background inserts don't search or evict on the call path");
    }
    let mut m = move_table(&t);
    let x = m.x;
    let t_e = 1 + m.f.horizon; // the evictor still sees P's oldest entry live: its search finds no room
    let t2 = 1 + m.f.horizon + 1; // the relocation sees it expired: room for X's copy
    let (k, y) = (m.key(MV_A, MV_B), m.key(MV_A, MV_Q));
    let f = Arc::new(m.f);
    let (paused, resume, done_e) = (Signal::new(), Signal::new(), Signal::new());
    let _victim = set_hook(&hooks::VICTIM, {
        let (paused, resume) = (paused.clone(), resume.clone());
        move |()| {
            hooks::VICTIM.clear();
            paused.close();
            resume.wait();
        }
    });
    {
        let (f, done_e) = (f.clone(), done_e.clone());
        std::thread::spawn(move || {
            f.touch_at(y, 0, t_e); // chooses X's source, the lightest of A and Q, then waits
            done_e.close();
        });
    }
    paused.wait();
    let fired = Arc::new(AtomicBool::new(false));
    let _copied = set_hook(&hooks::COPIED, {
        let (f, fired) = (f.clone(), fired.clone());
        move |()| {
            hooks::COPIED.clear();
            fired.store(true, SeqCst);
            f.touch_at(x, 0, t2);
            resume.close(); // the evictor goes on
            done_e.wait();
        }
    });
    f.touch_at(k, 0, t2); // relocates X to P
    if !fired.load(SeqCst) {
        fatalf!(t, "k's insert did not relocate X");
    }
    let r = f.count_at(x, f.horizon, t2);
    if r.count != 2 {
        fatalf!(t, "X reads {}, want 2 events (relocations {}, evictions {})", show(&r), f.relocations(), f.evictions());
    }
}

/// TestMoveAfterEvictionKeepsTouches drives one interleaving in moveTable. A
/// relocation reads X in A (to copy it to P); before it copies, another key's
/// first touch evicts X, the lightest entry of A and Q, to the stash. The
/// relocation then copies X's old words to P and hands off, which fails (X's
/// source holds the evictor's key). While copies were visible from their
/// install (until claims A23), X's next touch counted in that copy, and the
/// failed hand-off cleared it with the event: X read 1 of 2. Now the touch
/// waits out the move and finds X in the stash. X must end with every event:
/// the one in the stash and the touch.
#[test]
fn TestMoveAfterEvictionKeepsTouches() {
    let t = T::new("TestMoveAfterEvictionKeepsTouches");
    if force_ct() || force_bg() {
        skip!(t, "ConstantTime and Background inserts don't search or evict on the call path");
    }
    let mut m = move_table(&t);
    let x = m.x;
    let t_e = 1 + m.f.horizon; // the evictor still sees P's oldest entry live: its search finds no room
    let t2 = 1 + m.f.horizon + 1; // the relocation sees it expired: room for X's copy
    let src = m.f.find(MV_A, (fingerprint(x) << 1) | 1, tag(x));
    let (k, y) = (m.key(MV_A, MV_B), m.key(MV_A, MV_Q));
    let f = Arc::new(m.f);
    let (picked, copied) = (Arc::new(AtomicBool::new(false)), Arc::new(AtomicBool::new(false)));
    let done: Arc<Mutex<Option<Done>>> = Arc::default();
    let _picked = set_hook(&hooks::PICKED, {
        let (f, picked) = (f.clone(), picked.clone());
        move |i| {
            if i != src || picked.load(SeqCst) {
                return;
            }
            picked.store(true, SeqCst);
            f.touch_at(y, 0, t_e); // evicts X: no move of X has begun
        }
    });
    let _copied = set_hook(&hooks::COPIED, {
        let (f, picked, copied, done) = (f.clone(), picked.clone(), copied.clone(), done.clone());
        move |()| {
            if !picked.load(SeqCst) || copied.load(SeqCst) {
                return;
            }
            copied.store(true, SeqCst);
            let f = f.clone();
            // the touch runs whole, or waits for the move
            *done.lock().unwrap() = Some(during(move || {
                f.touch_at(x, 0, t2);
            }));
        }
    });
    f.touch_at(k, 0, t2); // relocates X to P
    let (p, c) = (picked.load(SeqCst), copied.load(SeqCst));
    if !p || !c {
        fatalf!(t, "k's insert did not relocate X after reading it (picked {p}, copied {c})");
    }
    if let Some(d) = done.lock().unwrap().take() {
        let _ = d.recv(); // <-done
    }
    if f.evictions() == 0 {
        fatalf!(t, "y's insert evicted nothing");
    }
    let r = f.count_at(x, f.horizon, t2);
    if r.count != 2 {
        fatalf!(t, "X reads {}, want 2 events (relocations {}, evictions {})", show(&r), f.relocations(), f.evictions());
    }
}

/// sweepSetup: a filter holding one key K, touched once at sweepT0, whose
/// touches don't sweep inline; i is K's slot (word index).
#[track_caller]
fn sweep_setup(t: &T) -> (Arc<Filter>, u64, isize) {
    let f = match Filter::new(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), clock: TestClock::new(1).func(), ..Config::default() }) {
        Ok(f) => Arc::new(f),
        Err(e) => fatalf!(t, "{e}"),
    };
    let mut k = 0;
    let mut j = 1u64;
    while k == 0 {
        let h = mix64(j);
        if (h & DECAY_EVERY) != 0 {
            k = h;
        }
        j += 1;
    }
    f.touch_at(k, 0, SWEEP_T0);
    let i = f.locate(k, fingerprint(k), tag(k));
    if i < 0 {
        fatalf!(t, "K is not in the table");
    }
    (f, k, i)
}

/// TestSweepRacingReinsertKeepsKey: the decay sweep clears K's expired slot.
/// Between its first and last write to the slot, K's next touch, 32 units on,
/// puts K back in the slot with a fresh register bit-identical to the old one
/// (one event, K's tag, the same 4 bits: a unit's, an onset since D29). The
/// sweep must leave the new register alone. Before (claims A26), the sweep
/// cleared word0, then CASed word1 from the old register to 0, which took the
/// new one: K's fingerprint was left with an empty register, a slot no lookup
/// matches, and K's next touch went into a second slot (Upper 1 for 2
/// events).
#[test]
fn TestSweepRacingReinsertKeepsKey() {
    let t = T::new("TestSweepRacingReinsertKeepsKey");
    let (f, k, i) = sweep_setup(&t);
    let t1 = SWEEP_T0 + 32 * f.unit; // past the horizon (24 units); 32 units: the same epoch bits
    let fired = Arc::new(AtomicBool::new(false));
    let done: Arc<Mutex<Option<Done>>> = Arc::default();
    let hook = set_hook(&hooks::SWEEPING, {
        let (f, fired, done) = (f.clone(), fired.clone(), done.clone());
        move |()| {
            if fired.swap(true, SeqCst) {
                return;
            }
            let f = f.clone();
            // the touch runs whole, or waits for the sweep
            *done.lock().unwrap() = Some(during(move || {
                f.touch_at(k, 0, t1);
            }));
        }
    });
    f.sweep_line(i as u64 / WORDS_PER_LINE, t1);
    drop(hook);
    if !fired.load(SeqCst) {
        fatalf!(t, "the sweep did not take K's slot");
    }
    if let Some(d) = done.lock().unwrap().take() {
        wait_for(&t, &d, "K's touch");
    }
    f.touch_at(k, 0, t1);
    let r = f.count_at(k, f.horizon, t1);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "K reads {} after two touches at t1, want Upper >= 2", show(&r));
    }
}

/// TestSweepRacingTouchPutsSlotBack: the decay sweep, at a clock past K's
/// horizon, takes K's slot; a touch of K at a clock a horizon behind, which
/// found the slot before, moves its last touch (word0) on first, as a touch
/// in the same unit does. The sweep's clear of word0 then fails, and it must
/// put K's register back: K's touches at that clock find their history.
#[test]
fn TestSweepRacingTouchPutsSlotBack() {
    let t = T::new("TestSweepRacingTouchPutsSlotBack");
    let (f, k, i) = sweep_setup(&t);
    let t_s = SWEEP_T0 + f.horizon + 2 * f.unit; // the sweep's clock: K expired
    let t_l = SWEEP_T0 + 1; // the touch's: K's unit
    let (w0, w1) = (ld(f.w(i)), ld(f.w(i + 1)));
    let moved = Arc::new(AtomicBool::new(false));
    let hook = set_hook(&hooks::SWEEPING, {
        let (f, moved) = (f.clone(), moved.clone());
        move |()| {
            if !moved.load(SeqCst) {
                moved.store(cas(f.w(i), w0, (w0 & !TS_MASK) | t_l), SeqCst);
            }
        }
    });
    f.sweep_line(i as u64 / WORDS_PER_LINE, t_s);
    drop(hook);
    if !moved.load(SeqCst) {
        fatalf!(t, "the touch did not move K's last touch while the sweep held the slot");
    }
    let got = ld(f.w(i + 1));
    if got != w1 {
        errorf!(t, "K's word1 is {got:#x} after the sweep, want its register {w1:#x} back");
    }
    f.touch_at(k, 0, t_l); // the touch's count, after the sweep
    let r = f.count_at(k, f.horizon, t_l);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "K reads {} after two touches, want Upper >= 2", show(&r));
    }
}

/// TestSweepHoldLaggingTouchFindsSlot: as TestSweepRacingTouchPutsSlotBack,
/// but the lagging touch, 5 units after K's last, is live at the sweep's
/// clock, and it counts while the sweep holds the slot: having moved word0,
/// its count of the register finds word1 busy, and it looks K up again. That
/// lookup must wait out the hold (it counts as a move of K's stripe), then
/// find K's slot put back. Without, it missed the held slot and put K, with
/// the touch's event, in a second slot, which K's touches and reads, finding
/// the first, then passed over.
#[test]
fn TestSweepHoldLaggingTouchFindsSlot() {
    let t = T::new("TestSweepHoldLaggingTouchFindsSlot");
    let (f, k, i) = sweep_setup(&t);
    let t_s = SWEEP_T0 + f.horizon + f.unit; // the sweep's clock: K expired
    let t_l = SWEEP_T0 + 5 * f.unit; // the touch's: live at tS
    let w0 = ld(f.w(i));
    let moved = Arc::new(AtomicBool::new(false));
    let done: Arc<Mutex<Option<Done>>> = Arc::default();
    let hook = set_hook(&hooks::SWEEPING, {
        let (f, moved, done) = (f.clone(), moved.clone(), done.clone());
        move |()| {
            if moved.load(SeqCst) {
                return;
            }
            let m = cas(f.w(i), w0, (w0 & !TS_MASK) | t_l);
            moved.store(m, SeqCst);
            if !m {
                return;
            }
            let f = f.clone();
            // the touch's next look, its first count having failed; it runs
            // whole, or waits for the sweep
            *done.lock().unwrap() = Some(during(move || {
                f.touch_at(k, 0, t_l);
            }));
        }
    });
    f.sweep_line(i as u64 / WORDS_PER_LINE, t_s);
    drop(hook);
    if !moved.load(SeqCst) {
        fatalf!(t, "the touch did not move K's last touch while the sweep held the slot");
    }
    if let Some(d) = done.lock().unwrap().take() {
        wait_for(&t, &d, "K's touch");
    }
    let n = f.occupancy(t_l);
    if n != 1 {
        fatalf!(t, "K holds {n} slots, want 1");
    }
    f.touch_at(k, 0, t_s);
    let r = f.count_at(k, f.horizon, t_s);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "K reads {} after touches at {t_l} and {t_s}, want Upper >= 2", show(&r));
    }
}

/// laggingTouchDuringHold returns a hook for testHookInstalling: when an
/// install holds slot i (word1 busy), a touch of k at clock tL, which found
/// the slot before, moves its word0 on (the install then loses the slot and
/// puts k's register back); having found word1 busy when it counted, the
/// touch looks k up again, in a thread whose channel is put in done. moved
/// reports whether the touch moved word0.
fn lagging_touch_during_hold(
    f: &Arc<Filter>,
    k: u64,
    i: isize,
    t_l: u64,
    done: &Arc<Mutex<Option<Done>>>,
    moved: &Arc<AtomicBool>,
) -> impl Fn(isize) + Send + Sync + 'static {
    let w0 = ld(f.w(i));
    let (f, done, moved) = (f.clone(), done.clone(), moved.clone());
    move |j| {
        if moved.load(SeqCst) || j != i {
            return;
        }
        let m = cas(f.w(i), w0, (w0 & !TS_MASK) | t_l);
        moved.store(m, SeqCst);
        if !m {
            return;
        }
        let f = f.clone();
        // the touch runs whole, or waits for the hold
        *done.lock().unwrap() = Some(during(move || {
            f.touch_at(k, 0, t_l);
        }));
    }
}

/// TestInsertOverExpiredHoldLaggingTouch: a first touch of L, at a clock a
/// horizon past K's last touch, takes K's expired slot (install: word1 busy,
/// then word0). Meanwhile a touch of K at a clock 5 units after K's last,
/// live at L's, which found the slot before, moves word0 on: L's install
/// loses the slot and puts K's register back. That touch's next look at K,
/// during the hold, must not put K in a second slot (claims A27).
#[test]
fn TestInsertOverExpiredHoldLaggingTouch() {
    let t = T::new("TestInsertOverExpiredHoldLaggingTouch");
    let (f, k, i) = sweep_setup(&t);
    let t_s = SWEEP_T0 + f.horizon + f.unit; // L's clock: K expired
    let t_l = SWEEP_T0 + 5 * f.unit; // K's touch's: live at tS
    let line = i as u64 / WORDS_PER_LINE;
    let mut l = 0u64; // L: K's line its primary, its own claim stripe, no inline sweep
    let mut j = 1u64 << 40;
    while l == 0 {
        let h = mix64(j);
        if f.primary(h) == line
            && (h & DECAY_EVERY) != 0
            && (h >> (64 - CLAIM_STRIPE_BITS)) != (k >> (64 - CLAIM_STRIPE_BITS))
            && fingerprint(h) != fingerprint(k)
        {
            l = h;
        }
        j += 1;
    }
    let done: Arc<Mutex<Option<Done>>> = Arc::default();
    let moved = Arc::new(AtomicBool::new(false));
    let hook = set_hook(&hooks::INSTALLING, lagging_touch_during_hold(&f, k, i, t_l, &done, &moved));
    f.touch_at(l, 0, t_s);
    drop(hook);
    if !moved.load(SeqCst) {
        fatalf!(t, "L's insert did not take K's slot");
    }
    if let Some(d) = done.lock().unwrap().take() {
        wait_for(&t, &d, "K's touch");
    }
    let n = key_slots(&f, k);
    if n != 1 {
        fatalf!(t, "K holds {n} slots, want 1");
    }
    f.touch_at(k, 0, t_s);
    let r = f.count_at(k, f.horizon, t_s);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "K reads {} after touches at {t_l} and {t_s}, want Upper >= 2", show(&r));
    }
}

/// TestMoveOverExpiredHoldLaggingTouch: as
/// TestInsertOverExpiredHoldLaggingTouch, with the hold a relocation's. A
/// 4-line table: k's lines A and B are full and live, and their entries'
/// other lines are full, except X's: X, homed at P, sits displaced in A, and
/// P holds an entry E expired at t2. k's first touch at t2 moves X to P, into
/// E's slot, while a lagging touch of E moves E's word0 on. E's other line,
/// Q, has room: the touch's next look at E, during the hold, must not put E
/// there.
#[test]
fn TestMoveOverExpiredHoldLaggingTouch() {
    let t = T::new("TestMoveOverExpiredHoldLaggingTouch");
    if force_ct() || force_bg() {
        skip!(t, "ConstantTime and Background inserts don't search or evict on the call path");
    }
    let f = match Filter::new(Config { capacity: 13, horizon: MINUTE, resolution: ms(1), stash: 64, overflow: -1, hot: -1, ..Config::default() }) {
        Ok(f) => Arc::new(f),
        Err(e) => fatalf!(t, "{e}"),
    };
    if f.slots() != 16 {
        fatalf!(t, "want 4 lines, got {} slots", f.slots());
    }
    let mut stripes = std::collections::HashSet::new();
    let mut next = 0u64;
    // an unused key with those lines, on a stripe of its own
    let mut key = |primary: u64, alt: u64| -> u64 {
        loop {
            let h = mix64(next.wrapping_add(1 << 52));
            let (p, s) = (f.primary(h), h >> (64 - CLAIM_STRIPE_BITS));
            if p == primary && f.alt(p, fingerprint(h)) == alt && (h & DECAY_EVERY) != 0 && !stripes.contains(&s) {
                stripes.insert(s);
                return h;
            }
            next += 1;
        }
    };
    const T0: u64 = 1;
    const T1: u64 = 1000;
    let t2 = T0 + f.horizon + 1; // E expired, the rest (from t1) live
    let t_l = T0 + 5 * f.unit; // E's touch's clock: live at t2
    let e = key(MV_P, MV_Q);
    f.touch_at(e, 0, T0);
    for _ in 0..3 {
        f.touch_at(key(MV_P, MV_B), 0, T1);
    }
    let x = key(MV_P, MV_A);
    f.touch_at(x, 0, T1); // P is full: X goes to A, displaced
    for la in [[MV_A, MV_B], [MV_B, MV_A]] {
        while count_line(&f, la[0], T1) < 4 {
            f.touch_at(key(la[0], la[1]), 0, T1);
        }
    }
    if f.find(MV_A, (fingerprint(x) << 1) | 1, tag(x)) < 0 || count_line(&f, MV_P, T1) != 4 || count_line(&f, MV_Q, t_l) != 0 {
        fatalf!(t, "the table is not as the test needs");
    }
    let k = key(MV_A, MV_B);
    let done: Arc<Mutex<Option<Done>>> = Arc::default();
    let moved = Arc::new(AtomicBool::new(false));
    let i = f.locate(e, fingerprint(e), tag(e));
    let hook = set_hook(&hooks::INSTALLING, lagging_touch_during_hold(&f, e, i, t_l, &done, &moved));
    f.touch_at(k, 0, t2); // moves X to P, into E's slot
    drop(hook);
    if !moved.load(SeqCst) {
        fatalf!(t, "no relocation into E's slot");
    }
    if let Some(d) = done.lock().unwrap().take() {
        wait_for(&t, &d, "E's touch");
    }
    let n = key_slots(&f, e);
    if n != 1 {
        fatalf!(t, "E holds {n} slots, want 1");
    }
}
