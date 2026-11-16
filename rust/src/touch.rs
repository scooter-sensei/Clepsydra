//! touch.go: touch, count, read, lookupStable, the overflow read, realign and
//! its 16-byte CAS.

use std::sync::MutexGuard;
use std::sync::atomic::Ordering::SeqCst;
use std::thread::yield_now;
use std::time::Instant;

use crate::Result;
use crate::cpu;
use crate::ct::OP_COUNT;
use crate::decay::DECAY_EVERY;
use crate::filter::{Core, check_tick};
use crate::hooks;
use crate::hot::is_hot;
use crate::slots::*;
use crate::stash::{SRC_RETRY, SRC_STASH, SRC_TABLE};

/// ctInside, as touch's i0: the call is touchCT's.
pub(crate) const CT_INSIDE: isize = -2;

impl Core {
    /// count is CountAt; i0, if not -1, is a slot where a lookup just found
    /// h (warm), as for touch. In ConstantTime its work is padded (ctBegin,
    /// ctEnd), and sketchLive reads the overflow table whatever it holds.
    pub(crate) fn count(&self, h: u64, window: u64, now: u64, i0: isize) -> Result {
        let mut timing = None;
        if self.ct {
            let (t0, pad) = self.ct_begin(OP_COUNT, now);
            timing = Some((t0, Instant::now(), pad));
        }
        let (w0, w1, src) = self.read(h, now, i0);
        let mut r = Result::default();
        if src != 0 && age(now, w0 & TS_MASK) <= self.horizon {
            r = self.estimate_at(h, w1, w0 & TS_MASK, now, window, (w0 & SAT_BIT) != 0);
            r.seen = r.count > 0;
        } else {
            r.degraded = self.lost_recently(now);
        }
        if self.sketch_live(now) {
            self.add_sketch(&mut r, h, window, now);
        }
        if let Some((t0, s, pad)) = timing {
            self.ct_end(OP_COUNT, h, t0, s, pad);
        }
        r
    }

    /// estimateAt is estimate for key h, whose register (or hot entry) is in
    /// w1 and whose lastTouch is last. If w1 points to an entry the key gave
    /// up meanwhile (a touch demoted it), the key's slot is looked up again,
    /// without its stripe (the caller may hold it). If it can't be found, the
    /// answer fails closed.
    #[inline]
    pub(crate) fn estimate_at(&self, h: u64, mut w1: u64, mut last: u64, now: u64, window: u64, mut sat: bool) -> Result {
        if !is_hot(w1) {
            // A register's estimate never fails and reads no id (viewOf
            // needs it only for a hot entry): the loop's first answer.
            return self.estimate(0, w1, last, now, window, sat).0;
        }
        let (fp, tg) = (fingerprint(h), tag(h));
        let id = self.stash_id(h, fp, tg);
        for _ in 0..8 {
            let (r, ok) = self.estimate(id, w1, last, now, window, sat);
            if ok {
                return r;
            }
            let (i, w0, nw1) = self.lookup_stable(h, fp, tg, false);
            if i < 0 {
                break;
            }
            (w1, last, sat) = (nw1, last.max(w0 & TS_MASK), (w0 & SAT_BIT) != 0);
        }
        Result { count: 1, upper: 1, saturated: true, ..Result::default() }
    }

    /// read returns h's slot words from the table or, if it was evicted, the
    /// stash, and where they came from (0: neither). i0, if not -1, is a slot
    /// where a lookup just found h (warm): the first look reads it instead of
    /// scanning, and a miss is concluded only from a scan.
    pub(crate) fn read(&self, h: u64, now: u64, i0: isize) -> (u64, u64, i32) {
        let (fp, tg) = (fingerprint(h), tag(h));
        if !self.ct {
            // lookupStable, inline: one scan whether the key is found or not.
            // The stash gate is read between the scan and the stability
            // check: a waiting key that leaves the wait room for the table
            // counts as a move of its stripe (placeRoomKey), so if no move
            // began by the check, the key was still waiting when the gate was
            // read.
            let m = self.move_of(tg);
            let mut try_ = 0;
            loop {
                let (b, d) = Core::moves_bd(m);
                let (mut i, mut fpd) = (i0, fp << 1); // warm finds a key undisplaced in its primary line
                if try_ > 0 || i < 0 {
                    (i, fpd) = self.locate_at(h, fp, tg);
                }
                let (w0, w1, ok) = self.slot_words(i, fpd, tg);
                if ok {
                    return (w0, w1, SRC_TABLE);
                }
                let may = self.stash_may(h, fp, tg, now);
                if i < 0 && b == d && m.begun.load(SeqCst) == b {
                    if !may {
                        return (0, 0, 0);
                    }
                    break;
                }
                if try_ > 0 {
                    yield_now();
                }
                try_ += 1;
            }
        }
        // Under the key's stripe: an eviction moves the key to the stash
        // while holding it, so the key is found in one of the two.
        let _mu = self.claim_mu[(h >> (64 - CLAIM_STRIPE_BITS)) as usize].lock();
        let m = self.move_of(tg);
        loop {
            let (b, d) = Core::moves_bd(m);
            let (i, w0, w1) = self.lookup_stable(h, fp, tg, true);
            let (s0, s1, src) = self.recall(h, fp, tg, now, false);
            if src == SRC_RETRY {
                continue;
            }
            if i >= 0 {
                return (w0, w1, SRC_TABLE);
            }
            if src == 0 && !self.settled(m, b, d) {
                continue;
            }
            return (s0, s1, src);
        }
    }

    /// lookupStable returns the index and words of h's slot, or -1 if h has
    /// none, reading both lines if both (ConstantTime). A lookup scans one
    /// line and then the other, so an entry that a relocation moves into the
    /// line already scanned and out of the other in between is missed by both
    /// scans; an eviction likewise takes a key out of the table before its
    /// history reaches the stash. A miss is trusted only if no such move of a
    /// key on h's tag stripe (moveOf) began or ended around the scan. Moves
    /// never wait on a lookup, so the retry loop ends.
    pub(crate) fn lookup_stable(&self, h: u64, fp: u64, tg: u64, both: bool) -> (isize, u64, u64) {
        let m = self.move_of(tg);
        loop {
            let (b, d) = Core::moves_bd(m);
            let (i, fpd) = if both { self.locate_both(h, fp, tg) } else { self.locate_at(h, fp, tg) };
            if i >= 0 {
                let (w0, w1, ok) = self.slot_words(i, fpd, tg);
                if ok {
                    return (i, w0, w1);
                }
            } else if b == d && m.begun.load(SeqCst) == b {
                return (-1, 0, 0);
            }
            yield_now();
        }
    }

    /// sketchLive: the overflow table may hold a count at now. ConstantTime
    /// reads it whatever it holds.
    #[inline]
    pub(crate) fn sketch_live(&self, now: u64) -> bool {
        if !self.cms {
            return match self.gh.get() {
                Some(g) => self.ct || now <= g.until.load(SeqCst),
                None => false,
            };
        }
        match self.sk.get() {
            Some(sk) => self.ct || now <= sk.until.load(SeqCst),
            None => false,
        }
    }

    /// addSketch adds h's events in the overflow table to r, the answer from
    /// the table or stash. The overflow holds only histories the stash
    /// dropped, and a key that comes back starts a fresh slot, so the two
    /// never count the same event. Upper takes the overflow's bound, Count
    /// the key's own entry (the count-min sketch: the part that clears the
    /// noise), and Degraded is set when there is one: the key likely has a
    /// dropped history.
    pub(crate) fn add_sketch(&self, r: &mut Result, h: u64, window: u64, now: u64) {
        let id = self.stash_id(h, fingerprint(h), tag(h));
        let (s, ok) = if !self.cms {
            self.gh.get().expect("the overflow table").estimate(id, window.min(self.horizon), now)
        } else {
            self.sk.get().expect("the overflow sketch").estimate(id, window.min(self.horizon), now, self.ct)
        };
        if !ok {
            return;
        }
        r.count += s.count;
        r.upper += s.upper;
        r.saturated = r.saturated || s.saturated;
        r.seen = r.seen || s.seen;
        r.degraded = r.degraded || s.count > 0;
    }

    /// touch is TouchAt. i0, if not -1, is a slot where a lookup just found
    /// h (warm): its first look starts there if the slot still holds h. i0
    /// ctInside is touchCT's call: every attempt takes h's stripe and makes
    /// the same lookups whatever h's state (both lines, the stash), then
    /// counts in h's slot or inserts it, the stripe held throughout.
    ///
    /// The count is the slot protocol's one copy, inline here: called as a
    /// function it cost the reference's cache-resident hit 1.05x and the
    /// touch batch 1.08x (placement-randomized, 6 layouts, cycles/op).
    ///
    /// A register is read at the unit of the slot's last touch (word0), so
    /// the two must change unit together where a touch or a read can see the
    /// pair (claims D29). A touch inside the last touch's unit moves word0
    /// lock-free and counts with bump, which reads the register against word0
    /// as it is then. A touch that starts a unit moves word0 and the register
    /// together (realign): by one 16-byte CAS of both words, or else under
    /// word1 busy. Before, the register kept 4 bits of its unit and only a
    /// jump of 8 units or more went through realign (a touch that moved word0
    /// first, the register after, then read a register at a lag by its stored
    /// unit; before realign, a second touch of the key between those two
    /// writes read the old register as the new unit's, and the first then
    /// aged that touch's event by the gap, or, past the horizon, reset it
    /// away: claims A21). A hot entry keeps its own full unit: its key's
    /// touches count lock-free, and one that starts a unit moves word0 only
    /// while word1 still points to the entry (stamp), so that a demotion,
    /// which puts a register at its unit there, and the touch can't cross;
    /// one that finds the slot past the horizon resets through realign.
    /// Before, it moved word0, then reset word1 by a CAS from the hot
    /// pointer; a touch after its word0 found the key live and added to the
    /// entry, which leaves word1 as it was, and the reset dropped that event
    /// (claims A30).
    pub(crate) fn touch(&self, h: u64, window: u64, now: u64, want_count: bool, i0: isize) -> Result {
        let inside = i0 == CT_INSIDE; // the reference's local ct
        if !inside {
            if (h & DECAY_EVERY) == 0 {
                self.decay_step(now, 1);
            }
            if self.ct {
                return self.touch_ct(h, window, now, want_count);
            }
        }
        let (fp, tg) = (fingerprint(h), tag(h));
        let stripe = (h >> (64 - CLAIM_STRIPE_BITS)) as usize;
        let mut held: Option<MutexGuard<'_, ()>> = None; // ConstantTime: h's stripe, while held
        let mut first = true;
        loop {
            let was_first = first;
            first = false;
            // released between attempts: a claim that fails on another
            // stripe's TryLock must not hold its own while it retries
            drop(held.take());
            let (mut i, mut fpd) = (-1isize, 0u64); // fpd: what the lookup matched in the slot (word0>>47)
            if inside {
                held = Some(self.claim_mu[stripe].lock());
                let (li, w0, _) = self.lookup_stable(h, fp, tg, true);
                i = li;
                let (s0, s1, src) = self.recall(h, fp, tg, now, true);
                if i < 0 {
                    let mut r = self.miss(h, fp, tg, window, now, want_count, s0, s1, src);
                    if r.count < 0 {
                        continue;
                    }
                    drop(held.take());
                    if self.sketch_live(now) {
                        self.add_sketch(&mut r, h, window, now);
                    }
                    return r;
                }
                if src == SRC_STASH {
                    // a stale copy: the table's entry is newer
                    self.stash_put(self.stash_id(h, fp, tg), s0, s1, now);
                }
                fpd = w0 >> 47;
            }
            if was_first && !inside && i0 >= 0 {
                // (slotWords of no slot is no match: skip the call)
                let (_, _, ok) = self.slot_words(i0, fp << 1, tg);
                if ok {
                    // warm finds a key undisplaced in its primary line
                    (i, fpd) = (i0, fp << 1);
                }
            }
            if i < 0 {
                (i, fpd) = self.locate_at(h, fp, tg);
            }
            if i < 0 {
                // Miss: serialize first touches of the same key so two racers
                // can't each claim a slot (which would split its count). The
                // lock covers only this path; hits never take it. A tick past
                // MaxTick panics before it: a caller that recovers keeps a
                // usable filter.
                check_tick(now);
                let mu = self.claim_mu[stripe].lock();
                let m = self.move_of(tg);
                let (b, d) = Core::moves_bd(m);
                let (li, w0, _) = self.lookup_stable(h, fp, tg, false);
                i = li;
                if i < 0 {
                    let (s0, s1, src) = self.recall(h, fp, tg, now, true);
                    if src == SRC_RETRY || (src == 0 && !self.settled(m, b, d)) {
                        drop(mu);
                        continue;
                    }
                    let mut r = self.miss(h, fp, tg, window, now, want_count, s0, s1, src);
                    drop(mu);
                    if r.count < 0 {
                        continue;
                    }
                    if self.sketch_live(now) {
                        self.add_sketch(&mut r, h, window, now);
                    }
                    return r;
                }
                drop(mu); // another first touch got there first: count as a hit
                fpd = w0 >> 47;
            }
            // The count: slot i held h when it was found.
            let w0 = ld(self.w(i));
            if (w0 >> 47) != fpd {
                continue; // slot was reclaimed, or handed to another key, between find and load; retry
            }
            let last = w0 & TS_MASK;
            let stale = age(now, last) > self.horizon;
            let (w1, rf, over, ok);
            let (nu, lu) = (self.unit_of(now), self.unit_of(last));
            let (mut old, mut far, mut wait) = (0u64, false, false);
            if nu > lu {
                // else farAhead's answer is (0, false, false): skip the call
                (old, far, wait) = self.far_ahead(i, nu, lu, stale);
            }
            if wait {
                yield_now();
                continue; // another touch is realigning the slot
            } else if far {
                if cpu::has_cas2() && (old >> TAG_SHIFT) == tg && !is_hot(old) {
                    // realign's usual case, without its call
                    check_tick(now);
                    (w1, over, ok) = self.realign2_u(i, w0, old, tg, now, nu, lu);
                } else {
                    (w1, over, ok) = self.realign(h, i, w0, old, tg, now, nu, lu);
                }
                rf = now;
            } else {
                if nu > lu {
                    // a hot key into a new unit: word0 moves only while word1 still points to the entry
                    hooks::STAMPING.run(());
                    if !self.stamp(i, w0, old, tg, now) {
                        continue;
                    }
                } else if now > last && !cas(self.w(i), w0, (w0 & !TS_MASK) | now) {
                    continue;
                }
                if nu > lu {
                    hooks::STAMPED.run(());
                }
                (w1, rf, over, ok) = self.bump(i, fpd, tg, now, nu, last, lu, stale);
            }
            if !ok {
                continue; // the slot was handed to another key, or moved on, meanwhile; look again
            }
            let mut sat = (w0 & SAT_BIT) != 0;
            if over != sat {
                sat = self.mark_sat(i, fpd, over);
            }
            let mut r = Result::default();
            if want_count {
                r = self.estimate_at(h, w1, now.max(rf), now, window, sat);
            }
            r.seen = !stale && age(now, last) <= window;
            drop(held.take());
            // CheckAndTouch asks only for Seen, which addSketch can set but
            // never clear: a hit already Seen skips the overflow read.
            // ConstantTime reads the overflow table whatever it holds.
            if self.sketch_live(now) && (want_count || !r.seen || self.ct) {
                self.add_sketch(&mut r, h, window, now);
            }
            return r;
        }
    }

    /// farAhead: a touch in unit nu would move slot i's last touch (in unit
    /// lu) to a later unit, and word1 (old) must move with it: a register
    /// always (realign), a hot entry's pointer only when the key is past the
    /// horizon (a reset; else the entry keeps its own full unit and the touch
    /// moves word0 alone, by stamp). wait: word1 is busy, held by a writer
    /// that may publish a register at the old unit.
    ///
    /// Returns (old, far, wait).
    #[inline]
    pub(crate) fn far_ahead(&self, i: isize, nu: i64, lu: i64, stale: bool) -> (u64, bool, bool) {
        if nu <= lu {
            return (0, false, false);
        }
        let old = ld(self.w(i + 1));
        if (old >> TAG_SHIFT) == TAG_BUSY {
            return (old, false, true);
        }
        if is_hot(old) {
            return (old, stale, false);
        }
        (old, true, false)
    }

    /// stamp moves slot i's last touch (w0) to now, in a later unit, while
    /// word1 still holds p, a hot entry's pointer of tag tg: by one 16-byte
    /// CAS of both words, or else under a hold of word1. A demotion meanwhile
    /// has put a register there, at word0's unit, which the last touch must
    /// not leave without it (demoteSlot pins that unit the same way). False:
    /// a word changed; look again.
    pub(crate) fn stamp(&self, i: isize, w0: u64, p: u64, tg: u64, now: u64) -> bool {
        let n0 = (w0 & !TS_MASK) | now;
        if cpu::has_cas2() {
            return cpu::cas2(&self.words[i as usize..i as usize + 2], w0, p, n0, p);
        }
        let m = self.move_of(tg);
        inc(&m.begun);
        let _done = Defer(|| inc(&m.done));
        if !cas(self.w(i + 1), p, BUSY) {
            return false;
        }
        let ok = cas(self.w(i), w0, n0);
        st(self.w(i + 1), p);
        ok
    }

    /// realign is touch's count of a touch at now in a later unit than slot
    /// i's last touch (word0 w0) and register (old), or past the horizon of
    /// the slot of a hot key (old points to its entry, which it gives up and
    /// starts afresh): word0 moves to now and word1 to the register, advanced
    /// to now's unit with the touch counted. With CMPXCHG16B (x86-64) both
    /// move by one CAS (realign2). Elsewhere word1 goes busy, word0 moves,
    /// then word1 is published; while word1 is busy, a lookup that misses the
    /// slot looks again: the hold counts as a move of the key's stripe. ok is
    /// false if the slot changed meanwhile.
    ///
    /// Returns (w1, over, ok).
    #[expect(clippy::too_many_arguments, reason = "the reference's signature, parameter for parameter")]
    pub(crate) fn realign(&self, _h: u64, i: isize, w0: u64, old: u64, tg: u64, now: u64, nu: i64, lu: i64) -> (u64, bool, bool) {
        check_tick(now);
        if (old >> TAG_SHIFT) != tg || (is_hot(old) && age(now, w0 & TS_MASK) <= self.horizon) {
            return (0, false, false);
        }
        if cpu::has_cas2() {
            return self.realign2_u(i, w0, old, tg, now, nu, lu);
        }
        let m = self.move_of(tg);
        inc(&m.begun);
        let _done = Defer(|| inc(&m.done));
        if !cas(self.w(i + 1), old, BUSY) {
            return (0, false, false);
        }
        if !cas(self.w(i), w0, (w0 & !TS_MASK) | now) {
            st(self.w(i + 1), old);
            return (0, false, false);
        }
        hooks::STAMPED.run(());
        let last = w0 & TS_MASK;
        let (mut w1, mut over) = self.next_reg_u(old, tg, last, now, lu, nu, age(now, last) > self.horizon);
        if over {
            let (pw, ok) = self.promote_held(i, w0, old, tg, last, now);
            if ok {
                (w1, over) = (pw, false);
            }
        }
        st(self.w(i + 1), w1);
        if is_hot(old) {
            self.slot_drop(self.slot_id((i as u64) / WORDS_PER_LINE, w0, old), old); // a stale hot key's reset
        }
        (w1, over, true)
    }

    /// realign2 is realign by one 16-byte CAS of both words from w0, old
    /// (claims A33): word1 never goes busy, so no lookup looks again and no
    /// move is counted, and a touch whose two loads straddle the CAS fails
    /// its own CAS from what it read. The CAS compares word0 too: a slot
    /// handed meanwhile to a key of old's tag can hold old bit for bit
    /// (promote). A promotion takes its entry first and publishes it by the
    /// same CAS, and closes it again if the CAS fails.
    #[cfg(all(test, target_arch = "x86_64"))] // cas2_test's, as Go's cas2_amd64_test.go
    pub(crate) fn realign2(&self, i: isize, w0: u64, old: u64, tg: u64, now: u64) -> (u64, bool, bool) {
        self.realign2_u(i, w0, old, tg, now, self.unit_of(now), self.unit_of(w0 & TS_MASK))
    }

    /// realign2U is realign2 with now's unit nu and the last touch's, lu.
    #[inline]
    #[expect(clippy::too_many_arguments, reason = "the reference's signature, parameter for parameter")]
    pub(crate) fn realign2_u(&self, i: isize, w0: u64, old: u64, tg: u64, now: u64, nu: i64, lu: i64) -> (u64, bool, bool) {
        let last = w0 & TS_MASK;
        let n0 = (w0 & !TS_MASK) | now;
        let (mut w1, mut over) = self.next_reg_u(old, tg, last, now, lu, nu, age(now, last) > self.horizon);
        let (mut idx, mut id) = (0u64, 0u64);
        if over {
            let (pw, x, y) = self.hot_entry_for(i, w0, old, tg, last, now);
            if pw != 0 {
                (w1, over, idx, id) = (pw, false, x, y);
            }
        }
        if !cpu::cas2(&self.words[i as usize..i as usize + 2], w0, old, n0, w1) {
            if idx != 0 {
                self.hot_close(idx, id); // never published
            }
            return (0, false, false);
        }
        if idx != 0 {
            inc(&self.promoted);
        }
        if is_hot(old) {
            self.slot_drop(self.slot_id((i as u64) / WORDS_PER_LINE, w0, old), old); // a stale hot key's reset
        }
        hooks::STAMPED.run(());
        (w1, over, true)
    }
}
