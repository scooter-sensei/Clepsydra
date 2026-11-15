//! roomrace_test.go, slotunit_race_test.go: the Background wait room racing
//! a key's touches and the placer, and the writers pinned to word0's unit
//! (claims D29).

use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicBool, AtomicI64};
use std::sync::{Arc, Mutex};

use super::check::*;
use super::helpers::*;
use crate::hooks;
use crate::slots::*;
use crate::{Config, Filter, Result};

/// TestReadWhileTouchTakesWaitingKey: with Background, a touch of a key
/// waiting in the wait room takes it out under the key's stripe and then
/// installs it (or puts it back): meanwhile the key is in neither place. If
/// the placer finds the key's set empty then, it clears the set's wait bit,
/// and a lock-free read that misses the table takes the clear bit for "not
/// waiting". The read must find the key's history, not answer absent (it
/// did: Upper 0 for a key with three events).
#[test]
fn TestReadWhileTouchTakesWaitingKey() {
    let t = T::new("TestReadWhileTouchTakesWaitingKey");
    if force_ct() {
        skip!(t, "Background has no effect with ConstantTime");
    }
    let f = match Filter::with(
        Config { capacity: 4096, horizon: MINUTE, resolution: ms(1), clock: TestClock::new(1).func(), background: true, ..Config::default() },
        |c| c.bg_manual = true,
    ) {
        Ok(f) => Arc::new(f),
        Err(e) => fatalf!(t, "{e}"),
    };
    let mut key = 0u64;
    let mut i = 1u64;
    while key == 0 {
        // a key whose lines are full waits
        let w = f.waited();
        f.touch_at(mix64(i), 0, 1);
        if f.waited() != w {
            key = mix64(i);
        }
        i += 1;
    }
    f.touch_at(key, 0, 1);
    f.touch_at(key, 0, 1); // three events, still waiting
    if f.waiting() != 1 || f.locate(key, fingerprint(key), tag(key)) >= 0 {
        fatalf!(t, "{} waiting; want the key alone, not in the table", f.waiting());
    }
    // got: a channel of one Result
    let (tx, rx) = std::sync::mpsc::channel::<Result>();
    let rx = Arc::new(Mutex::new(rx));
    let fired = Arc::new(AtomicBool::new(false));
    let hook = set_hook(&hooks::TAKEN, {
        let (f, fired, rx) = (f.clone(), fired.clone(), rx.clone());
        move |()| {
            if fired.swap(true, SeqCst) {
                return;
            }
            f.place_next(f.room.get(), 0); // the placer finds the set empty and clears its bit
            let (f2, tx2) = (f.clone(), tx.clone());
            std::thread::spawn(move || {
                let _ = tx2.send(f2.count_at(key, 60_000, 1));
            });
            // the read finished while the key was in neither place (put its
            // answer back), or it waits for the touch: let it finish
            if let Ok(r) = rx.lock().unwrap().recv_timeout(ms(20)) {
                let _ = tx.send(r);
            }
        }
    });
    f.touch_at(key, 0, 1);
    drop(hook);
    if !fired.load(SeqCst) {
        fatalf!(t, "the touch did not take the key from the wait room");
    }
    let r = rx.lock().unwrap().recv().expect("the read's answer");
    if r.upper < 3 && !r.saturated {
        fatalf!(t, "read during the touch: {}, want Upper >= 3", show(&r));
    }
    let r = f.count_at(key, 60_000, 1);
    if r.count != 4 {
        fatalf!(t, "after: {}, want 4 events", show(&r));
    }
}

/// TestWaitBesidePlacementKeepsKey: with Background, the placer commits a
/// waiting key L by marking its wait-room entry busy, installing it, and then
/// removing the entry, or putting its word1 back if the install failed. A
/// first touch of K meanwhile, whose lines are full and whose room set is
/// L's, waits in that set, where L's entry, last touched a horizon before K's
/// clock, looks expired. If K's wait takes L's way, busy as it is, the
/// placer's removal erases K's entry: K reads Upper 0 for its event, and its
/// next touch starts it afresh (the concurrent fuzzer's case 1068). K must
/// keep its event.
#[test]
fn TestWaitBesidePlacementKeepsKey() {
    let t = T::new("TestWaitBesidePlacementKeepsKey");
    if force_ct() {
        skip!(t, "Background has no effect with ConstantTime");
    }
    let f = match Filter::with(
        Config {
            capacity: 4096,
            horizon: MINUTE,
            resolution: ms(1),
            clock: TestClock::new(1).func(),
            background: true,
            hot: -1,
            ..Config::default()
        },
        |c| c.bg_manual = true,
    ) {
        Ok(f) => Arc::new(f),
        Err(e) => fatalf!(t, "{e}"),
    };
    const T0: u64 = 1;
    let t1 = T0 + f.horizon + 1; // entries last touched at t0 have expired
    let mut l = 0u64;
    let mut i = 1u64;
    while l == 0 {
        // a key whose lines are full waits
        let w = f.waited();
        f.touch_at(mix64(i), 0, T0);
        if f.waited() != w {
            l = mix64(i);
        }
        i += 1;
    }
    let Some(p) = f.room.get() else { fatalf!(t, "L waited, but there is no wait room") };
    let set = |h: u64| crate::cpu::mul_hi(mix64(f.stash_id(h, fingerprint(h), tag(h))), p.n);
    let lines = |h: u64| {
        let pl = f.primary(h);
        [pl, f.alt(pl, fingerprint(h))]
    };
    let ll = lines(l);
    let mut k = 0u64; // L's room set; two lines, neither of them L's; a claim stripe of its own
    let mut i = 1u64 << 40;
    while k == 0 {
        let (h, kl) = (mix64(i), lines(mix64(i)));
        if set(h) == set(l) && (h >> 56) != (l >> 56) && kl[0] != kl[1] && kl[0] != ll[0] && kl[0] != ll[1] && kl[1] != ll[0] && kl[1] != ll[1] {
            k = h;
        }
        i += 1;
    }
    let kl = lines(k);
    for line in kl {
        // K's lines full of entries live at t1, so its first touch waits
        let mut i = 1u64 << 44;
        while count_line(&f, line, t1) < 4 {
            let h = mix64(i);
            if f.primary(h) == line {
                f.touch_at(h, 0, t1);
            }
            i += 1;
        }
    }
    for line in ll {
        // room for L: its lines' entries from t0 are swept
        f.sweep_line(line, t1);
    }
    if f.waiting() != 1 {
        fatalf!(t, "{} keys waiting, want L alone", f.waiting());
    }
    let (fired, no_wait) = (Arc::new(AtomicBool::new(false)), Arc::new(AtomicBool::new(false)));
    let hook = set_hook(&hooks::PLACING, {
        let (f, fired, no_wait) = (f.clone(), fired.clone(), no_wait.clone());
        move |()| {
            if fired.swap(true, SeqCst) {
                return;
            }
            let w = f.waited();
            f.touch_at(k, 0, t1); // K waits in L's set while L's entry is busy
            if f.waited() == w {
                no_wait.store(true, SeqCst); // t.Error, reported below
            }
        }
    });
    f.place_next(Some(p), 0); // places L
    drop(hook);
    if no_wait.load(SeqCst) {
        errorf!(t, "K's first touch did not wait");
    }
    if !fired.load(SeqCst) {
        fatalf!(t, "the placer did not commit L");
    }
    if f.locate(l, fingerprint(l), tag(l)) < 0 {
        fatalf!(t, "L is not in the table");
    }
    let r = f.count_at(k, f.horizon, t1);
    if r.upper < 1 && !r.saturated {
        fatalf!(t, "K reads {} after L's placement, want Upper >= 1", show(&r));
    }
    let r = f.touch_at(k, f.horizon, t1);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "K's second touch reads {}, want Upper >= 2", show(&r));
    }
}

/// TestSlotUnitRaces (claims D29): a register stores no unit and is read at
/// word0's, so the three writers that put a register beside a word0 they did
/// not move, or move word0 beside a register they did not write, are pinned
/// to word0's unit (SlotUnit.tla). Each case drives the interleaving the
/// model's plain variant breaks, by 16-byte CAS and by hold, and requires the
/// slot (both words) and every answer to be what a sequential order of the
/// same touches leaves:
///
///   - demote: B's touch into unit u1 demotes K's hot entry while A's touch
///     into u2 > u1 has already stamped word0 into u2 (B's demotion must
///     fail, and A's demote at u2). A demotion by a CAS of word1 alone
///     leaves a register at u1 beside a last touch at u2.
///   - stamp: A reads word0 (at u1) and the pointer, then E, a touch at
///     word0's own tick, demotes the entry at u1 without moving word0; A's
///     stamp into u2 must fail and A realign. A stamp by a CAS of word0 alone
///     moves word0 past the register.
///   - unpoint: K's entry is reclaimed; S's touch into u1 finds it gone and
///     resets the pointer to a register, after T's touch into u2 stamped
///     word0 (S's reset must be at u2). A reset by a CAS of word1 alone puts
///     S's register, with its onset, at u1.
///
/// The undone guards were each checked to fail their case.
#[test]
fn TestSlotUnitRaces() {
    let t = T::new("TestSlotUnitRaces");
    type Case = fn(&T, &str);
    let cases: [(&str, Case); 3] = [("demote", slot_unit_demote), ("stamp", slot_unit_stamp), ("unpoint", slot_unit_unpoint)];
    for (name, run) in cases {
        realign_modes(|mode| run(&t, &format!("{name}/{mode}")));
    }
}

// slotUnitTicks: K's burst at t0 (unit 24), touches at t1 (unit 34, 700 ticks
// in, onset 4) and t2 (unit 47, within a horizon of t0).
const SLOT_UNIT_T1: u64 = SWEEP_T0 + 10 * 2_500 + 700;
const SLOT_UNIT_T2: u64 = SWEEP_T0 + 23 * 2_500 + 1_900;

/// slotUnitKey: a filter and K, promoted by a burst of 40 touches at t0 (its
/// entry at unit 24); with reclaim, the entry is then taken from K as the hot
/// table's reclaim does.
#[track_caller]
fn slot_unit_key(t: &T, name: &str, reclaim: bool) -> (Arc<Filter>, u64) {
    let f = Arc::new(new_minute_filter(1000));
    let k = key_at(&f, 7, two_line_fps(&f, 7, 0x6600, 1)[0], 0x321);
    for _ in 0..40 {
        f.touch_at(k, 0, SWEEP_T0);
    }
    let sl = slots_of(&f, k);
    if sl.len() != 1 || f.promoted() != 1 || !crate::hot::is_hot(ld(f.w(sl[0] + 1))) {
        fatalf!(t, "{name}: setup: K in {}, promoted {}, not hot", gov(&sl), f.promoted());
    }
    if reclaim {
        let e = &f.hot.get().expect("a hot table").e[crate::hot::hot_idx(ld(f.w(sl[0] + 1))) as usize];
        let ver = e.lock();
        e.id.store(0, SeqCst);
        e.unlock(ver);
    }
    (f, k)
}

/// slotUnitState: K's slot words and its answers at every window, at t2 and
/// later, with the hot table's counters.
fn slot_unit_state(f: &Filter, k: u64) -> String {
    let sl = slots_of(f, k);
    let mut b = format!("slots {}", sl.len());
    for &i in &sl {
        b += &format!(" words {:#x} {:#x}", ld(f.w(i)), ld(f.w(i + 1)));
    }
    for at in [SLOT_UNIT_T2, SLOT_UNIT_T2 + f.unit, SLOT_UNIT_T2 + 10 * f.unit] {
        for w in [500u64, 1_000, 2_500, 5_000, 15_000, 30_000, 60_000] {
            b += &format!("\n  at {at} window {w:5}: {}", show(&f.count_at(k, w, at)));
        }
    }
    b += &format!("\n  promoted {} demoted {}", f.promoted(), f.demoted());
    b
}

#[track_caller]
fn slot_unit_compare(t: &T, name: &str, raced: &str, want: &str) {
    if raced != want {
        fatalf!(t, "{name}: after the race:\n{raced}\nsequential:\n{want}");
    }
}

fn slot_unit_demote(t: &T, name: &str) {
    let (rf, k) = slot_unit_key(t, name, false);
    rf.touch_at(k, 0, SLOT_UNIT_T1); // B: demotes at u1
    if rf.demoted() != 1 {
        fatalf!(t, "{name}: setup: B's touch did not demote K (demoted {})", rf.demoted());
    }
    rf.touch_at(k, 0, SLOT_UNIT_T2); // A: realigns to u2
    let want = slot_unit_state(&rf, k);

    let (f, _) = slot_unit_key(t, name, false);
    let (stamped, adding) = (Arc::new(AtomicI64::new(0)), Arc::new(AtomicI64::new(0)));
    let (a_stamped, release, a_done) = (Signal::new(), Signal::new(), Signal::new());
    let h1 = set_hook(&hooks::STAMPED, {
        let (stamped, a_stamped, release) = (stamped.clone(), a_stamped.clone(), release.clone());
        move |()| {
            if stamped.fetch_add(1, SeqCst) + 1 == 2 {
                // A's stamp, into u2: hold A there
                a_stamped.close();
                release.wait();
            }
        }
    });
    let h2 = set_hook(&hooks::HOT_ADDING, {
        let (f, adding, a_stamped, a_done) = (f.clone(), adding.clone(), a_stamped.clone(), a_done.clone());
        move |_| {
            if adding.fetch_add(1, SeqCst) + 1 == 1 {
                // B, before it adds at u1: A stamps first
                let (f, a_done) = (f.clone(), a_done.clone());
                std::thread::spawn(move || {
                    f.touch_at(k, 0, SLOT_UNIT_T2);
                    a_done.close();
                });
                a_stamped.wait_timeout(secs(5)); // awaitHook
            }
        }
    });
    f.touch_at(k, 0, SLOT_UNIT_T1); // B
    release.close();
    if !a_done.wait_timeout(secs(5)) {
        fatalf!(t, "{name}: A's touch did not return");
    }
    drop((h1, h2));
    if stamped.load(SeqCst) < 2 {
        fatalf!(t, "{name}: A did not stamp during B's touch");
    }
    slot_unit_compare(t, name, &slot_unit_state(&f, k), &want);
}

fn slot_unit_stamp(t: &T, name: &str) {
    let (rf, k) = slot_unit_key(t, name, false);
    rf.touch_at(k, 0, SLOT_UNIT_T1); // E (as D's stamp and E's count): demotes at u1
    if rf.demoted() != 1 {
        fatalf!(t, "{name}: setup: the touch at t1 did not demote K (demoted {})", rf.demoted());
    }
    rf.touch_at(k, 0, SLOT_UNIT_T2); // A: realigns to u2
    rf.touch_at(k, 0, SLOT_UNIT_T1); // D's count: behind the last touch, counted at it
    let want = slot_unit_state(&rf, k);

    let (f, _) = slot_unit_key(t, name, false);
    let (stamping, stamped) = (Arc::new(AtomicI64::new(0)), Arc::new(AtomicI64::new(0)));
    let h1 = set_hook(&hooks::STAMPED, {
        let (f, stamped) = (f.clone(), stamped.clone());
        move |()| {
            if stamped.fetch_add(1, SeqCst) + 1 == 1 {
                // D has stamped word0 into u1: A's touch
                f.touch_at(k, 0, SLOT_UNIT_T2);
            }
        }
    });
    let h2 = set_hook(&hooks::STAMPING, {
        let (f, stamping) = (f.clone(), stamping.clone());
        move |()| {
            if stamping.fetch_add(1, SeqCst) + 1 == 2 {
                // A has read word0 and the pointer: E demotes, word0 unmoved
                f.touch_at(k, 0, SLOT_UNIT_T1);
            }
        }
    });
    f.touch_at(k, 0, SLOT_UNIT_T1); // D
    drop((h1, h2));
    if stamping.load(SeqCst) < 2 || f.demoted() != 1 {
        fatalf!(t, "{name}: the interleaving did not happen (stamps {}, demoted {})", stamping.load(SeqCst), f.demoted());
    }
    slot_unit_compare(t, name, &slot_unit_state(&f, k), &want);
}

fn slot_unit_unpoint(t: &T, name: &str) {
    let (rf, k) = slot_unit_key(t, name, true);
    rf.touch_at(k, 0, SLOT_UNIT_T2); // T: resets the pointer at u2
    rf.touch_at(k, 0, SLOT_UNIT_T1); // S: behind the last touch, counted at it
    let want = slot_unit_state(&rf, k);

    let (f, _) = slot_unit_key(t, name, true);
    let (stamped, adding) = (Arc::new(AtomicI64::new(0)), Arc::new(AtomicI64::new(0)));
    let (t_stamped, release, t_done) = (Signal::new(), Signal::new(), Signal::new());
    let h1 = set_hook(&hooks::STAMPED, {
        let (stamped, t_stamped, release) = (stamped.clone(), t_stamped.clone(), release.clone());
        move |()| {
            if stamped.fetch_add(1, SeqCst) + 1 == 2 {
                // T's stamp, into u2: hold T there
                t_stamped.close();
                release.wait();
            }
        }
    });
    let h2 = set_hook(&hooks::HOT_ADDING, {
        let (f, adding, t_stamped, t_done) = (f.clone(), adding.clone(), t_stamped.clone(), t_done.clone());
        move |_| {
            if adding.fetch_add(1, SeqCst) + 1 == 1 {
                // S has stamped word0 into u1 and read it: T stamps
                let (f, t_done) = (f.clone(), t_done.clone());
                std::thread::spawn(move || {
                    f.touch_at(k, 0, SLOT_UNIT_T2);
                    t_done.close();
                });
                t_stamped.wait_timeout(secs(5)); // awaitHook
            }
        }
    });
    f.touch_at(k, 0, SLOT_UNIT_T1); // S
    release.close();
    if !t_done.wait_timeout(secs(5)) {
        fatalf!(t, "{name}: T's touch did not return");
    }
    drop((h1, h2));
    if stamped.load(SeqCst) < 2 {
        fatalf!(t, "{name}: T did not stamp during S's touch");
    }
    slot_unit_compare(t, name, &slot_unit_state(&f, k), &want);
}
