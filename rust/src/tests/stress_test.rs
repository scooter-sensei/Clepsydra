//! soak_test.go (TestStressSoak), fuzz_test.go (TestFuzzUpper),
//! fuzzconc_test.go (TestFuzzConcurrent): the stress tier's checks of the
//! fail-closed contract, each skipped unless its switch is set (TF_STRESS,
//! TF_FUZZ, TF_CFUZZ). The tag amplifier (tagamp_test.go) is TagAmp in
//! helpers.rs.

use std::collections::HashMap;
use std::sync::Mutex;
use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use super::check::*;
use super::gorand::Rand;
use super::helpers::*;
use crate::slots::*;
use crate::{Config, Filter, Result};

/// TestStressSoak runs every operation from many threads against one filter
/// for TF_STRESS (a duration, e.g. 30s; skipped when unset), cycling the load
/// from half the capacity to 4x it, and checks the fail-closed contract on
/// every Count: unless the answer is flagged Saturated, Upper is never below
/// the key's true count in the window. One exception is documented
/// (docs/paper/06-security.md): a key that left more than one history in an
/// overflow bucket's floor can read up to all but the largest of them low, so
/// a low answer whose bucket floor is live is counted, not failed. Each thread
/// owns its keys, so it knows their true counts exactly; the clock is shared
/// and moved by its own thread. TF_STRESS_TAGS=n amplifies tag coincidences
/// as the concurrent fuzzer's TF_CFUZZ_TAGS does (TagAmp; the soak uses fewer
/// than 20,000 keys).
#[test]
fn TestStressSoak() {
    let t = T::new("TestStressSoak");
    let Some(d) = parse_duration(&getenv("TF_STRESS")).filter(|d| !d.is_zero()) else {
        skip!(t, "set TF_STRESS=<duration> to run");
    };
    const CAPACITY: i64 = 4096;
    let workers = 4.max(2 * procs());
    let clock = TestClock::new(1);
    // Hot entries for every heavy key (8 per worker) and most of the pool's
    // keys that pass a register's 31 events in a unit, so their answers are
    // checked rather than Saturated (refused promotions are the fuzzer's
    // few-hot-entries cases and the attack tests'). 10 per worker left 63% of
    // answers Saturated, nearly all refused promotions; 80 leaves 24%, mostly
    // histories in the overflow table while the pool is 4-8x the table (30 s
    // soaks).
    let f = match Filter::new(Config {
        capacity: CAPACITY,
        horizon: secs(10),
        resolution: ms(1),
        clock: clock.func(),
        spill_min: 1,
        hot: 80 * workers as i64,
        ..Config::default()
    }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    const HORIZON: u64 = 10_000;
    let stop = AtomicBool::new(false);
    let (ops, queries, sat, low, floored) = (AtomicI64::new(0), AtomicI64::new(0), AtomicI64::new(0), AtomicI64::new(0), AtomicI64::new(0));
    let amp = TagAmp::from_env("TF_STRESS_TAGS");
    let first_low: Mutex<Option<String>> = Mutex::new(None);
    std::thread::scope(|s| {
        let t0 = Instant::now();
        {
            let (clock, stop) = (&clock, &stop);
            s.spawn(move || {
                // 1 ms of filter time per 20 µs of wall time: the horizon turns over every 0.2 s
                while !stop.load(SeqCst) {
                    clock.set(1 + (t0.elapsed().as_nanos() / 20_000) as u64);
                    std::thread::sleep(us(20));
                }
            });
        }
        for g in 0..workers as u64 {
            let (f, clock, stop, amp, first_low) = (&f, &clock, &stop, amp.as_ref(), &first_low);
            let (ops, queries, sat, low, floored) = (&ops, &queries, &sat, &low, &floored);
            s.spawn(move || {
                let mut rng = Rand::new(g, 1);
                // Owned keys: a few heavy ones that always come back, plus a
                // pool whose size cycles to push the table past capacity.
                let mut cache = HashMap::new();
                let mut heavy = [0u64; 8];
                for x in &mut heavy {
                    *x = amp_h(amp, rng.uint64(), Some(&mut cache));
                }
                let mut logs: HashMap<u64, Vec<u64>> = HashMap::new();
                let mut hs = [0u64; 16];
                let mut res = [false; 16];
                let mut cnt = [0i64; 16];
                let mut n = 0u64;
                while !stop.load(SeqCst) {
                    let unix_ms = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_millis()).unwrap_or(0);
                    let phase = (unix_ms / 500) % 4; // pool: 0.5x, 1x, 2x, 4x capacity in total
                    let pool = ((CAPACITY / 2) << phase) as u64 / workers as u64;
                    let h = if rng.int_n(32) == 0 {
                        heavy[rng.int_n(heavy.len() as i64) as usize]
                    } else {
                        amp_h(amp, mix64((g << 40) | rng.uint64_n(pool)), Some(&mut cache))
                    };
                    let now = clock.get();
                    let op = rng.int_n(10);
                    if op < 6 {
                        f.touch_at(h, 0, now);
                        logs.entry(h).or_default().push(now);
                    } else if op < 9 {
                        let w = 1 + rng.uint64_n(HORIZON);
                        let r = f.count_at(h, w, now);
                        // Other threads may have run at a later clock and
                        // rightly expired events that are older than the
                        // horizon by then; the clock read after the call
                        // bounds theirs.
                        let after = clock.get();
                        let l = logs.get(&h).map(Vec::as_slice).unwrap_or(&[]);
                        let exact = l.iter().filter(|&&ts| now - ts <= w && after - ts <= HORIZON).count() as i64;
                        queries.fetch_add(1, SeqCst);
                        if r.saturated {
                            sat.fetch_add(1, SeqCst);
                        } else if r.upper < exact && floor_live(f, h, w, now) {
                            floored.fetch_add(1, SeqCst);
                        } else if r.upper < exact && low.fetch_add(1, SeqCst) == 0 {
                            let again = f.count_at(h, w, now);
                            *first_low.lock().unwrap() = Some(format!(
                                "got {}; asked again: {}; window {w} ms: exact {exact} (now {now}, events {}); {}",
                                show(&r),
                                show(&again),
                                gov(l),
                                where_is(f, h, now)
                            ));
                        }
                    } else {
                        for x in &mut hs {
                            *x = heavy[rng.int_n(heavy.len() as i64) as usize];
                        }
                        f.check_and_touch_batch(&hs, HORIZON, now, &mut res);
                        for &x in &hs {
                            logs.entry(x).or_default().push(now);
                        }
                        f.velocity_count_batch(&hs, HORIZON, now, &mut cnt);
                    }
                    ops.fetch_add(1, SeqCst);
                    if n % 4096 == 0 {
                        // forget events older than the horizon
                        f.continuous_decay();
                        logs.retain(|_, l| {
                            let i = l.iter().take_while(|&&ts| now - ts > HORIZON).count();
                            l.drain(..i);
                            !l.is_empty()
                        });
                    }
                    n += 1;
                }
            });
        }
        std::thread::sleep(d);
        stop.store(true, SeqCst);
    });
    logf!(
        t,
        "{workers} workers, {}: {} ops, {} counts checked ({} Saturated, {} low behind a live bucket floor); evictions {}, restored {}, lost {}, promoted {}, hot refused {}",
        go_duration(d.as_nanos() as i64),
        ops.load(SeqCst),
        queries.load(SeqCst),
        sat.load(SeqCst),
        floored.load(SeqCst),
        f.evictions(),
        f.restored(),
        f.lost(),
        f.promoted(),
        f.hot_refused()
    );
    let n = low.load(SeqCst);
    if n > 0 {
        errorf!(t, "{n} unflagged answers with Upper below the true count; first: {}", first_low.lock().unwrap().clone().unwrap_or_default());
    }
    let occ = f.occupancy(clock.get());
    if occ > f.slots() {
        errorf!(t, "occupancy {occ} > {} slots", f.slots());
    }
}

/// TestFuzzUpper (TF_FUZZ=<operations>, e.g. 1e10) checks the fail-closed
/// contract over random configurations and operation streams: unless an
/// answer is Saturated, its Upper is never below the key's true count in the
/// window (docs/paper/04-operations.md, trust flags). It checks Count's
/// answers and Touch's own (which include the touch). The documented
/// exceptions are counted, not failed: a low answer behind a live overflow
/// floor (a key floored more than once), and, with the overflow table off, a
/// low answer within a horizon of a lost history (Lost counts them; the key
/// may have come back since, so the answer itself need not be Degraded).
/// Histories are never shed (SpillMin 1), as in the soak.
///
/// Each case is a fresh filter driven by one thread from a seeded stream, so
/// a failure replays exactly: TF_FUZZ_CASE=<seed> runs one case. Callers'
/// clocks lag the newest by up to a few units, as concurrent callers' do (an
/// eviction's clock can be behind the key's last touch). Cases run GOMAXPROCS
/// at a time from seed TF_FUZZ_SEED (default 1) until TF_FUZZ operations have
/// run. The concurrent soak (TestStressSoak) covers races; this covers
/// configurations, clocks and operation mixes.
#[test]
fn TestFuzzUpper() {
    let t = T::new("TestFuzzUpper");
    let one = getenv("TF_FUZZ_CASE");
    if !one.is_empty() {
        let seed: u64 = match one.parse() {
            Ok(s) => s,
            Err(e) => fatalf!(t, "{e}"),
        };
        let r = fuzz_case(seed);
        logf!(t, "case {seed}: {}", r.summary());
        if !r.fail.is_empty() {
            fatalf!(t, "{}", r.fail);
        }
        return;
    }
    let total: f64 = getenv("TF_FUZZ").parse().unwrap_or(0.0);
    if total.is_nan() || total <= 0.0 {
        skip!(t, "set TF_FUZZ=<operations> to run");
    }
    let mut first = 1u64;
    let s = getenv("TF_FUZZ_SEED");
    if !s.is_empty() {
        first = match s.parse() {
            Ok(s) => s,
            Err(e) => fatalf!(t, "{e}"),
        };
    }
    let (next, done) = (AtomicU64::new(first), AtomicU64::new(0));
    let sum: Mutex<(FuzzResult, Vec<String>)> = Mutex::default();
    let t0 = Instant::now();
    std::thread::scope(|s| {
        for _ in 0..procs() {
            s.spawn(|| {
                while done.load(SeqCst) < total as u64 {
                    let seed = next.fetch_add(1, SeqCst);
                    let r = fuzz_case(seed);
                    done.fetch_add(r.ops, SeqCst);
                    let mut g = sum.lock().unwrap();
                    g.0.add(&r);
                    if !r.fail.is_empty() {
                        g.1.push(format!("case {seed}: {}", r.fail));
                    }
                }
            });
        }
    });
    let el = t0.elapsed();
    let (sum, fails) = sum.into_inner().unwrap();
    logf!(
        t,
        "{} cases from seed {first} in {} ({:.1} M ops/s): {}",
        next.load(SeqCst) - first,
        go_duration(round_secs(el)),
        sum.ops as f64 / el.as_secs_f64() / 1e6,
        sum.summary()
    );
    for f in fails {
        errorf!(t, "{f}");
    }
}

/// `el.Round(time.Second)`, in nanoseconds.
fn round_secs(el: Duration) -> i64 {
    ((el.as_nanos() + 500_000_000) / 1_000_000_000 * 1_000_000_000) as i64
}

/// fuzzResult tallies one case (or several).
#[derive(Default, Clone)]
struct FuzzResult {
    ops: u64,
    checks: u64,
    sat: u64,
    floored: u64,
    lost_low: u64,
    shared: u64,
    // cases by configuration
    ct: u64,
    bg: u64,
    no_stash: u64,
    no_overflow: u64,
    small_overflow: u64,
    no_hot: u64,
    small_hot: u64,
    evictions: u64,
    folded: u64,
    promoted: u64,
    waited: u64,
    fail: String,
}

impl FuzzResult {
    fn add(&mut self, o: &FuzzResult) {
        self.ops += o.ops;
        self.checks += o.checks;
        self.sat += o.sat;
        self.floored += o.floored;
        self.lost_low += o.lost_low;
        self.shared += o.shared;
        self.ct += o.ct;
        self.bg += o.bg;
        self.no_stash += o.no_stash;
        self.no_overflow += o.no_overflow;
        self.small_overflow += o.small_overflow;
        self.no_hot += o.no_hot;
        self.small_hot += o.small_hot;
        self.evictions += o.evictions;
        self.folded += o.folded;
        self.promoted += o.promoted;
        self.waited += o.waited;
    }

    fn summary(&self) -> String {
        format!(
            "{} ops, {} answers checked ({} Saturated, {} low behind a live floor, {} low after a loss with the overflow off, {} off for a key whose identity another shares); \
             cases with ConstantTime {}, Background {}, no stash {}, no overflow {}, small overflow {}, no hot entries {}, few hot entries {}; \
             evictions {}, folded {}, promoted {}, waited {}",
            self.ops,
            self.checks,
            self.sat,
            self.floored,
            self.lost_low,
            self.shared,
            self.ct,
            self.bg,
            self.no_stash,
            self.no_overflow,
            self.small_overflow,
            self.no_hot,
            self.small_hot,
            self.evictions,
            self.folded,
            self.promoted,
            self.waited
        )
    }
}

/// The first index of l (sorted) holding x or more (`slices.BinarySearch`'s
/// position).
fn lower_bound(l: &[u64], x: u64) -> usize {
    l.partition_point(|&v| v < x)
}

/// fuzzTruth keeps every key's event times (sorted) within the horizon of the
/// newest clock any call has used, and keep ticks more (for the checks of
/// answers that read high, which reach behind a window).
#[derive(Default)]
struct FuzzTruth {
    ev: HashMap<u64, Vec<u64>>,
    horizon: u64,
    newest: u64,
    keep: u64,
}

impl FuzzTruth {
    fn events(&self, h: u64) -> &[u64] {
        self.ev.get(&h).map(Vec::as_slice).unwrap_or(&[])
    }

    /// since is the number of h's events at lo or later, later than any
    /// clock included (a lagging caller's window holds them).
    fn since(&self, h: u64, lo: u64) -> i64 {
        let l = self.events(h);
        (l.len() - lower_bound(l, lo)) as i64
    }

    fn add(&mut self, h: u64, ts: u64) {
        let l = self.ev.entry(h).or_default();
        l.push(ts);
        let mut i = l.len() - 1;
        while i > 0 && l[i - 1] > l[i] {
            // a lagging clock: keep the times sorted
            l.swap(i - 1, i);
            i -= 1;
        }
    }

    /// count is the true count of h's events in [now-window, now] that are
    /// within the horizon of the newest clock (a call at that clock may have
    /// expired older ones), window capped at the horizon as the filter caps
    /// it.
    fn count(&self, h: u64, window: u64, now: u64) -> i64 {
        let l = self.events(h);
        let mut lo = self.newest - self.newest.min(self.horizon);
        let w = window.min(self.horizon);
        if now - now.min(w) > lo {
            lo = now - w;
        }
        let (a, b) = (lower_bound(l, lo), lower_bound(l, now + 1));
        0.max(b as i64 - a as i64)
    }

    /// trim drops events past the horizon of the newest clock and keep, from
    /// every key.
    fn trim(&mut self) {
        let lo = self.newest - self.newest.min(self.horizon + self.keep);
        self.ev.retain(|_, l| {
            let i = lower_bound(l, lo);
            l.drain(..i);
            !l.is_empty()
        });
    }
}

/// tail: the last n of l.
fn tail(l: &[u64], n: usize) -> &[u64] {
    &l[l.len().saturating_sub(n)..]
}

/// fuzzConfig draws a case's configuration (horizon H in ticks of 1 ms) and
/// tallies it in res.
fn fuzz_config(rng: &mut Rand, res: &mut FuzzResult) -> (Config, i64, u64) {
    let mut capacity = 1i64 << (6 + rng.int_n(9)); // 64 .. 32K keys
    capacity += rng.int_n(capacity);
    const HORIZONS: [u64; 6] = [48, 96, 240, 1_000, 10_000, 60_000];
    let h = HORIZONS[rng.int_n(HORIZONS.len() as i64) as usize];
    let mut cfg = Config { capacity, horizon: ms(h), resolution: ms(1), spill_min: 1, ..Config::default() };
    if rng.int_n(4) == 0 {
        cfg.load = 0.5 + 0.5 * rng.float64();
    }
    match rng.int_n(8) {
        0 => (cfg.stash, res.no_stash) = (-1, 1),
        1 | 2 => cfg.stash = 1 + rng.int_n(64),
        _ => {}
    }
    match rng.int_n(8) {
        0 => (cfg.overflow, res.no_overflow) = (-1, 1),
        1 | 2 => (cfg.overflow, res.small_overflow) = (1 + rng.int_n(256), 1),
        _ => {}
    }
    match rng.int_n(8) {
        0 => (cfg.hot, res.no_hot) = (-1, 1),
        1 | 2 => (cfg.hot, res.small_hot) = (1 + rng.int_n(8), 1),
        _ => {}
    }
    (cfg, capacity, h)
}

/// What fuzzCase's checks of one answer need of the case.
struct FuzzCase<'a> {
    f: &'a Filter,
    overflow: i64, // cfg.Overflow
    horizon: u64,  // H
    reach: u64,
    desc: String,
}

impl FuzzCase<'_> {
    /// shared: another key of the case has h's identity (line, fingerprint
    /// and tag), so the filter can't tell their events apart.
    fn shared(&self, tr: &FuzzTruth, h: u64) -> bool {
        let f = self.f;
        let id = f.stash_id(h, fingerprint(h), tag(h));
        tr.ev.keys().any(|&k| k != h && f.stash_id(k, fingerprint(k), tag(k)) == id)
    }

    /// high checks the other direction, on what the register answers (the
    /// overflow table's part, which counts a dropped history's events whole
    /// and other keys' through floors and fingerprint matches, taken out):
    /// Upper counts no event from further back than the window reaches by
    /// reach, and Seen says whether the key has an event in the window
    /// (before this touch, for Touch). Events older than the newest clock's
    /// horizon may have been expired by a call at that clock, so they needn't
    /// be seen.
    #[expect(clippy::too_many_arguments, reason = "a helper of the Go test, parameter for parameter")]
    fn high(&self, tr: &FuzzTruth, res: &mut FuzzResult, op: &str, h: u64, window: u64, now: u64, r: &Result) -> bool {
        let (f, hz) = (self.f, self.horizon);
        let mut s = Result::default();
        if let Some(g) = f.gh.get() {
            s = g.estimate(f.stash_id(h, fingerprint(h), tag(h)), window.min(hz), now).0;
        }
        let own = if op == "Touch" { 1 } else { 0 }; // the model has this touch already
        let w = window.min(hz);
        let lo = now - now.min(w);
        let back = now - now.min(w + self.reach);
        let lo2 = lo.max(tr.newest - tr.newest.min(hz));
        let upper = r.upper - s.upper;
        let bad = if upper > tr.since(h, back) {
            format!("Upper {} (the overflow's {}) > {} events back to {back}", r.upper, s.upper, tr.since(h, back))
        } else if r.seen && !s.seen && tr.since(h, lo) <= own {
            format!("Seen with no event since {lo}")
        } else if !r.seen && tr.since(h, lo2) > own && !floor_live(f, h, window, now) && !(self.overflow < 0 && f.lost_recently(now)) {
            format!("not Seen with {} events since {lo2}", tr.since(h, lo2) - own)
        } else {
            return true;
        };
        if self.shared(tr, h) {
            res.shared += 1;
            return true;
        }
        res.fail = format!(
            "op {} {op}: {bad} (result {}); key {h:x} window {window} now {now} newest {}; events {}; {}; {}",
            res.ops,
            show(r),
            tr.newest,
            gov(tail(tr.events(h), 12)),
            where_is(f, h, now),
            self.desc
        );
        false
    }

    #[expect(clippy::too_many_arguments, reason = "a helper of the Go test, parameter for parameter")]
    fn check(&self, tr: &FuzzTruth, res: &mut FuzzResult, op: &str, h: u64, window: u64, now: u64, r: &Result) -> bool {
        res.checks += 1;
        if !self.high(tr, res, op, h, window, now, r) {
            return false;
        }
        if r.saturated {
            res.sat += 1;
            return true;
        }
        let exact = tr.count(h, window, now);
        if r.upper >= exact {
            return true;
        }
        let f = self.f;
        if floor_live(f, h, window, now) {
            res.floored += 1;
        } else if self.overflow < 0 && f.lost_recently(now) {
            res.lost_low += 1; // without the overflow table a dropped history is lost (Lost), held key or not
        } else {
            res.fail = format!(
                "op {} {op}: Upper {} < true {exact} (result {}); key {h:x} window {window} now {now} newest {}; events {}; {}; {}",
                res.ops,
                r.upper,
                show(r),
                tr.newest,
                gov(tail(tr.events(h), 12)),
                where_is(f, h, now),
                self.desc
            );
            return false;
        }
        true
    }
}

/// fuzzCase runs one case: a configuration and an operation stream drawn
/// from seed.
fn fuzz_case(seed: u64) -> FuzzResult {
    let mut res = FuzzResult::default();
    let mut rng = Rand::new(seed, 0x5eed_f022);
    let (mut cfg, capacity, hz) = fuzz_config(&mut rng, &mut res);
    let mut tt: u64 = 1 + rng.uint64_n(1 << 30); // the newest clock
    let clock = TestClock::new(tt);
    cfg.clock = clock.func();
    let mut f = match Filter::new(cfg.clone()) {
        Ok(f) => f,
        Err(e) => {
            res.fail = format!("New({}): {e}", cfg_desc(&cfg));
            return res;
        }
    };
    if rng.int_n(5) == 0 {
        f.tweak(|c| c.ct = true); // functional ConstantTime (no pad), as TF_CT=1 runs it
        res.ct = 1;
    }
    // Background cases come from their own stream, so every other case is
    // drawn as before. The placer runs at seeded points instead of in a
    // thread, so a case still replays exactly.
    let mut bg_rng = Rand::new(seed, 0xb6);
    let mut place_every = 0i64;
    if bg_rng.int_n(4) == 0 && !f.ct {
        f.tweak(|c| (c.bg, c.bg_manual) = (true, true));
        res.bg = 1;
        place_every = [1, 4, 32, 512][bg_rng.int_n(4) as usize];
    }
    fuzz_run(&f, &cfg, seed, capacity, hz, &mut tt, &clock, &mut rng, &mut bg_rng, place_every, &mut res);
    (res.evictions, res.folded, res.promoted, res.waited) = (f.evictions(), f.folded(), f.promoted(), f.waited());
    res
}

/// fuzzCase's operations, from the configuration on (it returns at the first
/// failure, as the reference's returns from fuzzCase).
#[expect(clippy::too_many_arguments, reason = "a helper of the Go test, parameter for parameter")]
fn fuzz_run(
    f: &Filter,
    cfg: &Config,
    _seed: u64,
    capacity: i64,
    hz: u64,
    tt: &mut u64,
    clock: &TestClock,
    rng: &mut Rand,
    bg_rng: &mut Rand,
    place_every: i64,
    res: &mut FuzzResult,
) {
    let ops = 1u64 << (16 + rng.int_n(7)); // 64K .. 4M
    let unit = f.unit;
    let max_lag = [0, 2, unit, 2 * unit][rng.int_n(4) as usize];
    // Operations per horizon, from idle to a flood of 64x the capacity.
    let per_h = capacity as f64 * [0.25, 1.0, 4.0, 16.0, 64.0][rng.int_n(5) as usize];
    let (tick, mut acc) = (hz as f64 / per_h, 0.0f64);
    let salt = rng.uint64();
    let key = |k: u64| mix64(salt ^ k);
    let heavy: Vec<u64> = (0..1 + rng.int_n(32) as u64).map(|i| key((1 << 62) | i)).collect();
    // An answer from a register (the table, the stash or the wait room)
    // counts a bucket the window's edge cuts whole: buckets are at most 8
    // units wide. A touch at a clock behind the register's unit counts in
    // that unit, so an event can sit up to a unit and maxLag later than it
    // happened. No answer counts an event further back than that.
    let reach = 9 * unit + max_lag + 1;
    let mut tr = FuzzTruth { ev: HashMap::new(), horizon: hz, newest: *tt, keep: reach + max_lag };
    const POOLS: [f64; 6] = [0.25, 0.5, 1.0, 2.0, 4.0, 8.0];
    let (mut pool, mut phase_left) = (capacity as u64, 0i64);
    let mut trim_at = *tt + hz;
    let mut hs = [0u64; 16];
    let mut out = [false; 16];
    let mut cnt = [0i64; 16];
    let c = FuzzCase {
        f,
        overflow: cfg.overflow,
        horizon: hz,
        reach,
        desc: format!(
            "config {} ct {} bg {} place 1/{place_every} horizon {hz} ticks unit {unit} lag <= {max_lag} ops/horizon {per_h:.0}",
            cfg_desc(cfg),
            f.ct,
            f.bg
        ),
    };
    // TF_FUZZ_TRACE=<key in hex> prints every operation on that key with
    // where its history is afterwards (for replaying a failing case).
    let trace_key = u64::from_str_radix(&getenv("TF_FUZZ_TRACE"), 16).unwrap_or(0);
    let mut trace_in = false;
    let trace = |tr: &FuzzTruth, ops: u64, tt: u64, op: &str, h: u64, now: u64, detail: &dyn Fn() -> String| {
        if trace_key != 0 && h == trace_key {
            eprintln!("op {ops} {op} now {now} (T {tt}) {}: {}; true {}", detail(), where_is(f, h, now), gov(tail(tr.events(h), 8)));
        }
    };
    while res.ops < ops {
        if phase_left == 0 {
            // the pool of live keys cycles across the table's capacity
            pool = 1.max((POOLS[rng.int_n(POOLS.len() as i64) as usize] * capacity as f64) as u64);
            phase_left = 1 + rng.int_n(4 * capacity);
        }
        phase_left -= 1;
        acc += tick * 2.0 * rng.float64(); // the clock moves on, perH operations per horizon on average
        if acc >= 1.0 {
            *tt += acc as u64;
            acc -= (acc as u64) as f64;
        }
        if rng.float64() * per_h < 0.1 {
            // about once per 10 horizons, a jump of up to 3 horizons
            *tt += 1 + rng.uint64_n(3 * hz);
        }
        clock.set(*tt);
        let mut now = *tt;
        if max_lag > 0 && rng.int_n(4) == 0 {
            now -= rng.uint64_n(max_lag + 1);
        }
        tr.newest = tr.newest.max(now);
        let h = if rng.int_n(16) == 0 { heavy[rng.int_n(heavy.len() as i64) as usize] } else { key(rng.uint64_n(pool)) };
        let window = 1 + rng.uint64_n(hz + hz / 4); // windows past the horizon too
        let op = rng.int_n(100);
        if op < 40 {
            tr.add(h, now);
            let r = f.touch_at(h, window, now);
            trace(&tr, res.ops, *tt, "Touch", h, now, &|| gov_result(&r));
            if !c.check(&tr, res, "Touch", h, window, now, &r) {
                return;
            }
        } else if op < 45 {
            // a burst in one tick (promotes heavy keys)
            let n = 1 + rng.int_n(64);
            for _ in 0..n {
                f.touch_at(h, 0, now);
                tr.add(h, now);
            }
            trace(&tr, res.ops, *tt, "burst", h, now, &|| n.to_string());
            res.ops += n as u64 - 1;
        } else if op < 50 {
            let s = f.check_and_touch_at(h, window, now);
            tr.add(h, now);
            trace(&tr, res.ops, *tt, "CheckAndTouch", h, now, &|| s.to_string());
        } else if op < 55 {
            let (s, n) = f.touch_and_count_at(h, window, now);
            tr.add(h, now);
            trace(&tr, res.ops, *tt, "TouchAndCount", h, now, &|| format!("[{s} {n}]"));
        } else if op < 85 {
            let r = f.count_at(h, window, now);
            trace(&tr, res.ops, *tt, "Count", h, now, &|| gov_result(&r));
            if !c.check(&tr, res, "Count", h, window, now, &r) {
                return;
            }
        } else if op < 90 {
            let (s, v) = (f.contains_at(h, window, now), f.velocity_count_at(h, window, now));
            trace(&tr, res.ops, *tt, "Contains+VelocityCount", h, now, &|| format!("[{s} {v}]"));
            res.ops += 1;
        } else if op < 97 {
            for x in &mut hs {
                *x = if rng.int_n(2) == 0 { heavy[rng.int_n(heavy.len() as i64) as usize] } else { key(rng.uint64_n(pool)) };
            }
            if rng.int_n(2) == 0 {
                f.check_and_touch_batch(&hs, window, now, &mut out);
            } else {
                f.touch_and_count_batch(&hs, window, now, &mut out, &mut cnt);
            }
            for &x in &hs {
                tr.add(x, now);
                trace(&tr, res.ops, *tt, "batch", x, now, &String::new);
            }
            f.velocity_count_batch(&hs, window, now, &mut cnt);
            res.ops += 1; // two calls (of 16 keys each)
        } else {
            f.decay_step(now, 1 + rng.int_n(16) as u64);
            if trace_key != 0 {
                trace(&tr, res.ops, *tt, "decay", trace_key, now, &String::new);
            }
        }
        if place_every > 0 && bg_rng.int_n(place_every) == 0 {
            f.place_next(f.room.get(), 0);
        }
        if trace_key != 0 {
            // report the operation after which the traced key's slot or stash entry is gone
            let w = where_is(f, trace_key, *tt);
            let inside = w.contains("slot") || w.contains("stash");
            if inside != trace_in {
                let keys: Vec<String> = hs.iter().map(|x| format!("{x:x}")).collect();
                eprintln!(
                    "op {} (key {h:x} now {now} T {}; a batch's keys: [{}]): traced key in table or stash: {inside}; {w}",
                    res.ops,
                    *tt,
                    keys.join(" ")
                );
                trace_in = inside;
            }
        }
        if tr.newest >= trim_at && !tr.ev.is_empty() {
            tr.trim();
            trim_at = tr.newest + hz;
        }
        res.ops += 1;
    }
    let occ = f.occupancy(tr.newest);
    if occ > f.slots() {
        res.fail = format!("occupancy {occ} > {} slots; {}", f.slots(), c.desc);
    }
}

/// TestFuzzConcurrent (TF_CFUZZ=<duration>, e.g. 10m) is the soak
/// (TestStressSoak) over the fuzzer's configurations (fuzzConfig: load,
/// stash, overflow table, hot entries, horizon), with ConstantTime
/// (functional, as TF_CT=1 runs it) or Background (its placer a thread) in
/// some cases. A case runs 2 to 2×GOMAXPROCS threads on one filter. Each owns
/// its keys, so it knows their true counts: it touches, counts and batches
/// them (repeats included) at a shared clock that the threads move on,
/// reading it with a lag of up to two units, and checks Upper on every Touch
/// and Count as TestFuzzUpper does, with its documented exceptions. One
/// operation in eight is on a few keys every thread shares, so a key's reads
/// race its touches; a Count of a shared key must cover the events whose
/// touches returned before it began. Events a call at the newest clock any
/// call has used could have expired are left out of the truth. With
/// TF_CFUZZ_STRAGGLE=1, thread 0 of every case is a straggler, its calls half
/// a horizon to a horizon behind the clock. With TF_CFUZZ_TAGS=n, every key's
/// tag is one of n values and its fingerprint unique in the case, and
/// capacity at most 4096 (TagAmp). A case's configuration and streams come
/// from its seed (TF_CFUZZ_SEED, default 1, for the first), but its run
/// depends on its races: TF_CFUZZ_CASE=<seed> runs one case TF_CFUZZ_RUNS
/// times (default 20).
#[test]
fn TestFuzzConcurrent() {
    let t = T::new("TestFuzzConcurrent");
    let one = getenv("TF_CFUZZ_CASE");
    if !one.is_empty() {
        let seed: u64 = match one.parse() {
            Ok(s) => s,
            Err(e) => fatalf!(t, "{e}"),
        };
        let mut runs: i64 = getenv("TF_CFUZZ_RUNS").parse().unwrap_or(0);
        if runs <= 0 {
            runs = 20;
        }
        let mut sum = FuzzResult::default();
        for _ in 0..runs {
            let r = cfuzz_case(seed);
            sum.add(&r);
            if !r.fail.is_empty() {
                fatalf!(t, "case {seed}: {}", r.fail);
            }
        }
        logf!(t, "case {seed}: {}", sum.summary());
        return;
    }
    let Some(d) = parse_duration(&getenv("TF_CFUZZ")).filter(|d| !d.is_zero()) else {
        skip!(t, "set TF_CFUZZ=<duration> to run");
    };
    let mut seed = 1u64;
    let s = getenv("TF_CFUZZ_SEED");
    if !s.is_empty() {
        seed = match s.parse() {
            Ok(s) => s,
            Err(e) => fatalf!(t, "{e}"),
        };
    }
    let first = seed;
    let mut sum = FuzzResult::default();
    let t0 = Instant::now();
    while t0.elapsed() < d {
        let r = cfuzz_case(seed);
        sum.add(&r);
        if !r.fail.is_empty() {
            errorf!(t, "case {seed}: {}", r.fail);
        }
        seed += 1;
    }
    let el = t0.elapsed();
    logf!(
        t,
        "{} cases from seed {first} in {} ({:.1} M ops/s): {}",
        seed - first,
        go_duration(round_secs(el)),
        sum.ops as f64 / el.as_secs_f64() / 1e6,
        sum.summary()
    );
}

/// cfuzzCase runs one concurrent case.
fn cfuzz_case(seed: u64) -> FuzzResult {
    let mut res = FuzzResult::default();
    let mut rng = Rand::new(seed, 0xc0_f022);
    let (mut cfg, mut capacity, hz) = fuzz_config(&mut rng, &mut res);
    let amp = TagAmp::from_env("TF_CFUZZ_TAGS");
    if amp.is_some() {
        // fewer than 65,536 keys a case
        capacity = capacity.min(4096);
        cfg.capacity = capacity;
    }
    // the clock callers read; the newest any call has used
    let clock = TestClock::new(1 + rng.uint64_n(1 << 30));
    let newest = AtomicU64::new(clock.get());
    cfg.clock = clock.func();
    let mode = rng.int_n(5);
    if mode == 1 {
        (cfg.background, res.bg) = (true, 1);
    }
    let mut f = match Filter::new(cfg.clone()) {
        Ok(f) => f,
        Err(e) => {
            res.fail = format!("New({}): {e}", cfg_desc(&cfg));
            return res;
        }
    };
    if mode == 0 {
        f.tweak(|c| c.ct = true);
        res.ct = 1;
    }
    let workers = 2 + rng.int_n(2 * procs() as i64 - 1) as u64;
    let ops = 1u64 << (13 + rng.int_n(5)); // per thread: 8K .. 128K
    let max_lag = [0, 2, f.unit, 2 * f.unit][rng.int_n(4) as usize];
    // Operations per horizon, all threads together, from idle to a flood of
    // 64x the capacity; the pool of live keys cycles across the capacity.
    let per_h = capacity as f64 * [0.25, 1.0, 4.0, 16.0, 64.0][rng.int_n(5) as usize];
    let mut desc = format!(
        "config {} ct {} bg {} goroutines {workers} horizon {hz} ticks unit {} lag <= {max_lag} ops/horizon {per_h:.0}",
        cfg_desc(&cfg),
        f.ct,
        f.bg,
        f.unit
    );
    let mut keys = vec![0u64; 1 + rng.int_n(16) as usize];
    let straggle = env_on("TF_CFUZZ_STRAGGLE");
    if straggle {
        desc += " straggler";
    }
    for (i, k) in keys.iter_mut().enumerate() {
        *k = amp_h(amp.as_ref(), mix64((1 << 62) | i as u64), None);
    }
    let sh = CfuzzShared { keys, tr: Mutex::new(FuzzTruth { horizon: hz, ..FuzzTruth::default() }) };
    let total: Mutex<FuzzResult> = Mutex::new(res);
    std::thread::scope(|s| {
        for g in 0..workers {
            let wseed = rng.uint64();
            let w = CfuzzWorker {
                f: &f,
                g,
                seed: wseed,
                ops,
                share: capacity / workers as i64 + 1,
                hz,
                max_lag,
                per_h,
                straggler: straggle && g == 0,
                clock: &clock,
                newest: &newest,
                sh: &sh,
                amp: amp.as_ref(),
            };
            let (total, desc) = (&total, &desc);
            s.spawn(move || {
                let r = w.run();
                let mut res = total.lock().unwrap();
                res.add(&r);
                if !r.fail.is_empty() && res.fail.is_empty() {
                    res.fail = format!("{}; {desc}", r.fail);
                }
            });
        }
    });
    let mut res = total.into_inner().unwrap();
    (res.evictions, res.folded, res.promoted, res.waited) = (f.evictions(), f.folded(), f.promoted(), f.waited());
    let occ = f.occupancy(newest.load(SeqCst));
    if occ > f.slots() && res.fail.is_empty() {
        res.fail = format!("occupancy {occ} > {} slots; {desc}", f.slots());
    }
    res
}

/// cfuzzShared: the keys every thread of a case touches and counts, and
/// their events whose touches have returned.
struct CfuzzShared {
    keys: Vec<u64>,
    tr: Mutex<FuzzTruth>,
}

impl CfuzzShared {
    fn add(&self, h: u64, ts: u64) {
        self.tr.lock().unwrap().add(h, ts);
    }

    /// snapshot is h's events so far, as a truth of its own.
    fn snapshot(&self, h: u64) -> FuzzTruth {
        let tr = self.tr.lock().unwrap();
        FuzzTruth { ev: HashMap::from([(h, tr.events(h).to_vec())]), horizon: tr.horizon, ..FuzzTruth::default() }
    }
}

/// cfuzzWorker is one thread of a concurrent case: ops operations on its own
/// keys (g in the top bits), share its share of the capacity, and on the
/// shared keys. A straggler's calls run half a horizon to a horizon behind
/// the clock: a caller descheduled that long, or one passing its own
/// timestamps. Its events stay in the truth while a horizon of the newest
/// clock holds them; its touches then race inserts and sweeps that see its
/// keys expired.
struct CfuzzWorker<'a> {
    f: &'a Filter,
    g: u64,
    seed: u64,
    ops: u64,
    share: i64,
    hz: u64,
    max_lag: u64,
    per_h: f64,
    straggler: bool,
    clock: &'a TestClock,
    newest: &'a AtomicU64,
    sh: &'a CfuzzShared,
    amp: Option<&'a TagAmp>,
}

impl CfuzzWorker<'_> {
    fn run(&self) -> FuzzResult {
        let (f, g, hz, newest) = (self.f, self.g, self.hz, self.newest);
        let mut res = FuzzResult::default();
        let mut rng = Rand::new(self.seed, g);
        let mut cache = HashMap::new();
        let amp = self.amp;
        let mut key = |k: u64| amp_h(amp, mix64((g << 48) ^ k), Some(&mut cache));
        let heavy: Vec<u64> = (0..1 + rng.int_n(4) as u64).map(|i| key((1 << 40) | i)).collect();
        let mut tr = FuzzTruth { ev: HashMap::new(), horizon: hz, newest: newest.load(SeqCst), keep: 0 };
        const POOLS: [f64; 6] = [0.25, 0.5, 1.0, 2.0, 4.0, 8.0];
        let (mut pool, mut phase_left) = (self.share as u64, 0i64);
        let (tick, mut acc) = (hz as f64 / self.per_h, 0.0f64);
        let mut trim_at = tr.newest + hz;
        let mut hs = [0u64; 16];
        let mut out = [false; 16];
        let mut cnt = [0i64; 16];
        let check = |tr: &mut FuzzTruth, res: &mut FuzzResult, op: &str, h: u64, window: u64, now: u64, r: Result| -> bool {
            res.checks += 1;
            if r.saturated {
                res.sat += 1;
                return true;
            }
            tr.newest = tr.newest.max(newest.load(SeqCst)); // a call at this clock may have expired older events
            let exact = tr.count(h, window, now);
            if r.upper >= exact {
                return true;
            }
            if floor_live(f, h, window, now) {
                res.floored += 1;
            } else if f.sk_w == 0 && f.lost_recently(now) {
                res.lost_low += 1;
            } else {
                res.fail = format!(
                    "goroutine {g} op {} {op}: Upper {} < true {exact} (result {}); key {h:x} window {window} now {now} newest {}; events {}; {}",
                    res.ops,
                    r.upper,
                    show(&r),
                    tr.newest,
                    gov(tail(tr.events(h), 12)),
                    where_is(f, h, now)
                );
                return false;
            }
            true
        };
        while res.ops < self.ops {
            if phase_left == 0 {
                pool = 1.max((POOLS[rng.int_n(POOLS.len() as i64) as usize] * self.share as f64) as u64);
                phase_left = 1 + rng.int_n(4 * self.share);
            }
            phase_left -= 1;
            acc += tick * 2.0 * rng.float64();
            if acc >= 1.0 {
                self.clock.0.fetch_add(acc as u64, SeqCst);
                acc -= (acc as u64) as f64;
            }
            if rng.float64() * self.per_h < 0.1 {
                self.clock.0.fetch_add(1 + rng.uint64_n(3 * hz), SeqCst);
            }
            let mut now = self.clock.get();
            if self.max_lag > 0 && rng.int_n(4) == 0 {
                now -= rng.uint64_n(self.max_lag + 1);
            }
            if self.straggler {
                now -= (now - 1).min(hz / 2 + rng.uint64_n(hz / 2 + 1));
            }
            let mut n = newest.load(SeqCst);
            while now > n {
                match newest.compare_exchange(n, now, SeqCst, SeqCst) {
                    Ok(_) => break,
                    Err(cur) => n = cur,
                }
            }
            tr.newest = tr.newest.max(now);
            let window = 1 + rng.uint64_n(hz + hz / 4);
            if rng.int_n(8) == 0 {
                // a shared key
                let sh = self.sh;
                let h = sh.keys[rng.int_n(sh.keys.len() as i64) as usize];
                let op = rng.int_n(4);
                match op {
                    0 => {
                        f.touch_at(h, window, now);
                    }
                    1 => {
                        f.check_and_touch_at(h, window, now);
                    }
                    2 => {
                        f.touch_and_count_at(h, window, now);
                    }
                    _ => {
                        let mut before = sh.snapshot(h); // touches that returned before the count began
                        let r = f.count_at(h, window, now);
                        if r.saturated {
                            res.checks += 1;
                            res.sat += 1;
                        } else {
                            before.newest = tr.newest.max(newest.load(SeqCst));
                            let exact = before.count(h, window, now);
                            if r.upper < exact && !floor_live(f, h, window, now) && !(f.sk_w == 0 && f.lost_recently(now)) {
                                res.checks += 1;
                                res.fail = format!(
                                    "goroutine {g} op {} shared Count: Upper {} < true {exact} (result {}); key {h:x} window {window} now {now} newest {}; events {}; {}",
                                    res.ops,
                                    r.upper,
                                    show(&r),
                                    before.newest,
                                    gov(tail(before.events(h), 12)),
                                    where_is(f, h, now)
                                );
                                return res;
                            }
                            res.checks += 1;
                        }
                    }
                }
                if op < 3 {
                    sh.add(h, now);
                }
                if res.ops % 4096 == 0 {
                    let mut st = sh.tr.lock().unwrap();
                    st.newest = st.newest.max(newest.load(SeqCst));
                    st.trim();
                }
                res.ops += 1;
                continue;
            }
            let h = if rng.int_n(16) == 0 { heavy[rng.int_n(heavy.len() as i64) as usize] } else { key(rng.uint64_n(pool)) };
            let op = rng.int_n(100);
            if op < 40 {
                tr.add(h, now);
                let r = f.touch_at(h, window, now);
                if !check(&mut tr, &mut res, "Touch", h, window, now, r) {
                    return res;
                }
            } else if op < 45 {
                let n = 1 + rng.int_n(64);
                for _ in 0..n {
                    f.touch_at(h, 0, now);
                    tr.add(h, now);
                }
                res.ops += n as u64 - 1;
            } else if op < 50 {
                f.check_and_touch_at(h, window, now);
                tr.add(h, now);
            } else if op < 55 {
                f.touch_and_count_at(h, window, now);
                tr.add(h, now);
            } else if op < 85 {
                let r = f.count_at(h, window, now);
                if !check(&mut tr, &mut res, "Count", h, window, now, r) {
                    return res;
                }
            } else if op < 90 {
                let _ = f.contains_at(h, window, now);
                let _ = f.velocity_count_at(h, window, now);
                res.ops += 1;
            } else if op < 97 {
                // a batch of 16, drawn from 4 to 16 keys
                let mut ks = vec![0u64; 4 + rng.int_n(13) as usize];
                for k in &mut ks {
                    *k = if rng.int_n(2) == 0 { heavy[rng.int_n(heavy.len() as i64) as usize] } else { key(rng.uint64_n(pool)) };
                }
                for x in &mut hs {
                    *x = ks[rng.int_n(ks.len() as i64) as usize];
                }
                if rng.int_n(2) == 0 {
                    f.check_and_touch_batch(&hs, window, now, &mut out);
                } else {
                    f.touch_and_count_batch(&hs, window, now, &mut out, &mut cnt);
                }
                for &x in &hs {
                    tr.add(x, now);
                }
                f.velocity_count_batch(&hs, window, now, &mut cnt);
                res.ops += 1;
            } else {
                f.decay_step(now, 1 + rng.int_n(16) as u64);
            }
            if tr.newest >= trim_at && !tr.ev.is_empty() {
                tr.trim();
                trim_at = tr.newest + hz;
            }
            res.ops += 1;
        }
        res
    }
}
