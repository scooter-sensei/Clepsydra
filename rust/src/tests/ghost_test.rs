//! ghost_test.go.

use std::sync::Arc;
use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64};
use std::time::{Duration, Instant};

use super::check::*;
use super::helpers::*;
use crate::Result;
use crate::ghost::*;
use crate::hooks;

/// Concurrent adds to a small ghost table across many slice boundaries. An
/// add decodes the entries' 2-bit slices against the epoch it read; if
/// another add moved the epoch on and wrote an entry for the new slice
/// meanwhile, that entry decodes as four slices old, dead, and must not be
/// taken for a free one and overwritten. Each worker reads back its last
/// three histories after every add: each must keep its count in Upper,
/// through its own entry or a floor, unless the shared clock has moved far
/// enough during the read for the history to have rightly aged out.
///
/// The interleaving is made to happen rather than waited for: on one add pass
/// in 16 a worker stops in the window (GHOST_ADDING: the pass has loaded the
/// epoch and not yet its buckets; GHOST_FLOOR, one raise in 4: a floor raise
/// likewise) and yields until other adds have moved the epoch on one to three
/// slices, then a few times more so the mover can write its entry for the new
/// slice. The scan that follows decodes against the stale epoch (an entry one
/// slice newer reads as four slices old, two or three newer as live but old)
/// thousands of times a run, where preemption alone landed there a few dozen
/// times. The workers run on every processor. The test fails if no stop ever
/// saw the epoch move, since it would no longer test the race
/// (docs/artifact/claims.md, A47, has the measurements).
#[test]
fn TestGhostEpochRace() {
    let t = T::new("TestGhostEpochRace");
    let rounds: i64 = if short() { 8 } else { 40 };
    // 4 buckets and a clock that moves 200 ticks per add keep about 75
    // histories live (of 92 entries) and cross a slice boundary every 25 adds.
    const HORIZON: u64 = 10_000;
    const N: u64 = 5;
    const WORKERS: u64 = 8;
    const PER: u64 = 4000;
    let (lost, stops, stale) = (Arc::new(AtomicI64::new(0)), Arc::new(AtomicI64::new(0)), Arc::new(AtomicI64::new(0)));
    for round in 0..rounds {
        let g = Arc::new(Ghost::new(4 * 64, HORIZON, 1 + round % 2, 1 + round % 3)); // one and two candidate buckets, one to three floors
        // stop holds the calling worker in its window on one pass in every,
        // until the epoch has moved on one to three slices (2 ms at most).
        // rand_uint64 draws from a per-thread generator: no state shared
        // between the workers.
        let stop = {
            let (g, stops, stale) = (g.clone(), stops.clone(), stale.clone());
            Arc::new(move |every: u64| {
                let x = rand_uint64();
                if x % every != 0 {
                    return;
                }
                stops.fetch_add(1, SeqCst);
                let (ep, moves) = (g.epoch.load(SeqCst), 1 + (x >> 32) % 3);
                let deadline = Instant::now() + Duration::from_millis(2);
                while g.epoch.load(SeqCst) < ep + moves {
                    if Instant::now() > deadline {
                        return;
                    }
                    std::thread::yield_now();
                }
                for _ in 0..3 {
                    // the mover writes its entry for the new slice
                    std::thread::yield_now();
                }
                stale.fetch_add(1, SeqCst);
            })
        };
        let _adding = set_hook(&hooks::GHOST_ADDING, {
            let stop = stop.clone();
            move |()| stop(16)
        });
        let _floor = set_hook(&hooks::GHOST_FLOOR, {
            let stop = stop.clone();
            move |()| stop(4)
        });
        let clock = AtomicU64::new(HORIZON);
        std::thread::scope(|s| {
            for w in 0..WORKERS {
                let (g, clock, lost, t) = (&g, &clock, &lost, &t);
                s.spawn(move || {
                    let mut ring = [(0u64, 0u64); 3]; // id, last
                    for i in 0..PER {
                        let now = clock.fetch_add(200, SeqCst) + 200;
                        let id = (((round as u64) << 48) | (w << 32) | i) + 1;
                        g.add(id, N, now, false, now);
                        ring[(i % 3) as usize] = (id, now);
                        let at = clock.load(SeqCst);
                        for &(rid, last) in &ring {
                            if rid == 0 || at.wrapping_sub(last) > HORIZON - g.slice_len {
                                continue;
                            }
                            let (e, _) = g.estimate(rid, HORIZON, at);
                            // A slice's events are dead once an add has seen
                            // the slice three later: the clock may have got
                            // there during the read.
                            let after = clock.load(SeqCst);
                            if after / g.slice_len >= last / g.slice_len + 3 {
                                continue;
                            }
                            if e.upper < N as i64 && lost.fetch_add(1, SeqCst) + 1 <= 5 {
                                logf!(t, "round {round}: history {rid:x} added at {last} reads {} at {at}", show(&e));
                            }
                        }
                    }
                });
            }
        });
    }
    logf!(t, "{} stops in the window, {} across an epoch move", stops.load(SeqCst), stale.load(SeqCst));
    if lost.load(SeqCst) > 0 {
        fatalf!(t, "{} reads found a live history below its count", lost.load(SeqCst));
    }
    if stale.load(SeqCst) == 0 {
        fatalf!(t, "no worker stopped in its window saw the epoch move: the race went untested");
    }
}

/// TestGhostAddAcrossEpoch: TestGhostEpochRace's interleaving, made to happen
/// once and checked exactly. An add loads the epoch; before it scans its
/// bucket another add moves the epoch on a slice and writes an entry for the
/// new slice there. Decoded against the epoch the first add loaded, that entry
/// is four slices old, dead: the first add must not take its place (the
/// defect of changes 28, an add that read the epoch once).
#[test]
fn TestGhostAddAcrossEpoch() {
    let t = T::new("TestGhostAddAcrossEpoch");
    const HORIZON: u64 = 10_000; // slices of 5,000 ticks
    let g = Arc::new(Ghost::new(64, HORIZON, 2, 3)); // one bucket: the histories meet in it
    g.add(1, 5, 52_000, false, 52_000); // slice 10: the epoch is 11
    let fired = Arc::new(AtomicBool::new(false));
    let _h = set_hook(&hooks::GHOST_ADDING, {
        let (g, fired) = (g.clone(), fired.clone());
        move |()| {
            hooks::GHOST_ADDING.clear();
            fired.store(true, SeqCst);
            g.add(2, 7, 55_000, false, 55_000); // slice 11: the epoch moves on; history 2 takes the first free entry
        }
    });
    g.add(3, 9, 54_999, false, 54_999); // slice 10, the epoch loaded before the move
    hooks::GHOST_ADDING.clear();
    if !fired.load(SeqCst) {
        fatalf!(t, "the hook didn't run: the add's window is gone");
    }
    for (id, want) in [(1u64, 5i64), (2, 7), (3, 9)] {
        let (r, ok) = g.estimate(id, HORIZON, 55_000);
        if !ok || r.count != want {
            errorf!(t, "history {id} reads {}, want {want} events", show(&r));
        }
    }
}

/// TestGhostFloorAcrossEpoch: the same for a floor. A raise loads the epoch;
/// before it loads the floor another raise moves the epoch on a slice and
/// lifts the floor in the new slice. Decoded against the stale epoch the
/// higher floor is dead, and writing the first raise's count over it would
/// lower it (42cc598: "raiseFloor could lower a floor the same way"): every
/// key reading the bucket must still see the higher floor in Upper.
#[test]
fn TestGhostFloorAcrossEpoch() {
    let t = T::new("TestGhostFloorAcrossEpoch");
    const HORIZON: u64 = 10_000; // slices of 5,000 ticks
    let g = Arc::new(Ghost::new(64, HORIZON, 1, 1)); // one bucket, one floor that every fingerprint reads
    g.add(1, 5, 52_000, false, 52_000); // slice 10: the epoch is 11
    let fired = Arc::new(AtomicBool::new(false));
    let _h = set_hook(&hooks::GHOST_FLOOR, {
        let (g, fired) = (g.clone(), fired.clone());
        move |()| {
            hooks::GHOST_FLOOR.clear();
            fired.store(true, SeqCst);
            g.advance(11); // slice 11: the epoch moves on
            g.raise_floor(0, 2, 20, 11, 55_000); // a history of 20 events leaves to the floor
        }
    });
    g.raise_floor(0, 3, 4, 10, 54_999); // a history of 4 events, slice 10, the epoch loaded before the move
    hooks::GHOST_FLOOR.clear();
    if !fired.load(SeqCst) {
        fatalf!(t, "the hook didn't run: the raise's window is gone");
    }
    for id in [1u64, 99] {
        let (r, ok) = g.estimate(id, HORIZON, 55_000);
        if !ok || r.upper < 20 {
            errorf!(t, "key {id} reads {}: the floor of 20 was lowered", show(&r));
        }
    }
}

/// TestGhostAddAheadOfClock: an evictor whose clock is behind the evicted
/// key's last touch spills a history whose newest event lies in a slice past
/// the one its own clock is in. The entry's 2-bit slice must still decode to
/// that slice: before, it decoded four slices old, so reads skipped it, adds
/// took its place as free and the next epoch swept it.
#[test]
fn TestGhostAddAheadOfClock() {
    let t = T::new("TestGhostAddAheadOfClock");
    const HORIZON: u64 = 10_000; // slices of 5,000 ticks
    let g = Ghost::new(4 * 64, HORIZON, 2, 3);
    g.add(1, 5, 80_008, false, 79_989); // last touch 80,008; the evictor's clock reads 79,989
    for now in [79_989u64, 80_008, 80_037] {
        let (r, _) = g.estimate(1, HORIZON, now);
        if r.count != 5 {
            fatalf!(t, "at {now} the history reads {}, want 5 events", show(&r));
        }
    }
    g.add(2, 5, 85_000, false, 85_000); // a later slice: the epoch moves on, dead entries are swept
    let (r, _) = g.estimate(1, HORIZON, 85_000);
    if r.count != 5 {
        fatalf!(t, "after the next epoch the history reads {}, want 5 events", show(&r));
    }
}

/// TestGhostReadAcrossEpoch: a read loads its buckets, then an add moves the
/// epoch on two slices and sweeps the entries dead by then, before the read
/// decodes what it loaded. Decoded against the new epoch, an entry four
/// slices older than it (the sweep's, loaded before it was cleared) reads as
/// current: a history whose newest event left the horizon reads as live,
/// Seen with its Count. Against the epoch the lines were loaded under it is
/// dead.
#[test]
fn TestGhostReadAcrossEpoch() {
    let t = T::new("TestGhostReadAcrossEpoch");
    const HORIZON: u64 = 10_000; // slices of 5,000 ticks
    for choices in [1i64, 2] {
        let g = Arc::new(Ghost::new(4 * 64, HORIZON, choices, 3));
        g.add(1, 5, 1_000, false, 1_000); // slice 0
        g.add(2, 5, 10_500, false, 10_500); // slice 2: the epoch moves on, nothing is dead yet
        // At 20,100 (slice 4) history 1's newest event is 19,100 old: dead.
        // History 2's (slice 2) is live, so the read gets past g.until.
        const NOW: u64 = 20_100;
        let (r, ok) = g.estimate(1, HORIZON, NOW);
        if ok || r != Result::default() {
            fatalf!(t, "choices {choices}: history 1 reads {} before the add, want nothing", show(&r));
        }
        let h = set_hook(&hooks::GHOST_READ, {
            let g = g.clone();
            move |()| {
                hooks::GHOST_READ.clear();
                g.add(3, 5, NOW - 50, false, NOW - 50); // slice 4: the epoch moves on two, slice 0 is swept
            }
        });
        let (r, ok) = g.estimate(1, HORIZON, NOW);
        drop(h);
        if ok || r != Result::default() {
            errorf!(t, "choices {choices}: history 1, dead, reads {} across the epoch's move", show(&r));
        }
        let (r, ok) = g.estimate(2, HORIZON, NOW);
        if !ok || r.count != 5 {
            errorf!(t, "choices {choices}: history 2, live, reads {}", show(&r));
        }
    }
}

/// TestGhostScanLayout: estimate's scan decodes three entries per word
/// (entries 1 and 2 beside the floors in word 0, three in each of words 1-7),
/// so the layout must keep three entries of ghEntryBits to a word.
#[test]
fn TestGhostScanLayout() {
    let t = T::new("TestGhostScanLayout");
    if GH_PER_WORD != 3 || 3 * GH_ENTRY_BITS > 64 {
        fatalf!(t, "ghPerWord {GH_PER_WORD}, ghEntryBits {GH_ENTRY_BITS}: estimate's scan decodes three entries per word");
    }
}
