//! ghost.go: the overflow table.
//!
//! Ghost table: an overflow store that keeps each dropped history as a
//! 21-bit entry (14-bit fingerprint, 5-bit event count, 2-bit time slice),
//! three to a word, in buckets of one cache line (8 words: a floor and 23
//! entries). It takes the count-min sketch's bytes, but a key's answer comes
//! from its own entries instead of counters shared with every other dropped
//! key, so a flood adds no noise: only a fingerprint collision (about live
//! entries / 16384 per bucket read) or a bucket's floor can raise a key that
//! never spilled.
//!
//! Each history has two candidate buckets and goes to the one with fewer live
//! entries, and a read covers both, so buckets fill evenly: no floor rose
//! with the table 93% full, where one bucket per history had floored
//! histories at 64% (verify/expected/flood-sizing.txt). Reading two buckets
//! doubles fingerprint matches (about 0.3% of keys once the table is full).
//! When both are full the lightest history in them (newcomer included)
//! leaves, and its bucket's floor keeps the largest count that left. A bucket
//! has three floors, 7 bits each in entry 0's place, and a history's
//! fingerprint picks its floor: every key reading the bucket adds its own
//! floor to Upper, so a raised floor reaches a third of the bucket's keys,
//! and Upper never reads below the truth for a key with at most one history
//! in a floor. A key floored more than once in the same floor can read low by
//! all but its largest floored history; summing floors instead closes that,
//! but light floods then push every floor to 31 and deny every new key
//! (docs/record/decisions.md). Floored counts the histories that left to a
//! floor: from the first, strict limits deny the keys that read it.
//!
//! Time is kept per entry as the slice (Horizon/2) of the history's newest
//! event, mod 4. epoch is the newest slice an add has seen; before an add
//! moves it on, entries that are dead by then are swept, so live entries
//! decode unambiguously against the current epoch (and only against it: see
//! add). All events of a history count as long as its newest one is inside
//! the window: Upper stays a bound, and Count reads a little high for windows
//! shorter than the horizon.

use std::sync::Mutex;
use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicBool, AtomicU64};

use crate::Result;
use crate::cpu::mul_hi;
use crate::hooks;
use crate::slots::*;

pub(crate) const GH_FP_BITS: u32 = 14;
pub(crate) const GH_CNT_BITS: u32 = 5;
pub(crate) const GH_SL_BITS: u32 = 2;
pub(crate) const GH_ENTRY_BITS: u32 = GH_FP_BITS + GH_CNT_BITS + GH_SL_BITS; // 21
pub(crate) const GH_PER_WORD: usize = 3;
pub(crate) const GH_WORDS: usize = 8; // per bucket: one cache line
pub(crate) const GH_CNT_MAX: u64 = (1 << GH_CNT_BITS) - 1;
pub(crate) const GH_ENTRY_MASK: u64 = (1 << GH_ENTRY_BITS) - 1;
pub(crate) const GH_SUB_BITS: u32 = GH_CNT_BITS + GH_SL_BITS; // a floor: count and slice, no fingerprint
pub(crate) const GH_SUB_MASK: u64 = (1 << GH_SUB_BITS) - 1;
pub(crate) const GH_SUB_MAX: i64 = (GH_ENTRY_BITS / GH_SUB_BITS) as i64; // floors that fit in entry 0

pub(crate) struct Ghost {
    pub(crate) words: AlignedWords,
    pub(crate) nb: u64,      // buckets
    pub(crate) choices: i64, // candidate buckets per history: 1, or 2 (the default)
    pub(crate) sub: i64,     // floors per bucket, picked by fingerprint: 1..3
    pub(crate) slice_len: u64,
    pub(crate) horizon: u64,
    pub(crate) mu: Mutex<()>,
    pub(crate) epoch: AtomicU64,   // newest slice + 1 seen by an add (0: none)
    pub(crate) until: AtomicU64,   // tick before which an entry may be live
    pub(crate) sat: AtomicBool,    // some entry saturated (count is a floor)
    pub(crate) floored: AtomicU64, // histories that left to a bucket's floor
}

#[inline(always)]
pub(crate) fn gh_entry(fp: u64, cnt: u64, sl: u64) -> u64 {
    (fp << (GH_CNT_BITS + GH_SL_BITS)) | (cnt << GH_SL_BITS) | (sl & 3)
}

#[inline(always)]
pub(crate) fn gh_fp(e: u64) -> u64 {
    e >> (GH_CNT_BITS + GH_SL_BITS)
}

#[inline(always)]
pub(crate) fn gh_cnt(e: u64) -> u64 {
    (e >> GH_SL_BITS) & GH_CNT_MAX
}

impl Ghost {
    /// newGhost sizes the table to bytes (rounded down to whole buckets).
    pub(crate) fn new(bytes: u64, horizon: u64, choices: i64, sub: i64) -> Ghost {
        let nb = 1.max(bytes / (GH_WORDS as u64 * 8));
        let w = AlignedWords::new((nb * GH_WORDS as u64) as usize);
        w.prefault();
        Ghost {
            words: w,
            nb,
            choices: 1.max(choices.min(2)),
            sub: 1.max(sub.min(GH_SUB_MAX)),
            slice_len: (horizon + 1) / 2,
            horizon,
            mu: Mutex::new(()),
            epoch: AtomicU64::new(0),
            until: AtomicU64::new(0),
            sat: AtomicBool::new(false),
            floored: AtomicU64::new(0),
        }
    }

    /// slot: id's candidate buckets (first word index of each), how many are
    /// distinct, and its fingerprint (never 0). The first bucket and the
    /// fingerprint are the one-choice table's.
    #[inline]
    pub(crate) fn slot(&self, id: u64) -> ([usize; 2], usize, u64) {
        let x = mix64(id ^ 0x5bd1e9955bd1e995);
        let b = mul_hi(x, self.nb);
        let mut fp = x & ((1 << GH_FP_BITS) - 1);
        if fp == 0 {
            fp = 1;
        }
        let mut base = [(b as usize) * GH_WORDS, (b as usize) * GH_WORDS];
        let mut nb = 1;
        if self.choices > 1 {
            let b2 = mul_hi(mix64(id ^ 0x2545f4914f6cdd1d), self.nb);
            if b2 != b {
                (base[1], nb) = ((b2 as usize) * GH_WORDS, 2);
            }
        }
        (base, nb, fp)
    }

    /// real decodes an entry's slice against epoch ep (newest slice + 1).
    #[inline(always)]
    pub(crate) fn real(&self, e: u64, ep: u64) -> u64 {
        let cur = ep.wrapping_sub(1);
        cur.wrapping_sub(cur.wrapping_sub(e) & 3)
    }

    /// live: slice sl's events can be within window of now.
    #[inline(always)]
    pub(crate) fn live(&self, sl: u64, window: u64, now: u64) -> bool {
        let end = sl.wrapping_mul(self.slice_len).wrapping_add(self.slice_len).wrapping_sub(1);
        age(now, end) <= window.min(self.horizon)
    }

    /// at returns entry k (0..23) of the bucket at base (its word's index
    /// and its shift there); k 0 holds the floors.
    #[inline(always)]
    pub(crate) fn at(&self, base: usize, k: usize) -> (usize, u32) {
        (base + k / GH_PER_WORD, (k % GH_PER_WORD) as u32 * GH_ENTRY_BITS)
    }

    /// floorShift is the bit offset, in a bucket's first word, of the floor
    /// that covers fingerprint fp.
    #[inline(always)]
    pub(crate) fn floor_shift(&self, fp: u64) -> u32 {
        (fp % self.sub as u64) as u32 * GH_SUB_BITS
    }

    /// floorOf is the live floor covering fp in the bucket at base, at epoch
    /// ep.
    pub(crate) fn floor_of(&self, base: usize, fp: u64, ep: u64, now: u64) -> u64 {
        let e = (ld(&self.words[base]) >> self.floor_shift(fp)) & GH_SUB_MASK;
        if e == 0 || !self.live(self.real(e, ep), self.horizon, now) {
            return 0;
        }
        gh_cnt(e)
    }

    /// add records a dropped history: n events, the newest at tick last.
    pub(crate) fn add(&self, id: u64, mut n: u64, last: u64, mut sat: bool, now: u64) {
        if n == 0 || age(now, last) > self.horizon {
            return;
        }
        // The epoch must reach the history's own slice, even when the
        // caller's clock is behind its newest event (an evictor with an older
        // clock): an entry from a slice past the epoch would decode four
        // slices old, dead to reads and free to the next add.
        let sl = last / self.slice_len;
        let ns = now.max(last) / self.slice_len;
        if self.epoch.load(SeqCst) < ns + 1 {
            self.advance(ns);
        }
        if sat || n >= GH_CNT_MAX {
            (n, sat) = (GH_CNT_MAX, true);
            self.sat.store(sat, SeqCst);
        }
        let u = sl * self.slice_len + self.slice_len - 1 + self.horizon;
        if self.until.load(SeqCst) < u {
            loop {
                let cur = self.until.load(SeqCst);
                if cur >= u || cas(&self.until, cur, u) {
                    break;
                }
            }
        }
        let (bases, nb, fp) = self.slot(id);
        'outer: loop {
            // Entries' 2-bit slices are decoded against ep, so a write is
            // committed only if ep is still the epoch: an entry another add
            // wrote after moving the epoch on would read four slices old,
            // dead, and be overwritten.
            let ep = self.epoch.load(SeqCst);
            if sl + 3 < ep {
                // older than the live slices (a caller far behind the others)
                return;
            }
            hooks::GHOST_ADDING.run(());
            // One pass over both buckets: a matching entry to merge into,
            // else the less loaded bucket's free or dead entry, else the
            // lightest live entry of either.
            let (mut free, mut live) = ([0i64; 2], [0i64; 2]);
            let (mut victim, mut vb) = (-1i64, 0usize);
            let (mut vc, mut vsl) = (GH_CNT_MAX + 1, 0u64);
            // Word by word: one load per word, its entries decoded from it in
            // order (word 0's first lane holds the floors); a word with every
            // entry empty is skipped, its first entry free.
            for bi in 0..nb {
                let base = bases[bi];
                free[bi] = -1;
                for wi in 0..GH_WORDS {
                    let w = &self.words[base + wi];
                    let old = ld(w);
                    let (mut k, mut sh) = ((wi * GH_PER_WORD) as i64, 0u32);
                    if wi == 0 {
                        (k, sh) = (1, GH_ENTRY_BITS);
                    }
                    if (old >> sh) == 0 {
                        if free[bi] < 0 {
                            free[bi] = k;
                        }
                        continue;
                    }
                    while sh < GH_PER_WORD as u32 * GH_ENTRY_BITS {
                        let e = (old >> sh) & GH_ENTRY_MASK;
                        if e == 0 || !self.live(self.real(e, ep), self.horizon, now) {
                            if free[bi] < 0 {
                                free[bi] = k;
                            }
                            (k, sh) = (k + 1, sh + GH_ENTRY_BITS);
                            continue;
                        }
                        live[bi] += 1;
                        if gh_fp(e) == fp {
                            let esl = self.real(e, ep).max(sl);
                            let ne = gh_entry(fp, GH_CNT_MAX.min(gh_cnt(e) + n), esl);
                            if self.epoch.load(SeqCst) == ep && cas(w, old, (old & !(GH_ENTRY_MASK << sh)) | (ne << sh)) {
                                return;
                            }
                            continue 'outer;
                        }
                        let c = gh_cnt(e);
                        if c < vc || (c == vc && self.real(e, ep) < vsl) {
                            (victim, vb, vc, vsl) = (k, bi, c, self.real(e, ep));
                        }
                        (k, sh) = (k + 1, sh + GH_ENTRY_BITS);
                    }
                }
            }
            let mut bi = -1i64;
            for i in 0..nb {
                if free[i] >= 0 && (bi < 0 || live[i] < live[bi as usize]) {
                    bi = i as i64;
                }
            }
            if bi >= 0 {
                let bi = bi as usize;
                let (wi, sh) = self.at(bases[bi], free[bi] as usize);
                let w = &self.words[wi];
                let old = ld(w);
                let e = (old >> sh) & GH_ENTRY_MASK;
                if e != 0 && self.live(self.real(e, ep), self.horizon, now) {
                    continue; // taken meanwhile
                }
                if self.epoch.load(SeqCst) == ep && cas(w, old, (old & !(GH_ENTRY_MASK << sh)) | (gh_entry(fp, n, sl) << sh)) {
                    return;
                }
                continue;
            }
            if self.epoch.load(SeqCst) != ep {
                continue;
            }
            // Full: the lightest history leaves (the newcomer if it is the
            // lightest) and its bucket's floor rises to cover it. The floor
            // goes up first, so no reader sees the history in neither place.
            if n <= vc {
                // The newcomer may go to either bucket's floor: the one
                // already higher, so a clean bucket stays clean.
                let mut fb = 0;
                if nb > 1 && self.floor_of(bases[1], fp, ep, now) > self.floor_of(bases[0], fp, ep, now) {
                    fb = 1;
                }
                self.raise_floor(bases[fb], fp, n, sl, now);
                return;
            }
            let (wi, sh) = self.at(bases[vb], victim as usize);
            let w = &self.words[wi];
            let old = ld(w);
            let e = (old >> sh) & GH_ENTRY_MASK;
            if gh_cnt(e) != vc || gh_fp(e) == fp {
                continue;
            }
            self.raise_floor(bases[vb], gh_fp(e), vc, self.real(e, ep), now);
            if self.epoch.load(SeqCst) == ep && cas(w, old, (old & !(GH_ENTRY_MASK << sh)) | (gh_entry(fp, n, sl) << sh)) {
                return;
            }
        }
    }

    /// raiseFloor lifts the bucket's floor covering fingerprint fp to at
    /// least n events, live until slice sl's events expire.
    pub(crate) fn raise_floor(&self, base: usize, fp: u64, n: u64, sl: u64, now: u64) {
        inc(&self.floored);
        let (w, sh) = (&self.words[base], self.floor_shift(fp));
        loop {
            let ep = self.epoch.load(SeqCst); // as in add: a floor written after the epoch moved must not read as dead
            hooks::GHOST_FLOOR.run(());
            let old = ld(w);
            let e = (old >> sh) & GH_SUB_MASK;
            let (mut c, mut fsl) = (0u64, sl);
            if e != 0 && self.live(self.real(e, ep), self.horizon, now) {
                (c, fsl) = (gh_cnt(e), sl.max(self.real(e, ep)));
            }
            let ne = gh_entry(0, c.max(n), fsl);
            if ne == e || (self.epoch.load(SeqCst) == ep && cas(w, old, (old & !(GH_SUB_MASK << sh)) | (ne << sh))) {
                return;
            }
        }
    }

    /// advance moves the epoch to slice ns, first clearing every entry that
    /// is dead by then (so the 2-bit slices of the rest decode
    /// unambiguously).
    pub(crate) fn advance(&self, ns: u64) {
        let _g = self.mu.lock().unwrap_or_else(std::sync::PoisonError::into_inner);
        let ep = self.epoch.load(SeqCst);
        if ep >= ns + 1 {
            return;
        }
        if ep != 0 {
            for i in 0..self.words.len() {
                loop {
                    let old = ld(&self.words[i]);
                    let mut nw = old;
                    for k in 0..GH_PER_WORD {
                        let (mut sh, mut n, mut mask) = (k as u32 * GH_ENTRY_BITS, 1, GH_ENTRY_MASK);
                        if k == 0 && i % GH_WORDS == 0 {
                            // the floors, each with its own slice
                            (n, mask) = (GH_SUB_MAX, GH_SUB_MASK);
                        }
                        for _ in 0..n {
                            let e = (old >> sh) & mask;
                            if e != 0 && self.real(e, ep).wrapping_add(2) < ns {
                                nw &= !(mask << sh);
                            }
                            sh += GH_SUB_BITS;
                        }
                    }
                    if nw == old || cas(&self.words[i], old, nw) {
                        break;
                    }
                }
            }
        }
        self.epoch.store(ns + 1, SeqCst);
    }

    /// estimate is id's dropped events within window: matching entries count
    /// in Count and Upper, the floors in Upper only. It loads both candidate
    /// lines whatever they hold, so its loads don't depend on the key
    /// (ConstantTime); decoding them does, inside the pad.
    ///
    /// The lines are decoded against the epoch they were loaded under: if an
    /// add moved it on meanwhile, they are loaded again. Against the new
    /// epoch, an entry its sweep cleared after the load decoded as four
    /// slices younger, so a history dead for a horizon read as live; against
    /// the old one, an entry written for the new epoch decodes as four slices
    /// older, dead (TestGhostReadAcrossEpoch). An add moves the epoch at most
    /// once per slice, so a read loads again at most once per slice.
    pub(crate) fn estimate(&self, id: u64, window: u64, now: u64) -> (Result, bool) {
        let (bases, nb, fp) = self.slot(id);
        let mut line = [[0u64; GH_WORDS]; 2];
        let mut ep;
        loop {
            ep = self.epoch.load(SeqCst);
            if ep == 0 || now > self.until.load(SeqCst) {
                return (Result::default(), false);
            }
            let mut empty = true;
            for bi in 0..nb {
                let base = bases[bi];
                for i in 0..GH_WORDS {
                    let w = ld(&self.words[base + i]);
                    line[bi][i] = w;
                    if w != 0 {
                        empty = false;
                    }
                }
            }
            // Both buckets empty: nothing to decode against the wrong epoch,
            // so no re-check. Only advance's sweep clears an entry, and only
            // a dead one.
            if empty {
                return (Result::default(), false);
            }
            hooks::GHOST_READ.run(());
            if self.epoch.load(SeqCst) == ep {
                break;
            }
        }
        // raiseFloor counts a floor before it writes it, and floored is
        // loaded after the words: a load that holds a floor sees floored > 0.
        // Entries 1 and 2 share word 0 with the floors; words 1-7 hold three
        // each (TestGhostScanLayout).
        let mut r = Result::default();
        let has_floor = self.floored.load(SeqCst) > 0;
        // An entry of the key's: its count in Count and Upper.
        let own = |r: &mut Result, e: u64| {
            if e != 0 && gh_fp(e) == fp && self.live(self.real(e, ep), window, now) {
                let c = gh_cnt(e) as i64;
                r.upper += c;
                r.count += c;
                r.saturated = r.saturated || c == GH_CNT_MAX as i64;
            }
        };
        for l in line.iter().take(nb) {
            if has_floor {
                let e = (l[0] >> self.floor_shift(fp)) & GH_SUB_MASK;
                if e != 0 && self.live(self.real(e, ep), window, now) {
                    let c = gh_cnt(e) as i64;
                    r.upper += c;
                    r.saturated = r.saturated || c == GH_CNT_MAX as i64;
                }
            }
            let w0 = l[0];
            if (w0 >> GH_ENTRY_BITS) != 0 {
                own(&mut r, (w0 >> GH_ENTRY_BITS) & GH_ENTRY_MASK);
                own(&mut r, (w0 >> (2 * GH_ENTRY_BITS)) & GH_ENTRY_MASK);
            }
            for &lw in &l[1..] {
                if lw == 0 {
                    continue;
                }
                own(&mut r, lw & GH_ENTRY_MASK);
                own(&mut r, (lw >> GH_ENTRY_BITS) & GH_ENTRY_MASK);
                own(&mut r, (lw >> (2 * GH_ENTRY_BITS)) & GH_ENTRY_MASK);
            }
        }
        if r.upper == 0 {
            return (Result::default(), false);
        }
        r.seen = r.count > 0;
        (r, true)
    }
}
