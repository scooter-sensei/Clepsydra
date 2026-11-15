//! decay.go: the decay sweep.

use std::sync::atomic::Ordering::SeqCst;

use crate::cpu;
use crate::filter::{Core, check_tick};
use crate::hooks;
use crate::slots::*;

/// decayEvery gates the inline decay step to 1/64 of operations (by key
/// hash) so the shared cursor's cache line is not touched on every call.
pub(crate) const DECAY_EVERY: u64 = 63;

impl Core {
    /// ContinuousDecay reclaims expired slots at a rate set by the clock:
    /// one full pass of the table per Horizon of elapsed ticks, spread across
    /// callers in steps of at most a few cache lines. It never blocks and
    /// never pauses readers. Expiry correctness does not depend on it (ages
    /// are evaluated arithmetically on every access); it only frees slots and
    /// keeps occupancy honest. touch calls it inline on 1/64 of operations;
    /// calling it from a ticker as well is optional. Returns lines swept.
    pub(crate) fn continuous_decay(&self) -> i64 {
        self.decay_step(self.clock(), 4)
    }

    pub(crate) fn decay_step(&self, now: u64, budget: u64) -> i64 {
        let (hi, lo) = cpu::mul64(now, self.lines);
        if hi >= self.horizon {
            return 0;
        }
        let target = cpu::div64(hi, lo, self.horizon);
        let c = self.cursor.load(SeqCst);
        if c >= target {
            return 0;
        }
        check_tick(now); // past it, every slot would read expired (only a step that sweeps gets here)
        let mut start = c;
        if target - start > self.lines {
            // fell behind by over a lap: skip ahead, lazy expiry covered it
            start = target - self.lines;
        }
        let n = (target - start).min(budget);
        if !cas(&self.cursor, c, start + n) {
            return 0; // another caller took this step
        }
        for l in start..start + n {
            self.sweep_line(l % self.lines, now);
        }
        n as i64
    }

    /// sweepLine clears line's slots expired at now. It takes a slot as
    /// install does, for no key: word1 busy first, then word0 to 0 (or word1
    /// back if word0 changed), then word1 to 0. A first touch takes a free
    /// slot only with word1 not busy, so none lands in the slot while it is
    /// cleared. Before, the sweep cleared word0, then CASed word1 from the
    /// register it had read to 0: a first touch could take the slot between
    /// the two and install a register bit-identical to the old one (its
    /// key's, one event, the same 4 epoch bits), which the CAS then cleared,
    /// leaving the key's fingerprint with an empty register, a slot no lookup
    /// matches (claims A26).
    ///
    /// The hold counts as a move of the stripe of the word1 it takes busy,
    /// as every hold does (moveOf): a touch at a clock that finds the
    /// occupant live may move word0 on during it (the sweep then puts word1
    /// back), and its count, finding word1 busy, looks the key up again. That
    /// lookup must not miss the slot and put the key in a second one.
    pub(crate) fn sweep_line(&self, line: u64, now: u64) {
        let base = (line * WORDS_PER_LINE) as isize;
        let mut s = 0;
        while s < WORDS_PER_LINE as isize {
            let i = base + s;
            s += 2;
            let w0 = ld(self.w(i));
            if w0 == 0 || age(now, w0 & TS_MASK) <= self.horizon {
                continue;
            }
            hooks::SWEEP_SEEN.run(i);
            let w1 = ld(self.w(i + 1));
            if (w1 >> TAG_SHIFT) == TAG_BUSY {
                continue; // being handed over
            }
            hooks::SWEEP_READ.run(i);
            let m = self.move_of(w1 >> TAG_SHIFT); // the stripe of whoever holds w1 when the CAS takes it
            inc(&m.begun);
            if !cas(self.w(i + 1), w1, BUSY) {
                inc(&m.done);
                continue; // touched since
            }
            hooks::SWEEPING.run(());
            if cas(self.w(i), w0, 0) {
                st(self.w(i + 1), 0);
                self.slot_drop(self.slot_id(line, w0, w1), w1);
            } else {
                st(self.w(i + 1), w1); // touched since: put word1 back
            }
            inc(&m.done);
        }
    }
}
