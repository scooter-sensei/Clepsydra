//! hot_test.go, hotdrop_test.go, hotspill_test.go.

use std::sync::Arc;
use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64};

use super::check::*;
use super::helpers::*;
use crate::hooks;
use crate::hot::{HOT_IDX_SHIFT, is_hot};
use crate::slots::*;
use crate::{Config, Filter};

/// Many workers send the same keys past the register's capacity at once, so
/// promotions race each other, while a fill to 98% slot load relocates and
/// evicts them: every key must end with its exact count, in one slot.
#[test]
fn TestHotPromotionUnderRaces() {
    let t = T::new("TestHotPromotionUnderRaces");
    let rounds = if short() { 6 } else { 40 };
    const KEYS: usize = 128;
    const WORKERS: usize = 8;
    const PER: usize = 200;
    const AT: u64 = 60_000; // a multiple of 8 units, where a 60 s window covers every bucket whole
    for round in 0..rounds as u64 {
        let hot_n: i64 = if env_on("TF_HOTOFF") { -1 } else { 2 * KEYS as i64 };
        let f = match Filter::new(Config { capacity: 4000, horizon: MINUTE, resolution: ms(1), hot: hot_n, ..Config::default() }) {
            Ok(f) => f,
            Err(e) => fatalf!(t, "{e}"),
        };
        let salt = round << 40;
        let hs: Vec<u64> = (0..KEYS as u64).map(|i| mix64(salt + i)).collect();
        for &h in &hs {
            for _ in 0..3 {
                // heavier than the fill, so eviction never picks them over a one-event key
                f.check_and_touch_at(h, 0, AT);
            }
        }
        let sent = AtomicI64::new(0);
        std::thread::scope(|s| {
            for w in 0..WORKERS {
                let (f, hs, sent) = (&f, &hs, &sent);
                s.spawn(move || {
                    for n in 0..PER * KEYS / 2 {
                        f.check_and_touch_at(hs[(n + w * 7) % KEYS], 0, AT);
                        sent.fetch_add(1, SeqCst);
                    }
                });
            }
            let fill = (f.slots() as f64 * 0.98) as i64 - KEYS as i64;
            for c in 0..fill as u64 {
                f.check_and_touch_at(mix64(salt + (1 << 36) + c), 0, AT);
            }
        });
        let mut total = 0i64;
        for (i, &h) in hs.iter().enumerate() {
            let (fp, tg) = (fingerprint(h), tag(h));
            let p = f.primary(h);
            // As lookups match: undisplaced in the primary line, displaced
            // in the alternate (findAny would also count another key that
            // has this identity but is homed at the alternate line).
            let (a, b) = (f.find(p, fp << 1, tg), f.find(f.alt(p, fp), (fp << 1) | 1, tg));
            if a >= 0 && b >= 0 {
                fatalf!(t, "round {round} key {i} in two slots");
            }
            let r = f.count_at(h, 60_000, AT);
            if r.saturated && hot_n > 0 {
                fatalf!(t, "round {round} key {i} Saturated with {} hot entries free", 2 * KEYS as i64 - f.hot_in_use());
            }
            total += r.count;
        }
        let want = sent.load(SeqCst) + 3 * KEYS as i64;
        if total != want {
            fatalf!(
                t,
                "round {round}: counted {total} of {want} events (promoted {}, refused {}, relocations {}, evictions {})",
                f.promoted(),
                f.hot_refused(),
                f.relocations(),
                f.evictions()
            );
        }
    }
}

/// A key that bursts past the register is promoted; once it slows to a
/// trickle its entry is handed back as a register, and its counts stay exact
/// (against an exact log) before, across and after both moves.
#[test]
fn TestHotDemotion() {
    let t = T::new("TestHotDemotion");
    let clock = TestClock::new(1);
    let f = match Filter::new(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), clock: clock.func(), ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    let h = mix64(7);
    let mut log: Vec<u64> = Vec::new();
    let check = |when: &str, log: &[u64]| {
        let now = clock.get();
        for w in [1_000u64, 5_000, 15_000, 60_000] {
            let exact = log.iter().filter(|&&ts| now - ts <= w).count() as i64;
            let r = f.count_at(h, w, now);
            if !r.saturated && r.upper < exact {
                fatalf!(t, "{when}, window {}s: exact {exact}, got {r:?}", w / 1000);
            }
        }
    };
    for _ in 0..200 {
        // a burst in one tick: promoted
        f.touch_at(h, 0, clock.get());
        log.push(clock.get());
    }
    if f.promoted() != 1 || f.hot_in_use() != 1 {
        fatalf!(t, "burst: promoted {}, in use {}", f.promoted(), f.hot_in_use());
    }
    check("after the burst", &log);
    while clock.get() < 3 * 60_000 {
        // then one event a second
        f.touch_at(h, 0, clock.get());
        log.push(clock.get());
        check("trickle", &log);
        clock.add(1000);
    }
    if f.demoted() != 1 || f.hot_in_use() != 0 {
        fatalf!(t, "trickle: demoted {}, in use {}", f.demoted(), f.hot_in_use());
    }
    let r = f.count_at(h, 60_000, clock.get());
    if r.count != 60 || r.saturated {
        fatalf!(t, "after demotion, 60 s window: {r:?}, want Count 60");
    }
}

/// With fewer hot entries than heavy keys, the rest saturate as without hot
/// entries (flagged, so the fail-closed contract holds), HotRefused counts
/// them, and entries whose history has aged out are taken by new keys.
#[test]
fn TestHotTableExhaustion() {
    let t = T::new("TestHotTableExhaustion");
    let clock = TestClock::new(1);
    let f = match Filter::new(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), hot: 4, clock: clock.func(), ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    const HEAVY: usize = 10;
    let mut sent = [0i64; HEAVY];
    let mut now = 1u64;
    while now < 60_000 {
        // 100 events per second per key
        clock.set(now);
        for (k, s) in sent.iter_mut().enumerate() {
            f.touch_at(mix64(k as u64), 0, now);
            *s += 1;
        }
        now += 10;
    }
    clock.set(now);
    let (mut exact, mut sat) = (0, 0);
    for k in 0..HEAVY {
        let r = f.count_at(mix64(k as u64), 60_000, now);
        if r.saturated {
            sat += 1;
        } else if r.upper < sent[k] || r.count < sent[k] * 99 / 100 || r.count > sent[k] * 101 / 100 {
            errorf!(t, "key {k} unflagged: sent {}, got {r:?}", sent[k]);
        } else {
            exact += 1;
        }
    }
    if exact != 4 || sat != HEAVY - 4 || f.hot_refused() == 0 || f.hot_in_use() != 4 {
        fatalf!(t, "exact {exact} saturated {sat} (want 4, {}); refused {}, in use {}", HEAVY - 4, f.hot_refused(), f.hot_in_use());
    }
    // A horizon and two units of silence later the entries are dead: new
    // heavy keys take them.
    now += 60_000 + 2 * 2500 + 1;
    clock.set(now);
    let before = f.promoted();
    for _ in 0..40 {
        for k in 0..4u64 {
            f.touch_at(mix64(100 + k), 0, now);
        }
    }
    let got = f.promoted() - before;
    if got != 4 {
        fatalf!(t, "after the horizon {got} of 4 new heavy keys were promoted (in use {}, refused {})", f.hot_in_use(), f.hot_refused());
    }
    for k in 0..4u64 {
        let r = f.count_at(mix64(100 + k), 60_000, now);
        if r.count != 40 || r.saturated {
            errorf!(t, "new heavy key {k}: {r:?}, want Count 40");
        }
    }
}

/// TestRestoreAfterHotEntryReclaimed: a hot key K is evicted to the stash,
/// its stash entry pointing at its hot entry. A promotion at a clock more
/// than a horizon past K's newest event reclaims the entry for another key.
/// K's next touch, at a clock behind that one but within a horizon of K's
/// last touch, restores K's history: the entry is no longer K's. That touch
/// must still be counted (it went nowhere, its slot left pointing at the
/// other key's entry, and K's next touch started afresh: Upper 1 for 2).
#[test]
fn TestRestoreAfterHotEntryReclaimed() {
    let t = T::new("TestRestoreAfterHotEntryReclaimed");
    if force_ct() || force_bg() {
        skip!(t, "ConstantTime and Background inserts don't install on the call path");
    }
    let f = must(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), hot: 1, ..Config::default() });
    const T0: u64 = 60_000;
    let (k, l) = (mix64(1), mix64(2));
    for _ in 0..32 {
        // past the register's 31 events in a unit: promoted
        f.touch_at(k, 0, T0);
    }
    if f.promoted() != 1 {
        fatalf!(t, "K not promoted ({})", f.promoted());
    }
    // Evict K as claimAt does: its words to the stash, its slot to another key (here: freed).
    let (fp, tg) = (fingerprint(k), tag(k));
    let i = f.locate(k, fp, tg);
    let (w0, w1) = (ld(f.w(i)), ld(f.w(i + 1)));
    if !is_hot(w1) {
        fatalf!(t, "K's slot holds no hot entry");
    }
    f.stash_put(f.stash_id(k, fp, tg), w0, w1, T0);
    st(f.w(i), 0);
    st(f.w(i + 1), 0);
    let t_l = T0 + f.horizon + 3 * f.unit; // K's entry is past the horizon here
    for _ in 0..32 {
        f.touch_at(l, 0, t_l);
    }
    if f.promoted() != 2 {
        fatalf!(t, "L not promoted ({}): K's entry not reclaimed", f.promoted());
    }
    let t_k = T0 + f.horizon - 1; // K's clock: behind L's, within a horizon of K's last touch
    f.touch_at(k, 0, t_k);
    f.touch_at(k, 0, t_k);
    let r = f.count_at(k, f.horizon, t_k);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "K reads {r:?} after two touches, want Upper >= 2");
    }
}

/// hotReclaimSetup: a filter with one hot entry; K promoted at t0 and
/// evicted as claimAt does (words to the stash, slot freed); both of K's
/// lines then full of entries live until t0 + horizon. reclaim promotes
/// another key at a clock more than a horizon past K's newest event, which
/// takes K's entry. tK is a clock behind that one, within a horizon of K's
/// last touch. Returns (f, k, tK); `hot_reclaim` is reclaim.
fn hot_reclaim_setup(t: &T, bg: bool) -> (Arc<Filter>, u64, u64) {
    let clock = TestClock::new(1);
    let f = Filter::with(
        Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), hot: 1, clock: clock.func(), background: bg, ..Config::default() },
        |c| c.bg_manual = bg,
    );
    let f = match f {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    const T0: u64 = 60_000;
    let k = mix64(1);
    for _ in 0..32 {
        // past the register's 31 events in a unit: promoted
        f.touch_at(k, 0, T0);
    }
    let (fp, tg) = (fingerprint(k), tag(k));
    let i = f.locate(k, fp, tg);
    if f.promoted() != 1 || i < 0 || !is_hot(ld(f.w(i + 1))) {
        fatalf!(t, "K not promoted ({}) or not in the table", f.promoted());
    }
    f.stash_put(f.stash_id(k, fp, tg), ld(f.w(i)), ld(f.w(i + 1)), T0);
    st(f.w(i), 0);
    st(f.w(i + 1), 0);
    let p = f.primary(k);
    for line in [p, f.alt(p, fp)] {
        let mut j = 1u64 << 40;
        while count_line(&f, line, T0) < 4 {
            let h = mix64(j);
            if f.primary(h) == line {
                f.touch_at(h, 0, T0);
            }
            j += 1;
        }
    }
    let t_k = T0 + f.horizon - 1;
    (Arc::new(f), k, t_k)
}

fn hot_reclaim(t: &T, f: &Filter) {
    const T0: u64 = 60_000;
    let (l, t_l) = (mix64(2), T0 + f.horizon + 3 * f.unit);
    for _ in 0..32 {
        f.touch_at(l, 0, t_l);
    }
    if f.promoted() != 2 {
        errorf!(t, "L not promoted ({}): K's entry not reclaimed", f.promoted());
    }
}

/// TestHotEntryReclaimedDuringRestore: as TestRestoreAfterHotEntryReclaimed,
/// but the entry is reclaimed while K's restoring touch is in its claim
/// (relocating an entry of K's full lines to make room), after the check that
/// its entry is still K's. K's touch must still be counted.
#[test]
fn TestHotEntryReclaimedDuringRestore() {
    let t = Arc::new(T::new("TestHotEntryReclaimedDuringRestore"));
    if force_ct() || force_bg() {
        skip!(t, "ConstantTime and Background inserts don't search on the call path");
    }
    let (f, k, t_k) = hot_reclaim_setup(&t, false);
    let fired = Arc::new(AtomicBool::new(false));
    let hook = {
        let (f, fired, t) = (f.clone(), fired.clone(), t.clone());
        move |()| {
            if !fired.swap(true, SeqCst) {
                hot_reclaim(&t, &f);
            }
        }
    };
    let _h1 = set_hook(&hooks::COPIED, hook.clone());
    let _h2 = set_hook(&hooks::VICTIM, hook);
    f.touch_at(k, 0, t_k);
    if !fired.load(SeqCst) {
        fatalf!(t, "K's restore neither relocated nor evicted");
    }
    f.touch_at(k, 0, t_k);
    let r = f.count_at(k, f.horizon, t_k);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "K reads {r:?} after two touches, want Upper >= 2");
    }
}

/// TestHotEntryReclaimedBeforeWait: with Background, K's restoring touch
/// finds its lines full and waits in the wait room, after its entry was
/// reclaimed. It must wait with this touch counted, not the dead entry (with
/// which every retry would take K out of the room and wait again).
#[test]
fn TestHotEntryReclaimedBeforeWait() {
    let t = T::new("TestHotEntryReclaimedBeforeWait");
    if force_ct() {
        skip!(t, "Background has no effect with ConstantTime");
    }
    let (f, k, t_k) = hot_reclaim_setup(&t, true);
    hot_reclaim(&t, &f);
    let done = {
        let f = f.clone();
        go(move || {
            f.touch_at(k, 0, t_k);
            f.touch_at(k, 0, t_k);
        })
    };
    if !closed_within(&done, secs(5)) {
        fatalf!(t, "K's touches did not return");
    }
    if f.waiting() != 1 {
        errorf!(t, "{} keys waiting, want K", f.waiting());
    }
    let r = f.count_at(k, f.horizon, t_k);
    if r.upper < 2 && !r.saturated {
        fatalf!(t, "K reads {r:?} after two touches, want Upper >= 2");
    }
}

/// A filter with 4 hot entries on a clock that reads 1, and K promoted at
/// t0.
fn promoted_key(t: &T) -> (Filter, u64) {
    const T0: u64 = 60_000;
    let clock = TestClock::new(1);
    let f = match Filter::new(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), hot: 4, clock: clock.func(), ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    let k = mix64(1);
    for _ in 0..32 {
        // past the register's 31 events in a unit: promoted
        f.touch_at(k, 0, T0);
    }
    if f.promoted() != 1 || f.hot_in_use() != 1 {
        fatalf!(t, "K not promoted (promoted {}, in use {})", f.promoted(), f.hot_in_use());
    }
    (f, k)
}

/// TestSlotExitFreesHotEntry (claims A40): a hot key's history that its slot
/// discards past the horizon gives its entry up at once: by a touch's reset
/// (realign, with its 16-byte CAS and with its hold), by the sweep, and by an
/// insert over the expired occupant. Before, the entry stayed held, and
/// HotInUse counted it, until a promotion found no free entry and reclaimed
/// it.
#[test]
fn TestSlotExitFreesHotEntry() {
    let t = T::new("TestSlotExitFreesHotEntry");
    const T0: u64 = 60_000;
    // reset
    realign_modes(|mode| {
        let (f, k) = promoted_key(&t);
        let now = T0 + f.horizon + f.unit; // K's history is past the horizon: its touch starts afresh
        f.touch_at(k, 0, now);
        let n = f.hot_in_use();
        if n != 0 {
            fatalf!(t, "reset/{mode}: hot entries in use after K's reset: {n}, want 0");
        }
        let r = f.count_at(k, f.horizon, now);
        if r.count != 1 || r.saturated {
            fatalf!(t, "reset/{mode}: K reads {r:?} after its reset, want count 1");
        }
    });
    // sweep
    {
        let (f, k) = promoted_key(&t);
        let i = f.locate(k, fingerprint(k), tag(k));
        f.sweep_line((i as u64) / WORDS_PER_LINE, T0 + f.horizon + 1);
        if ld(f.w(i)) != 0 {
            fatalf!(t, "sweep: K's expired slot not swept");
        }
        let n = f.hot_in_use();
        if n != 0 {
            fatalf!(t, "sweep: hot entries in use after the sweep: {n}, want 0");
        }
    }
    // insert
    {
        let (f, k) = promoted_key(&t);
        let i = f.locate(k, fingerprint(k), tag(k));
        let (line, t_l) = ((i as u64) / WORDS_PER_LINE, T0 + f.horizon + 1);
        // Fill K's line's other slots, live at tL, so a key of this line takes K's.
        let mut s = 0;
        while s < WORDS_PER_LINE as isize {
            let j = (line * WORDS_PER_LINE) as isize + s;
            if j != i {
                st(f.w(j), (0x7777 << 48) | t_l);
                st(f.w(j + 1), (0x777 << TAG_SHIFT) | 1);
            }
            s += 2;
        }
        let mut j = 1u64 << 40;
        loop {
            let h = mix64(j);
            if f.primary(h) == line && fingerprint(h) != fingerprint(k) {
                f.touch_at(h, 0, t_l);
                break;
            }
            j += 1;
        }
        if f.locate(k, fingerprint(k), tag(k)) == i {
            fatalf!(t, "insert: K's expired slot not taken");
        }
        let n = f.hot_in_use();
        if n != 0 {
            fatalf!(t, "insert: hot entries in use after an insert over K's expired slot: {n}, want 0");
        }
    }
}

/// TestExpiredStashWayKeepsSharedEntry (claims A40): an expired stash way of
/// K's that points to the hot entry K's live slot uses (the entry was
/// reclaimed past the horizon and given to K again) keeps the entry when
/// another key's history takes the way: closing it there would close K's live
/// entry, and K's next touch would start afresh. Only a slot's own exits
/// close its entry.
#[test]
fn TestExpiredStashWayKeepsSharedEntry() {
    let t = T::new("TestExpiredStashWayKeepsSharedEntry");
    if force_ct() || force_bg() {
        skip!(t, "ConstantTime and Background inserts don't install on the call path");
    }
    let clock = TestClock::new(1);
    let f = must(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), hot: 4, clock: clock.func(), ..Config::default() });
    const T0: u64 = 200_000;
    let k = mix64(1);
    for _ in 0..32 {
        f.touch_at(k, 0, T0);
    }
    let (fp, tg) = (fingerprint(k), tag(k));
    let (i, id) = (f.locate(k, fp, tg), f.stash_id(k, fp, tg));
    if f.promoted() != 1 || i < 0 || !is_hot(ld(f.w(i + 1))) {
        fatalf!(t, "K not promoted");
    }
    // K's old history in the stash, long expired, pointing to K's entry.
    f.stash_put(id, (fp << 48) | 1_000, ld(f.w(i + 1)), 1_000);
    let st = f.stash.get().expect("the stash");
    let (set, _) = st.set(id);
    let t_l = T0 + 10;
    let mut j = 1u64 << 40;
    loop {
        // another key of the set takes the expired way
        let h = mix64(j);
        j += 1;
        let oid = f.stash_id(h, fingerprint(h), tag(h));
        if st.set(oid).0 != set || oid == id {
            continue;
        }
        let (w1, _) = f.next_reg(0, tag(h), t_l, t_l, true);
        f.stash_put(oid, (fingerprint(h) << 48) | t_l, w1, t_l);
        break;
    }
    let n = f.hot_in_use();
    if n != 1 {
        fatalf!(t, "hot entries in use: {n}, want 1 (K's live one)");
    }
    f.touch_at(k, 0, t_l);
    let r = f.count_at(k, f.horizon, t_l);
    if r.upper < 33 {
        fatalf!(t, "K reads {r:?} after another key took its expired stash way, want Upper >= 33");
    }
}

/// The filter of the hot-add races: two lines on a clock that reads 1, in
/// the plain mode (the race is in the plain path's order of steps: TF_CT,
/// TF_BG).
fn two_line_filter(t: &T, stash: i64) -> Filter {
    let clock = TestClock::new(1);
    match Filter::with(Config { capacity: 4, horizon: MINUTE, resolution: ms(1), clock: clock.func(), stash, ..Config::default() }, |c| {
        (c.ct, c.bg) = (false, false);
    }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    }
}

/// TestHotAddAfterSpillCounts (claims A32): K is hot, its 40 events at t0.
/// K's touch T at t1, a horizon less a second later, moves word0 and reads
/// K's slot (the hot pointer). Before T adds to the entry, an insert of K2 at
/// t1 + 6 s finds both lines full and evicts K, whose entry's events are past
/// the horizon at K2's clock (the lightest slot), and with no stash spills
/// K's history to the overflow table. T's add counts by K's identity in K's
/// entry, which no slot or stash points to any more: before, it succeeded
/// there and the event was gone (case 9787 of the concurrent fuzzer).
#[test]
fn TestHotAddAfterSpillCounts() {
    let t = T::new("TestHotAddAfterSpillCounts");
    let f = Arc::new(two_line_filter(&t, -1));
    if f.lines != 2 {
        fatalf!(t, "lines {}, want 2", f.lines);
    }
    const T0: u64 = 60_000;
    let t1 = T0 + 59_000; // within a horizon of K's last touch
    let t2 = t1 + 6_000; // K's events past the horizon (their unit too)
    let fps = two_line_fps(&f, 0, 0x1100, 9);
    let k = key_at(&f, 1, fps[7], 0x5a5);
    for _ in 0..40 {
        f.touch_at(k, 0, T0);
    }
    let ks = slots_of(&f, k);
    if ks.len() != 1 || !is_hot(ld(f.w(ks[0] + 1))) {
        fatalf!(t, "setup: K in {ks:?}, not hot");
    }
    for j in 0..7u64 {
        // two events each at t1: heavier than K at t2
        let h = key_at(&f, j % 2, fps[j as usize], 0x100 + j);
        f.touch_at(h, 0, t1);
        f.touch_at(h, 0, t1);
    }
    if f.occupancy(t1) != 8 {
        fatalf!(t, "setup: occupancy {}, want 8", f.occupancy(t1));
    }
    let k2 = key_at(&f, 0, fps[8], 0x5a6);
    let i = ks[0];
    let (fired, gone) = (Arc::new(AtomicBool::new(false)), Arc::new(AtomicBool::new(false)));
    {
        let _h = {
            let (f, fired, gone) = (f.clone(), fired.clone(), gone.clone());
            set_hook(&hooks::HOT_ADDING, move |j| {
                if j != i || fired.swap(true, SeqCst) {
                    return;
                }
                f.touch_at(k2, 0, t2);
                gone.store(slots_of(&f, k).is_empty(), SeqCst);
            })
        };
        f.touch_at(k, 0, t1); // T
    }
    if !fired.load(SeqCst) || !gone.load(SeqCst) {
        fatalf!(t, "K2 did not evict K during K's hot add");
    }
    let r = f.count_at(k, f.horizon, t2);
    if r.upper < 1 && !r.saturated {
        fatalf!(t, "K reads {r:?} after a touch at t1");
    }
}

/// TestHotAddAfterStashDropCounts (claims A32): as
/// TestHotAddAfterSpillCounts, but K's history goes to the stash (one set of
/// 4 ways), and is dropped from it before T adds: K's entry holds only events
/// past the horizon, so the history weighs nothing (T's event isn't in it
/// yet), and the fourth put after it, finding no free or expired way, takes
/// K's as the lightest, with nothing to spill. T's add counted in K's entry,
/// which no slot or stash pointed to any more, and the event was gone (case
/// 9787).
#[test]
fn TestHotAddAfterStashDropCounts() {
    let t = T::new("TestHotAddAfterStashDropCounts");
    let f = Arc::new(two_line_filter(&t, 4));
    if f.lines != 2 || f.stash_n != 1 {
        fatalf!(t, "lines {}, stash sets {}; want 2, 1", f.lines, f.stash_n);
    }
    const T0: u64 = 60_000;
    let t1 = T0 + 59_000; // within a horizon of K's last touch
    let t2 = t1 + 6_000; // K's events past the horizon (their unit too)
    let fps = two_line_fps(&f, 0, 0x1100, 13);
    let k = key_at(&f, 1, fps[7], 0x5a5);
    for _ in 0..40 {
        f.touch_at(k, 0, T0);
    }
    let ks = slots_of(&f, k);
    if ks.len() != 1 || !is_hot(ld(f.w(ks[0] + 1))) {
        fatalf!(t, "setup: K in {ks:?}, not hot");
    }
    for j in 0..7u64 {
        // two events each at t1: heavier than K, and than the newcomers, at t2
        let h = key_at(&f, j % 2, fps[j as usize], 0x100 + j);
        f.touch_at(h, 0, t1);
        f.touch_at(h, 0, t1);
    }
    if f.occupancy(t1) != 8 || f.evictions() != 0 {
        fatalf!(t, "setup: occupancy {}, evictions {}; want 8, 0", f.occupancy(t1), f.evictions());
    }
    let i = ks[0];
    let id = f.stash_id(k, fingerprint(k), tag(k));
    // fired, gone, stashed, dropped
    let flags: Arc<[AtomicBool; 4]> = Default::default();
    let ev = Arc::new(AtomicU64::new(0));
    {
        let _h = {
            let (f, flags, ev, fps) = (f.clone(), flags.clone(), ev.clone(), fps.clone());
            set_hook(&hooks::HOT_ADDING, move |j| {
                if j != i || flags[0].swap(true, SeqCst) {
                    return;
                }
                // K2 evicts K (into the stash's first way); K3-K5 each evict
                // the newcomer before them (one event: lighter than the rest)
                // into the next ways; K6's eviction finds all four ways live
                // and K's the lightest.
                for n in 0..5u64 {
                    f.touch_at(key_at(&f, n % 2, fps[8 + n as usize], 0x5a6 + n), 0, t2);
                    if n == 0 {
                        flags[1].store(slots_of(&f, k).is_empty(), SeqCst);
                        flags[2].store(f.stash_get(id, t2, false).2, SeqCst);
                    }
                }
                let in_stash = f.stash_get(id, t2, false).2;
                flags[3].store(!in_stash, SeqCst);
                ev.store(f.evictions(), SeqCst);
            })
        };
        f.touch_at(k, 0, t1); // T
    }
    let [fired, gone, stashed, dropped] = [0, 1, 2, 3].map(|n| flags[n].load(SeqCst));
    let ev = ev.load(SeqCst);
    if !fired || !gone || !stashed || !dropped || ev != 5 {
        fatalf!(t, "during K's hot add: K evicted {gone}, stashed {stashed}, then dropped {dropped}; evictions {ev}, want 5");
    }
    let r = f.count_at(k, f.horizon, t2);
    if r.upper < 1 && !r.saturated {
        fatalf!(t, "K reads {r:?} after a touch at t1");
    }
}

/// TestSpillOfForeignHotEntryIsNotLost: without an overflow table, a stash
/// drop whose hot entry is no longer the history's own (reclaimed past the
/// horizon, as a restore's hotGone treats it, or already spilled) has nothing
/// left to lose: Lost and the Degraded window stay as they were, as with the
/// overflow table. A history that is its own is still lost.
#[test]
fn TestSpillOfForeignHotEntryIsNotLost() {
    let t = T::new("TestSpillOfForeignHotEntryIsNotLost");
    let clock = TestClock::new(1);
    let f = match Filter::new(Config {
        capacity: 1000,
        horizon: MINUTE,
        resolution: ms(1),
        overflow: -1,
        hot: 4,
        clock: clock.func(),
        ..Config::default()
    }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    f.hot_table(); // entry 1, never handed out, is no one's
    const NOW: u64 = 60_000;
    let k = mix64(7);
    let (fp, tg) = (fingerprint(k), tag(k));
    let (id, w0) = (f.stash_id(k, fp, tg), (fp << 48) | NOW);
    let foreign = (tg << TAG_SHIFT) | (1 << HOT_IDX_SHIFT);
    if !is_hot(foreign) {
        fatalf!(t, "not a hot pointer");
    }
    f.spill(id, w0, foreign, NOW);
    if f.lost() != 0 || f.lost_recently(NOW) {
        fatalf!(t, "a drop whose hot entry is another's: Lost {}, lost recently {}; want 0, false", f.lost(), f.lost_recently(NOW));
    }
    let (own, _) = f.next_reg(0, tg, NOW, NOW, true);
    f.spill(id, w0, own, NOW);
    if f.lost() != 1 || !f.lost_recently(NOW) {
        fatalf!(t, "a drop of a history with one event: Lost {}, lost recently {}; want 1, true", f.lost(), f.lost_recently(NOW));
    }
}
