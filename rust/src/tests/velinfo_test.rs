//! velinfo_test.go (velWorkload and velWins are in helpers.rs).

use std::cell::RefCell;

use super::check::*;
use super::helpers::*;
use crate::Config;
use crate::slots::*;

/// geom is an aligned register geometry: unit in ticks, and per bucket
/// (newest first) its alignment step 2^logStep and offset, in units, as
/// register.rs's bucketLogStep and bucketOff.
#[derive(Clone)]
struct Geom {
    unit: u64,
    log_step: Vec<u32>,
    off: Vec<i64>,
}

/// simBucket is one bucket of a model's state: [lo, hi) in ticks, c events.
#[derive(Clone, Copy, Default)]
struct SimBucket {
    lo: f64,
    hi: f64,
    c: i64,
}

impl Geom {
    fn new(unit: u64, log_step: &[u32], off: &[i64]) -> Geom {
        Geom { unit, log_step: log_step.to_vec(), off: off.to_vec() }
    }

    fn start(&self, u: i64, j: usize) -> i64 {
        (u & !((1i64 << self.log_step[j]) - 1)) - self.off[j]
    }

    /// buckets lays ts (events up to last) into g's buckets at the epoch of
    /// last's unit, as the register holds them (no counter limits). A touch
    /// more than a horizon after the one before starts the register afresh
    /// (bump's reset of a stale slot), so only events since the last such gap
    /// count.
    fn buckets(&self, mut ts: &[u64]) -> Vec<SimBucket> {
        let mut i = ts.len() - 1;
        while i > 0 {
            if ts[i] - ts[i - 1] > 60_000 {
                ts = &ts[i..];
                break;
            }
            i -= 1;
        }
        let last = ts[ts.len() - 1];
        let e = (last / self.unit) as i64;
        let mut out = vec![SimBucket::default(); self.log_step.len()];
        let mut top = (e + 1) as f64 * self.unit as f64;
        for j in 0..out.len() {
            let lo = self.start(e, j) as f64 * self.unit as f64;
            out[j] = SimBucket { lo, hi: top, c: 0 };
            top = lo;
        }
        for &t in ts {
            for b in out.iter_mut() {
                if t as f64 >= b.lo {
                    b.c += 1;
                    break;
                }
            }
        }
        out
    }
}

/// simEstimate is register.rs's estimate over a bucket list (newest first):
/// the newest non-empty bucket ends at last, which holds one event counted
/// whole; the cut bucket is spread uniformly. known lists exact event times
/// (newest first) taken out of their buckets' counts and counted exactly;
/// onsets[j] >= 0 is a lower bound on bucket j's events (its first one lies
/// in [onsets[j], onsets[j]+onStep)).
fn sim_estimate(bs: &[SimBucket], last: u64, now: u64, w: u64, known: &[u64], onsets: Option<&[f64]>, _on_step: f64) -> f64 {
    if now - last > w {
        return 0.0;
    }
    let from = now as f64 - w as f64;
    let mut est = 0.0f64;
    for &t in known {
        if t as f64 >= from {
            est += 1.0;
        }
    }
    let mut newest = true;
    for (j, b) in bs.iter().enumerate() {
        let mut c = b.c as f64;
        let (lo, mut hi) = (b.lo, b.hi);
        let mut nk = 0.0f64;
        for &t in known {
            if t as f64 >= lo && (t as f64) < hi {
                nk += 1.0;
            }
        }
        c -= nk;
        if b.c == 0 {
            if from >= lo {
                break;
            }
            continue;
        }
        let mut a = 0.0f64;
        if newest {
            newest = false;
            hi = lo.max((last as f64).min(hi));
            if nk == 0.0 && from < hi {
                a = 1.0; // the event at last, counted whole
            }
        }
        if let Some(&oldest) = known.last() {
            hi = hi.min(oldest as f64); // older events lie before the oldest known one
            if hi < lo {
                hi = lo;
            }
        }
        if let Some(onsets) = onsets {
            if c - a > 0.0 && onsets[j] >= 0.0 {
                // one event in [on, on+step), the others in [on, hi]
                let on = lo.max(onsets[j]);
                if from <= on {
                    est += c;
                    if from <= lo {
                        continue;
                    }
                    break;
                }
                let rest = c - a;
                let mut p = 0.0f64;
                if hi > on {
                    p = 0.0f64.max(1.0f64.min((hi - from) / (hi - on)));
                }
                est += a + rest * p;
                if from > lo {
                    break;
                }
                continue;
            }
        }
        if from <= lo {
            est += c;
            continue;
        }
        if from < hi {
            est += a + (c - a) * (hi - from) / (hi - lo);
        }
        break;
    }
    est
}

fn sim_round(est: f64, inside: bool) -> f64 {
    if !inside {
        return 0.0;
    }
    1.max((est + 0.5) as i64) as f64
}

type Model = Box<dyn Fn(&[u64], u64, u64) -> f64>;

fn onset(g: &Geom, bits: u32, all: bool) -> Model {
    let g = g.clone();
    Box::new(move |ts, now, w| {
        let last = ts[ts.len() - 1];
        let bs = g.buckets(ts);
        let mut on = vec![0.0f64; bs.len()];
        let step = g.unit as f64 / (1i64 << bits) as f64;
        for j in 0..on.len() {
            on[j] = -1.0;
            if j > 0 && !all {
                continue;
            }
            for &x in ts {
                // first event in bucket j
                if x as f64 >= bs[j].lo && (x as f64) < bs[j].hi {
                    let width = bs[j].hi - bs[j].lo;
                    let s = step * width / g.unit as f64;
                    if bits >= 30 {
                        on[j] = x as f64;
                    } else {
                        on[j] = bs[j].lo + ((x as f64 - bs[j].lo) / s).floor() * s;
                    }
                    break;
                }
            }
        }
        let s = if bits >= 30 { 0.0 } else { step };
        sim_round(sim_estimate(&bs, last, now, w, &[], Some(&on), s), now - last <= w)
    })
}

fn last_m(g: &Geom, m: usize) -> Model {
    let g = g.clone();
    Box::new(move |ts, now, w| {
        let last = ts[ts.len() - 1];
        let known: Vec<u64> = ts.iter().rev().take(m).copied().collect();
        sim_round(sim_estimate(&g.buckets(ts), last, now, w, &known, None, 0.0), now - last <= w)
    })
}

fn plain(g: &Geom) -> Model {
    let g = g.clone();
    Box::new(move |ts, now, w| {
        let last = ts[ts.len() - 1];
        sim_round(sim_estimate(&g.buckets(ts), last, now, w, &[], None, 0.0), now - last <= w)
    })
}

/// TestVelocityInformation (TF_EXP=1) asks what information about a key's
/// recent events target 7 (velocity error <= 0.05, tfbench's vel MAE) needs,
/// beyond TestVelocityBound's model floor. It replays tfbench's velocity
/// workload and scores models built from each key's exact event times: the
/// register as laid out (no counter limits; the filter until claims D29), the
/// register plus side information a few more bits could carry (the offset of
/// each bucket's first event, the exact times of the key's last m events;
/// with the newest bucket's offset in 4 bits it must equal the filter, which
/// keeps it in the bits its register's unit took before D29), and wider
/// geometries. Each model is estimate's interpolation (register.rs) with the
/// side information used where it applies. Then the filter's error by traffic
/// shape, as a share of each window's total.
#[test]
fn TestVelocityInformation() {
    let t = T::new("TestVelocityInformation");
    if !env_on("TF_EXP") {
        skip!(t, "set TF_EXP=1");
    }
    let f = must(Config { capacity: 200_000, horizon: MINUTE, resolution: ms(1), ..Config::default() });
    let cur = Geom::new(2500, &[0, 0, 1, 2, 3, 3, 3], &[0, 1, 2, 4, 8, 16, 24]);
    // H/48 near now, 8 buckets to cover the horizon (sized as register.rs
    // sizes its own, at 15 events per 1.25 s: 4, 4, 5, 6, 7, 8, 8, 8 = 50
    // bits); and H/24 with two buckets per width (5, 5, 5, 6, 6, 7, 7, 8, 8 =
    // 57 bits).
    let h48 = Geom::new(1250, &[0, 0, 1, 2, 3, 4, 4, 4], &[0, 1, 2, 4, 8, 16, 32, 48]);
    let h24x9 = Geom::new(2500, &[0, 0, 0, 1, 1, 2, 2, 3, 3], &[0, 1, 2, 3, 5, 7, 11, 15, 23]);
    let models: Vec<(&str, Model)> = vec![
        ("register", plain(&cur)),
        ("+onset newest 2b", onset(&cur, 2, false)),
        ("+onset newest 4b", onset(&cur, 4, false)),
        ("+onset each 3b", onset(&cur, 3, true)),
        ("+onset each exact", onset(&cur, 30, true)),
        ("+last 2 exact", last_m(&cur, 2)),
        ("+last 4 exact", last_m(&cur, 4)),
        ("+last 8 exact", last_m(&cur, 8)),
        ("H/48, 8 buckets", plain(&h48)),
        ("H/24, 9 buckets", plain(&h24x9)),
    ];
    const NSHAPES: usize = 5;
    let nm = models.len() + 1;
    let err_sum = RefCell::new(vec![[[0.0f64; 5]; NSHAPES]; nm]);
    let exact_sum = RefCell::new([[0.0f64; 5]; NSHAPES]);
    let hs: Vec<u64> = (0..2000u64).map(|k| mix64(k + (1 << 50))).collect();
    vel_workload(
        |k, tk| {
            f.check_and_touch_at(hs[k], 0, tk);
        },
        |k, ts, wi, now| {
            let w = VEL_WINS[wi];
            let exact = (ts.len() - ts.partition_point(|&x| x + w < now)) as f64;
            let sh = k % 5;
            exact_sum.borrow_mut()[sh][wi] += exact;
            let mut err_sum = err_sum.borrow_mut();
            err_sum[0][sh][wi] += (f.velocity_count_at(hs[k], w, now) as f64 - exact).abs();
            if ts.is_empty() {
                return;
            }
            for (m, (_, est)) in models.iter().enumerate() {
                err_sum[m + 1][sh][wi] += (est(ts, now, w) - exact).abs();
            }
        },
    );
    let (err_sum, exact_sum) = (err_sum.into_inner(), exact_sum.into_inner());
    let mut names = vec!["filter"];
    names.extend(models.iter().map(|(n, _)| *n));
    logf!(t, "normalized MAE (sum|est-exact| / sum exact), tfbench's velocity workload");
    logf!(t, "{:<20} {:>7} {:>7} {:>7} {:>7} {:>7} {:>7}", "model", "1s", "5s", "15s", "30s", "60s", "mean");
    for (m, name) in names.iter().enumerate() {
        let (mut line, mut mean) = (String::new(), 0.0);
        for wi in 0..5 {
            let (mut e, mut x) = (0.0, 0.0);
            for sh in 0..NSHAPES {
                e += err_sum[m][sh][wi];
                x += exact_sum[sh][wi];
            }
            mean += e / x / 5.0;
            line += &format!(" {}", pad7(e / x));
        }
        logf!(t, "{name:<20}{line} {}", pad7(mean));
    }
    logf!(t, "the filter's error by traffic shape, share of each window's total");
    let shapes = ["steady", "bursts", "ramp", "sparse", "on/off"];
    for sh in 0..NSHAPES {
        let mut line = String::new();
        for wi in 0..5 {
            let mut tot = 0.0;
            for s in 0..NSHAPES {
                tot += err_sum[0][s][wi];
            }
            line += &format!(" {}", pad7(err_sum[0][sh][wi] / tot));
        }
        logf!(t, "{:<20}{line}", shapes[sh]);
    }
    for wi in 0..5 {
        // the filter is the register with its newest bucket's 4-bit onset (claims D29)
        for sh in 0..NSHAPES {
            if err_sum[0][sh][wi] != err_sum[3][sh][wi] {
                errorf!(
                    t,
                    "the register model with its onset differs from the filter: shape {sh} window {wi}: {} vs {}",
                    go_g(err_sum[3][sh][wi]),
                    go_g(err_sum[0][sh][wi])
                );
            }
        }
    }
}
