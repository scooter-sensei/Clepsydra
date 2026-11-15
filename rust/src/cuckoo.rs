//! cuckoo.go: miss, claim, claimAt, eviction, install, relocation, abandon.

use std::hint::black_box;
use std::sync::atomic::Ordering::SeqCst;

use crate::Result;
use crate::filter::Core;
use crate::hooks;
use crate::hot::is_hot;
use crate::register::hot;
use crate::slots::*;
use crate::stash::{DEFER_MARK, SRC_ROOM, SRC_STASH, STASH_WAYS};

/// relocDepth bounds the cuckoo path an insert into two full lines may take
/// before evicting.
pub(crate) const RELOC_DEPTH: i32 = 3;
/// relocBudget caps the alternate lines one insert may scan. The usable part
/// above the first 8 shrinks with pressure (see claim).
pub(crate) const RELOC_BUDGET: u64 = 500;
pub(crate) const PRESSURE_SHIFT: u32 = 16;
pub(crate) const PRESSURE_ONE: u32 = 1 << PRESSURE_SHIFT;

/// noStripe, as claimAt's stripe: the caller holds no claim stripe.
pub(crate) const NO_STRIPE: u64 = 1 << CLAIM_STRIPE_BITS;

// claimAt modes: free slot, path or eviction (claimFull); free slot only,
// else claimWait (claimDefer).
pub(crate) const CLAIM_FULL: i32 = 0;
pub(crate) const CLAIM_DEFER: i32 = 1;
pub(crate) const CLAIM_WAIT: isize = -2;

/// putFunc installs a new key's entry in slot i (word index) if the slot
/// still holds w0/w1, as install does; a Background placement's put also
/// takes the key out of the wait room as it goes in. `(i, w0, w1, mine0,
/// mine1)`.
pub(crate) type PutFunc<'a> = Option<&'a dyn Fn(isize, u64, u64, u64, u64) -> bool>;

impl Core {
    /// miss installs h, absent from the table, with this touch counted: a
    /// fresh register, or its evicted history from the stash. Count < 0 means
    /// it lost a race for a slot and the caller should look again. The caller
    /// holds h's claim stripe, so no other first touch of h runs meanwhile.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn miss(&self, h: u64, fp: u64, tg: u64, window: u64, now: u64, want_count: bool, s0: u64, s1: u64, src: i32) -> Result {
        let mut r = Result::default();
        // Taken out of the wait room (srcRoom): its move ends once it is placed.
        let room = src == SRC_ROOM;
        let (mut w1, _) = self.next_reg(0, tg, now, now, true); // first event, published with the key
        let (mut last, mut sat) = (now, 0u64);
        let mut hot_restore = false;
        if src != 0 && age(now, s0 & TS_MASK) <= self.horizon && !self.hot_gone(self.stash_id(h, fp, tg), s1) {
            last = s0 & TS_MASK;
            if is_hot(s1) {
                // it keeps its hot entry; the touch is counted once it is installed
                (w1, hot_restore) = (s1, true);
            } else {
                let over;
                (w1, over) = self.next_reg(s1, tg, last, now, false);
                if over || ((s0 & SAT_BIT) != 0 && hot(w1)) {
                    sat = SAT_BIT;
                }
            }
            r.seen = age(now, last) <= window;
            r.restored = (s0 & DEFER_MARK) == 0; // a key that waited for a slot was never evicted
        } else if self.lost_recently(now) {
            r.degraded = true;
        }
        // A recalled history keeps its last touch even when this caller's
        // clock is behind it (the register counted this touch in that unit
        // too): stamped with the older clock, a later window starting between
        // the two would miss the history's newest event.
        let stamp = now.max(last);
        let mut mode = CLAIM_FULL;
        if self.ct || self.bg {
            mode = CLAIM_DEFER;
        }
        let mut ci = self.claim(h, fp, w1, now, stamp, sat, mode);
        if ci == CLAIM_WAIT {
            // No free slot: wait for one, in the stash (ConstantTime; pads
            // place it) or the wait room (Background; the placer thread
            // places it). If more than one key per 16 wait-room entries
            // waits, the placer isn't keeping up: search now.
            if (self.ct || self.deferred.load(SeqCst) <= (self.room_n * STASH_WAYS / 16) as i64)
                && self.stash_wait(self.stash_id(h, fp, tg), (fp << 48) | sat | stamp, w1, now)
            {
                // An evicted history that waits came back all the same: this
                // touch is Restored (and counted once; the touch that
                // installs it later recalls a waiting entry, which isn't).
                inc(&self.waited);
                if self.bg {
                    self.bg_kick();
                }
            } else {
                // its set is full: search (and evict) now rather than drop a history
                ci = self.claim(h, fp, w1, now, stamp, sat, CLAIM_FULL);
            }
        }
        if ci == -1 && (src == SRC_STASH || room) {
            // put it back for the retry
            self.put_back(self.stash_id(h, fp, tg), s0, s1, now);
        }
        if room {
            // in the table, or back in the room or the stash
            inc(&self.move_of(tg).done);
        }
        if ci == -1 {
            return Result { count: -1, ..Result::default() };
        }
        if hot_restore && !self.hot_add(w1, self.stash_id(h, fp, tg), now, -1) {
            // The entry was reclaimed since hotGone: look again. The key's
            // slot (or wait) now points to another key's entry; the next look
            // finds that and counts this touch afresh (bump's reset, or
            // hotGone).
            return Result { count: -1, ..Result::default() };
        }
        if src == SRC_STASH && r.restored {
            inc(&self.restored);
        }
        if !want_count {
            return r;
        }
        let mut e = self.estimate_at(h, w1, stamp, now, window, sat != 0);
        (e.seen, e.restored, e.degraded) = (r.seen, r.restored, r.degraded);
        e
    }

    /// claim installs fp in an empty or expired slot of the primary line,
    /// then the alternate line. If all eight are live it tries to relocate
    /// one of them to its own alternate line (one cuckoo step). Only then
    /// does it evict, taking the slot with the fewest events in the horizon
    /// (oldest on ties), so a spray of fresh keys cannot flush a repeat
    /// offender. w1 is the new key's tagged register, published together with
    /// its word0. Returns word0 index, or -1 if it lost a race for a slot and
    /// the caller should look the key up again (a racing writer may have
    /// completed it). The caller holds the key's claim stripe. With mode
    /// claimDefer (ConstantTime and Background) it searches and evicts
    /// nothing: with no free slot it returns claimWait, and the key waits for
    /// a slot (miss). The entry's last touch is stamp; now is the caller's
    /// clock (expiry, eviction scores).
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn claim(&self, h: u64, fp: u64, w1: u64, now: u64, stamp: u64, sat: u64, mode: i32) -> isize {
        let p = self.primary(h);
        self.claim_at(
            [p, self.alt(p, fp)],
            [(fp << 48) | sat | stamp, (fp << 48) | DISP_BIT | sat | stamp],
            w1,
            now,
            h >> (64 - CLAIM_STRIPE_BITS),
            mode,
            None,
        )
    }

    /// claimAt is claim for an entry with lines and words mine (primary,
    /// alternate), on claim stripe stripe, which the caller holds (noStripe:
    /// none). put installs the entry (None: install; see putNew).
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn claim_at(&self, lines: [u64; 2], mine: [u64; 2], w1: u64, now: u64, stripe: u64, mode: i32, put: PutFunc<'_>) -> isize {
        for (li, &line) in lines.iter().enumerate() {
            let base = (line * WORDS_PER_LINE) as isize;
            let mut s = 0;
            while s < WORDS_PER_LINE as isize {
                let w0 = ld(self.w(base + s));
                if w0 == 0 || age(now, w0 & TS_MASK) > self.horizon {
                    hooks::OPEN_SEEN.run(base + s);
                    let o1 = ld(self.w(base + s + 1));
                    if self.put_over(put, base + s, w0, o1, mine[li], w1) {
                        if w0 != 0 {
                            self.slot_drop(self.slot_id(line, w0, o1), o1); // the expired occupant's
                        }
                        return base + s;
                    }
                    return -1;
                }
                s += 2;
            }
        }
        if mode == CLAIM_DEFER {
            return CLAIM_WAIT;
        }
        // Iterative deepening: all short paths before any long one. The deep
        // budget shrinks as recent searches fail, so an overfull table falls
        // back to cheap eviction instead of searching in vain.
        let p = self.pressure.load(SeqCst);
        let mut budget = 8 + (((RELOC_BUDGET - 8) * (PRESSURE_ONE as u64 - p as u64)) >> PRESSURE_SHIFT) as i64;
        self.warm_alts(lines);
        for depth in 0..=RELOC_DEPTH {
            for (li, &line) in lines.iter().enumerate() {
                let base = (line * WORDS_PER_LINE) as isize;
                let mut s = 0;
                while s < WORDS_PER_LINE as isize {
                    if self.relocate(base + s, line, mine[li], w1, now, depth, &mut budget, None, put) {
                        self.pressure.store(p - (p >> 3), SeqCst); // EWMA toward "searches succeed"
                        return base + s;
                    }
                    s += 2;
                }
            }
        }
        self.pressure.store(p - (p >> 3) + (PRESSURE_ONE >> 3), SeqCst); // EWMA toward "searches fail"
        let (mut victim, mut vli, mut victim_score, mut victim_w0, mut victim_w1) = (-1isize, 0usize, u64::MAX, 0u64, 0u64);
        for (li, &line) in lines.iter().enumerate() {
            let base = (line * WORDS_PER_LINE) as isize;
            let mut s = 0;
            while s < WORDS_PER_LINE as isize {
                let w0 = ld(self.w(base + s));
                let ts = w0 & TS_MASK;
                let vw1 = ld(self.w(base + s + 1));
                s += 2;
                // Read after the slot: a copy goes in only once its move is
                // counted, so a key whose move is under way is left alone. A
                // busy word1 (a hold, or a move's hidden copy) can't be
                // taken.
                let mv = self.move_of(vw1 >> TAG_SHIFT);
                if (vw1 >> TAG_SHIFT) == TAG_BUSY || mv.begun.load(SeqCst) != mv.done.load(SeqCst) {
                    continue;
                }
                let score = (self.mass(self.slot_id(line, w0, vw1), vw1, ts, now).min((1 << 16) - 1) << 48) | ts;
                if score < victim_score {
                    (victim, vli, victim_score, victim_w0, victim_w1) = (base + s - 2, li, score, w0, vw1);
                }
            }
        }
        if victim < 0 {
            return -1; // every candidate is mid-move: look again
        }
        hooks::VICTIM.run(());
        self.evict(lines[vli], mine[vli], w1, now, stripe, put, victim, victim_w0, victim_w1)
    }

    /// evict is claimAt's eviction of the slot at word index victim (in
    /// line, holding victimW0/victimW1) for the new entry mine0/mine1:
    /// claimAt's return.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn evict(
        &self,
        line: u64,
        mine0: u64,
        mine1: u64,
        now: u64,
        stripe: u64,
        put: PutFunc<'_>,
        victim: isize,
        victim_w0: u64,
        victim_w1: u64,
    ) -> isize {
        // The victim's history goes to the stash under the victim's own claim
        // stripe (its hash's top bits are its fp's), so a first touch of the
        // victim, which takes that stripe, finds it either still here or
        // already stashed. TryLock: the caller holds its own stripe.
        let vs = victim_w0 >> (64 - CLAIM_STRIPE_BITS);
        let _mu;
        if vs != stripe {
            match self.claim_mu[vs as usize].try_lock() {
                Some(g) => _mu = g,
                None => return -1,
            }
        }
        // A move of the victim's stripe that began after the scan may have
        // copied the victim, and its hand-off would then fail: back off (see
        // moveCount).
        let m = self.move_of(victim_w1 >> TAG_SHIFT);
        m.evicting.fetch_add(1, SeqCst);
        let _evicting = Defer(|| {
            m.evicting.fetch_add(-1, SeqCst);
        });
        if m.begun.load(SeqCst) != m.done.load(SeqCst) {
            return -1;
        }
        // Between the install and the stash a lock-free lookup finds the
        // victim in neither; the move count tells it to look again.
        inc(&m.begun);
        let _done = Defer(|| inc(&m.done));
        if !self.put_new(put, victim, victim_w0, victim_w1, mine0, mine1) {
            return -1;
        }
        inc(&self.evictions);
        self.stash_put(self.slot_id(line, victim_w0, victim_w1), victim_w0, victim_w1, now);
        victim
    }

    /// putNew installs a new key's entry with put, or install if put is
    /// None.
    #[inline]
    pub(crate) fn put_new(&self, put: PutFunc<'_>, i: isize, w0: u64, w1: u64, mine0: u64, mine1: u64) -> bool {
        if let Some(put) = put {
            return put(i, w0, w1, mine0, mine1);
        }
        self.install(i, w0, w1, mine0, mine1)
    }

    /// putOver is putNew over slot i's expired occupant w0 (0: none), whose
    /// word1 read w1. The hold (word1 busy until word0 is taken) counts as a
    /// move of the stripe of w1's tag, whoever has w1 by then: a touch at a
    /// clock that finds the occupant live may move word0 on during it (the
    /// install then puts word1 back), and its count, finding word1 busy,
    /// looks the key up again; that lookup, or another touch's, must not miss
    /// the slot and put the key in a second one (claims A27). w0 and w1 are
    /// two reads: a key installed between them has w1, and the hold, counted
    /// on w0's stripe (or on none, over a word0 of 0), hid it from its own
    /// touches and reads (claims A28).
    pub(crate) fn put_over(&self, put: PutFunc<'_>, i: isize, w0: u64, w1: u64, mine0: u64, mine1: u64) -> bool {
        if w1 == 0 || (w1 >> TAG_SHIFT) == TAG_BUSY {
            // free (word1 is 0 only once word0 is), or held: no hold to count
            return self.put_new(put, i, w0, w1, mine0, mine1);
        }
        let m = self.move_of(w1 >> TAG_SHIFT); // whoever holds w1 when the install takes it
        inc(&m.begun);
        let ok = self.put_new(put, i, w0, w1, mine0, mine1);
        inc(&m.done);
        ok
    }

    /// warmAlts loads one word of the alternate line of every entry in lines
    /// without using it: the misses are in flight together, and the search
    /// that follows finds the lines on their way or cached.
    pub(crate) fn warm_alts(&self, lines: [u64; 2]) {
        self.warm_line(lines[0]);
        self.warm_line(lines[1]);
    }

    /// warmLine loads one word of the alternate line of each entry in line.
    /// Four misses at once is about what the reference's host keeps in flight
    /// (claims S42); a whole level of the search at once (32) is not.
    pub(crate) fn warm_line(&self, line: u64) {
        let mut sink = 0u64;
        let base = (line * WORDS_PER_LINE) as isize;
        let mut s = 0;
        while s < WORDS_PER_LINE as isize {
            let w0 = ld(self.w(base + s));
            if w0 != 0 {
                sink = sink.wrapping_add(ld(&self.words[(self.alt(line, w0 >> 48) * WORDS_PER_LINE) as usize]));
            }
            s += 2;
        }
        black_box(sink);
    }

    /// install hands slot i (word index), if it still holds w0/w1, to the
    /// entry mine0/mine1. word1 goes busy first: a reader of the old occupant
    /// then misses rather than matching a half-written slot, and a late
    /// writer to the old register (bump) backs off. Nobody else writes a busy
    /// word1, so it is then set with plain stores.
    pub(crate) fn install(&self, i: isize, w0: u64, w1: u64, mine0: u64, mine1: u64) -> bool {
        if (w1 >> TAG_SHIFT) == TAG_BUSY || !cas(self.w(i + 1), w1, BUSY) {
            return false;
        }
        hooks::INSTALLING.run(i);
        if !cas(self.w(i), w0, mine0) {
            st(self.w(i + 1), w1); // lost the slot; put word1 back
            return false;
        }
        st(self.w(i + 1), mine1);
        true
    }

    /// relocate moves the entry at word index i (in line) to its alternate
    /// line and installs mine0/mine1 in its place. If that line is full it
    /// first relocates one of its entries, up to depth more steps (a bounded
    /// cuckoo path).
    ///
    /// The copy is installed hidden (word1 busy): no lookup finds it and no
    /// touch counts in it. The hand-off takes the source only if its words
    /// are still those copied (install's CASes), and only then is the copy's
    /// word1 published, so a key is never live in two slots. Before, the copy
    /// was visible from its install: a touch that had found the source a
    /// moment before counted there while reads found the copy (they scan its
    /// line first), and a copy of words read before the key was evicted came
    /// back as a second history whose touches the failed hand-off then
    /// dropped (claims A23). If the source changed (a touch reached it, or
    /// the key was evicted) the move is abandoned and the copy, which counted
    /// nothing, cleared.
    ///
    /// The move counts in its key's stripe (f.moves) from before the copy is
    /// installed to its publication or clearing: a lookup that misses
    /// meanwhile (between the hand-off and the publication the key is in no
    /// slot a lookup matches) looks again, and an evictor leaves the source
    /// alone. parent is the move whose copy (mine0; mine1 is busy, the parent
    /// publishes it) this one installs, None for a new key, which put
    /// installs.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn relocate(
        &self,
        i: isize,
        line: u64,
        mine0: u64,
        mine1: u64,
        now: u64,
        depth: i32,
        budget: &mut i64,
        parent: Option<&MoveCount>,
        put: PutFunc<'_>,
    ) -> bool {
        let w0 = ld(self.w(i));
        if w0 == 0 {
            return false;
        }
        if *budget <= 0 {
            return false;
        }
        *budget -= 1;
        let w1 = ld(self.w(i + 1));
        if (w1 >> TAG_SHIFT) == TAG_BUSY || ld(self.w(i)) != w0 {
            return false; // being installed or realigned, or touched since: a copy would pair stale words
        }
        hooks::PICKED.run(i);
        let a = self.alt(line, w0 >> 48);
        let base = (a * WORDS_PER_LINE) as isize;
        let moved = w0 ^ DISP_BIT; // displaced iff it wasn't
        let m = self.move_of(w1 >> TAG_SHIFT); // the entry's key's stripe
        let replace = |d: isize| -> bool {
            // the hidden copy is at d; now take the source
            let _done = Defer(|| inc(&m.done));
            if let Some(parent) = parent {
                inc(&parent.begun); // the parent's copy goes in (hidden) with this install
                if parent.evicting.load(SeqCst) != 0 {
                    inc(&parent.done);
                    self.abandon(d);
                    return false;
                }
            }
            if self.put_new(put, i, w0, w1, mine0, mine1) {
                // deeper levels pass no put: install
                st(self.w(d + 1), w1); // the source held w0/w1 until the hand-off: publish
                inc(&self.relocations);
                return true;
            }
            if let Some(parent) = parent {
                inc(&parent.done); // ...or doesn't
            }
            self.abandon(d); // a touch reached the source first, or the key was evicted
            false
        };
        let mut s = 0;
        while s < WORDS_PER_LINE as isize {
            let d0 = ld(self.w(base + s));
            if d0 != 0 && age(now, d0 & TS_MASK) <= self.horizon {
                s += 2;
                continue;
            }
            inc(&m.begun);
            if m.evicting.load(SeqCst) != 0 {
                // an eviction of this stripe is under way: it may take the source
                inc(&m.done);
                return false;
            }
            let d1 = ld(self.w(base + s + 1));
            if self.put_over(None, base + s, d0, d1, moved, BUSY) {
                if d0 != 0 {
                    self.slot_drop(self.slot_id(a, d0, d1), d1); // the expired occupant's
                }
                hooks::COPIED.run(());
                return replace(base + s);
            }
            inc(&m.done);
            s += 2;
        }
        if depth > 0 {
            self.warm_line(a); // the next lines of a's entries together, not one miss at a time
            let mut s = 0;
            while s < WORDS_PER_LINE as isize {
                if self.relocate(base + s, a, moved, BUSY, now, depth - 1, budget, Some(m), None) {
                    return replace(base + s);
                }
                s += 2;
            }
        }
        false
    }

    /// abandon clears the hidden copy at word index d of a move whose source
    /// changed before the hand-off, word0 first (a slot is never a
    /// fingerprint with an empty register). Nobody else writes a busy word1.
    pub(crate) fn abandon(&self, d: isize) {
        st(self.w(d), 0);
        st(self.w(d + 1), 0);
    }
}
