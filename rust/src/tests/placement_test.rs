//! placement_test.go.

use std::collections::HashMap;

use super::check::*;
use super::helpers::*;
use crate::slots::*;
use crate::{Config, Filter};

const SPL: usize = SLOTS_PER_LINE as usize;

/// TestPlacementLimit asks whether the scorecard's missed keys are forced by
/// the table layout or caused by the filter's bounded cuckoo search.
///
/// It replays the scorecard's churn stream (cmd/tfbench, same seeds) through
/// an exact placement oracle with the filter's geometry: the same lines, 4
/// slots each, and the same two candidate lines per key (primary, alt). The
/// oracle searches without bound (BFS for a shortest augmenting path), so it
/// places a new key whenever any arrangement of the live keys plus that key
/// exists, and drops the key only when none does. Every drop is checked with
/// a Hall-violation certificate: a set S of full lines whose keys all have
/// both candidates in S, plus the new key whose candidates are in S, is
/// 4|S|+1 keys for 4|S| slots. The real filter runs on the same stream.
///
/// Output lines starting with "placement" are deterministic and are compared
/// byte for byte by the regression tier.
#[test]
fn TestPlacementLimit() {
    let t = T::new("TestPlacementLimit");
    if short() {
        skip!(t, "replays three 4-horizon churn streams");
    }
    const CAP_N: i64 = 200_000;
    const H_TICKS: u64 = 60_000;
    for (load, seed) in [(1.0f64, 2u64), (1.1, 3), (1.15, 4)] {
        let f = must(Config { capacity: CAP_N, horizon: MINUTE, resolution: ms(1), ..Config::default() });
        let mut o = Oracle::new(&f, H_TICKS);
        let mut last: HashMap<u64, u64> = HashMap::new();
        let (mut pos, mut filter_fn, mut oracle_fn) = (0i64, 0i64, 0i64);
        let mut pruned = 0u64;
        churn(load, seed, CAP_N, H_TICKS, mix64, |h, w, now| {
            if now % H_TICKS == 0 && now != pruned {
                pruned = now;
                last.retain(|_, lt| now - *lt <= 2 * H_TICKS);
            }
            let got = f.check_and_touch_at(h, w, now);
            let ogot = o.touch(&t, h, w, now);
            let want = last.get(&h).is_some_and(|&lt| now - lt <= w);
            last.insert(h, now);
            if now < H_TICKS || !want {
                return;
            }
            pos += 1;
            if !got {
                filter_fn += 1;
            }
            if !ogot {
                oracle_fn += 1;
            }
        });
        let mut hist = String::new();
        for (d, &n) in o.moves.iter().enumerate() {
            if n > 0 {
                hist += &format!(" {d}:{n}");
            }
        }
        logf!(
            t,
            "placement load={load:.2} seed={seed} inserts={} oracle_drops={} oracle_peak_slot_load={:.4} oracle_missed={oracle_fn} filter_evictions={} filter_missed={filter_fn} positives={pos}",
            o.inserts,
            o.drops,
            o.peak as f64 / (4 * f.lines) as f64,
            f.evictions()
        );
        logf!(t, "placement load={load:.2} shortest_path_moves(moves:inserts){hist}");
    }
}

/// oracle is an exact placement model of the filter's table: each live key
/// sits in one of its two candidate lines, and inserts take a shortest
/// augmenting path when one exists. Keys expire Horizon after their last
/// touch, as in the filter.
struct Oracle<'a> {
    f: &'a Filter,
    horizon: u64,
    ids: HashMap<u64, i32>, // live key hash -> id
    h: Vec<u64>,
    last: Vec<u64>,
    cand: Vec<[i32; 2]>,
    line: Vec<i32>, // line holding the key
    free: Vec<i32>,
    slots: Vec<[i32; SPL]>, // key ids, -1 = empty
    used: Vec<u8>,
    queue: Vec<(i32, u64)>, // touches in time order: id, t
    qhead: usize,

    // BFS state, stamped to avoid clearing.
    seen: Vec<u32>,
    stamp: u32,
    from: Vec<i32>, // line the key in via moves out of
    via: Vec<i32>,  // key that moves into this line
    depth: Vec<i32>,
    bfs: Vec<i32>,
    target: i32, // line place freed a slot in
    inserts: i64,
    drops: i64,
    live: i64,
    peak: i64,
    moves: [i64; 32], // shortest augmenting path length (moves) per insert; 31 = 31+
}

impl<'a> Oracle<'a> {
    fn new(f: &'a Filter, horizon: u64) -> Oracle<'a> {
        let n = f.lines as usize;
        Oracle {
            f,
            horizon,
            ids: HashMap::new(),
            h: Vec::new(),
            last: Vec::new(),
            cand: Vec::new(),
            line: Vec::new(),
            free: Vec::new(),
            slots: vec![[-1; SPL]; n],
            used: vec![0; n],
            queue: Vec::new(),
            qhead: 0,
            seen: vec![0; n],
            stamp: 0,
            from: vec![0; n],
            via: vec![0; n],
            depth: vec![0; n],
            bfs: Vec::new(),
            target: 0,
            inserts: 0,
            drops: 0,
            live: 0,
            peak: 0,
            moves: [0; 32],
        }
    }

    /// touch returns whether h was seen within w, then records the touch.
    fn touch(&mut self, t: &T, h: u64, w: u64, now: u64) -> bool {
        self.expire(now);
        if let Some(&id) = self.ids.get(&h) {
            let seen = now - self.last[id as usize] <= w;
            self.last[id as usize] = now;
            self.queue.push((id, now));
            return seen;
        }
        self.inserts += 1;
        let p = self.f.primary(h);
        let c = [p as i32, self.f.alt(p, fingerprint(h)) as i32];
        let Some(moves) = self.place(t, c) else {
            self.drops += 1;
            return false;
        };
        self.moves[moves.min(self.moves.len() - 1)] += 1;
        let id;
        if let Some(x) = self.free.pop() {
            id = x;
            (self.h[id as usize], self.last[id as usize], self.cand[id as usize]) = (h, now, c);
        } else {
            id = self.h.len() as i32;
            self.h.push(h);
            self.last.push(now);
            self.cand.push(c);
            self.line.push(-1);
        }
        self.ids.insert(h, id);
        self.put(id, self.target);
        self.queue.push((id, now));
        self.live += 1;
        self.peak = self.peak.max(self.live);
        false
    }

    /// expire removes keys whose last touch is more than horizon before now.
    fn expire(&mut self, now: u64) {
        while self.qhead < self.queue.len() && now - self.queue[self.qhead].1 > self.horizon {
            let (id, t) = self.queue[self.qhead];
            self.qhead += 1;
            if self.last[id as usize] == t && self.line[id as usize] >= 0 {
                self.remove(id);
            }
        }
        if self.qhead > 1 << 20 {
            self.queue.drain(..self.qhead);
            self.qhead = 0;
        }
    }

    fn put(&mut self, id: i32, line: i32) {
        for s in 0..SPL {
            if self.slots[line as usize][s] < 0 {
                self.slots[line as usize][s] = id;
                self.used[line as usize] += 1;
                self.line[id as usize] = line;
                return;
            }
        }
        panic!("oracle: put into a full line");
    }

    fn take(&mut self, id: i32) {
        let l = self.line[id as usize] as usize;
        for s in 0..SPL {
            if self.slots[l][s] == id {
                self.slots[l][s] = -1;
                self.used[l] -= 1;
                self.line[id as usize] = -1;
                return;
            }
        }
        panic!("oracle: key not in its line");
    }

    fn remove(&mut self, id: i32) {
        self.take(id);
        self.ids.remove(&self.h[id as usize]);
        self.free.push(id);
        self.live -= 1;
    }

    /// place frees a slot in one of the candidate lines c, moving keys along
    /// a shortest augmenting path, and sets target to that line. It returns
    /// the number of moves, or None (with a verified certificate) if no
    /// placement exists.
    fn place(&mut self, t: &T, c: [i32; 2]) -> Option<usize> {
        for l in c {
            if (self.used[l as usize] as usize) < SPL {
                self.target = l;
                return Some(0);
            }
        }
        self.stamp += 1;
        self.bfs.clear();
        for l in c {
            let lu = l as usize;
            if self.seen[lu] != self.stamp {
                (self.seen[lu], self.from[lu], self.depth[lu]) = (self.stamp, -1, 0);
                self.bfs.push(l);
            }
        }
        let mut i = 0;
        while i < self.bfs.len() {
            let m = self.bfs[i];
            i += 1;
            for k in self.slots[m as usize] {
                let mut other = self.cand[k as usize][0];
                if other == m {
                    other = self.cand[k as usize][1];
                }
                let ou = other as usize;
                if other == m || self.seen[ou] == self.stamp {
                    continue;
                }
                (self.seen[ou], self.from[ou], self.via[ou], self.depth[ou]) = (self.stamp, m, k, self.depth[m as usize] + 1);
                if (self.used[ou] as usize) < SPL {
                    // augment back along the path
                    let moves = self.depth[ou] as usize;
                    let mut cur = other;
                    while self.from[cur as usize] >= 0 {
                        let (k, prev) = (self.via[cur as usize], self.from[cur as usize]);
                        self.take(k);
                        self.put(k, cur);
                        cur = prev;
                    }
                    self.target = cur;
                    return Some(moves);
                }
                self.bfs.push(other);
            }
        }
        // Certificate: every line reached is full, and every key in them has
        // both candidates among them, so they hold 4|S| keys that need these
        // 4|S| slots, plus the new key.
        for &m in &self.bfs {
            if self.used[m as usize] as usize != SPL {
                fatalf!(t, "oracle: line {m} reached but not full");
            }
            for k in self.slots[m as usize] {
                for l in self.cand[k as usize] {
                    if self.seen[l as usize] != self.stamp {
                        fatalf!(t, "oracle: key {k} in line {m} has candidate {l} outside the closed set");
                    }
                }
            }
        }
        None
    }
}

/// TestOracleMatchesMaxFlow checks the oracle against an independent exact
/// method on small overloaded tables: before every insert, a from-scratch
/// maximum bipartite b-matching (Kuhn's augmenting paths) of the live keys
/// plus the new key must place all of them exactly when the oracle can.
#[test]
fn TestOracleMatchesMaxFlow() {
    let t = T::new("TestOracleMatchesMaxFlow");
    const CAP_N: i64 = 170; // 50 lines
    const H_TICKS: u64 = 6_000;
    let (mut feasible, mut infeasible) = (0, 0);
    for seed in 1..=20u64 {
        let f = must(Config { capacity: CAP_N, horizon: MINUTE, resolution: ms(1), ..Config::default() });
        let mut o = Oracle::new(&f, H_TICKS);
        churn(1.4, seed, CAP_N, H_TICKS, mix64, |h, w, now| {
            o.expire(now); // as touch does first; then ask max-flow
            let live = o.ids.contains_key(&h);
            let mut want = false;
            if !live {
                let p = f.primary(h);
                let mut keys: Vec<[i32; 2]> = vec![[p as i32, f.alt(p, fingerprint(h)) as i32]];
                for &id in o.ids.values() {
                    keys.push(o.cand[id as usize]);
                }
                want = kuhn_places_all(&keys, f.lines as usize);
            }
            let drops = o.drops;
            o.touch(&t, h, w, now);
            if !live {
                let got = o.drops == drops;
                if got != want {
                    fatalf!(t, "seed {seed} now {now}: oracle placed={got}, max-flow placed={want}");
                }
                if want {
                    feasible += 1;
                } else {
                    infeasible += 1;
                }
            }
        });
    }
    logf!(t, "agreed on {feasible} feasible and {infeasible} infeasible inserts");
    if infeasible == 0 || feasible == 0 {
        fatalf!(t, "workload did not exercise both outcomes");
    }
}

/// kuhnPlacesAll reports whether every key can sit in one of its two lines
/// with at most slotsPerLine keys per line.
fn kuhn_places_all(keys: &[[i32; 2]], lines: usize) -> bool {
    fn try_(keys: &[[i32; 2]], owner: &mut [Vec<usize>], k: usize, seen: &mut [bool]) -> bool {
        for l in keys[k] {
            let l = l as usize;
            if seen[l] {
                continue;
            }
            seen[l] = true;
            if owner[l].len() < SPL {
                owner[l].push(k);
                return true;
            }
            for i in 0..owner[l].len() {
                let other = owner[l][i];
                if try_(keys, owner, other, seen) {
                    owner[l][i] = k;
                    return true;
                }
            }
        }
        false
    }
    let mut owner: Vec<Vec<usize>> = vec![Vec::new(); lines];
    for k in 0..keys.len() {
        if !try_(keys, &mut owner, k, &mut vec![false; lines]) {
            return false;
        }
    }
    true
}

/// TestLoadThreshold computes the load threshold of 2-choice cuckoo hashing
/// with buckets of l slots: c*/l, where x solves 2l = x·Q(x,l)/Q(x,l+1), c* =
/// x/(2·Q(x,l)) and Q(x,y) = P(Poisson(x) >= y) (Cain, Sanders and Wormald
/// 2007; Fernholz and Ramachandran 2007). Below it, a placement of all keys
/// exists with high probability as the table grows. It checks the values
/// tabulated in Walzer, "What if we tried Less Power?" (arXiv 2307.00644,
/// §1.1.1) and the classic 1/2 for one slot.
#[test]
fn TestLoadThreshold() {
    let t = T::new("TestLoadThreshold");
    // P(Poisson(x) >= y)
    let poisson_tail = |x: f64, y: i64| -> f64 {
        let (mut term, mut cdf) = ((-x).exp(), 0.0);
        for j in 0..y {
            cdf += term;
            term *= x / (j + 1) as f64;
        }
        1.0 - cdf
    };
    let threshold = |l: i64| -> f64 {
        let (mut lo, mut hi) = (1e-9f64, 100.0f64);
        for _ in 0..200 {
            let x = (lo + hi) / 2.0;
            if x * poisson_tail(x, l) / poisson_tail(x, l + 1) < (2 * l) as f64 {
                lo = x;
            } else {
                hi = x;
            }
        }
        let x = (lo + hi) / 2.0;
        x / (2.0 * poisson_tail(x, l)) / l as f64
    };
    for (l, want) in [(1i64, 0.5f64), (2, 0.89701), (3, 0.95915), (4, 0.98037)] {
        let got = threshold(l);
        logf!(t, "placement threshold choices=2 slots={l} load={got:.5}");
        if (got - want).abs() > 5e-6 {
            errorf!(t, "slots={l}: threshold {got:.6}, want {want:.5}");
        }
    }
}

/// TestComparedEntries measures how many stored entries an absent-key lookup
/// compares its 28-bit identity with, at the scorecard's static-FPR setup
/// (200k keys at the default 85% load). With the displaced bit it compares
/// only entries homed at its primary line; without it, every entry in both
/// candidate lines. Each comparison is a false positive with probability
/// 2^-28, so the expected FPR is the mean count / 2^28.
#[test]
fn TestComparedEntries() {
    let t = T::new("TestComparedEntries");
    const CAP_N: u64 = 200_000;
    const PROBES: u64 = 1_000_000;
    let f = must(Config { capacity: CAP_N as i64, horizon: MINUTE, resolution: ms(1), ..Config::default() });
    for i in 0..CAP_N {
        f.check_and_touch_at(mix64(i), 0, 1);
    }
    settle(&f);
    let (mut with, mut without) = (0i64, 0i64);
    for i in 0..PROBES {
        let h = mix64(CAP_N + i);
        let fp = fingerprint(h);
        let p = f.primary(h);
        let a = f.alt(p, fp);
        for (li, l) in [p, a].into_iter().enumerate() {
            if li == 1 && a == p {
                break;
            }
            let mut s = 0;
            while s < WORDS_PER_LINE {
                let w0 = ld(&f.words[(l * WORDS_PER_LINE + s) as usize]);
                s += 2;
                if w0 == 0 {
                    continue;
                }
                without += 1;
                if ((w0 & DISP_BIT) != 0) == (li == 1) {
                    with += 1;
                }
            }
        }
    }
    let (mw, mo) = (with as f64 / PROBES as f64, without as f64 / PROBES as f64);
    let two28 = (1u64 << 28) as f64;
    logf!(
        t,
        "placement compared_entries probes={PROBES} with_displaced_bit={mw:.4} without={mo:.4} expected_fpr_with={} without={}",
        go_e(mw / two28, 3),
        go_e(mo / two28, 3)
    );
    let (mut stored, mut displaced) = (0i64, 0i64);
    let mut i = 0;
    while i < f.words.len() {
        let w0 = ld(&f.words[i]);
        if w0 != 0 {
            stored += 1;
            if (w0 & DISP_BIT) != 0 {
                displaced += 1;
            }
        }
        i += 2;
    }
    logf!(t, "placement displaced_entries stored={stored} in_alternate_line={displaced} ({:.1}%)", 100.0 * displaced as f64 / stored as f64);
}
