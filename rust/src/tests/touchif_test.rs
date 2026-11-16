//! touchif_test.go.

use std::sync::atomic::AtomicI64;
use std::sync::atomic::Ordering::SeqCst;
use std::sync::mpsc;
use std::time::Duration;

use super::check::*;
use super::gorand::Rand;
use super::gosort;
use super::helpers::*;
use crate::Config;
use crate::slots::*;

/// TestTouchIfBurst (claims A45): G threads at one tick, each calling until
/// refused, on fresh keys: admitted per key must equal the limit.
#[test]
fn TestTouchIfBurst() {
    let t = T::new("TestTouchIfBurst");
    const G: u64 = 8;
    for nk in [1usize, 1000] {
        for limit in [10u64, 100, 1000] {
            let f = must(Config { capacity: 200_000, horizon: MINUTE, resolution: ms(1), ..Config::default() });
            let adm: Vec<AtomicI64> = (0..nk).map(|_| AtomicI64::new(0)).collect();
            std::thread::scope(|s| {
                for g in 0..G {
                    let (f, adm) = (&f, &adm);
                    s.spawn(move || {
                        let mut r = Rand::new(g, 1);
                        // until every key has refused this thread once (the
                        // first reading stopped after 4 x keys refusals in
                        // all, before every key was full)
                        let (mut refused, mut left) = (vec![false; nk], nk);
                        while left > 0 {
                            let k = r.int_n(nk as i64) as usize;
                            let (_, ok) = f.touch_if_under_at(mix64(k as u64 + (1 << 45)), 60_000, limit, 1000);
                            if ok {
                                adm[k].fetch_add(1, SeqCst);
                            } else if !refused[k] {
                                (refused[k], left) = (true, left - 1);
                            }
                        }
                    });
                }
            });
            let (mut lo, mut hi) = (1i64 << 62, 0i64);
            for a in &adm {
                (lo, hi) = (lo.min(a.load(SeqCst)), hi.max(a.load(SeqCst)));
            }
            logf!(t, "burst keys {nk:4} limit {limit:4}: admitted per key min {lo} max {hi}; evictions {}, promoted {}", f.evictions(), f.promoted());
            if lo != limit as i64 || hi != limit as i64 {
                errorf!(t, "burst keys {nk} limit {limit}: admitted {lo} to {hi} per key");
            }
        }
    }
}

/// TestTouchIfSustained (claims A45): G threads over three horizons on a
/// shared clock (1 ms steps, 4 calls each per step); admitted per key at
/// every window position must stay within the limit.
#[test]
fn TestTouchIfSustained() {
    let t = T::new("TestTouchIfSustained");
    const G: usize = 8;
    const H_TICKS: u64 = 60_000;
    for (w, limit) in [(1_000u64, 100u64), (5_000, 500)] {
        for nk in [1usize, 1000] {
            let f = must(Config { capacity: 200_000, horizon: MINUTE, resolution: ms(1), ..Config::default() });
            let sat = AtomicI64::new(0);
            let mut ticks = 3 * H_TICKS;
            if nk > 1 || short() {
                ticks = H_TICKS / 2; // 1,000 keys: half a horizon (the per-position check is per key)
            }
            // per thread: key<<32 | tick
            let logs: Vec<Vec<u64>> = std::thread::scope(|s| {
                let (done_tx, done_rx) = mpsc::channel::<()>();
                let mut step = Vec::new();
                let mut workers = Vec::new();
                for g in 0..G {
                    let (tx, rx) = mpsc::sync_channel::<u64>(1);
                    step.push(tx);
                    let (f, sat, done) = (&f, &sat, done_tx.clone());
                    workers.push(s.spawn(move || {
                        let mut log: Vec<u64> = Vec::new();
                        let mut r = Rand::new(g as u64, 2);
                        for now in rx {
                            for _ in 0..4 {
                                let k = r.int_n(nk as i64) as u64;
                                let (res, ok) = f.touch_if_under_at(mix64(k + (1 << 44)), w, limit, now);
                                if ok {
                                    log.push((k << 32) | now);
                                }
                                if res.saturated {
                                    sat.fetch_add(1, SeqCst);
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
            for log in &logs {
                for &x in log {
                    per[(x >> 32) as usize].push(x & ((1 << 32) - 1));
                }
            }
            let (mut over, mut positions, mut sum) = (0, 0i64, 0i64);
            let mut min_w = 1i64 << 30;
            for ts in &mut per {
                gosort::slice(ts, |a, b| a < b);
                let mut lo = 0;
                for hi in 0..ts.len() {
                    // window ending at each admission's tick
                    while ts[lo] + w < ts[hi] {
                        lo += 1;
                    }
                    let mut j = hi;
                    while j + 1 < ts.len() && ts[j + 1] == ts[hi] {
                        j += 1;
                    }
                    let n = (j - lo + 1) as i64;
                    positions += 1;
                    sum += n;
                    min_w = min_w.min(n);
                    if n > limit as i64 {
                        over += 1;
                    }
                }
            }
            logf!(
                t,
                "sustained keys {nk:4} W={}s limit {limit}: positions {positions}, over the limit {over}, mean admitted per window {:.1} (min {min_w}); evictions {}, saturated answers {}",
                w / 1000,
                sum as f64 / positions.max(1) as f64,
                f.evictions(),
                sat.load(SeqCst)
            );
            if over != 0 {
                errorf!(t, "sustained keys {nk} W={}s limit {limit}: over the limit at {over} window positions", w / 1000);
            }
        }
    }
}

/// TestTouchIfVelocity (claims A45): one thread, tfbench's velocity
/// workload's events as requests, against the filter's own admitted log and
/// an exact sliding-log limiter fed the same requests.
#[test]
fn TestTouchIfVelocity() {
    let t = T::new("TestTouchIfVelocity");
    for (w, limit) in [(1_000u64, 5u64), (5_000, 20)] {
        let f = must(Config { capacity: 200_000, horizon: MINUTE, resolution: ms(1), ..Config::default() });
        let mut own: Vec<Vec<u64>> = vec![Vec::new(); 2000];
        let mut ex: Vec<Vec<u64>> = vec![Vec::new(); 2000];
        let in_win = |ts: &[u64], now: u64| -> u64 { ts.iter().rev().take_while(|&&x| x + w >= now).count() as u64 };
        let (mut reqs, mut agree, mut admit_over, mut refused_under, mut refused_under_upper, mut both_admit, mut ex_admit) =
            (0i64, 0i64, 0i64, 0i64, 0i64, 0i64, 0i64);
        vel_workload(
            |k, tk| {
                reqs += 1;
                let h = mix64(k as u64 + (1 << 50));
                let truth = in_win(&own[k], tk);
                let (r, ok) = f.touch_if_under_at(h, w, limit, tk);
                if ok {
                    if truth >= limit {
                        admit_over += 1;
                    }
                    own[k].push(tk);
                } else if truth < limit {
                    refused_under += 1;
                    if r.upper as u64 > truth || r.saturated {
                        refused_under_upper += 1;
                    }
                }
                let eok = in_win(&ex[k], tk) < limit;
                if eok {
                    ex[k].push(tk);
                    ex_admit += 1;
                }
                if eok == ok {
                    agree += 1;
                    if ok {
                        both_admit += 1;
                    }
                }
            },
            |_, _, _, _| {},
        );
        logf!(
            t,
            "velocity W={}s limit {limit}: requests {reqs}, agreement with exact {:.4} (exact admits {ex_admit}, both {both_admit}); admitted at/over own limit {admit_over}; refused under own limit {refused_under}, of which Upper > truth {refused_under_upper}",
            w / 1000,
            agree as f64 / reqs as f64
        );
        if admit_over != 0 || refused_under_upper != refused_under {
            errorf!(
                t,
                "velocity W={}s limit {limit}: {admit_over} admitted at or over the limit; {} refused under it without Upper > truth",
                w / 1000,
                refused_under - refused_under_upper
            );
        }
    }
}

/// TestTouchIfUnderLostHistory: without an overflow table a dropped history
/// is lost, and an unseen key reads Degraded while its events may still be in
/// a window (TestLostAtNewestEvent): Upper may then be below the truth, so
/// TouchIfUnderAt refuses such a key; once the lost events are past a horizon
/// it admits again.
#[test]
fn TestTouchIfUnderLostHistory() {
    let t = T::new("TestTouchIfUnderLostHistory");
    let f = must(Config { capacity: 64, horizon: Duration::from_secs(1), resolution: ms(1), overflow: -1, ..Config::default() });
    let h = mix64(1);
    let (fp, tg) = (fingerprint(h), tag(h));
    let (w1, _) = f.next_reg(0, tg, 1000, 1000, true);
    f.spill(f.stash_id(h, fp, tg), (fp << 48) | 1000, w1, 1000);
    let (r, ok) = f.touch_if_under_at(h, 1000, 5, 1500);
    if ok || !r.degraded || r.seen {
        errorf!(t, "an unseen key read Degraded after a lost history: {}, admitted {ok}; want refused", show(&r));
    }
    let (r, ok) = f.touch_if_under_at(h, 1000, 5, 2001);
    if !ok {
        errorf!(t, "past a horizon after the lost event: {}, refused", show(&r));
    }
}
