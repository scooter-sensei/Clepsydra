//! The perf tier's benchmarks: the twelve of go/test.sh's PERF_BENCH
//! (go/bench_test.go), run as Go's testing.B runs them: each benchmark
//! function is called again for every N, its setup included and its timer
//! reset after it, N grows by Go's predictN until a run takes -benchtime
//! (default 1s), and RunParallel hands out iterations in Go's grains. Output
//! lines read as go test -bench prints them ("BenchmarkName-<threads>  N  x
//! ns/op"), so verify scripts that parse one parse the other (bench/ab.sh).
//!
//! ```text
//! cargo bench --bench bench -- [-bench PATTERN] [-count N] [-benchtime SECONDS]
//! ```
//!
//! PATTERN is the part of a regular expression the tiers use: `.` (every
//! benchmark), a plain substring, or `^Benchmark(A|B|...)$` (std has no
//! regex engine; see `matches`).

use std::hint::black_box;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use clepsydra::{Config, Filter};

/// sink keeps the reads' answers alive.
static SINK: AtomicU64 = AtomicU64::new(0);

/// testing.B: N, the timer, and RunParallel.
struct B {
    n: i64,
    start: Instant,
    duration: Duration,
    timer_on: bool,
    previous_n: i64,
    previous_duration: Duration,
    extra: String, // ReportMetric's "x unit" fields
}

/// testing.PB: next hands out the grain of iterations each worker takes from
/// the shared count at a time.
struct Pb<'a> {
    global_n: &'a AtomicU64,
    grain: u64,
    cache: u64,
    bn: u64,
}

impl Pb<'_> {
    fn next(&mut self) -> bool {
        if self.cache == 0 {
            let n = self.global_n.fetch_add(self.grain, Ordering::SeqCst) + self.grain;
            if n <= self.bn {
                self.cache = self.grain;
            } else if n < self.bn + self.grain {
                self.cache = self.bn + self.grain - n;
            } else {
                return false;
            }
        }
        self.cache -= 1;
        true
    }
}

impl B {
    fn new() -> B {
        B {
            n: 1,
            start: Instant::now(),
            duration: Duration::ZERO,
            timer_on: false,
            previous_n: 0,
            previous_duration: Duration::ZERO,
            extra: String::new(),
        }
    }

    fn reset_timer(&mut self) {
        if self.timer_on {
            self.start = Instant::now();
        }
        self.duration = Duration::ZERO;
    }

    fn start_timer(&mut self) {
        if !self.timer_on {
            self.start = Instant::now();
            self.timer_on = true;
        }
    }

    fn stop_timer(&mut self) {
        if self.timer_on {
            self.duration += self.start.elapsed();
            self.timer_on = false;
        }
    }

    fn report_metric(&mut self, v: f64, unit: &str) {
        self.extra += &format!("\t{} {unit}", go_g4(v));
    }

    /// RunParallel: body runs on GOMAXPROCS threads, which share b.N's
    /// iterations in grains of about 100 µs (at most 1e4).
    fn run_parallel(&mut self, body: impl Fn(&mut Pb) + Sync) {
        let mut grain = 0u64;
        if self.previous_n > 0 && !self.previous_duration.is_zero() {
            grain = (1e5 * self.previous_n as f64 / self.previous_duration.as_nanos() as f64) as u64;
        }
        grain = grain.clamp(1, 10_000);
        let n = AtomicU64::new(0);
        let bn = self.n as u64;
        std::thread::scope(|s| {
            for _ in 0..procs() {
                let (n, body) = (&n, &body);
                s.spawn(move || body(&mut Pb { global_n: n, grain, cache: 0, bn }));
            }
        });
    }
}

/// `%10.4g` of Go's fmt, as testing.B prints a reported metric.
fn go_g4(v: f64) -> String {
    let s = if v == 0.0 {
        "0".to_string()
    } else {
        let exp = v.abs().log10().floor() as i32;
        if !(-4..4).contains(&exp) {
            let m = format!("{v:.3e}");
            let (mant, e) = m.split_once('e').unwrap_or((&m, "0"));
            let e: i32 = e.parse().unwrap_or(0);
            let mant = mant.trim_end_matches('0').trim_end_matches('.');
            format!("{mant}e{}{:02}", if e < 0 { '-' } else { '+' }, e.abs())
        } else {
            let prec = (3 - exp).max(0) as usize;
            let t = format!("{v:.prec$}");
            if t.contains('.') { t.trim_end_matches('0').trim_end_matches('.').to_string() } else { t }
        }
    };
    format!("{s:>10}")
}

fn procs() -> usize {
    std::thread::available_parallelism().map(|n| n.get()).unwrap_or(1)
}

/// mix64 is the SplitMix64 finalizer (the reference's benchmarks' keys).
fn mix64(mut x: u64) -> u64 {
    x ^= x >> 30;
    x = x.wrapping_mul(0xbf58476d1ce4e5b9);
    x ^= x >> 27;
    x = x.wrapping_mul(0x94d049bb133111eb);
    x ^ (x >> 31)
}

const BENCH_KEYS: i64 = 1 << 20; // 1M live keys: table is 32 MiB, far beyond L2/L3

fn cfg(capacity: i64) -> Config {
    Config { capacity, horizon: Duration::from_secs(60), ..Config::default() }
}

fn new_filter(capacity: i64) -> Filter {
    Filter::new(cfg(capacity)).expect("New")
}

/// benchFilter: 1M keys, each touched once at 1000.
fn bench_filter() -> (Filter, Vec<u64>) {
    let f = new_filter(BENCH_KEYS);
    let hs: Vec<u64> = (0..BENCH_KEYS as u64).map(mix64).collect();
    for &h in &hs {
        f.check_and_touch_at(h, 0, 1000);
    }
    (f, hs)
}

// ---- the benchmarks (go/bench_test.go) ----

/// Hit path on a cold table: one random cache-line miss per op.
fn check_and_touch_hit(b: &mut B) {
    let (f, hs) = bench_filter();
    let w = f.ticks(Duration::from_secs(30));
    b.reset_timer();
    for i in 0..b.n {
        f.check_and_touch_at(hs[(i & (BENCH_KEYS - 1)) as usize], w, 2000 + (i >> 10) as u64);
    }
}

fn check_and_touch_miss_insert(b: &mut B) {
    let (f, _) = bench_filter();
    let w = f.ticks(Duration::from_secs(30));
    b.reset_timer();
    for i in 0..b.n {
        f.check_and_touch_at(mix64((BENCH_KEYS + i) as u64), w, 2000 + (i >> 10) as u64);
    }
}

fn touch_and_count(b: &mut B) {
    let (f, hs) = bench_filter();
    let w = f.ticks(Duration::from_secs(30));
    b.reset_timer();
    for i in 0..b.n {
        black_box(f.touch_and_count_at(hs[(i & (BENCH_KEYS - 1)) as usize], w, 2000 + (i >> 10) as u64));
    }
}

fn velocity_count(b: &mut B) {
    let (f, hs) = bench_filter();
    let w = f.ticks(Duration::from_secs(30));
    b.reset_timer();
    let mut s = 0u64;
    for i in 0..b.n {
        s = s.wrapping_add(f.velocity_count_at(hs[(i & (BENCH_KEYS - 1)) as usize], w, 2000) as u64);
    }
    SINK.fetch_add(s, Ordering::Relaxed);
}

fn check_and_touch_parallel(b: &mut B) {
    let (f, hs) = bench_filter();
    let w = f.ticks(Duration::from_secs(30));
    let seed = AtomicU64::new(0);
    b.reset_timer();
    b.run_parallel(|pb| {
        let mut i = (seed.fetch_add(1, Ordering::SeqCst) + 1) * 7919;
        while pb.next() {
            i += 1;
            f.check_and_touch_at(hs[(i & (BENCH_KEYS as u64 - 1)) as usize], w, 2000 + (i >> 10));
        }
    });
}

/// Hit path with a cache-resident working set (4K keys).
fn check_and_touch_hit_hot(b: &mut B) {
    const N: i64 = 1 << 12;
    let f = new_filter(N);
    let hs: Vec<u64> = (0..N as u64).map(mix64).collect();
    for &h in &hs {
        f.check_and_touch_at(h, 0, 1000);
    }
    let w = f.ticks(Duration::from_secs(30));
    b.reset_timer();
    for i in 0..b.n {
        f.check_and_touch_at(hs[(i & (N - 1)) as usize], w, 2000 + (i >> 10) as u64);
    }
}

/// Worst case: every insert lands in two full lines (no expiry).
fn insert_saturated(b: &mut B) {
    let f = new_filter(BENCH_KEYS / 4); // 512K slots
    for i in 0..BENCH_KEYS as u64 {
        f.check_and_touch_at(mix64(i), 0, 1000);
    }
    let w = f.ticks(Duration::from_secs(30));
    b.reset_timer();
    for i in 0..b.n {
        f.check_and_touch_at(mix64((1 << 40) + i as u64), w, 1000);
    }
}

fn check_and_touch_batch(b: &mut B) {
    let (f, hs) = bench_filter();
    let w = f.ticks(Duration::from_secs(30));
    let mut out = [false; 64];
    b.reset_timer();
    let mut i = 0;
    while i < b.n {
        let lo = (i & (BENCH_KEYS - 1)) as usize;
        f.check_and_touch_batch(&hs[lo..lo + 64], w, 2000 + (i >> 10) as u64, &mut out);
        i += 64;
    }
}

fn insert_near_full(b: &mut B) {
    const N: i64 = 1 << 18;
    let f = new_filter(N);
    let live = (N * 115 / 100) as u64; // ~98% of slots
    let per = f.ticks(Duration::from_secs(60)) / live;
    let mut ctr = 0u64;
    while ctr < live {
        f.check_and_touch_at(mix64(ctr), 0, 1 + ctr * per);
        ctr += 1;
    }
    b.reset_timer();
    for _ in 0..b.n {
        ctr += 1;
        f.check_and_touch_at(mix64(ctr), 0, 1 + ctr * per);
    }
    let e = f.evictions() as f64 / b.n as f64;
    b.report_metric(e, "evict/op");
}

/// VelocityCount with the table cache-resident: the estimator's CPU cost.
fn velocity_count_hot(b: &mut B) {
    const N: i64 = 1 << 12;
    let f = new_filter(N);
    let hs: Vec<u64> = (0..N as u64).map(mix64).collect();
    for &h in &hs {
        for k in 0..5u64 {
            f.check_and_touch_at(h, 0, 1000 + k * 3_000_000);
        }
    }
    let w = f.ticks(Duration::from_secs(30));
    b.reset_timer();
    let mut s = 0u64;
    for i in 0..b.n {
        s = s.wrapping_add(f.velocity_count_at(hs[(i & (N - 1)) as usize], w, 20_000_000) as u64);
    }
    SINK.fetch_add(s, Ordering::Relaxed);
}

fn contains_absent(b: &mut B) {
    let (f, _) = bench_filter();
    let w = f.ticks(Duration::from_secs(30));
    b.reset_timer();
    let mut s = 0u64;
    for i in 0..b.n {
        s += f.contains_at(mix64((1 << 40) + i as u64), w, 2000) as u64;
    }
    SINK.fetch_add(s, Ordering::Relaxed);
}

/// Steady-state insert: time advances so keys expire as fast as they arrive,
/// holding ~1.0x capacity live (the scorecard's insert column).
fn insert_steady(b: &mut B) {
    let (f, _) = bench_filter();
    let w = f.ticks(Duration::from_secs(30));
    let per_key = f.ticks(Duration::from_secs(60)) / BENCH_KEYS as u64;
    let mut ctr = 0u64;
    for _ in 0..2 * BENCH_KEYS {
        // until the initial keys have expired
        ctr += 1;
        f.check_and_touch_at(mix64(BENCH_KEYS as u64 + ctr), w, 1000 + ctr * per_key);
    }
    b.reset_timer();
    for _ in 0..b.n {
        ctr += 1;
        f.check_and_touch_at(mix64(BENCH_KEYS as u64 + ctr), w, 1000 + ctr * per_key);
    }
}

type BenchFn = fn(&mut B);

const BENCHMARKS: [(&str, BenchFn); 12] = [
    ("CheckAndTouchHit", check_and_touch_hit),
    ("CheckAndTouchMissInsert", check_and_touch_miss_insert),
    ("TouchAndCount", touch_and_count),
    ("VelocityCount", velocity_count),
    ("CheckAndTouchParallel", check_and_touch_parallel),
    ("CheckAndTouchHitHot", check_and_touch_hit_hot),
    ("InsertSaturated", insert_saturated),
    ("CheckAndTouchBatch", check_and_touch_batch),
    ("InsertNearFull", insert_near_full),
    ("VelocityCountHot", velocity_count_hot),
    ("ContainsAbsent", contains_absent),
    ("InsertSteady", insert_steady),
];

/// runN calls the benchmark once with b.N = n, timing what follows its
/// ResetTimer.
fn run_n(b: &mut B, f: BenchFn, n: i64) {
    b.n = n;
    b.extra.clear();
    b.duration = Duration::ZERO;
    b.timer_on = false;
    b.start_timer();
    f(b);
    b.stop_timer();
    b.previous_n = n;
    b.previous_duration = b.duration;
}

/// predictN is Go's: the iterations that would take goal at the last rate,
/// plus a fifth, growing at most 100x and at least by one, under 1e9.
fn predict_n(goal_ns: i64, prev_iters: i64, mut prev_ns: i64, last: i64) -> i64 {
    if prev_ns <= 0 {
        prev_ns = 1;
    }
    let mut n = goal_ns.wrapping_mul(prev_iters) / prev_ns; // as Go: int64 (goal ~1e9 x at most 1e9 iterations)
    n += n / 5;
    n = n.min(100 * last);
    n = n.max(last + 1);
    n.min(1_000_000_000)
}

/// matches reports whether name matches the pattern: `.` or empty (every
/// name), `^Benchmark(A|B|C)$` and its parts (anchors, one group of
/// alternatives), or else a plain substring.
fn matches(pattern: &str, name: &str) -> bool {
    if pattern.is_empty() || pattern == "." {
        return true;
    }
    let (start, p) = match pattern.strip_prefix('^') {
        Some(p) => (true, p),
        None => (false, pattern),
    };
    let (end, p) = match p.strip_suffix('$') {
        Some(p) => (true, p),
        None => (false, p),
    };
    let alts: Vec<String> = match (p.find('('), p.rfind(')')) {
        (Some(i), Some(j)) if i < j => p[i + 1..j].split('|').map(|a| format!("{}{a}{}", &p[..i], &p[j + 1..])).collect(),
        _ => vec![p.to_string()],
    };
    alts.iter().any(|a| match (start, end) {
        (true, true) => name == a,
        (true, false) => name.starts_with(a.as_str()),
        (false, true) => name.ends_with(a.as_str()),
        (false, false) => name.contains(a.as_str()),
    })
}

fn main() {
    let mut pattern = String::from(".");
    let mut count = 1;
    let mut benchtime = Duration::from_secs(1);
    let mut args = std::env::args().skip(1);
    while let Some(a) = args.next() {
        match a.as_str() {
            "-bench" => pattern = args.next().unwrap_or_default(),
            "-count" => count = args.next().and_then(|v| v.parse().ok()).unwrap_or(1).max(1),
            "-benchtime" => benchtime = Duration::from_secs_f64(args.next().and_then(|v| v.parse().ok()).unwrap_or(1.0)),
            "--bench" => {} // cargo bench's own flag
            _ => {
                eprintln!("usage: bench [-bench PATTERN] [-count N] [-benchtime SECONDS]");
                std::process::exit(2);
            }
        }
    }
    let procs = procs();
    for (name, f) in BENCHMARKS {
        let name = format!("Benchmark{name}");
        if !matches(&pattern, &name) {
            continue;
        }
        for _ in 0..count {
            let mut b = B::new();
            run_n(&mut b, f, 1);
            let mut n = 1i64;
            while b.duration < benchtime && n < 1_000_000_000 {
                let last = n;
                n = predict_n(benchtime.as_nanos() as i64, b.previous_n, b.duration.as_nanos() as i64, last);
                run_n(&mut b, f, n);
            }
            let ns = b.duration.as_nanos() as f64 / b.n as f64;
            println!("{name}-{procs}\t{:10}\t{ns:12.2} ns/op{}", b.n, b.extra);
        }
    }
    println!("sink {}", SINK.load(Ordering::Relaxed) & 1);
}
