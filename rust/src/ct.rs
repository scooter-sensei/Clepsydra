//! ct.go: ConstantTime's pads and the placement steps.

use std::cell::Cell;
use std::hash::{BuildHasher, Hasher};
use std::sync::atomic::AtomicI64;
use std::sync::atomic::Ordering::SeqCst;
use std::time::Instant;

use crate::Result;
use crate::cuckoo::CLAIM_FULL;
use crate::filter::{Core, check_tick, since_ns};
use crate::slots::*;
use crate::stash::{DEFER_MARK, STASH_STRIPES, STASH_WAYS, StashEntry, StashTable};
use crate::touch::CT_INSIDE;

pub(crate) const OP_TOUCH: usize = 0;
pub(crate) const OP_COUNT: usize = 1;
// The estimate (in ps) moves up autoUp when a sampled call's own work ran
// past it and down autoDown otherwise, so it settles where 0.5% of calls run
// past it (autoDown/(autoUp+autoDown)). One slow call (a pause) moves it
// 100 ns.
pub(crate) const AUTO_UP: i64 = 99_500;
pub(crate) const AUTO_DOWN: i64 = 500;
pub(crate) const AUTO_START_NS: i64 = 4_000;
pub(crate) const AUTO_MAX_NS: i64 = 50_000;

/// padDither is the span over which a ConstantTime pad's end is drawn: a mean
/// cost of 64 ns per call.
pub(crate) const PAD_DITHER_NS: u64 = 128;

#[repr(align(64))]
#[derive(Default)]
pub(crate) struct QuantilePad {
    pub(crate) q: AtomicI64, // ps
}

thread_local! {
    /// The dither's generator (the reference's `math/rand/v2` top-level
    /// source: per thread, randomly seeded).
    static DITHER: Cell<u64> = Cell::new({
        let mut h = std::collections::hash_map::RandomState::new().build_hasher();
        h.write_u64(0x9e3779b97f4a7c15);
        h.finish() | 1
    });
}

/// `rand.Uint64N(n)` for a power of two n.
#[inline]
fn dither(n: u64) -> u64 {
    DITHER.with(|s| {
        // xorshift64*
        let mut x = s.get();
        x ^= x >> 12;
        x ^= x << 25;
        x ^= x >> 27;
        s.set(x);
        (x.wrapping_mul(0x2545f4914f6cdd1d) >> 32) & (n - 1)
    })
}

impl Core {
    /// ctBegin starts a ConstantTime call of kind op and returns its start
    /// and pad (ns). Touch's pad is ConstantTime; Count's, whose paths differ
    /// far less (no insert), is a running estimate of its own work's 99.5th
    /// percentile, at most ConstantTime; with AutoPad both are. Before the
    /// call's own work, so that how much it does can't depend on the caller's
    /// key, it spends what the pad leaves beyond that estimate placing keys
    /// that wait in the stash.
    pub(crate) fn ct_begin(&self, op: usize, now: u64) -> (Instant, i64) {
        let t0 = Instant::now();
        let own = self.pad_q[op].q.load(SeqCst) / 1000;
        let pad = if op == OP_TOUCH && !self.auto {
            self.pad
        } else if self.auto {
            own
        } else {
            own.min(self.pad)
        };
        self.place(t0, pad - own, now);
        (t0, pad)
    }

    /// ctEnd moves op's own-work estimate with the work done since s (a
    /// sample of calls, 1 in 4 by key, moves it) and spins until the pad,
    /// plus a random dither, has passed since t0. The spin reads the clock
    /// every ~37 ns (the reference's Ryzen host), so a pad ending at a fixed
    /// time ends at a phase of those reads that follows the call's own work,
    /// and keys whose work time is narrow show it (Count at a fixed 1 µs pad:
    /// D 0.019-0.041; with the dither 0.007-0.014, claims S41). The dither
    /// spans several reads and doesn't depend on the key.
    pub(crate) fn ct_end(&self, op: usize, h: u64, t0: Instant, s: Instant, mut pad: i64) {
        if ((h >> 8) & 3) == 0 {
            let qp = &self.pad_q[op].q;
            let est = qp.load(SeqCst) / 1000;
            if since_ns(s) > est {
                if est < AUTO_MAX_NS {
                    qp.fetch_add(AUTO_UP, SeqCst);
                }
            } else if est > 0 {
                qp.fetch_add(-AUTO_DOWN, SeqCst);
            }
        }
        pad += dither(PAD_DITHER_NS) as i64;
        while since_ns(t0) < pad {
            std::hint::spin_loop();
        }
    }

    /// place spends up to budget since t0 on keys that wait in the stash for
    /// a slot (an insert that found its two lines full defers the cuckoo
    /// search to here, so no call's own work includes one). A step takes the
    /// next stash set with a waiting key and inserts its waiting keys as a
    /// first touch outside ConstantTime would: free slot, cuckoo path, or
    /// eviction of the slot with the fewest events into the stash. A step
    /// starts only if the rest of the budget covers a step's running mean
    /// cost. If more than one key per 16 stash entries waits (pads too short
    /// for the insert rate), every call places one set regardless: calls then
    /// run longer for every key alike, which says the table is under insert
    /// pressure, not which keys were seen, and the stash's sets stay far from
    /// full (a waiting key whose set is full is inserted at once, which takes
    /// time).
    pub(crate) fn place(&self, t0: Instant, budget: i64, now: u64) {
        let Some(p) = self.stash.get() else {
            return;
        };
        let mut forced = self.deferred.load(SeqCst) > (p.e.len() / 16) as i64;
        while self.deferred.load(SeqCst) > 0 {
            let mut est = self.place_ns.load(SeqCst);
            if est == 0 {
                est = 1000;
            }
            if !forced && budget - since_ns(t0) < est {
                self.place_ns.store(est - est / 64, SeqCst); // a step that ran long once mustn't stop placing for good
                return;
            }
            forced = false;
            let s = Instant::now();
            if !self.place_next(Some(p), now) {
                return;
            }
            self.place_ns.store(est + (since_ns(s) - est) / 8, SeqCst);
        }
    }

    /// placeNext places the waiting keys of table p's next set (after the
    /// last one visited) whose wait bit is set; false if no bit is set.
    pub(crate) fn place_next(&self, p: Option<&StashTable>, now: u64) -> bool {
        let Some(p) = p else {
            return false;
        };
        let nw = p.wait.len() as u64;
        let start = p.at.load(SeqCst);
        for k in 0..nw + 1 {
            let wi = (start / 64 + k) % nw;
            let mut b = ld(&p.wait[wi as usize]);
            if k == 0 {
                b &= !((1u64 << (start % 64)) - 1); // bits before the cursor wait for the wrap-around
            }
            if b == 0 {
                continue;
            }
            let set = wi * 64 + u64::from(b.trailing_zeros());
            p.at.store(set + 1, SeqCst);
            if self.room.get().is_some_and(|r| std::ptr::eq(r, p)) {
                self.place_room_set(p, set);
            } else {
                self.place_set(p, set, now);
            }
            return true;
        }
        false
    }

    /// placeSet moves the waiting keys of table p's set i into the table. A
    /// key whose stripe is busy, or that loses a race for its slot, is left
    /// for a later step.
    pub(crate) fn place_set(&self, p: &StashTable, i: u64, now: u64) {
        let (base, mu) = ((i * STASH_WAYS) as usize, &p.mu[(i % STASH_STRIPES as u64) as usize]);
        let mut ids = [0u64; STASH_WAYS as usize];
        let mut n = 0;
        {
            let _g = mu.lock();
            for k in base..base + STASH_WAYS as usize {
                let e = p.get(k);
                if e.id != 0 && (e.w0 & DEFER_MARK) != 0 {
                    if age(now, e.w0 & TS_MASK) > self.horizon {
                        self.stash_write(p, k, StashEntry::default()); // expired while it waited
                        continue;
                    }
                    ids[n] = e.id;
                    n += 1;
                }
            }
            if n == 0 {
                p.wait[(i / 64) as usize].fetch_and(!(1u64 << (i % 64)), SeqCst);
            }
        }
        for &id in &ids[..n] {
            let (fp, tg) = ((id >> TAG_BITS) & ((1 << 16) - 1), id & ((1 << TAG_BITS) - 1));
            let stripe = fp >> (16 - CLAIM_STRIPE_BITS);
            let Some(_cm) = self.claim_mu[stripe as usize].try_lock() else {
                continue;
            };
            let (w0, w1, ok) = self.table_get(Some(p), id, now, true);
            if ok {
                let pl = id >> 28;
                let al = self.alt(pl, fp);
                if self.find(pl, fp << 1, tg) >= 0 || self.find(al, (fp << 1) | 1, tg) >= 0 {
                    self.stash_put(id, w0, w1, now); // the table holds the key: no longer waiting
                } else if self.claim_at([pl, al], [w0 & !DISP_BIT, w0 | DISP_BIT], w1, now, stripe, CLAIM_FULL, None) < 0 {
                    self.stash_put_as(id, w0, w1, now, true);
                }
            }
        }
    }

    /// placeRoomSet places the waiting keys of the wait room's set i
    /// (Background).
    pub(crate) fn place_room_set(&self, p: &StashTable, i: u64) {
        let (base, mu) = ((i * STASH_WAYS) as usize, &p.mu[(i % STASH_STRIPES as u64) as usize]);
        let mut waiting = [StashEntry::default(); STASH_WAYS as usize];
        let mut n = 0;
        {
            let _g = mu.lock();
            for k in base..base + STASH_WAYS as usize {
                let e = p.get(k);
                if e.id != 0 && (e.w0 & DEFER_MARK) != 0 {
                    waiting[n] = e;
                    n += 1;
                }
            }
            if n == 0 {
                p.wait[(i / 64) as usize].fetch_and(!(1u64 << (i % 64)), SeqCst);
            }
        }
        for &e in &waiting[..n] {
            self.place_room_key(p, e);
        }
    }

    /// touchCT is touch in ConstantTime mode: its work (touch with ctInside:
    /// under the key's stripe, the same lookups whatever the key's state,
    /// then the count or the insert, then the overflow table) padded.
    pub(crate) fn touch_ct(&self, h: u64, window: u64, now: u64, want_count: bool) -> Result {
        check_tick(now); // before the stripe: realign's check would panic holding it
        let (t0, pad) = self.ct_begin(OP_TOUCH, now);
        let s = Instant::now();
        let r = self.touch(h, window, now, want_count, CT_INSIDE);
        self.ct_end(OP_TOUCH, h, t0, s, pad);
        r
    }
}
