//! buzz_test.go.
//!
//! Buzz entries (buzz.rs, claims A46): a conditional touch refused in doubt
//! gives its key an exact log of admitted events.

use std::sync::atomic::Ordering::SeqCst;
use std::sync::mpsc;

use super::check::*;
use super::gorand::Rand;
use super::gosort;
use super::helpers::*;
use crate::hooks::BUZZ_OFF;
use crate::slots::*;
use crate::{Config, Filter};

#[derive(Clone, Copy, PartialEq, Eq)]
struct BuzzRule {
    w: u64,
    limit: u64,
}

const fn rule(w: u64, limit: u64) -> BuzzRule {
    BuzzRule { w, limit }
}

impl std::fmt::Display for BuzzRule {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.pad(&format!("{}s/{}", go_g(self.w as f64 / 1000.0), self.limit))
    }
}

/// buzzRow is one reading: the same requests offered to an exact sliding
/// log, to TouchIfUnderAt without buzz entries (base) and with them.
#[derive(Default, Clone, Copy)]
struct BuzzRow {
    events: i64,
    admitted: [i64; 3],   // exact, base, buzz
    false_deny: [i64; 2], // base, buzz: refused while the key's own events in the window are under the limit
    given: u64,
    dropped: u64,
    decided: u64,
    evictions: u64,
    relocations: u64,
    bytes_base: i64,
    bytes_buzz: i64,
}

impl BuzzRow {
    fn share(&self, a: usize) -> f64 {
        self.admitted[a] as f64 / self.admitted[0] as f64
    }
    fn denied(&self, a: usize) -> f64 {
        self.false_deny[a] as f64 / self.events as f64
    }
}

impl std::fmt::Display for BuzzRow {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "requests {:7} exact admits {:7} | admitted/exact base {:.3} buzz {:.3} | false denials base {:.4} buzz {:.4} | entries given {:5} dropped {:6} | decided by a log {:7}",
            self.events,
            self.admitted[0],
            self.share(1),
            self.share(2),
            self.denied(0),
            self.denied(1),
            self.given,
            self.dropped,
            self.decided
        )
    }
}

/// ownWithin counts the events of ts (sorted) inside [now-w, now].
fn own_within(ts: &[u64], w: u64, now: u64) -> i64 {
    (ts.len() - ts.partition_point(|&x| x + w < now)) as i64
}

/// buzzOffer offers nkeys keys' Poisson requests, at mult times the rule's
/// rate, to the three limiters for the given horizons of 60 s (1 ms ticks),
/// in filters of Capacity capN. One request in 1/plainFrac is a plain TouchAt
/// in every arm (it counts, and no rule is asked). With churnLoad both
/// filters also take tfbench's churn stream at that load (its 4 horizons). It
/// fails the test if an arm admits a request while the key's window already
/// holds the limit.
#[expect(clippy::too_many_arguments, reason = "a helper of the Go test, parameter for parameter")]
fn buzz_offer(t: &T, cap_n: i64, nkeys: usize, ru: BuzzRule, mult: f64, horizons: u64, plain_frac: f64, churn_load: f64, churn_seed: u64) -> BuzzRow {
    const H_T: u64 = 60_000;
    let was = BUZZ_OFF.load(SeqCst);
    let _back = Defer(move || BUZZ_OFF.store(was, SeqCst));
    let new_f = || match Filter::new(Config { capacity: cap_n, horizon: MINUTE, resolution: ms(1), ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    let (fb, fz) = (new_f(), new_f());
    let mut r = Rand::new(4, ru.w + ru.limit);
    let mut evs: Vec<(u64, usize, bool)> = Vec::new(); // t, k, plain
    let rate = mult * ru.limit as f64 / ru.w as f64;
    let end = (horizons * H_T) as f64;
    for k in 0..nkeys {
        let mut x = r.exp_float64() / rate;
        while x < end {
            evs.push((x as u64 + 1, k, plain_frac > 0.0 && r.float64() < plain_frac));
            x += r.exp_float64() / rate;
        }
    }
    evs.sort_by_key(|a| a.0); // stable, as sort.SliceStable
    // per arm, per key: the ticks of its events (admitted, or plain)
    let mut own: [Vec<Vec<u64>>; 3] = std::array::from_fn(|_| vec![Vec::new(); nkeys]);
    let mut row = BuzzRow::default();
    let limit = ru.limit as i64;
    let mut offer = |e: (u64, usize, bool)| {
        let (et, ek, plain) = e;
        let h = mix64(ek as u64 + (1 << 50));
        if plain {
            for o in &mut own {
                o[ek].push(et);
            }
            fb.touch_at(h, ru.w, et);
            fz.touch_at(h, ru.w, et);
            return;
        }
        row.events += 1;
        if own_within(&own[0][ek], ru.w, et) < limit {
            own[0][ek].push(et);
            row.admitted[0] += 1;
        }
        for (a, f) in [&fb, &fz].into_iter().enumerate() {
            BUZZ_OFF.store(a == 0, SeqCst);
            let inside = own_within(&own[a + 1][ek], ru.w, et);
            let (_, ok) = f.touch_if_under_at(h, ru.w, ru.limit, et);
            if ok {
                if inside >= limit {
                    fatalf!(t, "{ru} x{mult:.1} arm {a}: admitted with {inside} events in the window (key {ek}, tick {et})");
                }
                own[a + 1][ek].push(et);
                row.admitted[a + 1] += 1;
            } else if inside < limit {
                row.false_deny[a] += 1;
            }
        }
        BUZZ_OFF.store(false, SeqCst);
    };
    if churn_load > 0.0 {
        let mut i = 0;
        churn(churn_load, churn_seed, cap_n, H_T, mix64, |h, w, now| {
            while i < evs.len() && evs[i].0 <= now {
                offer(evs[i]);
                i += 1;
            }
            fb.check_and_touch_at(h, w, now);
            fz.check_and_touch_at(h, w, now);
        });
        while i < evs.len() {
            offer(evs[i]);
            i += 1;
        }
    } else {
        for &e in &evs {
            offer(e);
        }
    }
    if fb.buzz.get().is_some() {
        fatalf!(t, "the base arm's filter holds a buzz table");
    }
    if let Some(tb) = fz.buzz.get() {
        (row.given, row.dropped, row.decided) = (tb.given.load(SeqCst), tb.dropped.load(SeqCst), tb.decided.load(SeqCst));
    }
    (row.evictions, row.relocations) = (fz.evictions(), fz.relocations());
    (row.bytes_base, row.bytes_buzz) = (fb.bytes(), fz.bytes());
    row
}

const SKIP_CT: &str = "ConstantTime filters keep TouchIfUnder as without buzz entries";

/// TestBuzzLimiter: 100 contested keys in a filter of Capacity 200,000, for 20
/// horizons (4 for the 1 s rule; a log answers for a window only once the
/// window starts after the log does, so a 60 s rule needs the length). Offered
/// 2x a rule a log decides, the buzz admits at least 0.97 of what an exact
/// sliding log admits; offered 0.8x, its false denials are at most a quarter
/// of the register's; no arm admits past a limit (buzzOffer asserts it at
/// every admission). A limit past the log (20) is decided as without entries.
#[test]
fn TestBuzzLimiter() {
    let t = T::new("TestBuzzLimiter");
    if force_ct() {
        skip!(t, "{SKIP_CT}");
    }
    for ru in [rule(1_000, 5), rule(10_000, 5), rule(60_000, 6), rule(60_000, 16)] {
        let horizons = if ru.w == 1_000 { 4 } else { 20 };
        let hi = buzz_offer(&t, 200_000, 100, ru, 2.0, horizons, 0.0, 0.0, 0);
        let lo = buzz_offer(&t, 200_000, 100, ru, 0.8, horizons, 0.0, 0.0, 0);
        logf!(t, "{ru:<7} x2.0 | {hi}");
        logf!(t, "{ru:<7} x0.8 | {lo}");
        if hi.share(2) < 0.97 {
            errorf!(t, "{ru} offered 2x: the buzz admits {:.3} of the exact log (0.97 wanted)", hi.share(2));
        }
        if lo.denied(1) > lo.denied(0) / 4.0 {
            errorf!(t, "{ru} offered 0.8x: false denials {:.4}, the register's {:.4} (a quarter wanted)", lo.denied(1), lo.denied(0));
        }
    }
    let past = buzz_offer(&t, 200_000, 100, rule(2_500, 20), 2.0, 2, 0.0, 0.0, 0);
    logf!(t, "2.5s/20 x2.0 | {past}");
    if past.admitted[1] != past.admitted[2] || past.given != 0 {
        errorf!(
            t,
            "a limit of 20 was decided by a log: admitted {}, without entries {}, entries given {}",
            past.admitted[2],
            past.admitted[1],
            past.given
        );
    }
}

/// TestBuzzMemory: nothing is allocated before an uncertain refusal. 10,000
/// keys asked 4 times each at one tick under a limit of 3: a refusal there is
/// certain (the register is exact at one tick).
#[test]
fn TestBuzzMemory() {
    let t = T::new("TestBuzzMemory");
    let f = must(Config { capacity: 200_000, horizon: MINUTE, resolution: ms(1), ..Config::default() });
    let b0 = f.bytes();
    for k in 0..10_000u64 {
        for _ in 0..4 {
            f.touch_if_under_at(mix64(k + (1 << 51)), 60_000, 3, 1000);
        }
    }
    if f.buzz.get().is_some() || f.bytes() != b0 {
        fatalf!(t, "memory before any uncertain refusal: table {}, Bytes {b0} -> {}", f.buzz.get().is_some(), f.bytes());
    }
    // An uncertain refusal: 5 events over 1.5 s, asked at 1 s / 5.
    let h = mix64(7);
    for i in 0..5u64 {
        f.touch_if_under_at(h, 1_000, 5, 2000 + i * 200);
    }
    if force_ct() {
        // ConstantTime keeps TouchIfUnder as without entries: no table, ever
        let (_, ok) = f.touch_if_under_at(h, 1_000, 5, 3300);
        if ok || f.buzz.get().is_some() {
            fatalf!(t, "ConstantTime: admitted {ok}, table {} after an uncertain refusal", f.buzz.get().is_some());
        }
        return;
    }
    let (_, ok) = f.touch_if_under_at(h, 1_000, 5, 3300);
    if ok || f.buzz.get().is_none() {
        fatalf!(t, "no entry after an uncertain refusal");
    }
    let per = (f.bytes() - b0) as f64 / 200_000.0;
    logf!(t, "Bytes {b0} -> {} once allocated: {per:.3} B per key of Capacity", f.bytes());
    if per > 0.6 {
        errorf!(t, "{per:.3} B per key once allocated (0.6 at most)");
    }
}

/// TestBuzzPlainTouch: a plain touch of a key that holds an entry changes its
/// words, so the entry is dropped and the next request is the register's.
#[test]
fn TestBuzzPlainTouch() {
    let t = T::new("TestBuzzPlainTouch");
    if force_ct() {
        skip!(t, "{SKIP_CT}");
    }
    let f = must(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), ..Config::default() });
    let h = mix64(9);
    for i in 0..5u64 {
        f.touch_if_under_at(h, 1_000, 5, 2000 + i * 200);
    }
    f.touch_if_under_at(h, 1_000, 5, 3300); // refused in doubt: an entry, its log from 3300
    let tb = match f.buzz.get() {
        Some(tb) if tb.given.load(SeqCst) == 1 => tb,
        _ => fatalf!(t, "no entry given"),
    };
    let (_, ok) = f.touch_if_under_at(h, 1_000, 5, 4400); // a window after the log's start
    if !ok || tb.decided.load(SeqCst) == 0 {
        fatalf!(t, "the log decided nothing (admitted {ok})");
    }
    f.touch_at(h, 0, 4500);
    let d = tb.dropped.load(SeqCst);
    f.touch_if_under_at(h, 1_000, 5, 4600);
    if tb.dropped.load(SeqCst) != d + 1 {
        errorf!(t, "a plain touch did not drop the entry (dropped {d} -> {})", tb.dropped.load(SeqCst));
    }
}

/// TestBuzzConcurrent: G threads on a shared clock, as TestTouchIfSustained,
/// with limits a log decides: the admitted events per key stay within the
/// limit at every window position, and logs decide some of them. Also run
/// under the race detector (stress tier).
#[test]
fn TestBuzzConcurrent() {
    let t = T::new("TestBuzzConcurrent");
    if force_ct() {
        skip!(t, "{SKIP_CT}");
    }
    const G: usize = 8;
    const H_TICKS: u64 = 60_000;
    for c in [rule(1_000, 5), rule(5_000, 10), rule(10_000, 5)] {
        for nk in [1usize, 200] {
            let f = must(Config { capacity: 200_000, horizon: MINUTE, resolution: ms(1), ..Config::default() });
            let mut ticks = 2 * H_TICKS;
            if short() {
                ticks = (H_TICKS / 4).max(3 * c.w); // a log decides once the window starts after it does
            }
            // per thread: key<<32 | tick
            let logs: Vec<Vec<u64>> = std::thread::scope(|s| {
                let (done_tx, done_rx) = mpsc::channel::<()>();
                let mut step = Vec::new();
                let mut workers = Vec::new();
                for g in 0..G {
                    let (tx, rx) = mpsc::sync_channel::<u64>(1);
                    step.push(tx);
                    let (f, done) = (&f, done_tx.clone());
                    workers.push(s.spawn(move || {
                        let mut log: Vec<u64> = Vec::new();
                        let mut r = Rand::new(g as u64, 3);
                        for now in rx {
                            for _ in 0..1 + r.int_n(2) {
                                // each key is asked about twice its limit per window
                                let k = r.int_n(nk as i64) as u64;
                                if r.float64() < 2.0 * c.limit as f64 * nk as f64 / (c.w as f64 * 1.5 * G as f64) {
                                    let (_, ok) = f.touch_if_under_at(mix64(k + (1 << 43)), c.w, c.limit, now);
                                    if ok {
                                        log.push((k << 32) | now);
                                    }
                                }
                            }
                            let _ = done.send(());
                        }
                        log
                    }));
                }
                for now in 1..=ticks {
                    for tx in &step {
                        let _ = tx.send(now);
                    }
                    for _ in 0..G {
                        let _ = done_rx.recv();
                    }
                }
                drop(step);
                workers.into_iter().map(|h| h.join().unwrap()).collect()
            });
            let mut per: Vec<Vec<u64>> = vec![Vec::new(); nk];
            let mut total = 0i64;
            for log in &logs {
                for &x in log {
                    per[(x >> 32) as usize].push(x & ((1 << 32) - 1));
                    total += 1;
                }
            }
            let (mut over, mut positions) = (0, 0);
            for ts in &mut per {
                gosort::slice(ts, |a, b| a < b);
                let mut lo = 0;
                for hi in 0..ts.len() {
                    while ts[lo] + c.w < ts[hi] {
                        lo += 1;
                    }
                    let mut j = hi;
                    while j + 1 < ts.len() && ts[j + 1] == ts[hi] {
                        j += 1;
                    }
                    positions += 1;
                    if (j - lo + 1) as u64 > c.limit {
                        over += 1;
                    }
                }
            }
            let decided = f.buzz.get().map_or(0, |tb| tb.decided.load(SeqCst));
            logf!(
                t,
                "keys {nk:3} {c}: admitted {total} ({:.2} of limit x windows x keys), window positions {positions}, over the limit {over}, decided by a log {decided}",
                total as f64 / (c.limit as f64 * ticks as f64 / c.w as f64 * nk as f64)
            );
            if over != 0 {
                errorf!(t, "keys {nk} {c}: over the limit at {over} window positions");
            }
            if decided == 0 {
                errorf!(t, "keys {nk} {c}: no decision was made by a log");
            }
        }
    }
}

/// TestBuzzSnapshot: a strict limiter's decisions across a snapshot (version
/// 3 carries the buzz table): 400 keys offered 2x 1 s / 5 for 4 horizons, once
/// uninterrupted and once snapshotted at 2 horizons and restored into a new
/// filter; every decision and answer must be the same.
#[test]
fn TestBuzzSnapshot() {
    let t = T::new("TestBuzzSnapshot");
    if force_ct() || force_bg() {
        skip!(t, "snapshots refuse ConstantTime and Background filters");
    }
    let cfg = Config { capacity: 20_000, horizon: MINUTE, resolution: ms(1), ..Config::default() };
    let mut r = Rand::new(5, 5);
    let mut evs: Vec<(u64, u64)> = Vec::new(); // t, k
    for k in 0..400u64 {
        let mut x = r.exp_float64() / 0.01;
        while x < 240_000.0 {
            evs.push((x as u64 + 1, k));
            x += r.exp_float64() / 0.01;
        }
    }
    evs.sort_by_key(|a| a.0); // stable
    let run = |snap_at: u64| -> (Vec<String>, u64) {
        let mut f = must(cfg.clone());
        let mut snapped = false;
        let mut out = Vec::new();
        for &(et, ek) in &evs {
            if !snapped && snap_at > 0 && et >= snap_at {
                snapped = true;
                let mut buf: Vec<u8> = Vec::new();
                if let Err(e) = f.write_to(&mut buf) {
                    fatalf!(t, "{e}");
                }
                let g = must(cfg.clone());
                if let Err(e) = g.read_from(&mut &buf[..]) {
                    fatalf!(t, "{e}");
                }
                f = g;
            }
            let (res, ok) = f.touch_if_under_at(mix64(ek + (1 << 49)), 1_000, 5, et);
            out.push(format!("{ok} {}", gov_result(&res)));
        }
        let decided = f.buzz.get().map_or(0, |tb| tb.decided.load(SeqCst));
        (out, decided)
    };
    let (u, du) = run(0);
    let (s, ds) = run(120_000);
    let diff = u.iter().zip(&s).filter(|(a, b)| a != b).count();
    logf!(t, "{} decisions, {diff} differ; decided by a log: {du} uninterrupted, {ds} after the restore", u.len());
    if diff != 0 || du == 0 {
        errorf!(t, "a restore changed {diff} decisions, or no log decided ({du})");
    }
}

/// TestBuzzReading (TF_EXP=1) reads the buzz as the lab arm B12b read it
/// (claims A46): admitted shares and false denials on seven rules, offered
/// 0.8x, 1.2x and 2x, in five scenarios: A 400 contested keys in a filter of
/// Capacity 200,000 (1,838 entries); B the same keys in a filter of 12,800
/// (112 entries: 3.6 contested keys an entry); C A with one request in ten a
/// plain touch; D 1,000 contested keys beside tfbench's churn stream at loads
/// 1.00 and 1.15; E limits past the log (reported, not judged). Every
/// admission is checked against the arm's own events (buzzOffer).
#[test]
fn TestBuzzReading() {
    let t = T::new("TestBuzzReading");
    if force_ct() {
        skip!(t, "{SKIP_CT}");
    }
    if !env_on("TF_EXP") {
        skip!(t, "set TF_EXP=1");
    }
    let rules = [rule(1_000, 5), rule(5_000, 10), rule(10_000, 3), rule(10_000, 5), rule(30_000, 10), rule(60_000, 6), rule(60_000, 16)];
    let mults = [0.8f64, 1.2, 2.0];
    let scen = |name: &str, cap_n: i64, plain: f64, rules: &[BuzzRule]| {
        for &ru in rules {
            for &mult in &mults {
                let row = buzz_offer(&t, cap_n, 400, ru, mult, 20, plain, 0.0, 0);
                logf!(t, "{name} {ru:<7} x{mult:.1} | {row}");
                if name == "A" && mult == 2.0 && ru == rules[0] {
                    logf!(
                        t,
                        "{name} memory: Bytes without entries {}, with {}: {:.3} B per key of Capacity",
                        row.bytes_base,
                        row.bytes_buzz,
                        (row.bytes_buzz - row.bytes_base) as f64 / cap_n as f64
                    );
                }
            }
        }
    };
    scen("A", 200_000, 0.0, &rules);
    scen("B", 12_800, 0.0, &rules);
    scen("C", 200_000, 0.1, &rules);
    scen("E", 200_000, 0.0, &[rule(2_500, 20), rule(5_000, 40)]);
    for (load, seed) in [(1.0f64, 2u64), (1.15, 4)] {
        for ru in [rule(1_000, 5), rule(10_000, 5)] {
            for mult in [0.8f64, 2.0] {
                let row = buzz_offer(&t, 200_000, 1000, ru, mult, 4, 0.0, load, seed);
                logf!(t, "D load {load:.2} {ru:<7} x{mult:.1} | {row} | evictions {} relocations {}", row.evictions, row.relocations);
            }
        }
    }
}
