//! register.go: the velocity register, bump, unpoint, strayed, mass,
//! estimate.
//!
//! Velocity register: 7 counters packed into the low 48 bits of word1.
//!
//! In units of u = Horizon/unitsPerHorizon, with U the current unit index,
//! bucket j covers [b_j, b_{j-1}) where b_{-1} = +inf, b_0 = U and, for
//! j >= 1,
//!
//! ```text
//! b_j = floor(U / s_j) * s_j - m_j     (s_j a power of two, nondecreasing)
//! ```
//!
//! Every boundary is a multiple of its own s_j and moves only when U crosses
//! one, so as U advances each old bucket lands whole inside one new bucket.
//! Advancing any distance is therefore a single re-bucketing pass keyed by
//! each old bucket's start: no per-tick work and no sweep. Widths grow with
//! age (fine resolution near now) and then stay flat so long windows keep
//! their resolution. Counter widths grow with bucket width.

use std::sync::atomic::Ordering::SeqCst;

use crate::Result;
use crate::cpu::{self, mul_hi};
use crate::filter::Core;
use crate::hooks;
use crate::hot::{SAT_NONE, View, is_hot};
use crate::slots::*;

pub(crate) const N_BUCKETS: usize = 7;
pub(crate) const UNITS_PER_HORIZON: u64 = GEOM_UNITS;
pub(crate) const REG_MASK: u64 = (1 << 48) - 1;

/// Geometry (see the comment at the top): unit = Horizon/24; widths 1, 1,
/// 1-2, 2-4, 4-8, 8, 8 units; coverage >= 24 units.
pub(crate) const GEOM_UNITS: u64 = 24;
pub(crate) const BUCKET_BITS0: u32 = 5;

/// satPerUnit is the newest bucket's capacity: a unit with more events
/// overflows, and every bucket that overflows holds at least this many.
pub(crate) const SAT_PER_UNIT: u64 = (1 << BUCKET_BITS0) - 1;

pub(crate) const BUCKET_LOG_STEP: [u32; N_BUCKETS] = [0, 0, 1, 2, 3, 3, 3];
pub(crate) const BUCKET_OFF: [i64; N_BUCKETS] = [0, 1, 2, 4, 8, 16, 24];
pub(crate) const BUCKET_BITS: [u32; N_BUCKETS] = [BUCKET_BITS0, 5, 6, 7, 8, 8, 9];

pub(crate) const BUCKET_SHIFT: [u32; N_BUCKETS] = {
    let mut sh = [0u32; N_BUCKETS];
    let mut j = 1;
    while j < N_BUCKETS {
        sh[j] = sh[j - 1] + BUCKET_BITS[j - 1];
        j += 1;
    }
    sh
};

/// bucketBase maps a register bit to the shift of the bucket holding it.
pub(crate) const BUCKET_BASE: [u8; 48] = {
    let mut b = [0u8; 48];
    let mut j = 0;
    while j < N_BUCKETS {
        let mut k = BUCKET_SHIFT[j];
        while k < BUCKET_SHIFT[j] + BUCKET_BITS[j] {
            b[k as usize] = BUCKET_SHIFT[j] as u8;
            k += 1;
        }
        j += 1;
    }
    b
};

pub(crate) const BUCKET_STEP_MASK: [i64; N_BUCKETS] = {
    let mut m = [0i64; N_BUCKETS];
    let mut j = 0;
    while j < N_BUCKETS {
        m[j] = !((1i64 << BUCKET_LOG_STEP[j]) - 1);
        j += 1;
    }
    m
};

pub(crate) type Register = [u64; N_BUCKETS];

// unpack, pack, hot and viewOf write the layout of bucketBits and bucketShift
// out in literals; TestRegisterLayoutLiterals holds them to it.
#[inline(always)]
pub(crate) fn unpack(w1: u64) -> Register {
    [w1 & 0x1F, (w1 >> 5) & 0x1F, (w1 >> 10) & 0x3F, (w1 >> 16) & 0x7F, (w1 >> 23) & 0xFF, (w1 >> 31) & 0xFF, (w1 >> 39) & 0x1FF]
}

#[inline(always)]
pub(crate) fn pack(r: &Register) -> u64 {
    r[0] | (r[1] << 5) | (r[2] << 10) | (r[3] << 16) | (r[4] << 23) | (r[5] << 31) | (r[6] << 39)
}

/// add puts n events into bucket j, spilling into older buckets when it is
/// full: the count is preserved and the event is aged at most one bucket.
/// Reports whether any of the n didn't fit in bucket j (spilled or, with
/// every older bucket full too, dropped).
pub(crate) fn reg_add(r: &mut Register, mut j: usize, mut n: u64) -> bool {
    let overflow = n > (1u64 << BUCKET_BITS[j]) - 1 - r[j];
    while j < N_BUCKETS && n > 0 {
        let room = (1u64 << BUCKET_BITS[j]) - 1 - r[j];
        let take = room.min(n);
        r[j] += take;
        n -= take;
        j += 1;
    }
    overflow
}

/// bucketStart is b_j in units (may be negative near U = 0).
#[inline(always)]
pub(crate) fn bucket_start(u: i64, j: usize) -> i64 {
    (u & BUCKET_STEP_MASK[j]) - BUCKET_OFF[j]
}

/// advance re-buckets r from unit u0 to unit u1 >= u0, reporting whether a
/// merge overflowed its new bucket.
pub(crate) fn reg_advance(r: &mut Register, u0: i64, u1: i64) -> bool {
    if u1 <= u0 {
        return false;
    }
    if u1 - u0 > 2 * UNITS_PER_HORIZON as i64 {
        *r = [0; N_BUCKETS];
        return false;
    }
    let mut starts = [0i64; N_BUCKETS];
    for (j, s) in starts.iter_mut().enumerate() {
        *s = bucket_start(u1, j);
    }
    let mut out: Register = [0; N_BUCKETS];
    let mut overflow = false;
    let mut j = 0;
    for k in 0..N_BUCKETS {
        if r[k] == 0 {
            continue;
        }
        let start = bucket_start(u0, k);
        while j < N_BUCKETS && start < starts[j] {
            j += 1;
        }
        if j == N_BUCKETS {
            break; // older buckets only start earlier: all dropped
        }
        overflow = reg_add(&mut out, j, r[k]) || overflow;
    }
    *r = out;
    overflow
}

// The register's unit is the unit of the slot's last touch (word0). It stores
// none: a touch that starts a unit moves the last touch and the register
// together (realign: one 16-byte CAS, or a hold of word1), and every other
// writer of word1 keeps the pair at one unit (claims D29). So a reader that
// loads the two words of one moment reads the register at the right unit, and
// a register is never read at a lag. Before, word1 kept 4 bits of its unit
// and was read against word0's (a touch moved word0, then word1, with a CAS
// each), and a touch 8 or more units on held the slot.
//
// The 4 bits that held the unit hold the newest bucket's onset instead: where
// in its unit its first event fell, in sixteenths, written by the event that
// starts the bucket (lowered by a caller whose clock is behind it). estimate
// spreads that bucket's events from the onset to the last touch, not from the
// unit's start. 0 says nothing: a bucket that took an event counted from an
// earlier unit (a lagging clock), a register restored from a snapshot that
// had none.

/// lowerOnset is w1 with its onset lowered to on, if on is earlier.
#[inline(always)]
pub(crate) fn lower_onset(mut w1: u64, on: u64) -> u64 {
    if on < ((w1 >> ONSET_SHIFT) & ONSET_MASK) {
        w1 = (w1 & !(ONSET_MASK << ONSET_SHIFT)) | (on << ONSET_SHIFT);
    }
    w1
}

/// hot reports whether some bucket of w1 holds satPerUnit or more events. A
/// bucket that overflowed (or took a spill-causing merge) holds at least that
/// many, and its count only grows until it ages out of the register, so while
/// a slot's saturated bit is set some bucket is hot.
#[inline]
pub(crate) fn hot(w1: u64) -> bool {
    if is_hot(w1) {
        return false; // a hot entry has no saturation of its own (its sat unit says what is missing)
    }
    (w1 & 0x1F) >= SAT_PER_UNIT
        || ((w1 >> 5) & 0x1F) >= SAT_PER_UNIT
        || ((w1 >> 10) & 0x3F) >= SAT_PER_UNIT
        || ((w1 >> 16) & 0x7F) >= SAT_PER_UNIT
        || ((w1 >> 23) & 0xFF) >= SAT_PER_UNIT
        || ((w1 >> 31) & 0xFF) >= SAT_PER_UNIT
        || ((w1 >> 39) & 0x1FF) >= SAT_PER_UNIT
}

impl Core {
    /// onsetOf is where the tick now falls in its unit u, in sixteenths.
    #[inline(always)]
    pub(crate) fn onset_of(&self, now: u64, u: i64) -> u64 {
        self.unit_of(16u64.wrapping_mul(now.wrapping_sub((u as u64).wrapping_mul(self.unit)))) as u64
    }

    /// nextReg returns word1 after counting one event at now in old, the
    /// register of key tg last touched at last; the result is at the unit of
    /// max(now, last), the last touch the slot will hold. reset (or a tag
    /// mismatch) starts a fresh register. A full register in the current unit
    /// comes back as is. overflow reports that an event spilled out of its
    /// bucket or was dropped (the slot's saturated bit must then be set).
    pub(crate) fn next_reg(&self, old: u64, tg: u64, last: u64, now: u64, reset: bool) -> (u64, bool) {
        self.next_reg_u(old, tg, last, now, self.unit_of(last), self.unit_of(now), reset)
    }

    /// nextRegU is nextReg with last's and now's units, lastU and nowU.
    #[inline]
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn next_reg_u(&self, old: u64, tg: u64, last: u64, now: u64, last_u: i64, now_u: i64, reset: bool) -> (u64, bool) {
        if !reset && (old >> TAG_SHIFT) == tg && last_u == now_u {
            // Fast path, same unit: increment the first bucket with room in
            // place (spilling as add does). Buckets are contiguous from bit
            // 0, so the lowest zero bit of the register lies in that bucket.
            // The onset stays: an event at or after the last touch is after
            // it.
            let free = !old & REG_MASK;
            if free == 0 {
                return (old, true); // every bucket full
            }
            let b = free.trailing_zeros();
            let mut w1 = old + (1u64 << BUCKET_BASE[b as usize]);
            if now < last {
                // a caller behind the last touch may be behind the onset too
                w1 = lower_onset(w1, self.onset_of(now, now_u));
            }
            return (w1, b >= BUCKET_BITS0);
        }
        let mut r: Register = [0; N_BUCKETS];
        let mut overflow = false;
        let e = now_u.max(last_u);
        let keep = !reset && (old >> TAG_SHIFT) == tg;
        if keep {
            r = unpack(old & REG_MASK);
            if now_u > last_u {
                overflow = reg_advance(&mut r, last_u, now_u);
            } // else a caller with a stale clock: count at e
        }
        let mut on = 0u64; // an event from a unit before e: its bucket's onset says nothing
        if now_u == e && (!keep || now_u > last_u) {
            on = self.onset_of(now, now_u); // the event starts the newest bucket
        }
        overflow = reg_add(&mut r, 0, 1) || overflow;
        ((tg << TAG_SHIFT) | (on << ONSET_SHIFT) | pack(&r), overflow)
    }

    /// bump counts one event at now in the register of the slot at word
    /// index i, which must still hold the caller's key: word0's fingerprint
    /// and displaced bit fpd, word1's tag tg. It reports false if the slot
    /// has been handed to another key since the caller found it. word0 is
    /// checked after word1 is read: a hand-over makes word1 busy before it
    /// changes word0, so an old value that passes both checks belonged to the
    /// caller's key when word0 was read. The CAS that counts comes after, and
    /// compares word1 alone: a slot handed meanwhile to a key with the same
    /// tag can hold a register bit-identical to the one read (two keys' first
    /// events in one unit), and the CAS then counts there. word0, read again
    /// after it, and word1 tell (strayed): bump then reports false, and the
    /// caller counts the event for its key afresh (the other key reads one
    /// high). Before, the event was the key's no more (claims A29). A key
    /// whose register would overflow is promoted to a hot entry instead
    /// (hot.rs); a promoted key's events go to its entry and word1 stays as
    /// it is.
    ///
    /// The register's epoch is read against word0's last touch as loaded
    /// here, after word1 (ref, returned), not the caller's: a realign since
    /// the caller's read moved both, and a pair loaded across one fails the
    /// CAS. now's unit is at most ref's (a later unit goes through realign).
    /// nu is now's unit, last and lu the caller's last touch and its unit:
    /// ref is usually one of the two.
    ///
    /// Returns (w1, ref, overflow, ok).
    #[inline]
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn bump(&self, i: isize, fpd: u64, tg: u64, now: u64, nu: i64, last: u64, lu: i64, mut reset: bool) -> (u64, u64, bool, bool) {
        loop {
            let old = ld(self.w(i + 1));
            let w0 = ld(self.w(i));
            if (old >> TAG_SHIFT) != tg || (w0 >> 47) != fpd {
                return (0, 0, false, false);
            }
            let rf = w0 & TS_MASK;
            if is_hot(old) {
                let id = self.slot_id((i as u64) / WORDS_PER_LINE, w0, old);
                if !reset {
                    hooks::HOT_ADDING.run(i);
                    if self.hot_add(old, id, now, i) {
                        return (old, rf, false, true);
                    }
                    if ld(self.w(i + 1)) != old {
                        continue; // word1 changed (a demotion, or the slot changed hands): look again
                    }
                }
                // The entry aged out with the key's history (or the caller
                // resets it): start afresh, at the unit word0 has when the
                // register replaces the pointer.
                let (nw, rf, ok) = self.unpoint(i, w0, old, fpd, tg, now, nu);
                if ok {
                    if reset {
                        self.slot_drop(id, old);
                    }
                    return (nw, rf, false, true);
                }
                reset = false; // a word changed: a concurrent touch may have reset it; merge
                continue;
            }
            let ref_u = if rf == last {
                lu
            } else if rf == now {
                nu // the caller's own CAS
            } else {
                self.unit_of(rf)
            };
            let (nw, over) = self.next_reg_u(old, tg, rf, now, ref_u, nu, reset);
            if over {
                let (pw, ok) = self.promote(i, w0, old, tg, rf, now);
                if ok {
                    return (pw, rf, false, true);
                }
            }
            hooks::BUMPING.run(i);
            if nw == old {
                return (nw, rf, over, true);
            }
            if cas(self.w(i + 1), old, nw) {
                hooks::BUMPED.run(i);
                if (ld(self.w(i)) >> 47) != fpd && self.strayed(i, tg) {
                    return (0, 0, false, false); // the count went to another key: count afresh
                }
                return (nw, rf, over, true);
            }
            reset = false; // a concurrent touch already reset it; merge
        }
    }

    /// unpoint replaces slot i's word1, old, a hot pointer whose entry is
    /// gone, by a fresh register holding the event at now (unit nu), at the
    /// unit of the last touch word0 has when it goes in: by one 16-byte CAS
    /// from w0 and old, or else under a hold of word1, word0 read under it. A
    /// plain CAS of word1 would not do: a touch of the key into a later unit
    /// moves word0 while word1 still points to the entry (touch's stamp), and
    /// the register would be read at that unit. ok is false if a word
    /// changed: look again.
    ///
    /// Returns (w1, ref, ok).
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn unpoint(&self, i: isize, mut w0: u64, old: u64, fpd: u64, tg: u64, now: u64, nu: i64) -> (u64, u64, bool) {
        if cpu::has_cas2() {
            let rf = w0 & TS_MASK;
            let (w1, _) = self.next_reg_u(old, tg, rf, now, self.unit_of(rf), nu, true);
            return (w1, rf, cpu::cas2(&self.words[i as usize..i as usize + 2], w0, old, w0, w1));
        }
        let m = self.move_of(tg);
        inc(&m.begun);
        let _done = Defer(|| inc(&m.done));
        if !cas(self.w(i + 1), old, BUSY) {
            return (0, 0, false);
        }
        w0 = ld(self.w(i));
        if (w0 >> 47) != fpd {
            st(self.w(i + 1), old);
            return (0, 0, false);
        }
        let rf = w0 & TS_MASK;
        let (w1, _) = self.next_reg_u(old, tg, rf, now, self.unit_of(rf), nu, true);
        st(self.w(i + 1), w1);
        (w1, rf, true)
    }

    /// strayed reports whether bump's count, a CAS of slot i's word1 (word
    /// index) from a register of the key with tag tg, may have gone to
    /// another key: the caller has read word0 after the CAS, and it is no
    /// longer the key's. Either the slot changed hands before the CAS, to a
    /// key whose register was bit-identical to the one read, so of tag tg,
    /// and the count went to that key; or it changed hands after, and the
    /// count went with the key's history (to the stash, or the key's new
    /// slot), and the slot now holds a key of another tag (but one time in
    /// 4096). word1 tells them apart once any hold of the slot is over: a
    /// holder may put back the register it took (realign, the sweep, an
    /// install that lost word0), so strayed waits out a busy word1 (holds
    /// never wait on touches) and decides on the load it waited on. A later
    /// hand-over to a key of tag tg reads as the first case, and the event is
    /// counted twice (the key reads one high); a same-tag key handed on to a
    /// key of another tag within the touch's window, as the second (one low).
    pub(crate) fn strayed(&self, i: isize, tg: u64) -> bool {
        loop {
            let w1 = ld(self.w(i + 1));
            if (w1 >> TAG_SHIFT) != TAG_BUSY {
                return w1 != 0 && (w1 >> TAG_SHIFT) == tg; // 0: a free slot (0 is also a tag)
            }
            std::thread::yield_now();
        }
    }

    /// mass is the event count of the key with identity id (register or hot
    /// entry in w1, lastTouch last) in buckets that still reach inside the
    /// horizon (for eviction scoring).
    pub(crate) fn mass(&self, id: u64, w1: u64, last: u64, now: u64) -> u64 {
        if age(now, last) > self.horizon {
            return 0;
        }
        if is_hot(w1) {
            let (v, ok) = self.view_of(id, w1, last);
            if !ok {
                return 0;
            }
            return self.view_mass(&v, now);
        }
        // Not hot: viewMass's walk without the view, each counter read from
        // word1 in place (viewOf's layout); with only bucket 0 filled, bucket
        // 0 alone (estimate's early case).
        let reg = w1 & REG_MASK;
        if reg == 0 {
            return 0;
        }
        let e = self.unit_of(last);
        let oldest = (now - now.min(self.horizon)) as i64 / self.unit as i64; // as viewMass
        if e + 1 <= oldest {
            return 0;
        }
        if (reg >> BUCKET_BITS0) == 0 {
            return reg;
        }
        let mut n = reg & ((1 << BUCKET_BITS0) - 1);
        let mut top = bucket_start(e, 0);
        let mut j = 1;
        while j < N_BUCKETS && top > oldest {
            n += (reg >> BUCKET_SHIFT[j]) & ((1u64 << BUCKET_BITS[j]) - 1);
            top = bucket_start(e, j);
            j += 1;
        }
        n
    }

    /// viewMass is mass for a register state already read.
    pub(crate) fn view_mass(&self, v: &View, now: u64) -> u64 {
        let oldest = (now - now.min(self.horizon)) as i64 / self.unit as i64; // unit of now-H, rounded down
        let mut n = 0u64;
        let mut top = v.e + 1;
        for j in 0..N_BUCKETS {
            if top <= oldest {
                break;
            }
            n += v.c[j];
            top = bucket_start(v.e, j);
        }
        n
    }

    /// estimate interpolates the count in (now-window, now] straight from
    /// the buckets as they were laid out at the register's own unit: no
    /// re-bucketing to now, so no resolution is lost to merging. Two facts
    /// from word0 make it exact at the extremes: nothing happened after last,
    /// so a window ending before it holds zero and one reaching it holds at
    /// least one; and the newest non-empty bucket's events all lie in [its
    /// start, last], one of them at last. The one bucket the window's
    /// trailing edge cuts is assumed uniform for Count (if it is the newest,
    /// the event at last counts whole and the rest are spread: velocity MAE
    /// 0.0807 -> 0.0779; from the bucket's onset, when it has one: 0.0779 ->
    /// 0.0671) and counted whole for Upper.
    ///
    /// sat is the slot's saturated bit: some event overflowed its bucket
    /// within the register's span. The bucket it overflowed from still covers
    /// its time and is hot (see hot), so if no bucket the window reaches is
    /// hot, no event in the window was moved or dropped and Upper bounds the
    /// true count. A hot entry (w1 points to one; id is the key's identity)
    /// never overflows; it is Saturated only for windows reaching back to the
    /// unit it was promoted in, if the register it came from had overflowed.
    /// ok is false if w1 points to an entry that is no longer the key's (a
    /// touch demoted it meanwhile).
    #[inline]
    pub(crate) fn estimate(&self, id: u64, w1: u64, last: u64, now: u64, window: u64, sat: bool) -> (Result, bool) {
        let window = window.min(self.horizon);
        if age(now, last) > window {
            return (Result::default(), true);
        }
        // A register with only bucket 0 filled, whose window reaches back to
        // bucket 0's start (or whose one event is now), counts bucket 0
        // whole, at least the last touch: the view's answer below, without
        // the view.
        if !is_hot(w1) {
            let reg = w1 & REG_MASK;
            if (reg >> BUCKET_BITS0) == 0 {
                let c0 = reg & ((1 << BUCKET_BITS0) - 1);
                if c0 == 1 && now == last && window > 0 {
                    return (Result { count: 1, upper: 1, saturated: false, ..Result::default() }, true);
                }
                let e = self.unit_of(last);
                if (now.wrapping_sub(window) as i64) <= e.wrapping_mul(self.unit as i64) {
                    let n = 1.max(c0 as i64);
                    return (Result { count: n, upper: n, saturated: sat && c0 >= SAT_PER_UNIT, ..Result::default() }, true);
                }
            }
        }
        let (v, ok) = self.view_of(id, w1, last);
        if !ok {
            return (Result::default(), false);
        }
        let e = v.e;
        let unit = self.unit as f64;
        let from = now as f64 - window as f64;
        let mut top = (e + 1) as f64 * unit;
        let mut newest = true;
        let (mut est, mut upper, mut hit) = (0.0f64, 0u64, false);
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
            hit = hit || c >= SAT_PER_UNIT;
            upper += c;
            let mut a = 0.0f64; // events known to lie inside the window, counted whole
            if newest {
                hi = lo.max((last as f64).min(hi));
                newest = false;
                if from < hi {
                    a = 1.0; // one of them lies exactly at last
                }
            }
            if from <= lo {
                est += c as f64;
                continue;
            }
            if j == 0 && v.on != 0 && (c as f64) > a {
                // The newest bucket's events lie from its onset to the last
                // touch (hi), not from the unit's start: all inside a window
                // that starts at or before the onset, else spread over
                // [onset, hi] (claims D29).
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
                est += a + (c as f64 - a) * (hi - from) / (hi - lo);
            }
            break;
        }
        let n = 1.max((est + 0.5) as i64);
        let mut r = Result { count: n, upper: n.max(upper as i64), saturated: sat && hit, ..Result::default() };
        if v.hot {
            r.saturated = v.sat != SAT_NONE && from < (v.sat + 1) as f64 * unit;
        }
        (r, true)
    }

    /// unitOf is t / f.unit via a precomputed reciprocal: one multiply
    /// instead of a 64-bit divide on every touch.
    #[inline(always)]
    pub(crate) fn unit_of(&self, t: u64) -> i64 {
        let mut q = mul_hi(t, self.unit_inv);
        if q.wrapping_mul(self.unit) > t {
            q -= 1;
        }
        q as i64
    }

    /// `m.begun.Load()`, `m.done.Load()`: a stripe's move counts.
    #[inline(always)]
    pub(crate) fn moves_bd(m: &MoveCount) -> (u64, u64) {
        (m.begun.load(SeqCst), m.done.load(SeqCst))
    }
}
