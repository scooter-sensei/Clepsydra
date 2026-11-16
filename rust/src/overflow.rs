//! overflow.go: spill, the overflow table's allocation, and the count-min
//! sketch the ghost table replaced (behind `Core::cms`, for the experiments
//! and the answer digest).
//!
//! Count-min overflow sketch: where a history went when the stash had to drop
//! it, before the ghost table (ghost.rs) replaced it as the default (it is
//! kept for the experiments that compare the two, Core::cms). It is a
//! count-min sketch over (key identity, time slice): every dropped event is
//! added to one counter per row. Collisions only add other keys' events, so
//! the smallest of a key's counters is never below its true count (Upper),
//! and no flood can erase a history before it ages out. Count is the
//! count-mean-min estimate (Deng & Rafiei 2007): each counter less the other
//! keys' events expected in it, the median over rows. It is unbiased, so a
//! flood raises keys that never spilled by noise around 0, not by the flood's
//! average load. Slices are Horizon/parts long; a ring of parts+2 covers the
//! horizon plus the slice being written, and a ring position is cleared when
//! a newer slice takes it. The default shape (4 rows, 8-bit counters,
//! Horizon/2 slices) packs a column's ring into one word; it beat 16-bit
//! counters and Horizon/4 slices at two thirds of the memory
//! (docs/artifact/claims.md A9).
//!
//! Adds are lock-free (a CAS per row); only handing a ring position to a new
//! slice takes the mutex. Reads are lock-free; counters only grow while their
//! slice is live.

use std::sync::Mutex;
use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicU32, AtomicU64};

use crate::Result;
use crate::cpu::mul_hi;
use crate::filter::Core;
use crate::ghost::Ghost;
use crate::hot::{SAT_NONE, View, hot_idx, is_hot};
use crate::register::{N_BUCKETS, bucket_start, hot};
use crate::slots::*;

pub(crate) const MAX_ROWS: usize = 4;
pub(crate) const MAX_SLICES: usize = 6;
/// sigmas: a slice's count-mean-min estimate counts only above this many
/// standard deviations of its noise, so keys that never spilled read exact
/// Counts, and a spilled one at most this much low.
pub(crate) const SIGMAS: f64 = 3.0;

/// sketchShape is the sketch's geometry: rows, counter bits (8 or 16), and
/// slices per horizon (the ring holds parts+2).
#[derive(Clone, Copy, Default, PartialEq, Eq, Debug)]
pub(crate) struct SketchShape {
    pub(crate) rows: i64,
    pub(crate) bits: i64,
    pub(crate) parts: i64,
}

pub(crate) const DEFAULT_SHAPE: SketchShape = SketchShape { rows: 4, bits: 8, parts: 2 };

impl SketchShape {
    /// bytes is the memory of a sketch of width w; 0 for the zero shape,
    /// which a filter with the overflow table off has (its snapshot's shape
    /// says 0).
    pub(crate) fn bytes(&self, w: u64) -> u64 {
        if self.bits == 0 {
            return 0;
        }
        let per = (32 / self.bits) as u64;
        (self.rows as u64) * w * ((self.parts as u64 + 2 + per - 1) / per) * 4
    }
}

pub(crate) struct Sketch {
    pub(crate) cells: Box<[AtomicU32]>, // [row][column][slice / per], bits per slice
    pub(crate) w: u64,                  // columns per row
    pub(crate) rows: usize,
    pub(crate) bits: u64,
    pub(crate) per: u64,       // counters per word
    pub(crate) nsl: u64,       // ring slices
    pub(crate) cw: u64,        // words per column
    pub(crate) cmask: u32,     // counter value mask
    pub(crate) csat: u32,      // counter's saturated flag: its count is a floor
    pub(crate) slice_len: u64, // ticks
    pub(crate) horizon: u64,
    pub(crate) mu: Mutex<()>,
    pub(crate) epochs: [AtomicU64; MAX_SLICES],  // slice number + 1 held by each ring position
    pub(crate) totals: [AtomicU64; MAX_SLICES],  // events added to each slice (the same in every row)
    pub(crate) squares: [AtomicU64; MAX_SLICES], // sum of each add's n squared: the noise's variance × w
    /// until is a tick before which some slice may hold a live count.
    pub(crate) until: AtomicU64,
}

impl Sketch {
    pub(crate) fn new(w: u64, horizon: u64, sh: SketchShape) -> Sketch {
        let w = w.max(2);
        let per = (32 / sh.bits) as u64;
        let nsl = (sh.parts + 2) as u64;
        let cw = (nsl + per - 1) / per;
        let parts = sh.parts as u64;
        Sketch {
            cells: (0..sh.rows as u64 * w * cw).map(|_| AtomicU32::new(0)).collect(),
            w,
            rows: sh.rows as usize,
            bits: sh.bits as u64,
            per,
            nsl,
            cw,
            cmask: (1u32 << (sh.bits - 1)) - 1,
            csat: 1u32 << (sh.bits - 1),
            slice_len: (horizon + parts - 1) / parts,
            horizon,
            mu: Mutex::new(()),
            epochs: Default::default(),
            totals: Default::default(),
            squares: Default::default(),
            until: AtomicU64::new(0),
        }
    }

    /// col is id's counter group in row r.
    #[inline]
    pub(crate) fn col(&self, id: u64, r: usize) -> u64 {
        let c = mul_hi(mix64(id.wrapping_add((r as u64).wrapping_mul(0x9e3779b97f4a7c15))), self.w);
        (r as u64 * self.w + c) * self.cw
    }

    /// cell loads the counter of ring position p in the group at base.
    #[inline]
    pub(crate) fn cell(&self, base: u64, p: u64) -> u32 {
        (self.cells[(base + p / self.per) as usize].load(SeqCst) >> (self.bits * (p % self.per))) & (self.csat | self.cmask)
    }

    /// add counts n events of id at tick t (sat: the count is a floor).
    pub(crate) fn add(&self, id: u64, t: u64, n: u64, sat: bool, now: u64) {
        if n == 0 || age(now, t) > self.horizon {
            return;
        }
        let sl = t / self.slice_len;
        let p = sl % self.nsl;
        let e = self.epochs[p as usize].load(SeqCst);
        if e != sl + 1 {
            if e > sl + 1 {
                return; // a newer slice took this place: t is past the horizon
            }
            self.renew(p, sl);
        }
        let sh = self.bits * (p % self.per);
        let full = self.csat | self.cmask;
        for r in 0..self.rows {
            let w = &self.cells[(self.col(id, r) + p / self.per) as usize];
            loop {
                let old = w.load(SeqCst);
                let c = (old >> sh) & full;
                let v = u64::from(c & self.cmask) + n;
                let mut nc = (v.min(u64::from(self.cmask)) as u32) | (c & self.csat);
                if sat || v >= u64::from(self.cmask) {
                    nc |= self.csat;
                }
                if w.compare_exchange(old, (old & !(full << sh)) | (nc << sh), SeqCst, SeqCst).is_ok() {
                    break;
                }
            }
        }
        self.totals[p as usize].fetch_add(n, SeqCst);
        self.squares[p as usize].fetch_add(n.wrapping_mul(n), SeqCst);
        let u = sl * self.slice_len + self.slice_len - 1 + self.horizon;
        if self.until.load(SeqCst) < u {
            loop {
                let cur = self.until.load(SeqCst);
                if cur >= u || cas(&self.until, cur, u) {
                    break;
                }
            }
        }
    }

    /// renew hands ring position p to slice sl, clearing it. Readers skip p
    /// meanwhile. An add that checked p's epoch just before can still land
    /// after the clear: an expired slice's events then count in the new one,
    /// which only raises answers.
    pub(crate) fn renew(&self, p: u64, sl: u64) {
        let _g = self.mu.lock().unwrap_or_else(std::sync::PoisonError::into_inner);
        if self.epochs[p as usize].load(SeqCst) >= sl + 1 {
            return;
        }
        self.epochs[p as usize].store(0, SeqCst);
        let keep = !((self.csat | self.cmask) << (self.bits * (p % self.per)));
        let mut i = p / self.per;
        while i < self.cells.len() as u64 {
            self.cells[i as usize].fetch_and(keep, SeqCst);
            i += self.cw;
        }
        self.totals[p as usize].store(0, SeqCst);
        self.squares[p as usize].store(0, SeqCst);
        self.epochs[p as usize].store(sl + 1, SeqCst);
    }

    /// estimate is id's events in the window ending at now, as Result
    /// fields: Upper is the count-min bound, with the slice at the window's
    /// edge whole; Count the count-mean-min estimate where it clears the
    /// noise, with that slice pro rata; Seen if Count rounds to at least 1.
    /// ok is false if no slice in the window has a count. full reads every
    /// row whatever it finds (ConstantTime); otherwise the first row is read
    /// alone and is usually enough to say no.
    pub(crate) fn estimate(&self, id: u64, window: u64, now: u64, full: bool) -> (Result, bool) {
        let cur = now / self.slice_len;
        let mut oldest = 0u64;
        if cur >= self.nsl - 1 {
            oldest = cur - (self.nsl - 2);
        }
        let mut c = [[0u64; MAX_ROWS]; MAX_SLICES];
        let mut sat = [true; MAX_SLICES];
        for row in 0..self.rows {
            let base = self.col(id, row);
            let mut any = false;
            for sl in oldest..=cur {
                let p = (sl % self.nsl) as usize;
                let v = self.cell(base, p as u64);
                c[p][row] = u64::from(v & self.cmask);
                sat[p] = sat[p] && (v & self.csat) != 0;
                any = any || (v != 0 && self.epochs[p].load(SeqCst) == sl + 1);
            }
            if !any && !full {
                return (Result::default(), false);
            }
        }
        let mut r = Result::default();
        let from = (now as i64).wrapping_sub(window as i64);
        let mut est = 0.0f64;
        for sl in oldest..=cur {
            let p = (sl % self.nsl) as usize;
            let (start, end) = ((sl * self.slice_len) as i64, (sl * self.slice_len + self.slice_len - 1).min(now) as i64);
            if self.epochs[p].load(SeqCst) != sl + 1 || end < from || age(now, end as u64) > self.horizon {
                continue;
            }
            let lo = *c[p][..self.rows].iter().min().expect("a sketch has rows");
            if lo == 0 {
                continue;
            }
            r.upper += lo as i64;
            r.saturated = r.saturated || sat[p];
            // Count-mean-min: each row less the noise expected in its counter.
            let total = self.totals[p].load(SeqCst) as f64;
            let mut d = [0.0f64; MAX_ROWS];
            for (row, &v) in c[p][..self.rows].iter().enumerate() {
                d[row] = v as f64 - (total - v as f64) / (self.w - 1) as f64;
            }
            d[..self.rows].sort_by(|a, b| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal));
            let mut m = ((d[(self.rows - 1) / 2] + d[self.rows / 2]) / 2.0).min(lo as f64);
            // Other keys' events arrive in lumps (a history at a time), so
            // the noise's variance is the sum of the lumps' squares over w.
            if m < SIGMAS * (self.squares[p].load(SeqCst) as f64 / self.w as f64).sqrt() {
                m = 0.0; // within the noise: other keys' events could explain it
            }
            if start >= from {
                est += m;
            } else {
                est += m * (end - from + 1) as f64 / (end - start + 1) as f64;
            }
        }
        if r.upper == 0 {
            return (Result::default(), false);
        }
        r.count = (est.max(0.0).min(r.upper as f64) + 0.5) as i64;
        r.seen = r.count > 0;
        (r, true)
    }
}

impl Core {
    /// spill sends a history the stash is dropping (identity id, slot words
    /// w0, w1) to the overflow table, or, without one, records it as lost. A
    /// hot history's entry is closed and its last state spilled: a touch that
    /// found the key's slot before the history left it adds by the key's
    /// identity, and its add then fails and it looks again, instead of
    /// counting in an entry nothing points to (claims A32). A live history
    /// with no events in the horizon (its touch has moved word0 and not yet
    /// counted, which it then does afresh) is neither spilled nor lost. Nor
    /// is one whose hot entry is no longer its own: the entry was reclaimed
    /// past the horizon (as a restore treats it: hotGone), or its history was
    /// already spilled or is back in a slot, so nothing of it is left here,
    /// with the overflow table or without.
    pub(crate) fn spill(&self, id: u64, w0: u64, w1: u64, now: u64) {
        let last = w0 & TS_MASK;
        if age(now, last) > self.horizon {
            return;
        }
        let v: View;
        if is_hot(w1) {
            let (hv, ok) = self.hot_close(hot_idx(w1), id);
            if !ok {
                return;
            }
            v = hv;
        } else {
            (v, _) = self.view_of(id, w1, last);
        }
        let m = self.view_mass(&v, now);
        if m == 0 {
            return; // no events in the horizon yet: the touch that moved word0 counts afresh
        }
        if self.sk_w == 0 {
            self.dropped(now.max(last)); // its events may be in a window until a horizon after its newest one
            return;
        }
        if self.spill_min > 1 && m < self.spill_min {
            inc(&self.shed);
            return;
        }
        let sat = ((w0 & SAT_BIT) != 0 && hot(w1)) || (v.hot && v.sat != SAT_NONE);
        if !self.cms {
            inc(&self.folded);
            self.ghost_table().add(id, m, last, sat, now);
            return;
        }
        let sk = self.overflow();
        inc(&self.folded);
        let e = v.e;
        let mut top = ((e + 1) as u64).wrapping_mul(self.unit);
        for j in 0..N_BUCKETS {
            let lo = bucket_start(e, j);
            let c = v.c[j];
            if c > 0 && top > 0 {
                // The bucket's newest tick (lastTouch for the newest):
                // placing its events as late as possible only widens the
                // windows that see them.
                sk.add(id, (top - 1).min(last), c, sat, now);
            }
            if lo <= 0 {
                break;
            }
            top = (lo as u64) * self.unit;
        }
    }

    /// overflow returns the overflow sketch, allocating it on the first
    /// spill: a filter that never drops a history never pays for it.
    pub(crate) fn overflow(&self) -> &Sketch {
        self.sk.get_or_init(|| Sketch::new(self.sk_w, self.horizon, self.sk_shape))
    }

    /// ghostTable returns the ghost overflow table, allocating it on the
    /// first spill, with the bytes the sketch would take.
    pub(crate) fn ghost_table(&self) -> &Ghost {
        self.gh.get_or_init(|| Ghost::new(self.sk_shape.bytes(self.sk_w), self.horizon, self.gh_choices, self.gh_sub))
    }

    /// Floored counts histories the overflow table had no room for: each
    /// left to a floor of its bucket, which then raises Upper for every key
    /// sharing that floor, so from the first one strict limits turn such keys
    /// away. It stays 0 below the table's capacity (a heavy flood of 4x the
    /// table's slots at the defaults). Alert on it.
    pub(crate) fn floored(&self) -> u64 {
        self.gh.get().map_or(0, |g| g.floored.load(SeqCst))
    }
}
