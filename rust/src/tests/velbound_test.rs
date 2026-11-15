//! velbound_test.go.

use std::cell::RefCell;

use super::check::*;
use super::helpers::*;
use crate::hot::View;
use crate::register::{N_BUCKETS, bucket_start};
use crate::slots::*;
use crate::{Config, Filter};

/// TestVelocityBound (TF_EXP=1) splits the velocity error (tfbench's vel MAE)
/// into what the register's information allows and what the estimator adds.
/// It replays tfbench's velocity workload (cmd/tfbench/accuracy.go,
/// velocityMAE: 2,000 keys in five traffic shapes, one query per ms for four
/// horizons) and scores, per window, sum|estimate - exact| / sum exact:
///
///   - filter: VelocityCountAt (must equal tfbench's figures);
///   - spread: estimate's interpolation as it was until eed54e6, every event
///     of the cut bucket spread uniformly;
///   - anchored: the newest bucket's last event, which lies exactly at the
///     slot's lastTouch, counts whole and only the other events are spread
///     (estimate until the onset, claims D29);
///   - onset: anchored, and the newest bucket's events spread from its onset
///     rather than its unit's start (what estimate does now, re-derived from
///     the key's register: it must equal filter);
///   - median: anchored, and the cut bucket's share is the median of
///     Binomial(events, window's share of the bucket), the best guess under
///     absolute error if events are uniform within a bucket;
///   - model floor: the expected |error| of that median under the same model,
///     i.e. what no estimator reading this register can avoid on Poisson
///     traffic;
///   - flat u: exact counts in aligned buckets of width u everywhere (no
///     counter or bucket limits), anchored, uniform in the cut bucket: what a
///     register with resolution u near now could reach.
#[test]
fn TestVelocityBound() {
    let t = T::new("TestVelocityBound");
    if !env_on("TF_EXP") {
        skip!(t, "set TF_EXP=1");
    }
    const CAPACITY: i64 = 200_000;
    let f = must(Config { capacity: CAPACITY, horizon: MINUTE, resolution: ms(1), ..Config::default() });
    // tfbench's velocity workload, same seed and order of draws (velWorkload).
    let hs: Vec<u64> = (0..2000u64).map(|k| mix64(k + (1 << 50))).collect();
    let wins = VEL_WINS;
    let flat = [2500u64, 1250, 625, 250, 100]; // H/24 (today's unit), H/48, H/96, H/240, H/600
    let mut names: Vec<String> = ["filter", "spread (until 10-22)", "anchored (until D29)", "onset (re-derived)", "median (anchored)", "model floor"]
        .map(String::from)
        .to_vec();
    for u in flat {
        names.push(format!("flat {}", go_duration((u * 1_000_000) as i64)));
    }
    let err_sum = RefCell::new(vec![[0.0f64; 5]; names.len()]);
    let exact_sum = RefCell::new([0.0f64; 5]);
    // estimate's own rounding: at least 1 once lastTouch is in the window
    let round = |est: f64, gated: bool| -> f64 { if !gated { 0.0 } else { 1.max((est + 0.5) as i64) as f64 } };
    vel_workload(
        |k, tk| {
            f.check_and_touch_at(hs[k], 0, tk);
        },
        |k, ts, wi, now| {
            let w = wins[wi];
            let exact = (ts.len() - ts.partition_point(|&x| x + w < now)) as f64;
            exact_sum.borrow_mut()[wi] += exact;
            let mut got: Vec<f64> = vec![f.velocity_count_at(hs[k], w, now) as f64];
            let (w0, w1, src) = f.read(hs[k], now, -1);
            let last = w0 & TS_MASK;
            let mut v = View::default();
            let mut ok = src != 0 && age(now, last) <= f.horizon;
            if ok {
                (v, ok) = f.view_of(f.stash_id(hs[k], fingerprint(hs[k]), tag(hs[k])), w1, last);
            }
            let inside = ok && age(now, last) <= w.min(f.horizon);
            for mode in 0..4 {
                // spread, anchored, onset, median
                let (mut est, mut mad) = (0.0, 0.0);
                if inside {
                    (est, mad) = reg_estimate(&f, &v, last, now, w, mode >= 1, mode == 3, mode == 2);
                }
                got.push(round(est, inside));
                if mode == 3 {
                    got.push(mad); // the floor is an expected |error|, scored as is
                }
            }
            for u in flat {
                got.push(ideal_estimate(ts, u, now, w));
            }
            let mut err_sum = err_sum.borrow_mut();
            for (e, &g) in got.iter().enumerate() {
                if names[e] == "model floor" {
                    err_sum[e][wi] += g;
                    continue;
                }
                err_sum[e][wi] += (g - exact).abs();
            }
        },
    );
    let (err_sum, exact_sum) = (err_sum.into_inner(), exact_sum.into_inner());
    logf!(t, "normalized MAE (sum|est-exact| / sum exact), tfbench's velocity workload");
    logf!(t, "{:<22} {:>7} {:>7} {:>7} {:>7} {:>7} {:>7}", "estimator", "1s", "5s", "15s", "30s", "60s", "mean");
    for (e, name) in names.iter().enumerate() {
        let (mut line, mut mean) = (String::new(), 0.0);
        for wi in 0..wins.len() {
            let x = err_sum[e][wi] / exact_sum[wi];
            mean += x / wins.len() as f64;
            line += &format!(" {}", pad7(x));
        }
        logf!(t, "{name:<22}{line} {}", pad7(mean));
    }
    let (a, b) = (err_sum[0], err_sum[3]);
    if a != b {
        let v = |x: &[f64]| gov(&x.iter().map(|&y| go_g(y)).collect::<Vec<_>>());
        errorf!(t, "re-derived estimate differs from the filter's: {} vs {}", v(&b), v(&a));
    }
}

/// regEstimate is estimate's interpolation (register.rs) for a register view,
/// with three options: anchor (the newest bucket's last event, at lastTouch,
/// counts whole), median (the cut bucket's share is the median of
/// Binomial(events, share) instead of its mean) and onset (the newest
/// bucket's events lie from its onset, as estimate reads it). mad is the
/// expected |error| of that median under the uniform model.
#[allow(clippy::too_many_arguments)]
fn reg_estimate(f: &Filter, v: &View, last: u64, now: u64, window: u64, anchor: bool, median: bool, onset: bool) -> (f64, f64) {
    let window = window.min(f.horizon);
    let e = v.e;
    let unit = f.unit as f64;
    let from = now as f64 - window as f64;
    let mut top = (e + 1) as f64 * unit;
    let mut newest = true;
    let (mut est, mut mad) = (0.0f64, 0.0f64);
    for j in 0..N_BUCKETS {
        let lo = bucket_start(e, j) as f64 * unit;
        let c = v.c[j];
        let mut hi = top;
        top = lo;
        if c == 0 {
            if from >= lo {
                break;
            }
            continue;
        }
        let mut a = 0.0;
        if newest {
            hi = lo.max((last as f64).min(hi));
            newest = false;
            if anchor && from < hi {
                a = 1.0;
            }
        }
        if from <= lo {
            est += c as f64;
            continue;
        }
        if onset && j == 0 && v.on != 0 && c as f64 > a {
            let on = lo + v.on as f64 * unit / 16.0;
            if from <= on {
                est += c as f64;
            } else if hi > on {
                est += a + (c as f64 - a) * 0.0f64.max(1.0f64.min((hi - from) / (hi - on)));
            } else {
                est += a;
            }
            break;
        }
        if from < hi {
            let p = (hi - from) / (hi - lo);
            if median {
                let (m, d) = binom_median(c as i64 - a as i64, p);
                est += a + m as f64;
                mad += d;
            } else {
                est += a + (c as f64 - a) * p;
            }
        }
        break;
    }
    (est, mad)
}

/// binomMedian returns a median of Binomial(n, p) and E|X - median|.
fn binom_median(n: i64, p: f64) -> (i64, f64) {
    if n <= 0 || p <= 0.0 {
        return (0, 0.0);
    }
    if p >= 1.0 {
        return (n, 0.0);
    }
    let ln = lgamma((n + 1) as f64);
    let (lp, lq) = (p.ln(), (-p).ln_1p());
    let pmf: Vec<f64> = (0..=n)
        .map(|k| {
            let a = lgamma((k + 1) as f64);
            let b = lgamma((n - k + 1) as f64);
            (ln - a - b + k as f64 * lp + (n - k) as f64 * lq).exp()
        })
        .collect();
    let (mut m, mut cum) = (n, 0.0f64);
    for (k, &q) in pmf.iter().enumerate() {
        cum += q;
        if cum >= 0.5 {
            m = k as i64;
            break;
        }
    }
    let mut d = 0.0f64;
    for (k, &q) in pmf.iter().enumerate() {
        d += q * ((k as i64 - m) as f64).abs();
    }
    (m, d)
}

/// idealEstimate counts h's events (ts, sorted) in [now-w, now] from exact
/// counts in aligned buckets of width u: whole buckets exactly, the bucket
/// the window's edge cuts uniformly, with the newest event counted whole when
/// that bucket is the newest. Gated and rounded as estimate is.
fn ideal_estimate(ts: &[u64], u: u64, now: u64, w: u64) -> f64 {
    let Some(&last) = ts.last() else {
        return 0.0;
    };
    if now - last > w {
        return 0.0;
    }
    let from = now - w.min(now);
    let bs = from / u * u;
    let be = bs + u;
    let after = ts.len() - ts.partition_point(|&x| x < be);
    let n_b = ts.len() - after - ts.partition_point(|&x| x < bs);
    let mut est = after as f64;
    if n_b > 0 {
        let (mut hi, mut a) = (be as f64, 0.0);
        if last < be {
            (hi, a) = (last as f64, 1.0);
        }
        if hi > bs as f64 {
            est += a + (n_b as f64 - a) * (hi - from as f64) / (hi - bs as f64);
        } else {
            est += n_b as f64;
        }
    }
    1.max((est + 0.5) as i64) as f64
}
