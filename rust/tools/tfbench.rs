//! go/cmd/tfbench, for `-exact`: every deterministic scorecard metric as the
//! raw counts it is computed from (go/cmd/tfbench/accuracy.go). The workloads
//! use seeded PCG streams and the unkeyed mix64, and static FPR sums
//! per-worker counts, so the output is a pure function of the code: the
//! regression tier compares it with go/verify/expected/exact.txt byte for
//! byte. The speed scorecard is the perf tier's job (bench/bench.rs).
//!
//! Accuracy is measured against exact ground truth (a map of last-seen times)
//! on a churn workload: a stream mixing new keys and repeats, run for 4
//! horizons, where every op is CheckAndTouch with a random window.

use std::collections::HashMap;
use std::sync::atomic::{AtomicI64, Ordering};
use std::time::Duration;

use clepsydra::{Config, Filter};

#[allow(dead_code)]
#[path = "../src/tests/goexp.rs"]
mod goexp;
#[allow(dead_code)]
#[path = "../src/tests/gorand.rs"]
mod gorand;
#[allow(dead_code)]
#[path = "../src/tests/gosort.rs"]
mod gosort;

use gorand::Rand;

const CAPACITY: i64 = 200_000;
const HORIZON: Duration = Duration::from_secs(60);
const H_TICKS: u64 = 60_000; // ms
const FPR_PROBES: i64 = 1_000_000_000;
const SPRAY_OFFENDERS: usize = 100;

fn new(capacity: i64) -> Filter {
    Filter::new(Config { capacity, horizon: HORIZON, resolution: Duration::from_millis(1), ..Config::default() }).expect("New")
}

/// mix64 is the SplitMix64 finalizer, the package's former HashUint64:
/// unkeyed, so the workloads hash the same in every run and on every commit.
fn mix64(mut x: u64) -> u64 {
    x ^= x >> 30;
    x = x.wrapping_mul(0xbf58476d1ce4e5b9);
    x ^= x >> 27;
    x = x.wrapping_mul(0x94d049bb133111eb);
    x ^ (x >> 31)
}

/// Go's `%.<prec>e`: the exponent signed, with at least two digits.
fn go_e(x: f64, prec: usize) -> String {
    let s = format!("{x:.prec$e}");
    match s.split_once('e') {
        Some((m, e)) => {
            let e: i32 = e.parse().unwrap_or(0);
            format!("{m}e{}{:02}", if e < 0 { '-' } else { '+' }, e.abs())
        }
        None => s,
    }
}

/// workload.Churn (go/internal/workload): a stream mixing new keys (60%) and
/// repeats of keys born within the last 1.5 horizons, over 4 horizons of
/// hTicks each, with one window drawn from Windows per operation. load sets
/// the arrival rate so that distinct keys live per horizon ~ load x capacity.
/// The stream is a pure function of its arguments (seeded PCG).
fn workload_churn(load: f64, seed: u64, capacity: i64, h_ticks: u64, hash: impl Fn(u64) -> u64, mut op: impl FnMut(u64, u64, u64)) {
    const WINDOWS: [u64; 3] = [1_000, 10_000, 60_000];
    let mut r = Rand::new(seed, 1);
    const NEW_FRAC: f64 = 0.6;
    let rate_per_ms = load * capacity as f64 / (NEW_FRAC * h_ticks as f64) / 1.375; // calibrated: live keys per horizon ~ load x capacity
    let mut keys: Vec<(u64, u64)> = Vec::new(); // h, t
    let mut head = 0usize; // keys[head..] is the reference's keys slice
    let mut next = 0u64;
    for now in 1..4 * h_ticks {
        let mut n = rate_per_ms as i64;
        if r.float64() < rate_per_ms - n as f64 {
            n += 1;
        }
        for _ in 0..n {
            let h;
            let live = &keys[head..];
            if live.is_empty() || r.float64() < NEW_FRAC {
                next += 1;
                h = hash(next);
                keys.push((h, now));
            } else {
                // repeat a key born within the last 1.5 horizons
                let lo = live.partition_point(|&(_, t)| t + h_ticks * 3 / 2 <= now);
                h = live[lo + r.int_n((live.len() - lo) as i64) as usize].0;
            }
            op(h, WINDOWS[r.int_n(WINDOWS.len() as i64) as usize], now);
        }
        if now % h_ticks == 0 {
            // drop keys too old to be repeated
            while head < keys.len() && keys[head].1 + 2 * h_ticks < now {
                head += 1;
            }
        }
    }
}

#[derive(Default)]
struct ChurnResult {
    fnr: f64,
    fpr: f64,
    live: f64,
    pos: i64,
    fn_: i64,
    neg: i64,
    fp: i64,
    nlive: i64,
    evictions: u64,
    relocations: u64,
}

/// churn replays the churn stream through CheckAndTouch and scores every
/// answer after the first horizon against exact ground truth.
fn churn(load: f64, seed: u64) -> ChurnResult {
    let f = new(CAPACITY);
    let mut last: HashMap<u64, u64> = HashMap::new();
    let (mut pos, mut neg, mut fn_, mut fp) = (0i64, 0i64, 0i64, 0i64);
    let mut pruned = 0u64;
    workload_churn(load, seed, CAPACITY, H_TICKS, mix64, |h, w, now| {
        if now % H_TICKS == 0 && now != pruned {
            // keep ground truth bounded
            pruned = now;
            last.retain(|_, t| now - *t <= 2 * H_TICKS);
        }
        let got = f.check_and_touch_at(h, w, now);
        let want = last.get(&h).is_some_and(|&lt| now - lt <= w);
        last.insert(h, now);
        if now < H_TICKS {
            return; // warm-up
        }
        if want {
            pos += 1;
            if !got {
                fn_ += 1;
            }
        } else {
            neg += 1;
            if got {
                fp += 1;
            }
        }
    });
    let live = last.values().filter(|&&t| 4 * H_TICKS - t <= H_TICKS).count() as i64;
    ChurnResult {
        fnr: fn_ as f64 / pos.max(1) as f64,
        fpr: fp as f64 / neg.max(1) as f64,
        live: live as f64 / CAPACITY as f64,
        pos,
        fn_,
        neg,
        fp,
        nlive: live,
        evictions: f.evictions(),
        relocations: f.relocations(),
    }
}

/// velWin is one window's velocity error: sum |estimate-exact| and sum exact
/// over its queries (both integers).
struct VelWin {
    w: u64,
    err_sum: f64,
    exact: f64,
}

/// velocityMAE: 2000 keys with mixed traffic shapes (steady, bursty, ramp,
/// sparse, on/off) over 5 horizons; every ~1ms one random key is queried for
/// a random window. Returns the mean over windows of sum|estimate-exact| /
/// sum exact, so short windows weigh as much as long ones.
fn velocity_mae() -> (f64, Vec<VelWin>) {
    let f = new(CAPACITY);
    let mut r = Rand::new(9, 9);
    const NKEYS: usize = 2000;
    const END: u64 = 5 * H_TICKS;
    let endf = END as f64;
    let mut evs: Vec<(u64, usize)> = Vec::new(); // t, k
    let mut times: Vec<Vec<u64>> = vec![Vec::new(); NKEYS];
    fn poisson(r: &mut Rand, evs: &mut Vec<(u64, usize)>, t0: f64, t1: f64, rate: f64, k: usize) {
        let mut t = t0 + r.exp_float64() / rate;
        while t < t1 {
            evs.push((t as u64 + 1, k));
            t += r.exp_float64() / rate;
        }
    }
    for k in 0..NKEYS {
        match k % 5 {
            0 => {
                // steady 0.2-5/s
                let rate = (0.2 + r.float64() * 4.8) / 1000.0;
                poisson(&mut r, &mut evs, 0.0, endf, rate, k);
            }
            1 => {
                // bursts of 5-20 within 200ms, every ~10s
                let mut t = r.exp_float64() * 10_000.0;
                while t < endf {
                    for _ in 0..5 + r.int_n(16) {
                        evs.push(((t + r.float64() * 200.0) as u64 + 1, k));
                    }
                    t += r.exp_float64() * 10_000.0;
                }
            }
            2 => {
                // ramp 0.1 -> 10/s
                let mut t = 0.0f64;
                while t < endf {
                    t += r.exp_float64() / ((0.1 + 9.9 * t / endf) / 1000.0);
                    evs.push((t as u64 + 1, k));
                }
            }
            3 => poisson(&mut r, &mut evs, 0.0, endf, 1.0 / 20_000.0, k), // sparse, ~1 per 20s
            _ => {
                // 5s on at 4/s, 5s off
                let mut t = 0.0f64;
                while t < endf {
                    poisson(&mut r, &mut evs, t, t + 5000.0, 4.0 / 1000.0, k);
                    t += 10_000.0;
                }
            }
        }
    }
    gosort::slice(&mut evs, |a, b| a.0 < b.0);
    let hs: Vec<u64> = (0..NKEYS as u64).map(|k| mix64(k + (1 << 50))).collect();
    let wins = [1_000u64, 5_000, 15_000, 30_000, 60_000];
    let (mut err_sum, mut exact_sum) = ([0.0f64; 5], [0.0f64; 5]);
    let mut i = 0;
    for now in H_TICKS..END {
        while i < evs.len() && evs[i].0 <= now {
            f.check_and_touch_at(hs[evs[i].1], 0, evs[i].0);
            times[evs[i].1].push(evs[i].0);
            i += 1;
        }
        let (k, wi) = (r.int_n(NKEYS as i64) as usize, r.int_n(wins.len() as i64) as usize);
        let w = wins[wi];
        let ts = &times[k];
        let exact = (ts.len() - ts.partition_point(|&x| x + w < now)) as i64; // in [now-w, now], as the filter counts
        let got = f.velocity_count_at(hs[k], w, now);
        err_sum[wi] += ((got - exact) as f64).abs();
        exact_sum[wi] += exact as f64;
    }
    let mut mean = 0.0;
    let mut per = Vec::new();
    for wi in 0..wins.len() {
        let per_win = err_sum[wi] / exact_sum[wi];
        mean += per_win / wins.len() as f64;
        per.push(VelWin { w: wins[wi], err_sum: err_sum[wi], exact: exact_sum[wi] });
    }
    (mean, per)
}

/// staticFPR: table at capacity, probe never-inserted keys with read-only
/// Contains.
fn static_fpr(probes: i64) -> i64 {
    let f = new(CAPACITY);
    for i in 0..CAPACITY as u64 {
        f.check_and_touch_at(mix64(i), 0, 1);
    }
    // FPR is ~1e-8, so it takes ~1e9 probes to see more than a handful; split
    // them across cores.
    let fp = AtomicI64::new(0);
    let workers = std::thread::available_parallelism().map_or(1, |n| n.get()) as i64;
    std::thread::scope(|s| {
        for w in 0..workers {
            let (f, fp) = (&f, &fp);
            s.spawn(move || {
                let mut n = 0i64;
                let mut i = w;
                while i < probes {
                    if f.contains_at(mix64((CAPACITY + i) as u64), H_TICKS, 2) {
                        n += 1;
                    }
                    i += workers;
                }
                fp.fetch_add(n, Ordering::SeqCst);
            });
        }
    });
    fp.load(Ordering::SeqCst)
}

/// spray: 100 repeat offenders (20 hits each), then 20x capacity of one-off
/// keys inside the window. Returns the offenders still exact.
fn spray() -> i64 {
    let f = new(CAPACITY);
    let off: Vec<u64> = (0..SPRAY_OFFENDERS as u64).map(|i| mix64((1 << 40) + i)).collect();
    for &h in &off {
        for _ in 0..20 {
            f.check_and_touch_at(h, 0, 1);
        }
    }
    for i in 0..20 * CAPACITY as u64 {
        f.check_and_touch_at(mix64(i), 0, 1 + i % 30_000);
    }
    off.iter().filter(|&&h| f.velocity_count_at(h, H_TICKS, 30_001) == 20).count() as i64
}

fn bytes_per_key() -> f64 {
    let probe = Filter::new(Config { capacity: CAPACITY, horizon: HORIZON, ..Config::default() }).expect("New");
    probe.bytes() as f64 / CAPACITY as f64 // the table: stash, overflow and hot entries come on first use
}

/// burstCount returns the smallest and largest VelocityCount(window) for one
/// key right after n touches in a single tick, over 64 burst times spread
/// across the register's 8-unit bucket cycle (the window's edge cuts the
/// buckets differently at each).
fn burst_count(n: i64, window: u64) -> (i64, i64) {
    let unit = H_TICKS / 24;
    let (mut lo, mut hi) = (i64::MAX, 0i64);
    for k in 0..64u64 {
        let f = new(1000);
        let h = mix64(42);
        let now = 10 * H_TICKS + k * 8 * unit / 64;
        for _ in 0..n {
            f.check_and_touch_at(h, 0, now);
        }
        let c = f.velocity_count_at(h, window, now);
        (lo, hi) = (lo.min(c), hi.max(c));
    }
    (lo, hi)
}

/// steadyCount returns the smallest and largest ratio of
/// VelocityCount(window) to the exact count for one key touched every `every`
/// ticks for 3 horizons, queried at each touch in the last horizon.
fn steady_count(every: u64, window: u64) -> (f64, f64) {
    let f = new(1000);
    let h = mix64(43);
    let (mut lo, mut hi) = (f64::INFINITY, 0.0f64);
    let mut now = 1u64;
    while now < 3 * H_TICKS {
        f.check_and_touch_at(h, 0, now);
        if now >= 2 * H_TICKS {
            let exact = (window / every + 1) as f64; // touches in [now-window, now], as the filter counts
            let r = f.velocity_count_at(h, window, now) as f64 / exact;
            (lo, hi) = (lo.min(r), hi.max(r));
        }
        now += every;
    }
    (lo, hi)
}

/// poissonCI95 is the exact (Garwood) 95% interval for a Poisson mean given k
/// observed events: the means at which P(X >= k) and P(X <= k) are 2.5%.
fn poisson_ci95(k: i64) -> (f64, f64) {
    // P(X <= k)
    let cdf = |mu: f64, k: i64| -> f64 {
        let (mut term, mut sum) = ((-mu).exp(), 0.0);
        for j in 0..=k {
            sum += term;
            term *= mu / (j + 1) as f64;
        }
        sum
    };
    // f decreasing in mu, root in (0, 10k+10)
    let solve = |f: &dyn Fn(f64) -> f64| -> f64 {
        let (mut a, mut b) = (0.0f64, 10.0 * k as f64 + 10.0);
        for _ in 0..200 {
            let m = (a + b) / 2.0;
            if f(m) > 0.0 {
                a = m;
            } else {
                b = m;
            }
        }
        (a + b) / 2.0
    };
    let mut lo = 0.0;
    if k > 0 {
        lo = solve(&|mu| 0.025 - (1.0 - cdf(mu, k - 1)));
    }
    let hi = solve(&|mu| cdf(mu, k) - 0.025);
    (lo, hi)
}

/// printExact prints every deterministic scorecard metric as the raw counts
/// it is computed from.
fn print_exact() {
    for (load, seed) in [(1.0f64, 2u64), (1.1, 3), (1.15, 4)] {
        let r = churn(load, seed);
        println!(
            "churn load={load:.2} seed={seed} positives={} missed={} negatives={} false_seen={} live_at_end={} evictions={} relocations={}  => FN {:.3}% FP {} live/capacity {:.2}",
            r.pos,
            r.fn_,
            r.neg,
            r.fp,
            r.nlive,
            r.evictions,
            r.relocations,
            100.0 * r.fnr,
            go_e(r.fpr, 2),
            r.live
        );
    }
    let n = static_fpr(FPR_PROBES);
    let (lo, hi) = poisson_ci95(n);
    let probes = FPR_PROBES as f64;
    println!(
        "static_fpr probes={FPR_PROBES} false_positives={n}  => {} (95% interval {}-{})",
        go_e(n as f64 / probes, 2),
        go_e(lo / probes, 2),
        go_e(hi / probes, 2)
    );
    let ok = spray();
    println!("spray offenders={SPRAY_OFFENDERS} exact={ok}  => {:.0}%", 100.0 * ok as f64 / SPRAY_OFFENDERS as f64);
    let (mean, per) = velocity_mae();
    for v in &per {
        println!("velocity window={}s abs_error_sum={:.0} exact_sum={:.0}  => {:.4}", v.w / 1000, v.err_sum, v.exact, v.err_sum / v.exact);
    }
    println!("velocity mean_of_windows => {mean:.4}");
    for w in [1_000u64, 5_000, 15_000, 60_000] {
        for n in [10i64, 31, 62, 100, 500, 762, 1000, 1273, 2000] {
            let (lo, hi) = burst_count(n, w);
            println!("burst window={}s touches={n} reported_min={lo} reported_max={hi}", w / 1000);
        }
    }
    for w in [1_000u64, 5_000, 15_000, 60_000] {
        for per_sec in [1u64, 5, 10, 12, 13, 20, 50, 100] {
            let (lo, hi) = steady_count(1000 / per_sec, w);
            println!("steady window={}s rate={per_sec}/s reported/exact min={lo:.3} max={hi:.3}", w / 1000);
        }
    }
    let probe = Filter::new(Config { capacity: CAPACITY, horizon: HORIZON, ..Config::default() }).expect("New");
    println!("memory capacity={CAPACITY} slots={} bytes={}  => {:.1} B/key", probe.slots(), probe.bytes(), bytes_per_key());
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.iter().any(|a| a == "-exact" || a == "--exact") {
        print_exact();
        return;
    }
    eprintln!("usage: tfbench -exact\n  print the deterministic metrics as raw counts (no timing, nothing written)");
    eprintln!("the speed scorecard is the perf tier's: ./test.sh perf");
    std::process::exit(2);
}
