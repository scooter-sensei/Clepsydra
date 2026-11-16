//! White-box helpers the Go suite keeps beside its tests: whereIs and
//! floorLive (go/soak_test.go), moveTable and countLine
//! (go/split_race_test.go), velWorkload (go/velinfo_test.go), and Go's
//! formats for the values the experiments print.

use std::collections::HashSet;

use super::check::*;
use super::gorand::Rand;
use super::gosort;
use crate::ghost::GH_SUB_MASK;
use crate::hot::is_hot;
use crate::slots::*;
use crate::stash::STASH_WAYS;
use crate::{Config, Filter, Result};

/// Go's `%+v` of a Result.
pub(crate) fn show(r: &Result) -> String {
    format!("{{Seen:{} Count:{} Upper:{} Saturated:{} Restored:{} Degraded:{}}}", r.seen, r.count, r.upper, r.saturated, r.restored, r.degraded)
}

/// Go's `%v` of a slice or array of integers: `[1 2 3]`.
pub(crate) fn gov<X: std::fmt::Display>(xs: &[X]) -> String {
    let parts: Vec<String> = xs.iter().map(ToString::to_string).collect();
    format!("[{}]", parts.join(" "))
}

/// whereIs describes where h's history is: its slots (with the register's or
/// hot entry's counts), its stash entry, its overflow entries and floor.
pub(crate) fn where_is(f: &Filter, h: u64, now: u64) -> String {
    let (fp, tg) = (fingerprint(h), tag(h));
    let id = f.stash_id(h, fp, tg);
    let p = f.primary(h);
    let mut s = String::new();
    for (li, l) in [p, f.alt(p, fp)].into_iter().enumerate() {
        let base = (l * WORDS_PER_LINE) as usize;
        let mut k = 0;
        while k < WORDS_PER_LINE as usize {
            let (w0, w1) = (ld(&f.words[base + k]), ld(&f.words[base + k + 1]));
            if (w0 >> 48) == fp && (w1 >> TAG_SHIFT) == tg {
                let (v, ok) = f.view_of(id, w1, w0 & TS_MASK);
                s += &format!(
                    "slot line{li}/{} disp{} last {} hot {} ok {ok} epoch {} counts {}; ",
                    k / 2,
                    (w0 & DISP_BIT) != 0,
                    w0 & TS_MASK,
                    is_hot(w1),
                    v.e,
                    gov(&v.c)
                );
            }
            k += 2;
        }
    }
    if f.stash_n > 0 {
        if let Some(st) = f.stash.get() {
            let (base, _) = st.set(id);
            for k in base..base + STASH_WAYS as usize {
                let e = st.get(k);
                if e.id == id {
                    let (v, ok) = f.view_of(id, e.w1, e.w0 & TS_MASK);
                    s += &format!("stash last {} hot {} ok {ok} epoch {} counts {}; ", e.w0 & TS_MASK, is_hot(e.w1), v.e, gov(&v.c));
                }
            }
        }
    }
    if let Some(p) = f.room.get() {
        let (base, _) = p.set(id);
        for k in base..base + STASH_WAYS as usize {
            let e = p.get(k);
            if e.id == id {
                let (v, ok) = f.view_of(id, e.w1, e.w0 & TS_MASK);
                s += &format!(
                    "room last {} busy {} hot {} ok {ok} epoch {} counts {} (wait bit {}); ",
                    e.w0 & TS_MASK,
                    (e.w1 >> TAG_SHIFT) == TAG_BUSY,
                    is_hot(e.w1),
                    v.e,
                    gov(&v.c),
                    p.waiting(id)
                );
            }
        }
    }
    if let Some(g) = f.gh.get() {
        let (r, _) = g.estimate(id, f.horizon, now);
        s += &format!("ghost {}", show(&r));
    }
    s
}

/// floorLive: one of h's overflow buckets has a floor live within window
/// that covers h's fingerprint, so h may have histories there that its answer
/// can't attribute to it.
pub(crate) fn floor_live(f: &Filter, h: u64, window: u64, now: u64) -> bool {
    let Some(g) = f.gh.get() else {
        return false;
    };
    let ep = ld(&g.epoch);
    let (bases, nb, fp) = g.slot(f.stash_id(h, fingerprint(h), tag(h)));
    for &base in &bases[..nb] {
        let e = (ld(&g.words[base]) >> g.floor_shift(fp)) & GH_SUB_MASK;
        if ep != 0 && e != 0 && g.live(g.real(e, ep), window.min(g.horizon), now) {
            return true;
        }
    }
    false
}

/// countLine is the number of entries in line l live at now.
pub(crate) fn count_line(f: &Filter, l: u64, now: u64) -> i64 {
    let mut n = 0;
    let mut s = 0;
    while s < WORDS_PER_LINE {
        let w0 = ld(&f.words[(l * WORDS_PER_LINE + s) as usize]);
        if w0 != 0 && age(now, w0 & TS_MASK) <= f.horizon {
            n += 1;
        }
        s += 2;
    }
    n
}

// Lines of moveTable's 4-line table: X is homed at P and sits displaced in A;
// every line is full.
pub(crate) const MV_P: u64 = 0;
pub(crate) const MV_A: u64 = 1;
pub(crate) const MV_B: u64 = 2;
pub(crate) const MV_Q: u64 = 3;

/// moveTable's table and its key maker.
pub(crate) struct MoveTable {
    pub(crate) f: Filter,
    pub(crate) x: u64,
    stripes: HashSet<u64>,
    next: u64,
}

impl MoveTable {
    /// key(primary, alt) returns an unused key with those lines (alt 4: any)
    /// on a claim stripe of its own, so no two keys of a test share a lock.
    pub(crate) fn key(&mut self, primary: u64, alt: u64) -> u64 {
        loop {
            let h = mix64(self.next.wrapping_add(1 << 52));
            let (p, s) = (self.f.primary(h), h >> (64 - CLAIM_STRIPE_BITS));
            if p == primary && (alt == 4 || self.f.alt(p, fingerprint(h)) == alt) && !self.stripes.contains(&s) {
                self.stripes.insert(s);
                return h;
            }
            self.next += 1;
        }
    }

    pub(crate) fn touch(&self, h: u64, n: i64, now: u64) {
        for _ in 0..n {
            self.f.touch_at(h, 0, now);
        }
    }
}

/// moveTable builds a full 4-line table at t1 = 1000: P holds one entry from
/// t0 = 1 (expired once t0 + horizon has passed) and three of 5 events; X,
/// homed at P, sits displaced in A with 1 event; A, B and Q fill up with
/// 5-event entries homed there.
#[track_caller]
pub(crate) fn move_table(t: &T) -> MoveTable {
    const T0: u64 = 1;
    const T1: u64 = 1000;
    let f = must(Config { capacity: 13, horizon: MINUTE, resolution: ms(1), stash: 64, overflow: -1, hot: -1, ..Config::default() });
    if f.slots() != 16 {
        fatalf!(t, "want 4 lines, got {} slots", f.slots());
    }
    let mut m = MoveTable { f, x: 0, stripes: HashSet::new(), next: 0 };
    let k = m.key(MV_P, 4);
    m.touch(k, 1, T0);
    for _ in 0..3 {
        let k = m.key(MV_P, 4);
        m.touch(k, 5, T1);
    }
    m.x = m.key(MV_P, MV_A);
    m.touch(m.x, 1, T1);
    for l in [MV_A, MV_B, MV_Q] {
        while count_line(&m.f, l, T1) < 4 {
            let k = m.key(l, 4);
            m.touch(k, 5, T1);
        }
    }
    for l in 0..4 {
        let n = count_line(&m.f, l, T1);
        if n != 4 {
            fatalf!(t, "line {l} holds {n} live entries at t1, want 4");
        }
    }
    if m.f.find(MV_A, (fingerprint(m.x) << 1) | 1, tag(m.x)) < 0 {
        fatalf!(t, "X is not displaced in A");
    }
    m
}

pub(crate) const VEL_WINS: [u64; 5] = [1_000, 5_000, 15_000, 30_000, 60_000];

/// velWorkload replays tfbench's velocity workload (cmd/tfbench/accuracy.go,
/// velocityMAE): same seed, same order of draws. visit is called once per
/// query with the key's event times so far (sorted), the query's window index
/// and now; touch once per event, before the queries that see it.
pub(crate) fn vel_workload(mut touch: impl FnMut(usize, u64), mut visit: impl FnMut(usize, &[u64], usize, u64)) {
    const H_TICKS: u64 = 60_000;
    let mut r = Rand::new(9, 9);
    const NKEYS: usize = 2000;
    const END: u64 = 5 * H_TICKS;
    let endf = END as f64;
    let mut evs: Vec<(u64, usize)> = Vec::new();
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
                let rate = (0.2 + r.float64() * 4.8) / 1000.0;
                poisson(&mut r, &mut evs, 0.0, endf, rate, k);
            }
            1 => {
                let mut t = r.exp_float64() * 10_000.0;
                while t < endf {
                    for _ in 0..5 + r.int_n(16) {
                        evs.push(((t + r.float64() * 200.0) as u64 + 1, k));
                    }
                    t += r.exp_float64() * 10_000.0;
                }
            }
            2 => {
                let mut t = 0.0f64;
                while t < endf {
                    t += r.exp_float64() / ((0.1 + 9.9 * t / endf) / 1000.0);
                    evs.push((t as u64 + 1, k));
                }
            }
            3 => poisson(&mut r, &mut evs, 0.0, endf, 1.0 / 20_000.0, k),
            _ => {
                let mut t = 0.0f64;
                while t < endf {
                    poisson(&mut r, &mut evs, t, t + 5000.0, 4.0 / 1000.0, k);
                    t += 10_000.0;
                }
            }
        }
    }
    gosort::slice(&mut evs, |a, b| a.0 < b.0);
    let mut times: Vec<Vec<u64>> = vec![Vec::new(); NKEYS];
    let mut i = 0;
    for now in H_TICKS..END {
        while i < evs.len() && evs[i].0 <= now {
            touch(evs[i].1, evs[i].0);
            times[evs[i].1].push(evs[i].0);
            i += 1;
        }
        let (k, wi) = (r.int_n(NKEYS as i64) as usize, r.int_n(5) as usize);
        visit(k, &times[k], wi, now);
    }
}

/// keyAt: a hash with primary line line, fingerprint fp and tag tg, whose
/// touches don't run the inline decay step.
pub(crate) fn key_at(f: &Filter, line: u64, fp: u64, tg: u64) -> u64 {
    let mut lo = ((line << 36) + f.lines - 1) / f.lines;
    loop {
        let h = (fp << 48) | (tg << 36) | (lo & ((1 << 36) - 1));
        if f.primary(h) == line && (h & crate::decay::DECAY_EVERY) != 0 {
            return h;
        }
        lo += 1;
    }
}

/// twoLineFPs: n fingerprints from start on whose alternate line from line
/// is another line.
pub(crate) fn two_line_fps(f: &Filter, line: u64, start: u64, n: usize) -> Vec<u64> {
    let mut out = Vec::new();
    let mut fp = start;
    while out.len() < n {
        if f.alt(line, fp) != line {
            out.push(fp);
        }
        fp += 1;
    }
    out
}

/// slotsOf: the word indexes of the slots where h is visible.
pub(crate) fn slots_of(f: &Filter, h: u64) -> Vec<isize> {
    let (fp, tg) = (fingerprint(h), tag(h));
    let p = f.primary(h);
    let mut out = Vec::new();
    for (li, l) in [p, f.alt(p, fp)].into_iter().enumerate() {
        if li == 1 && l == p {
            break;
        }
        let base = (l * WORDS_PER_LINE) as isize;
        let mut s = 0;
        while s < WORDS_PER_LINE as isize {
            let (w0, w1) = (ld(f.w(base + s)), ld(f.w(base + s + 1)));
            if (w0 >> 48) == fp && ((w0 & DISP_BIT) != 0) == (li == 1) && (w1 >> TAG_SHIFT) == tg {
                out.push(base + s);
            }
            s += 2;
        }
    }
    out
}

/// A filter with a one-minute horizon at 1 ms on a clock that reads 1, in
/// the plain mode whatever TF_CT and TF_BG say: these races are in the plain
/// paths' order of steps.
#[track_caller]
pub(crate) fn new_minute_filter(capacity: i64) -> Filter {
    let clock = TestClock::new(1);
    Filter::with(Config { capacity, horizon: MINUTE, resolution: ms(1), clock: clock.func(), ..Config::default() }, |c| {
        (c.ct, c.bg) = (false, false);
    })
    .unwrap_or_else(|e| panic!("New: {e}"))
}

/// a unit boundary (units of 2,500 ticks)
pub(crate) const SWEEP_T0: u64 = 60_000;

/// A goroutine's `done` channel: closed when it returns.
pub(crate) type Done = std::sync::mpsc::Receiver<()>;

/// `go func() { fn(); close(done) }()`.
pub(crate) fn go(f: impl FnOnce() + Send + 'static) -> Done {
    let (tx, rx) = std::sync::mpsc::channel::<()>();
    std::thread::spawn(move || {
        f();
        drop(tx);
    });
    rx
}

/// Whether done is closed within d (`select` on it and `time.After(d)`).
pub(crate) fn closed_within(done: &Done, d: std::time::Duration) -> bool {
    matches!(done.recv_timeout(d), Err(std::sync::mpsc::RecvTimeoutError::Disconnected))
}

/// during runs f in a thread and waits for it up to 20 ms: it may wait for a
/// hold its caller has; the channel is closed when it returns.
pub(crate) fn during(f: impl FnOnce() + Send + 'static) -> Done {
    let done = go(f);
    closed_within(&done, ms(20));
    done
}

#[track_caller]
pub(crate) fn wait_for(t: &T, done: &Done, what: &str) {
    if !closed_within(done, secs(5)) {
        fatalf!(t, "{what} did not return");
    }
}

/// A channel the test only closes and waits on (`make(chan struct{})`,
/// `close(ch)`, `<-ch`), shareable with hooks and threads.
#[derive(Clone, Default)]
pub(crate) struct Signal(std::sync::Arc<(std::sync::Mutex<bool>, std::sync::Condvar)>);

impl Signal {
    pub(crate) fn new() -> Signal {
        Signal::default()
    }

    /// `close(ch)`.
    pub(crate) fn close(&self) {
        *self.0.0.lock().unwrap_or_else(std::sync::PoisonError::into_inner) = true;
        self.0.1.notify_all();
    }

    /// `<-ch`.
    pub(crate) fn wait(&self) {
        let mut c = self.0.0.lock().unwrap_or_else(std::sync::PoisonError::into_inner);
        while !*c {
            c = self.0.1.wait(c).unwrap_or_else(std::sync::PoisonError::into_inner);
        }
    }

    /// `select { case <-ch: ... case <-time.After(d): ... }`: whether ch is
    /// closed within d.
    pub(crate) fn wait_timeout(&self, d: std::time::Duration) -> bool {
        let c = self.0.0.lock().unwrap_or_else(std::sync::PoisonError::into_inner);
        let (c, _) = self.0.1.wait_timeout_while(c, d, |closed| !*closed).unwrap_or_else(std::sync::PoisonError::into_inner);
        *c
    }
}

/// keySlots counts the slots of k's two lines holding k (its fingerprint and
/// tag).
pub(crate) fn key_slots(f: &Filter, k: u64) -> i64 {
    let (fp, tg) = (fingerprint(k), tag(k));
    let p = f.primary(k);
    let mut lines = vec![p];
    let a = f.alt(p, fp);
    if a != p {
        lines.push(a);
    }
    let mut n = 0;
    for l in lines {
        let mut s = 0;
        while s < WORDS_PER_LINE as isize {
            let j = l as isize * WORDS_PER_LINE as isize + s;
            if (ld(f.w(j)) >> 48) == fp && (ld(f.w(j + 1)) >> TAG_SHIFT) == tg {
                n += 1;
            }
            s += 2;
        }
    }
    n
}

/// realignModes runs f with realign's 16-byte CAS, when the CPU has it, and
/// then with its hold. f is given the mode's name.
pub(crate) fn realign_modes(mut f: impl FnMut(&str)) {
    if crate::cpu::has_cas2() {
        f("cas2");
        crate::cpu::set_has_cas2(false);
        let _back = Defer(|| crate::cpu::set_has_cas2(true));
        f("hold");
        return;
    }
    f("hold");
}

/// Go's `%v` of a Result: `{true 3 3 false false false}`.
pub(crate) fn gov_result(r: &Result) -> String {
    format!("{{{} {} {} {} {} {}}}", r.seen, r.count, r.upper, r.saturated, r.restored, r.degraded)
}

/// `hash/fnv`'s New64a.
pub(crate) struct Fnv64a(pub(crate) u64);

impl Fnv64a {
    pub(crate) fn new() -> Fnv64a {
        Fnv64a(0xcbf29ce484222325)
    }

    /// `fmt.Fprintf(h, ...)` of an already formatted string.
    pub(crate) fn write(&mut self, s: &str) {
        for &b in s.as_bytes() {
            self.0 = (self.0 ^ u64::from(b)).wrapping_mul(0x100000001b3);
        }
    }

    pub(crate) fn sum64(&self) -> u64 {
        self.0
    }
}

/// tagAmp (go/tagamp_test.go) amplifies tag coincidences in the concurrent
/// tests (TF_CFUZZ_TAGS, TF_STRESS_TAGS): it remaps each key's hash so its
/// 12-bit tag is one of n values and its fingerprint is unique in the run,
/// its primary line kept. Two keys that meet in a slot then share a tag one
/// time in n instead of one in 4096, and no two keys share an identity. The
/// races where a CAS from a value read earlier lands on a slot that changed
/// hands, because a same-tag key's register can be bit-identical to the old
/// occupant's (claims A28, A29), turn from one failure per 30 to 60 minutes of
/// the concurrent fuzzer into a few a minute. The tags have distinct low
/// bytes, so their keys' move counts stay on distinct stripes. Fingerprints
/// are 16 bits: a run must use fewer than 65,536 keys.
pub(crate) struct TagAmp {
    n: u64,
    mu: std::sync::Mutex<(std::collections::HashMap<u64, u64>, u64)>, // fp, next
}

impl TagAmp {
    /// newTagAmp: the amplifier the environment variable env asks for (n, up
    /// to 14), or None.
    pub(crate) fn from_env(env: &str) -> Option<TagAmp> {
        let n: i64 = getenv(env).parse().unwrap_or(0);
        if n <= 0 {
            return None;
        }
        Some(TagAmp { n: n.min(14) as u64, mu: Default::default() })
    }

    /// h is x remapped; cache, if not None, is the caller's own copy of the
    /// mapping.
    pub(crate) fn h(&self, x: u64, cache: Option<&mut std::collections::HashMap<u64, u64>>) -> u64 {
        if let Some(c) = &cache {
            if let Some(&y) = c.get(&x) {
                return y;
            }
        }
        let fp = {
            let mut g = self.mu.lock().unwrap_or_else(std::sync::PoisonError::into_inner);
            let (fps, next) = &mut *g;
            match fps.get(&x) {
                Some(&fp) => fp,
                None => {
                    *next += 1;
                    let fp = (*next * 0x9e37) & 0xffff; // a bijection of 1..65535: no fingerprint 0, stripes spread
                    fps.insert(x, fp);
                    fp
                }
            }
        };
        let tg = 0x010 + ((x >> 36) % self.n) * 0x011;
        let y = (fp << 48) | (tg << 36) | (x & ((1 << 36) - 1));
        if let Some(c) = cache {
            c.insert(x, y);
        }
        y
    }
}

/// `amp.h(x, cache)` on a possibly nil amplifier: x itself without one.
pub(crate) fn amp_h(a: Option<&TagAmp>, x: u64, cache: Option<&mut std::collections::HashMap<u64, u64>>) -> u64 {
    match a {
        Some(a) => a.h(x, cache),
        None => x,
    }
}

/// `time.ParseDuration` for the forms the suite's switches take ("60s",
/// "5m", "1m30s", "500ms", "2h", "1.5s"): None for anything else.
pub(crate) fn parse_duration(s: &str) -> Option<std::time::Duration> {
    if s.is_empty() {
        return None;
    }
    let (mut total, mut rest) = (0f64, s);
    while !rest.is_empty() {
        let j = rest.find(|c: char| !(c.is_ascii_digit() || c == '.')).unwrap_or(rest.len());
        if j == 0 {
            return None;
        }
        let v: f64 = rest[..j].parse().ok()?;
        rest = &rest[j..];
        let k = rest.find(|c: char| c.is_ascii_digit() || c == '.').unwrap_or(rest.len());
        let ns = match &rest[..k] {
            "ns" => 1.0,
            "us" | "µs" => 1e3,
            "ms" => 1e6,
            "s" => 1e9,
            "m" => 60e9,
            "h" => 3600e9,
            _ => return None,
        };
        total += v * ns;
        rest = &rest[k..];
    }
    Some(std::time::Duration::from_nanos(total as u64))
}

/// `runtime.GOMAXPROCS(0)`: the threads that run at once.
pub(crate) fn procs() -> usize {
    std::thread::available_parallelism().map(std::num::NonZero::get).unwrap_or(1)
}

/// Go's `%+v` of a Config, for the fuzzers' failure reports.
pub(crate) fn cfg_desc(c: &Config) -> String {
    format!(
        "{{Capacity:{} Horizon:{} Resolution:{} Load:{} Clock:{} CoarseClock:{} Key:{} Stash:{} Overflow:{} SpillMin:{} Hot:{} ConstantTime:{} Background:{}}}",
        c.capacity,
        go_duration(crate::filter::dur_ns(c.horizon)),
        go_duration(crate::filter::dur_ns(c.resolution)),
        go_g(c.load),
        if c.clock.is_some() { "set" } else { "<nil>" },
        go_duration(crate::filter::dur_ns(c.coarse_clock)),
        if c.key.is_some() { "set" } else { "<nil>" },
        c.stash,
        c.overflow,
        c.spill_min,
        c.hot,
        go_duration(crate::filter::dur_ns(c.constant_time)),
        c.background
    )
}

/// The reference's test data (go/testdata).
pub(crate) const TESTDATA_DIR: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/../go/testdata");

/// Windows are the query windows (ticks) the churn stream draws from
/// (go/internal/workload).
pub(crate) const CHURN_WINDOWS: [u64; 3] = [1_000, 10_000, 60_000];

/// Churn is go/internal/workload's Churn: a stream mixing new keys (60%) and
/// repeats of keys born within the last 1.5 horizons, over 4 horizons of
/// hTicks each, one window drawn from Windows per operation; load sets the
/// arrival rate so that distinct keys live per horizon ~ load x capacity. The
/// stream is a pure function of its arguments, as Go's.
pub(crate) fn churn(load: f64, seed: u64, capacity: i64, h_ticks: u64, hash: impl Fn(u64) -> u64, mut op: impl FnMut(u64, u64, u64)) {
    let mut r = Rand::new(seed, 1);
    const NEW_FRAC: f64 = 0.6;
    let rate_per_ms = load * capacity as f64 / (NEW_FRAC * h_ticks as f64) / 1.375;
    let mut keys: Vec<(u64, u64)> = Vec::new(); // h, t
    let mut head = 0usize; // keys[head..] is Go's keys slice
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
            op(h, CHURN_WINDOWS[r.int_n(CHURN_WINDOWS.len() as i64) as usize], now);
        }
        if now % h_ticks == 0 {
            // drop keys too old to be repeated
            while head < keys.len() && keys[head].1 + 2 * h_ticks < now {
                head += 1;
            }
        }
    }
}
