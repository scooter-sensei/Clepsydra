//! tagstripe_test.go: holds counted on the stripe of the tag they took, and
//! counts that land in a register of the same tag (claims A28, A29, A30).
//! keyAt, twoLineFPs, slotsOf, newMinuteFilter, during and waitFor are in
//! helpers.rs.

use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicBool, AtomicI64};
use std::sync::{Arc, Mutex};

use super::check::*;
use super::helpers::*;
use crate::hooks::{self, Hook};
use crate::slots::*;
use crate::{Filter, Result};

/// TestCountAcrossSameTagHandOver (claims A29): K's touch at the tick of its
/// last checks K's slot, then, before its count's CAS, an insert of K2, whose
/// tag is K's, evicts K (one event, the lightest of the 8 slots) and takes
/// its slot with a fresh register bit-identical to K's. The CAS from K's
/// register then succeeds, into K2's: before, K's history went to the stash
/// without the touch (case 12598's kind).
#[test]
fn TestCountAcrossSameTagHandOver() {
    let t = T::new("TestCountAcrossSameTagHandOver");
    let f = Arc::new(new_minute_filter(4));
    if f.lines != 2 {
        fatalf!(t, "lines {}, want 2", f.lines);
    }
    const T0: u64 = 60_000;
    const TG: u64 = 0x5a5;
    let fps = two_line_fps(&f, 0, 0x1100, 9);
    for j in 0..7u64 {
        // two events each
        let h = key_at(&f, j % 2, fps[j as usize], 0x100 + j);
        f.touch_at(h, 0, T0);
        f.touch_at(h, 0, T0);
    }
    let (k, k2) = (key_at(&f, 1, fps[7], TG), key_at(&f, 0, fps[8], TG));
    f.touch_at(k, 0, T0);
    let ks = slots_of(&f, k);
    if ks.len() != 1 || f.occupancy(T0) != 8 {
        fatalf!(t, "setup: K in {}, occupancy {}", gov(&ks), f.occupancy(T0));
    }
    let (i, kw1) = (ks[0], ld(f.w(ks[0] + 1)));
    let (fired, same) = (Arc::new(AtomicBool::new(false)), Arc::new(AtomicBool::new(false)));
    let hook = set_hook(&hooks::BUMPING, {
        let (f, fired, same) = (f.clone(), fired.clone(), same.clone());
        move |j| {
            if fired.load(SeqCst) || j != i {
                return;
            }
            fired.store(true, SeqCst);
            f.touch_at(k2, 0, T0);
            let sl = slots_of(&f, k2);
            same.store(sl.len() == 1 && sl[0] == i && ld(f.w(i + 1)) == kw1, SeqCst);
        }
    });
    f.touch_at(k, 0, T0);
    drop(hook);
    if !fired.load(SeqCst) || !same.load(SeqCst) {
        fatalf!(t, "K2 did not take K's slot with K's word1 ({kw1:#x}) during K's count");
    }
    let r = f.count_at(k, f.horizon, T0);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "K reads {} after 2 touches", show(&r));
    }
    let r = f.count_at(k2, f.horizon, T0);
    if r.upper < 1 {
        fatalf!(t, "K2 reads {} after 1 touch", show(&r));
    }
}

/// TestCountThenEvictionCountsOnce (claims A29): right after K's count's CAS,
/// an insert of K2, of another tag, evicts K (two events now, the lightest of
/// the 8 slots) and takes its slot. word0, read again, is K2's, but the count
/// went with K's history to the stash: K must not count it again (the slot
/// holds another tag). The first fix B looked again on any change of word0,
/// and K read one high (the racy stress's kind).
#[test]
fn TestCountThenEvictionCountsOnce() {
    let t = T::new("TestCountThenEvictionCountsOnce");
    let f = Arc::new(new_minute_filter(4));
    if f.lines != 2 {
        fatalf!(t, "lines {}, want 2", f.lines);
    }
    const T0: u64 = 60_000;
    let fps = two_line_fps(&f, 0, 0x1100, 9);
    for j in 0..7u64 {
        // three events each
        let h = key_at(&f, j % 2, fps[j as usize], 0x100 + j);
        for _ in 0..3 {
            f.touch_at(h, 0, T0);
        }
    }
    let (k, k2) = (key_at(&f, 1, fps[7], 0x5a5), key_at(&f, 0, fps[8], 0x5a6));
    f.touch_at(k, 0, T0);
    let ks = slots_of(&f, k);
    if ks.len() != 1 || f.occupancy(T0) != 8 {
        fatalf!(t, "setup: K in {}, occupancy {}", gov(&ks), f.occupancy(T0));
    }
    let i = ks[0];
    let (fired, taken) = (Arc::new(AtomicBool::new(false)), Arc::new(AtomicBool::new(false)));
    let hook = set_hook(&hooks::BUMPED, {
        let (f, fired, taken) = (f.clone(), fired.clone(), taken.clone());
        move |j| {
            if fired.load(SeqCst) || j != i {
                return;
            }
            fired.store(true, SeqCst);
            f.touch_at(k2, 0, T0);
            let sl = slots_of(&f, k2);
            taken.store(sl.len() == 1 && sl[0] == i, SeqCst);
        }
    });
    f.touch_at(k, 0, T0);
    drop(hook);
    if !fired.load(SeqCst) || !taken.load(SeqCst) {
        fatalf!(t, "K2 did not take K's slot after K's count");
    }
    let r = f.count_at(k, f.horizon, T0);
    if r.count != 2 || r.upper != 2 {
        fatalf!(t, "K reads {} after 2 touches", show(&r));
    }
}

/// TestCountThenMoveCountsOnce (claims A29): right after K's count's CAS, an
/// insert of N, whose two lines are full, moves K (the first entry of N's
/// first line) to K's other line and takes its slot. The count went with the
/// copy: K must not count it again.
#[test]
fn TestCountThenMoveCountsOnce() {
    let t = T::new("TestCountThenMoveCountsOnce");
    let f = Arc::new(new_minute_filter(1000));
    const P: u64 = 7;
    const T0: u64 = 60_000;
    let fk = two_line_fps(&f, P, 0x7100, 1)[0];
    let a = f.alt(P, fk);
    let mut fn_ = 0u64;
    for c in two_line_fps(&f, P, 0x7200, 64) {
        if f.alt(P, c) != a {
            fn_ = c;
            break;
        }
    }
    let b = f.alt(P, fn_);
    let k = key_at(&f, P, fk, 0x5a5);
    f.touch_at(k, 0, T0);
    for (j, l) in [P, P, P, b, b, b, b].into_iter().enumerate() {
        // N's lines full, K's other line empty
        let h = key_at(&f, l, 0x7300 + j as u64, 0x100 + j as u64);
        f.touch_at(h, 0, T0);
        f.touch_at(h, 0, T0);
    }
    let n = key_at(&f, P, fn_, 0x5a6);
    let ks = slots_of(&f, k);
    if ks.len() != 1 || ks[0] != (P * WORDS_PER_LINE) as isize || f.occupancy(T0) != 8 {
        fatalf!(t, "setup: K in {}, occupancy {}", gov(&ks), f.occupancy(T0));
    }
    let i = ks[0];
    let (fired, moved) = (Arc::new(AtomicBool::new(false)), Arc::new(AtomicBool::new(false)));
    let hook = set_hook(&hooks::BUMPED, {
        let (f, fired, moved) = (f.clone(), fired.clone(), moved.clone());
        move |j| {
            if fired.load(SeqCst) || j != i {
                return;
            }
            fired.store(true, SeqCst);
            f.touch_at(n, 0, T0);
            let (sn, sk) = (slots_of(&f, n), slots_of(&f, k));
            moved.store(sn.len() == 1 && sn[0] == i && sk.len() == 1 && sk[0] as u64 / WORDS_PER_LINE == a, SeqCst);
        }
    });
    f.touch_at(k, 0, T0);
    drop(hook);
    if !fired.load(SeqCst) || !moved.load(SeqCst) {
        fatalf!(t, "N did not move K out of its slot after K's count");
    }
    let r = f.count_at(k, f.horizon, T0);
    if r.count != 2 || r.upper != 2 {
        fatalf!(t, "K reads {} after 2 touches", show(&r));
    }
}

/// TestCountIntoHeldSameTagSlotLooksAgain (claims A29): K's count lands in
/// K2's register, as in TestCountAcrossSameTagHandOver; then, before K's
/// touch reads word0 again, a touch of K2 10 units on holds the slot
/// (realign) for 20 ms. word0 is K2's and word1 busy: the touch must wait out
/// the hold, which puts back a register of K's tag, and count afresh. Reading
/// the busy word1 as another tag, K would read one low. With realign's
/// 16-byte CAS there is no hold: K's touch finds K2's realign done, with K's
/// tag in word1.
#[test]
fn TestCountIntoHeldSameTagSlotLooksAgain() {
    let t = T::new("TestCountIntoHeldSameTagSlotLooksAgain");
    realign_modes(|mode| count_into_held_same_tag_slot(&t, mode));
}

fn count_into_held_same_tag_slot(t: &T, mode: &str) {
    let f = Arc::new(new_minute_filter(4));
    if f.lines != 2 {
        fatalf!(t, "{mode}: lines {}, want 2", f.lines);
    }
    const T0: u64 = 60_000;
    const TG: u64 = 0x5a5;
    let fps = two_line_fps(&f, 0, 0x1100, 9);
    for j in 0..7u64 {
        // two events each
        let h = key_at(&f, j % 2, fps[j as usize], 0x100 + j);
        f.touch_at(h, 0, T0);
        f.touch_at(h, 0, T0);
    }
    let (k, k2) = (key_at(&f, 1, fps[7], TG), key_at(&f, 0, fps[8], TG));
    f.touch_at(k, 0, T0);
    let ks = slots_of(&f, k);
    if ks.len() != 1 || f.occupancy(T0) != 8 {
        fatalf!(t, "{mode}: setup: K in {}, occupancy {}", gov(&ks), f.occupancy(T0));
    }
    let (i, kw1) = (ks[0], ld(f.w(ks[0] + 1)));
    let t1 = T0 + 10 * f.unit;
    let stage = Arc::new(AtomicI64::new(0));
    let in_hold = Arc::new(AtomicBool::new(false));
    let (held, release, done) = (Signal::new(), Signal::new(), Signal::new());
    let h1 = set_hook(&hooks::STAMPED, {
        let (in_hold, held, release) = (in_hold.clone(), held.clone(), release.clone());
        move |()| {
            // K2's realign, word1 busy
            if in_hold.compare_exchange(false, true, SeqCst, SeqCst).is_ok() {
                held.close();
                release.wait();
            }
        }
    });
    let h2 = set_hook(&hooks::BUMPING, {
        let (f, stage) = (f.clone(), stage.clone());
        move |j| {
            if stage.load(SeqCst) == 0 && j == i {
                stage.store(1, SeqCst);
                f.touch_at(k2, 0, T0);
                let sl = slots_of(&f, k2);
                if sl.len() == 1 && sl[0] == i && ld(f.w(i + 1)) == kw1 {
                    stage.store(2, SeqCst);
                }
            }
        }
    });
    let h3 = set_hook(&hooks::BUMPED, {
        let (f, stage, held, release, done) = (f.clone(), stage.clone(), held.clone(), release.clone(), done.clone());
        move |j| {
            if stage.load(SeqCst) == 2 && j == i {
                stage.store(3, SeqCst);
                let (f, done) = (f.clone(), done.clone());
                std::thread::spawn(move || {
                    f.touch_at(k2, 0, t1);
                    done.close();
                });
                held.wait();
                let release = release.clone();
                std::thread::spawn(move || {
                    // time.AfterFunc
                    std::thread::sleep(ms(20));
                    release.close();
                });
            }
        }
    });
    f.touch_at(k, 0, T0);
    drop((h2, h3));
    let st = stage.load(SeqCst);
    if st != 3 {
        fatalf!(t, "{mode}: stage {st}: K's count did not land in K2's slot");
    }
    if !done.wait_timeout(secs(5)) {
        fatalf!(t, "{mode}: K2's touch did not return");
    }
    drop(h1);
    let r = f.count_at(k, f.horizon, T0);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "{mode}: K reads {} after 2 touches", show(&r));
    }
}

/// keyPair: K and L with primary line 7 (empty), different fingerprints,
/// claim stripes and tags (the hooks run K's insert inside L's).
fn key_pair() -> (Arc<Filter>, u64, u64, u64) {
    let f = Arc::new(new_minute_filter(1000));
    const P: u64 = 7;
    let k = key_at(&f, P, two_line_fps(&f, P, 0x2200, 1)[0], 0x123);
    let l = key_at(&f, P, two_line_fps(&f, P, 0x6600, 1)[0], 0x456);
    (f, k, l, P)
}

/// tornOpenHold runs L's insert, which reads line p's first slot open (word0
/// 0); K's insert then takes the slot; L reads its word1 (K's) and its
/// install takes word1 busy from K's register, then fails on word0 and puts
/// it back. during runs while L holds K's slot.
#[track_caller]
fn torn_open_hold(t: &T, f: &Arc<Filter>, k: u64, l: u64, p: u64, t0: u64, during: impl Fn() + Send + Sync + 'static) {
    let s = (p * WORDS_PER_LINE) as isize;
    let stage = Arc::new(AtomicI64::new(0));
    let _h1 = set_hook(&hooks::OPEN_SEEN, {
        let (f, stage) = (f.clone(), stage.clone());
        move |i| {
            if stage.load(SeqCst) == 0 && i == s {
                stage.store(1, SeqCst);
                f.touch_at(k, 0, t0);
                stage.store(2, SeqCst);
            }
        }
    });
    let _h2 = set_hook(&hooks::INSTALLING, {
        let stage = stage.clone();
        move |i| {
            if stage.load(SeqCst) == 2 && i == s {
                stage.store(3, SeqCst);
                during();
            }
        }
    });
    f.touch_at(l, 0, t0);
    let st = stage.load(SeqCst);
    if st != 3 {
        fatalf!(t, "stage {st}: L's install did not take K's slot");
    }
}

/// TestTornOpenScanHoldHidesKey (claims A28): K's second touch runs while L
/// holds K's slot. The hold must count on K's stripe (the tag of the word1 it
/// took): before, putOver counted none over a word0 of 0, and K's touch,
/// missing K, put it in a second slot (case 3974's kind).
#[test]
fn TestTornOpenScanHoldHidesKey() {
    let t = T::new("TestTornOpenScanHoldHidesKey");
    let (f, k, l, p) = key_pair();
    const T0: u64 = 60_000;
    let done: Arc<Mutex<Option<Done>>> = Arc::default();
    torn_open_hold(&t, &f, k, l, p, T0, {
        let (f, done) = (f.clone(), done.clone());
        move || {
            let f = f.clone();
            *done.lock().unwrap() = Some(during(move || {
                f.touch_at(k, 0, T0);
            }));
        }
    });
    let d = done.lock().unwrap().take();
    match d {
        Some(d) => wait_for(&t, &d, "K's touch"),
        None => fatalf!(t, "K's touch did not run"),
    }
    let sl = slots_of(&f, k);
    if sl.len() != 1 {
        fatalf!(t, "K in slots {}", gov(&sl));
    }
    let r = f.count_at(k, f.horizon, T0);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "K reads {} after 2 touches", show(&r));
    }
}

/// TestTornOpenScanHoldRead (claims A28): a Count of K during L's hold must
/// wait it out: before, it found K in no slot and answered absent (case
/// 1220's kind).
#[test]
fn TestTornOpenScanHoldRead() {
    let t = T::new("TestTornOpenScanHoldRead");
    let (f, k, l, p) = key_pair();
    const T0: u64 = 60_000;
    let done: Arc<Mutex<Option<Done>>> = Arc::default();
    let r: Arc<Mutex<Result>> = Arc::default();
    torn_open_hold(&t, &f, k, l, p, T0, {
        let (f, done, r) = (f.clone(), done.clone(), r.clone());
        move || {
            let (f, r) = (f.clone(), r.clone());
            *done.lock().unwrap() = Some(during(move || {
                *r.lock().unwrap() = f.count_at(k, f.horizon, T0);
            }));
        }
    });
    let d = done.lock().unwrap().take();
    match d {
        Some(d) => wait_for(&t, &d, "K's count"),
        None => fatalf!(t, "K's count did not run"),
    }
    let r = *r.lock().unwrap();
    if r.upper < 1 && !r.saturated {
        fatalf!(t, "K read {} during another key's hold, after 1 touch", show(&r));
    }
}

/// sweepOverJ: a filter whose line 7's first slot (word index s) holds J,
/// tagged tgJ, touched once at sweepT0.
#[track_caller]
fn sweep_over_j(t: &T, tg_j: u64) -> (Arc<Filter>, u64, isize) {
    let f = Arc::new(new_minute_filter(1000));
    const P: u64 = 7;
    let j = key_at(&f, P, two_line_fps(&f, P, 0x3300, 1)[0], tg_j);
    f.touch_at(j, 0, SWEEP_T0);
    let s = (P * WORDS_PER_LINE) as isize;
    let sl = slots_of(&f, j);
    if sl.len() != 1 || sl[0] != s {
        fatalf!(t, "setup: J in {}", gov(&sl));
    }
    (f, P, s)
}

/// sweepHolds runs the decay sweep of line p at tS; when the hook seen fires
/// on slot s, K's touch at tS takes the slot (J expired there), and ok checks
/// the result; during runs while the sweep holds the slot.
#[expect(clippy::too_many_arguments, reason = "a helper of the Go test, parameter for parameter")]
#[track_caller]
fn sweep_holds(
    t: &T,
    f: &Arc<Filter>,
    p: u64,
    s: isize,
    k: u64,
    t_s: u64,
    seen: &'static Hook<isize>,
    ok: impl Fn(&Filter) -> bool + Send + Sync + 'static,
    during: impl Fn() + Send + Sync + 'static,
) {
    let stage = Arc::new(AtomicI64::new(0));
    let _h1 = set_hook(seen, {
        let (f, stage) = (f.clone(), stage.clone());
        move |i| {
            if stage.load(SeqCst) == 0 && i == s {
                stage.store(1, SeqCst);
                f.touch_at(k, 0, t_s);
                if ok(&f) {
                    stage.store(2, SeqCst);
                }
            }
        }
    });
    let _h2 = set_hook(&hooks::SWEEPING, {
        let stage = stage.clone();
        move |()| {
            if stage.load(SeqCst) == 2 {
                stage.store(3, SeqCst);
                during();
            }
        }
    });
    f.sweep_line(p, t_s);
    let st = stage.load(SeqCst);
    if st != 3 {
        fatalf!(t, "stage {st}: the sweep did not hold K's slot");
    }
}

/// A `during` that touches k at now on f, its channel put in done.
fn touch_during(f: &Arc<Filter>, k: u64, now: u64, done: &Arc<Mutex<Option<Done>>>) -> impl Fn() + Send + Sync + 'static {
    let (f, done) = (f.clone(), done.clone());
    move || {
        let f = f.clone();
        *done.lock().unwrap() = Some(during(move || {
            f.touch_at(k, 0, now);
        }));
    }
}

/// TestSweepTornHoldHidesKey (claims A28): the sweep reads the slot's word0
/// (J's, expired), K's insert takes the slot, the sweep reads word1 (K's) and
/// holds K's slot. Counted on J's stripe, the hold let K's next touch put K in
/// a second slot.
#[test]
fn TestSweepTornHoldHidesKey() {
    let t = T::new("TestSweepTornHoldHidesKey");
    let (f, p, s) = sweep_over_j(&t, 0x321);
    let t_s = SWEEP_T0 + f.horizon + 2 * f.unit;
    let k = key_at(&f, p, two_line_fps(&f, p, 0x4400, 1)[0], 0x654);
    let done: Arc<Mutex<Option<Done>>> = Arc::default();
    sweep_holds(
        &t,
        &f,
        p,
        s,
        k,
        t_s,
        &hooks::SWEEP_SEEN,
        move |f| {
            let sl = slots_of(f, k);
            sl.len() == 1 && sl[0] == s
        },
        touch_during(&f, k, t_s, &done),
    );
    let d = done.lock().unwrap().take();
    match d {
        Some(d) => wait_for(&t, &d, "K's touch"),
        None => fatalf!(t, "K's touch did not run"),
    }
    let (sl, r) = (slots_of(&f, k), f.count_at(k, f.horizon, t_s));
    if sl.len() != 1 || (r.upper < 2 && !r.saturated) {
        fatalf!(t, "K in slots {}, reads {} after 2 touches", gov(&sl), show(&r));
    }
}

/// TestSweepSameTagHoldHidesKey (claims A28): the sweep reads both of J's
/// words (expired, one event); K, whose tag is J's, takes the slot 32 units
/// on with a fresh register bit-identical to J's, and the sweep's CAS from
/// J's register holds K's slot. A consistent read of the two words (word0
/// again after word1) would not catch this; counting on the tag's stripe
/// does.
#[test]
fn TestSweepSameTagHoldHidesKey() {
    let t = T::new("TestSweepSameTagHoldHidesKey");
    const TG: u64 = 0x2a7;
    let (f, p, s) = sweep_over_j(&t, TG);
    let jw1 = ld(f.w(s + 1));
    let t_s = SWEEP_T0 + 32 * f.unit;
    let k = key_at(&f, p, two_line_fps(&f, p, 0x5500, 1)[0], TG);
    let done: Arc<Mutex<Option<Done>>> = Arc::default();
    sweep_holds(
        &t,
        &f,
        p,
        s,
        k,
        t_s,
        &hooks::SWEEP_READ,
        move |f| {
            let sl = slots_of(f, k);
            sl.len() == 1 && sl[0] == s && ld(f.w(s + 1)) == jw1
        },
        touch_during(&f, k, t_s, &done),
    );
    let d = done.lock().unwrap().take();
    match d {
        Some(d) => wait_for(&t, &d, "K's touch"),
        None => fatalf!(t, "K's touch did not run"),
    }
    let (sl, r) = (slots_of(&f, k), f.count_at(k, f.horizon, t_s));
    if sl.len() != 1 || (r.upper < 2 && !r.saturated) {
        fatalf!(t, "K in slots {}, reads {} after 2 touches", gov(&sl), show(&r));
    }
}

/// TestStaleHotResetKeepsTouches (claims A30): K is hot. At t1 touch A finds
/// K's slot past the horizon and resets it; touch B, after A's word0, finds K
/// live. Before, A moved word0, then reset whatever word1 held by a CAS: B's
/// event was gone whether B had added to the entry (word1 unchanged: 300
/// events at sweepT0 stay in the entry's oldest bucket 25 units on, so B
/// doesn't demote it; case 4291's kind) or demoted it (50 units on, the
/// entry's events are past it, and B hands the register, with its event, back
/// to the slot; case 3490's kind). With realign's 16-byte CAS, B runs after
/// A's realign.
#[test]
fn TestStaleHotResetKeepsTouches() {
    let t = T::new("TestStaleHotResetKeepsTouches");
    for gap in [25u64, 50] {
        realign_modes(|mode| stale_hot_reset(&t, &format!("gap{gap}/{mode}"), gap));
    }
}

fn stale_hot_reset(t: &T, name: &str, gap: u64) {
    let f = Arc::new(new_minute_filter(1000));
    let k = key_at(&f, 7, two_line_fps(&f, 7, 0x6600, 1)[0], 0x321);
    for _ in 0..300 {
        f.touch_at(k, 0, SWEEP_T0);
    }
    let sl = slots_of(&f, k);
    if sl.len() != 1 || !crate::hot::is_hot(ld(f.w(sl[0] + 1))) {
        fatalf!(t, "{name}: setup: K in {}, not hot", gov(&sl));
    }
    let t1 = SWEEP_T0 + gap * f.unit;
    let fired = Arc::new(AtomicBool::new(false));
    let done: Arc<Mutex<Option<Done>>> = Arc::default();
    let hook = set_hook(&hooks::STAMPED, {
        let (f, fired, done) = (f.clone(), fired.clone(), done.clone());
        move |()| {
            if !fired.swap(true, SeqCst) {
                let f = f.clone();
                *done.lock().unwrap() = Some(during(move || {
                    f.touch_at(k, 0, t1);
                }));
            }
        }
    });
    f.touch_at(k, 0, t1);
    drop(hook);
    if !fired.load(SeqCst) {
        fatalf!(t, "{name}: A did not move K's last touch");
    }
    if let Some(d) = done.lock().unwrap().take() {
        wait_for(t, &d, "B's touch");
    }
    let r = f.count_at(k, f.horizon, t1);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "{name}: K reads {} after 2 touches at t1", show(&r));
    }
}
