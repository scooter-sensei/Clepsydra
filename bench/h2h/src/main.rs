//! Head-to-head runner for the industry arms of part 1
//! (docs/artifact/head-to-head.md): Cloudflare's `pingora-limits` `Rate`, a
//! count-min estimator, and an exact sliding-window log per key in
//! `DashMap`. It replays the traces `h2h gen` writes (go/cmd/h2h/trace.go)
//! and prints one JSON line per pass, as go/cmd/h2h does for Clepsydra.
//!
//! h2h --arm pingora|dashmap --trace F --pass acc|t1|t12|lat1|lat12
//!     [--round R] [--threads T] [--answers A] [--slots S]

use dashmap::DashMap;
use pingora_limits::rate::Rate;
use sha2::{Digest, Sha256};
use std::alloc::{GlobalAlloc, Layout, System};
use std::collections::VecDeque;
use std::hint::black_box;
use std::sync::atomic::{compiler_fence, AtomicBool, AtomicIsize, Ordering};
use std::sync::Barrier;
use std::time::{Duration, Instant};

/// Live heap bytes, counted only while COUNTING is on (the accuracy pass);
/// the other passes pay one relaxed load per allocation.
struct Counting;
static COUNTING: AtomicBool = AtomicBool::new(false);
static LIVE: AtomicIsize = AtomicIsize::new(0);

unsafe impl GlobalAlloc for Counting {
    unsafe fn alloc(&self, l: Layout) -> *mut u8 {
        let p = System.alloc(l);
        if !p.is_null() && COUNTING.load(Ordering::Relaxed) {
            LIVE.fetch_add(l.size() as isize, Ordering::Relaxed);
        }
        p
    }
    unsafe fn alloc_zeroed(&self, l: Layout) -> *mut u8 {
        let p = System.alloc_zeroed(l);
        if !p.is_null() && COUNTING.load(Ordering::Relaxed) {
            LIVE.fetch_add(l.size() as isize, Ordering::Relaxed);
        }
        p
    }
    unsafe fn dealloc(&self, p: *mut u8, l: Layout) {
        if COUNTING.load(Ordering::Relaxed) {
            LIVE.fetch_sub(l.size() as isize, Ordering::Relaxed);
        }
        System.dealloc(p, l)
    }
    unsafe fn realloc(&self, p: *mut u8, l: Layout, new: usize) -> *mut u8 {
        let q = System.realloc(p, l, new);
        if !q.is_null() && COUNTING.load(Ordering::Relaxed) {
            LIVE.fetch_add(new as isize - l.size() as isize, Ordering::Relaxed);
        }
        q
    }
}

#[global_allocator]
static ALLOC: Counting = Counting;

// ---- traces (the layout of go/cmd/h2h/trace.go) ----

#[derive(Clone, Copy)]
struct Rec {
    key: u64,
    tick: u32,
    count: u32,
}

struct Probe {
    key: u64,
}

struct Trace {
    horizon_ms: u64,
    window_ms: u64,
    limit: u64,
    mult: u64,
    weight: u64,
    probe_tick: u64,
    ops: u64,
    recs: Vec<Rec>,
    probes: Vec<Probe>,
    sha: String,
}

fn u64_at(b: &[u8], i: usize) -> u64 {
    u64::from_le_bytes(b[i..i + 8].try_into().unwrap())
}
fn u32_at(b: &[u8], i: usize) -> u32 {
    u32::from_le_bytes(b[i..i + 4].try_into().unwrap())
}

fn read_trace(path: &str) -> Trace {
    let data = std::fs::read(path).expect("read trace");
    assert!(
        data.len() >= 128 && &data[..8] == b"CLPH2H01",
        "{path}: not a trace"
    );
    let sum = Sha256::digest(&data);
    let sha: String = sum[..8].iter().map(|b| format!("{b:02x}")).collect();
    let h: Vec<u64> = (0..12).map(|i| u64_at(&data, 8 + 8 * i)).collect();
    let (records, nprobes) = (h[7] as usize, h[8] as usize);
    assert_eq!(data.len(), 128 + 16 * (records + nprobes), "{path}: size");
    let mut off = 128;
    let mut recs = Vec::with_capacity(records);
    for _ in 0..records {
        recs.push(Rec {
            key: u64_at(&data, off),
            tick: u32_at(&data, off + 8),
            count: u32_at(&data, off + 12),
        });
        off += 16;
    }
    let mut probes = Vec::with_capacity(nprobes);
    for _ in 0..nprobes {
        probes.push(Probe {
            key: u64_at(&data, off),
        });
        off += 16;
    }
    Trace {
        horizon_ms: h[2],
        window_ms: h[3],
        limit: h[4],
        mult: h[5],
        weight: h[6],
        probe_tick: h[9],
        ops: h[10],
        recs,
        probes,
        sha,
    }
}

fn flood_name(t: &Trace) -> String {
    if t.weight == 1 {
        format!("light{}x", t.mult)
    } else {
        format!("heavy{}x", t.mult)
    }
}

/// The thread a key's records go to: go/cmd/h2h's `part`.
fn part(key: u64, threads: usize) -> usize {
    ((key.wrapping_mul(0x9e3779b97f4a7c15) >> 32) % threads as u64) as usize
}

// ---- arms ----

trait Arm: Sync {
    /// One request: check and record, as a limiter calls it.
    fn op(&self, key: u64, tick: u32);
    /// A read-only probe: (estimate, upper bound, strict decision).
    fn answer(&self, key: u64, tick: u32, limit: i64) -> (i32, i32, bool);
}

/// Cloudflare's count-min estimator, used as Pingora's rate-limiter example
/// uses it: `observe(&key, 1)` per request, interval = the limit's window.
struct Pingora {
    rate: Rate,
}

impl Arm for Pingora {
    fn op(&self, key: u64, _tick: u32) {
        black_box(self.rate.observe(&key, 1));
    }
    fn answer(&self, key: u64, _tick: u32, limit: i64) -> (i32, i32, bool) {
        let (prev, curr, frac) = self.rate.rate_with(&key, |c| {
            (c.prev_samples, c.curr_samples, c.current_interval_fraction)
        });
        // Cloudflare's proportional estimate, and both intervals whole for the strict decision.
        let est = (prev as f64 * (1.0 - frac) + curr as f64).round() as i32;
        let whole = (prev + curr) as i64;
        (est, whole as i32, whole >= limit)
    }
}

/// The exact arm: each key's event times (ms) within the horizon, in a
/// DashMap (default shards and hasher).
struct Exact {
    map: DashMap<u64, VecDeque<u32>>,
    horizon: u32,
    window: u32,
}

fn in_window(log: &VecDeque<u32>, now: u32, window: u32) -> usize {
    log.len() - log.partition_point(|&t| now - t > window)
}

impl Arm for Exact {
    fn op(&self, key: u64, tick: u32) {
        let mut e = self.map.entry(key).or_default();
        let log = e.value_mut();
        log.push_back(tick);
        while let Some(&first) = log.front() {
            if tick - first > self.horizon {
                log.pop_front();
            } else {
                break;
            }
        }
        black_box(in_window(log, tick, self.window));
    }
    fn answer(&self, key: u64, tick: u32, limit: i64) -> (i32, i32, bool) {
        let c = self
            .map
            .get(&key)
            .map_or(0, |log| in_window(&log, tick, self.window)) as i32;
        (c, c, c as i64 >= limit)
    }
}

// ---- timing (go/cmd/h2h/timing.go) ----

const SUB_BITS: u32 = 6;
const SUB: u64 = 1 << SUB_BITS;
const NBINS: usize = (2 * SUB + (63 - SUB_BITS as u64) * SUB) as usize;

struct Hist(Box<[u64; NBINS]>);

impl Hist {
    fn new() -> Self {
        Hist(Box::new([0; NBINS]))
    }
    fn add(&mut self, v: u64) {
        let b = if v < 2 * SUB {
            v as usize
        } else {
            let e = 63 - v.leading_zeros();
            (2 * SUB + (e - SUB_BITS - 1) as u64 * SUB + (v >> (e - SUB_BITS)) - SUB) as usize
        };
        self.0[b] += 1;
    }
    fn merge(&mut self, o: &Hist) {
        for (a, b) in self.0.iter_mut().zip(o.0.iter()) {
            *a += b;
        }
    }
    fn top(b: usize) -> u64 {
        let b = b as u64;
        if b < 2 * SUB {
            return b;
        }
        let e = (b - 2 * SUB) / SUB + SUB_BITS as u64 + 1;
        let m = (b - 2 * SUB) % SUB + SUB;
        ((m + 1) << (e - SUB_BITS as u64)) - 1
    }
    fn quantile(&self, q: f64, tpn: f64) -> f64 {
        let n: u64 = self.0.iter().sum();
        let rank = ((q * n as f64).ceil() as u64).max(1);
        let mut cum = 0;
        for (b, &c) in self.0.iter().enumerate() {
            cum += c;
            if cum >= rank {
                return Self::top(b) as f64 / tpn;
            }
        }
        f64::NAN
    }
}

#[inline(always)]
fn rdtscp() -> u64 {
    let mut aux = 0u32;
    unsafe { core::arch::x86_64::__rdtscp(&mut aux) }
}

fn tsc_per_ns() -> f64 {
    let (t0, c0) = (Instant::now(), rdtscp());
    while t0.elapsed() < Duration::from_millis(200) {}
    let (c1, t1) = (rdtscp(), Instant::now());
    (c1 - c0) as f64 / (t1 - t0).as_nanos() as f64
}

fn timer_floor(n: usize) -> Hist {
    let mut h = Hist::new();
    for _ in 0..n {
        let t0 = rdtscp();
        compiler_fence(Ordering::SeqCst);
        let t1 = rdtscp();
        h.add(t1 - t0);
    }
    h
}

// ---- passes ----

fn replay<A: Arm>(a: &A, recs: &[Rec]) {
    for r in recs {
        for _ in 0..r.count {
            a.op(r.key, r.tick);
        }
    }
}

fn replay_timed<A: Arm>(a: &A, recs: &[Rec], h: &mut Hist) {
    for r in recs {
        for _ in 0..r.count {
            let t0 = rdtscp();
            compiler_fence(Ordering::SeqCst);
            a.op(r.key, r.tick);
            compiler_fence(Ordering::SeqCst);
            let t1 = rdtscp();
            h.add(t1 - t0);
        }
    }
}

fn split(recs: &[Rec], threads: usize) -> Vec<Vec<Rec>> {
    let mut parts = vec![Vec::new(); threads];
    for r in recs {
        parts[part(r.key, threads)].push(*r);
    }
    parts
}

/// Runs f on each part from one start signal; returns the time from the
/// signal to the last thread's end.
fn parallel<F: Fn(usize, &[Rec]) + Sync>(parts: &[Vec<Rec>], f: F) -> Duration {
    let barrier = Barrier::new(parts.len() + 1);
    std::thread::scope(|s| {
        let handles: Vec<_> = parts
            .iter()
            .enumerate()
            .map(|(i, p)| {
                let (barrier, f) = (&barrier, &f);
                s.spawn(move || {
                    barrier.wait();
                    f(i, p);
                    Instant::now()
                })
            })
            .collect();
        barrier.wait();
        let t0 = Instant::now();
        let last = handles
            .into_iter()
            .map(|h| h.join().unwrap())
            .max()
            .unwrap();
        last.saturating_duration_since(t0)
    })
}

struct Out {
    pass: String,
    threads: usize,
    seconds: f64,
    lat: Option<(f64, f64, f64, f64, f64)>,
    bytes: i64,
    note: String,
}

fn run<A: Arm, M: Fn() -> A>(
    make: M,
    t: &Trace,
    pass: &str,
    threads: usize,
    answers: Option<&str>,
) -> Out {
    let mut o = Out {
        pass: pass.to_string(),
        threads: 1,
        seconds: 0.0,
        lat: None,
        bytes: 0,
        note: String::new(),
    };
    match pass {
        "acc" => {
            let mut buf: Vec<u8> = Vec::with_capacity(12 * t.probes.len());
            COUNTING.store(true, Ordering::SeqCst);
            let a = make();
            let t0 = Instant::now();
            replay(&a, &t.recs);
            o.seconds = t0.elapsed().as_secs_f64();
            o.bytes = LIVE.load(Ordering::SeqCst) as i64;
            COUNTING.store(false, Ordering::SeqCst);
            for p in &t.probes {
                let (est, up, deny) = a.answer(p.key, t.probe_tick as u32, t.limit as i64);
                buf.extend_from_slice(&est.to_le_bytes());
                buf.extend_from_slice(&up.to_le_bytes());
                buf.extend_from_slice(&[deny as u8, 0, 0, 0]);
            }
            if let Some(path) = answers {
                std::fs::write(path, &buf).expect("write answers");
            }
        }
        "t1" => {
            let a = make();
            let t0 = Instant::now();
            replay(&a, &t.recs);
            o.seconds = t0.elapsed().as_secs_f64();
        }
        "t12" => {
            let parts = split(&t.recs, threads);
            let a = make();
            o.threads = threads;
            o.seconds = parallel(&parts, |_, recs| replay(&a, recs)).as_secs_f64();
        }
        "lat1" | "lat12" => {
            let tpn = tsc_per_ns();
            let mut h = Hist::new();
            if pass == "lat1" {
                let a = make();
                let t0 = Instant::now();
                replay_timed(&a, &t.recs, &mut h);
                o.seconds = t0.elapsed().as_secs_f64();
            } else {
                let parts = split(&t.recs, threads);
                let hs: Vec<std::sync::Mutex<Hist>> = (0..threads)
                    .map(|_| std::sync::Mutex::new(Hist::new()))
                    .collect();
                let a = make();
                o.threads = threads;
                o.seconds = parallel(&parts, |i, recs| {
                    let mut local = Hist::new();
                    replay_timed(&a, recs, &mut local);
                    *hs[i].lock().unwrap() = local;
                })
                .as_secs_f64();
                for m in &hs {
                    h.merge(&m.lock().unwrap());
                }
            }
            let floor = timer_floor(1_000_000);
            o.lat = Some((
                h.quantile(0.5, tpn),
                h.quantile(0.99, tpn),
                h.quantile(0.999, tpn),
                h.quantile(1.0, tpn),
                floor.quantile(0.999, tpn),
            ));
        }
        _ => panic!("unknown pass {pass}"),
    }
    o
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let flag = |name: &str| {
        args.iter()
            .position(|a| a == name)
            .map(|i| args[i + 1].clone())
    };
    let arm = flag("--arm").expect("--arm");
    let trace = flag("--trace").expect("--trace");
    let pass = flag("--pass").unwrap_or_else(|| "acc".into());
    let round: usize = flag("--round").map_or(0, |s| s.parse().unwrap());
    let threads: usize = flag("--threads").map_or(12, |s| s.parse().unwrap());
    let answers = flag("--answers");
    // The name printed: the arm, or a label for a variant (the sizing runs).
    let label = flag("--label").unwrap_or_else(|| arm.clone());
    let t = read_trace(&trace);
    let started = Instant::now();
    let mut o = match arm.as_str() {
        "pingora" => {
            let slots: usize = flag("--slots").expect("--slots").parse().unwrap();
            let window = Duration::from_millis(t.window_ms);
            run(
                || Pingora {
                    rate: Rate::new_with_estimator_config(window, 4, slots),
                },
                &t,
                &pass,
                threads,
                answers.as_deref(),
            )
        }
        "dashmap" => {
            let (horizon, window) = (t.horizon_ms as u32, t.window_ms as u32);
            run(
                || Exact {
                    map: DashMap::new(),
                    horizon,
                    window,
                },
                &t,
                &pass,
                threads,
                answers.as_deref(),
            )
        }
        _ => panic!("unknown arm {arm}"),
    };
    // pingora-limits reads the wall clock: a pass that outlasts its interval
    // would have rotated the window mid-experiment (head-to-head.md, Passes).
    let void = arm == "pingora" && started.elapsed() >= Duration::from_millis(t.window_ms);
    if void {
        o.note = "void: the pass outlasted the interval".into();
    }
    let mops = t.ops as f64 / o.seconds / 1e6;
    let mut line = format!(
        r#"{{"arm":"{label}","flood":"{}","pass":"{}","round":{round},"threads":{},"trace":"{}","ops":{},"seconds":{:.6},"mops":{:.4}"#,
        flood_name(&t),
        o.pass,
        o.threads,
        t.sha,
        t.ops,
        o.seconds,
        mops
    );
    if let Some((p50, p99, p999, max, floor)) = o.lat {
        line += &format!(
            r#","p50_ns":{p50:.1},"p99_ns":{p99:.1},"p999_ns":{p999:.1},"max_ns":{max:.1},"timer_p999_ns":{floor:.1}"#
        );
    }
    if o.bytes != 0 {
        line += &format!(r#","bytes":{}"#, o.bytes);
    }
    if !o.note.is_empty() {
        line += &format!(r#","note":"{}""#, o.note);
    }
    line += "}";
    println!("{line}");
    if void {
        std::process::exit(3);
    }
}
