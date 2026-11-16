//! buzz.go and touchif.go: conditional touches and buzz entries.
//!
//! Buzz entries: an exact log for a conditional touch whose refusal was in
//! doubt (claims A46; the lab arm B12b, the feeding buzz: a bat calls faster
//! in the last moments of a strike, buying resolution where a decision turns
//! on it).
//!
//! TouchIfUnder refuses while Upper is at the limit, and Upper counts the
//! bucket the window's edge cuts as whole, so on windows shorter than a few
//! units a strict limiter admits 30-65% of its limit (paper §4.2). A refusal
//! is uncertain when the events certainly inside the window (the buckets
//! wholly inside it, and the last touch) are under the limit: the register
//! can't say whether the key is over, and every false denial is such a
//! refusal. On an uncertain refusal of a limit of buzzCap or less, the key
//! gets a buzz entry: the slot's two words as they are, and from then on the
//! tick of every event TouchIfUnder admits for it, the newest buzzCap of
//! them, in 16-bit steps of Horizon/32768.
//!
//! Trust. An entry answers only while the key's slot holds, bit for bit, what
//! the entry's last admission left (the displaced bit aside, which a
//! relocation flips), with a register that is not full in every bucket and
//! not saturated. Every event the filter counts for such a key changes a
//! word: some bucket's count, or the last touch. So while they match, the
//! events since the entry's start are exactly the logged ones. Anything else
//! that counts for the key (a plain touch, a promotion, an eviction whose
//! history is shed) changes a word; the entry is dropped, and the key is
//! decided by its register again, as without entries. An admission records as
//! the entry's words what one event at now makes of the words read before it
//! (nextReg), and the key's next conditional touch checks them: a plain touch
//! between the read and the admission leaves another count, and the entry is
//! dropped there, before it decides.
//!
//! Decision. With a trusted entry and a window that starts after the tick
//! before which the log may be missing events (its start, then the newest
//! event it dropped), the count is the log's: an event is inside unless its
//! step ends before the window starts; the overflow table's part of Upper is
//! added as without entries. A limit of buzzCap or less never needs an event
//! the log dropped: if fewer than the limit of its newest buzzCap are inside
//! the window, the next older one is outside it. So the log's count is at
//! least the truth, and a request is admitted only under the limit, as Upper
//! guarantees. (The one way an event counted for the key leaves no trace in
//! its words is the documented shedding of a history under SpillMin, which
//! Upper doesn't see either.)
//!
//! Release. An event a horizon old leaves the ring at the next admission, so
//! no step is read two horizons on (a step repeats every 65,536). A way is
//! free once its entry's newest event is older than the window it was given
//! for and eight units more; a key that finds none of its block's ways free
//! is decided by its register.
//!
//! The table is allocated on the first uncertain refusal: one entry per 128
//! slots, in 512 B blocks of buzzWays ways (the ways a key may use): the
//! ways' owners in the block's first line, so a key with no entry reads one
//! line, then the entries, 56 B each. With 64 blocks or more each block
//! belongs to one conditional stripe, whose lock (condMu) guards it; with
//! fewer, blocks are shared, each under a lock of its own stripe. Nothing but
//! TouchIfUnder reads it, and only for a limit of buzzCap or less;
//! ConstantTime filters keep TouchIfUnder as without entries. A snapshot
//! carries it.

use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering::SeqCst;

use crate::Result;
use crate::cpu::{self, mul_hi};
use crate::filter::Core;
use crate::hooks;
use crate::hot::is_hot;
use crate::register::{N_BUCKETS, REG_MASK, bucket_start};
use crate::slots::*;

pub(crate) const BUZZ_CAP: u64 = 16; // events a log holds: a limit up to it is decided by the log alone
pub(crate) const BUZZ_WAYS: usize = 8; // ways a key may use: one block
pub(crate) const BUZZ_STRIPES: usize = 64; // = len(condMu)
pub(crate) const BUZZ_N_SHIFT: u32 = 46; // meta: before (46 bits) | n (5) | head (4) | units to release (5)
pub(crate) const BUZZ_H_SHIFT: u32 = 51;
pub(crate) const BUZZ_U_SHIFT: u32 = 55;

/// condMu serializes conditional touches (TouchIfUnderAt) of keys sharing
/// its stripe (the hash's top 6 bits), across filters. The claim stripes
/// can't be used: touch takes them on a first touch.
pub(crate) static COND_MU: [PaddedMutex; 1 << 6] = [const { PaddedMutex::new() }; 1 << 6];

/// buzzEntry, as a value: read from its block and written back under the
/// block's guard.
#[derive(Clone, Copy, Default, PartialEq, Eq, Debug)]
pub(crate) struct BuzzEntry {
    pub(crate) w0: u64, // the key's slot as the entry's last admission, or its start, left it
    pub(crate) w1: u64,
    pub(crate) meta: u64,    // the tick at or before which the log may miss events, the ring's fill and head, the release span
    pub(crate) ev: [u64; 4], // a ring of buzzCap steps (tick / q, mod 2^16)
}

/// buzzBlock is the ways a key may use: their owners in one line, then the
/// entries. The reference's words are plain, under the block's guard (a
/// conditional stripe or the table's lock); here they are atomics read and
/// written under that guard.
#[repr(C, align(64))]
pub(crate) struct BuzzBlock {
    pub(crate) ids: [AtomicU64; BUZZ_WAYS], // each way's owner (stashID); 0: free
    e: [[AtomicU64; 7]; BUZZ_WAYS],
}

const _: () = assert!(size_of::<BuzzBlock>() == 512);

impl BuzzBlock {
    fn new() -> BuzzBlock {
        BuzzBlock { ids: Default::default(), e: std::array::from_fn(|_| Default::default()) }
    }

    /// Way i's entry.
    #[inline]
    pub(crate) fn entry(&self, i: usize) -> BuzzEntry {
        let c = &self.e[i];
        BuzzEntry { w0: ld(&c[0]), w1: ld(&c[1]), meta: ld(&c[2]), ev: [ld(&c[3]), ld(&c[4]), ld(&c[5]), ld(&c[6])] }
    }

    /// Writes way i's entry.
    #[inline]
    pub(crate) fn put(&self, i: usize, e: &BuzzEntry) {
        let c = &self.e[i];
        st(&c[0], e.w0);
        st(&c[1], e.w1);
        st(&c[2], e.meta);
        for k in 0..4 {
            st(&c[3 + k], e.ev[k]);
        }
    }
}

impl BuzzEntry {
    #[inline]
    pub(crate) fn before(&self) -> u64 {
        self.meta & TS_MASK
    }
    #[inline]
    pub(crate) fn n(&self) -> i64 {
        ((self.meta >> BUZZ_N_SHIFT) & 31) as i64
    }
    #[inline]
    pub(crate) fn head(&self) -> i64 {
        ((self.meta >> BUZZ_H_SHIFT) & 15) as i64
    }
    #[inline]
    pub(crate) fn units(&self) -> u64 {
        (self.meta >> BUZZ_U_SHIFT) & 31
    }

    #[inline]
    pub(crate) fn step(&self, k: i64) -> u64 {
        (self.ev[(k / 4) as usize] >> (16 * (k % 4))) & 0xFFFF
    }

    /// stepAt is the step index (tick / q) of the step at ring position k,
    /// decoded against rq, the step of a tick no event of the log is after
    /// (steps cover two horizons, and no step in the ring is older than a
    /// horizon before the log's newest event); 0 if it would be before tick
    /// 0.
    pub(crate) fn step_at(&self, k: i64, rq: u64) -> u64 {
        let d = rq.wrapping_sub(self.step(k)) & 0xFFFF;
        if d > rq {
            return 0;
        }
        rq - d
    }

    /// count is the log's Upper for [now-window, now]: an event counts
    /// unless its step ends before the window starts. A step s ends at
    /// (s+1)q - 1, so it reaches the window's start from exactly when
    /// s >= from/q.
    pub(crate) fn count(&self, now: u64, window: u64, q: u64) -> i64 {
        let rq = now.max(self.w0 & TS_MASK) / q;
        let fq = (now - now.min(window)) / q;
        let (mut n, head) = (0, self.head());
        for k in 0..self.n() {
            if self.step_at((head - 1 - k) & 15, rq) >= fq {
                n += 1;
            }
        }
        n
    }

    /// covers: every event the log may miss is outside [now-window, now].
    pub(crate) fn covers(&self, now: u64, window: u64) -> bool {
        now >= window && self.before() < now - window
    }

    /// log records an admitted event at tick t. Events a horizon old (their
    /// step ends a horizon before t) leave the ring first; a full ring drops
    /// its oldest step, and the log may from then on miss events up to that
    /// step's end.
    pub(crate) fn log(&mut self, t: u64, q: u64, horizon: u64) {
        let (mut before, mut n, head) = (self.before(), self.n(), self.head());
        let rq = t.max(self.w0 & TS_MASK) / q;
        if t >= horizon + q {
            // else no step can be a horizon old
            let fq = (t - horizon) / q;
            while n > 0 {
                if self.step_at((head - n) & 15, rq) >= fq {
                    break;
                }
                n -= 1;
            }
        }
        if n == BUZZ_CAP as i64 {
            before = before.max((self.step_at(head, rq) + 1) * q - 1) & TS_MASK;
        } else {
            n += 1;
        }
        let sh = 16 * (head % 4) as u32;
        let w = &mut self.ev[(head / 4) as usize];
        *w = (*w & !(0xFFFFu64 << sh)) | (((t / q) & 0xFFFF) << sh);
        self.meta = before | ((n as u64) << BUZZ_N_SHIFT) | ((((head + 1) & 15) as u64) << BUZZ_H_SHIFT) | (self.units() << BUZZ_U_SHIFT);
    }
}

pub(crate) struct BuzzTable {
    pub(crate) b: Box<[BuzzBlock]>,
    /// used: a bit per block, set before an entry goes in and cleared when
    /// the block's last entry leaves, by whoever guards the block. A key
    /// whose block's bit is clear holds no entry, and its conditional touch
    /// reads nothing of the table: most keys, when few are contested.
    pub(crate) used: Box<[AtomicU64]>,
    pub(crate) q: u64, // ticks per step
    pub(crate) mu: [PaddedMutex; BUZZ_STRIPES],
    /// given: entries handed out; dropped: entries whose slot no longer
    /// matched; decided: conditional touches a log decided.
    pub(crate) given: AtomicU64,
    pub(crate) dropped: AtomicU64,
    pub(crate) decided: AtomicU64,
}

impl BuzzTable {
    /// block returns the index of the block of the key with hash h and
    /// identity id, and the lock that guards it, or None. With 64 blocks or
    /// more, the blocks are split in 64 runs, one per conditional stripe (h's
    /// top 6 bits, condMu's index), so only keys whose conditional touches
    /// condMu already serializes share a block. With fewer, blocks are shared
    /// across stripes and locked.
    pub(crate) fn block(&self, h: u64, id: u64) -> (u64, Option<&PaddedMutex>) {
        let n = self.b.len() as u64;
        let x = mix64(id ^ 0x9e3779b97f4a7c15);
        if n < BUZZ_STRIPES as u64 {
            let k = mul_hi(x, n);
            return (k, Some(&self.mu[k as usize]));
        }
        let s = h >> (64 - 6);
        let lo = s * n / BUZZ_STRIPES as u64;
        let k = mul_hi(x, (s + 1) * n / BUZZ_STRIPES as u64 - lo);
        (lo + k, None)
    }

    /// mayHold: block k may hold an entry.
    #[inline]
    pub(crate) fn may_hold(&self, k: u64) -> bool {
        ((ld(&self.used[(k >> 6) as usize]) >> (k & 63)) & 1) != 0
    }

    /// leave frees way i of block k, clearing the block's bit if it was the
    /// last (the caller guards the block).
    pub(crate) fn leave(&self, k: u64, i: usize) {
        let b = &self.b[k as usize];
        st(&b.ids[i], 0);
        for x in &b.ids {
            if ld(x) != 0 {
                return;
            }
        }
        self.used[(k >> 6) as usize].fetch_and(!(1u64 << (k & 63)), SeqCst);
    }

    /// mark sets block k's bit from its owners (a restore).
    pub(crate) fn mark(&self, k: u64) {
        for x in &self.b[k as usize].ids {
            if ld(x) != 0 {
                self.used[(k >> 6) as usize].fetch_or(1u64 << (k & 63), SeqCst);
                return;
            }
        }
    }
}

impl Core {
    /// buzzTable returns the table, allocated on first use: one entry per
    /// 128 slots.
    pub(crate) fn buzz_table(&self) -> &BuzzTable {
        self.buzz.get_or_init(|| {
            let blocks = 2.max(self.lines * SLOTS_PER_LINE / (128 * BUZZ_WAYS as u64));
            BuzzTable {
                b: (0..blocks).map(|_| BuzzBlock::new()).collect(),
                used: (0..blocks.div_ceil(64)).map(|_| AtomicU64::new(0)).collect(),
                q: 1.max((self.horizon + (1 << 15) - 1) >> 15),
                mu: std::array::from_fn(|_| PaddedMutex::new()),
                given: AtomicU64::new(0),
                dropped: AtomicU64::new(0),
                decided: AtomicU64::new(0),
            }
        })
    }

    /// certain is the number of events of the register w1 (last touched at
    /// last) that lie inside [now-window, now] wherever they fall in their
    /// buckets: the buckets wholly inside it, and the last touch.
    pub(crate) fn certain(&self, w1: u64, last: u64, now: u64, window: u64) -> i64 {
        if age(now, last) > window {
            return 0;
        }
        let (v, _) = self.view_of(0, w1, last);
        let unit = self.unit as f64;
        let from = now as f64 - window as f64;
        let mut n = 0i64;
        for j in 0..N_BUCKETS {
            if from > bucket_start(v.e, j) as f64 * unit {
                break;
            }
            n += v.c[j] as i64;
        }
        1.max(n)
    }

    /// TouchIfUnderAt is TouchIfUnder on a pre-hashed key, in ticks (claims
    /// A45). Conditional touches of one key are serialized (a lock across the
    /// read and the touch), so concurrent callers never admit more than limit
    /// in any window; plain touches of the key are not, and count against it.
    /// Upper counts a bucket the window cuts as a whole, so on windows
    /// shorter than a few units it would admit fewer than limit: for a limit
    /// of 16 or less, a key refused in doubt gets an exact log of its
    /// admitted events, by which later requests are decided while nothing
    /// else counts for it (buzz entries; claims A46).
    pub(crate) fn touch_if_under_at(&self, h: u64, window: u64, limit: u64, now: u64) -> (Result, bool) {
        let _mu = COND_MU[(h >> (64 - 6)) as usize].lock();
        if limit <= BUZZ_CAP && !self.ct && !hooks::BUZZ_OFF.load(SeqCst) {
            return self.touch_if_buzz(h, window, limit, now);
        }
        let r = self.count_at(h, window, now);
        if r.saturated || (self.sk_w == 0 && r.degraded && !r.seen) || (r.upper.max(0) as u64) >= limit {
            return (r, false);
        }
        (self.touch_at(h, window, now), true)
    }

    /// touchIfBuzz is TouchIfUnderAt for a limit of buzzCap or less, outside
    /// ConstantTime (the caller holds h's conditional stripe). Its first half
    /// is CountAt and the strict recipe, as without entries, keeping the
    /// slot's words and the overflow table's part of Upper.
    pub(crate) fn touch_if_buzz(&self, h: u64, window: u64, limit: u64, now: u64) -> (Result, bool) {
        let (fp, tg) = (fingerprint(h), tag(h));
        let id = self.stash_id(h, fp, tg);
        let mut t = self.buzz.get();
        let mut k = 0u64;
        let mut mu: Option<&PaddedMutex> = None;
        let mut held = false; // h's block may hold an entry (h's own, if any, put there under h's stripe)
        if let Some(t) = t {
            (k, mu) = t.block(h, id);
            held = t.may_hold(k);
            if held && mu.is_none() {
                // its owners, loading while the slot is looked up (condMu guards them)
                cpu::prefetch(&t.b[k as usize].ids[0]);
            }
        }
        let (w0, w1, src) = self.read(h, now, -1);
        let last = w0 & TS_MASK;
        let live = src != 0 && age(now, last) <= self.horizon;
        let mut r = Result::default();
        if live {
            r = self.estimate_at(h, w1, last, now, window, (w0 & SAT_BIT) != 0);
            r.seen = r.count > 0;
        } else {
            r.degraded = self.lost_recently(now);
        }
        let mut over = r.upper;
        if self.sketch_live(now) {
            self.add_sketch(&mut r, h, window, now);
        }
        over = r.upper - over;
        let mut refuse = r.saturated || (self.sk_w == 0 && r.degraded && !r.seen) || (r.upper.max(0) as u64) >= limit;

        // A register the two words answer for: no hot entry, not saturated,
        // not full in every bucket.
        let plain = live && !is_hot(w1) && (w0 & SAT_BIT) == 0 && (!w1 & REG_MASK) != 0;
        let win = window.min(self.horizon);
        if !held {
            // no entry: as without entries, unless the refusal is in doubt
            if !refuse {
                return (self.touch_at(h, window, now), true);
            }
            if !self.uncertain(plain, &r, w1, last, now, win, over, limit) {
                return (r, false);
            }
            if t.is_none() {
                let nt = self.buzz_table();
                (k, mu) = nt.block(h, id);
                t = Some(nt);
            }
        }
        let t = t.expect("the buzz table");
        let _g = mu.map(|m| m.lock());
        let b = &t.b[k as usize];
        let mut way = -1i64;
        if held {
            for (i, x) in b.ids.iter().enumerate() {
                if ld(x) == id {
                    way = i as i64;
                    break;
                }
            }
        }
        let mut e: Option<BuzzEntry> = None;
        if way >= 0 {
            let x = b.entry(way as usize);
            if plain && ((w0 ^ x.w0) & !DISP_BIT) == 0 && w1 == x.w1 {
                e = Some(x);
            } else {
                t.leave(k, way as usize); // something else counted for the key, or its slot is gone
                inc(&t.dropped);
            }
        }
        if let Some(e) = &e {
            if e.covers(now, win) {
                r.upper = e.count(now, win, t.q) + over;
                r.count = r.count.min(r.upper);
                refuse = r.saturated || (r.upper as u64) >= limit;
                inc(&t.decided);
            }
        }
        if refuse {
            if e.is_none() && self.uncertain(plain, &r, w1, last, now, win, over, limit) {
                for i in 0..BUZZ_WAYS {
                    let x = b.entry(i);
                    if ld(&b.ids[i]) == 0 || age(now, x.w0 & TS_MASK) > x.units() * self.unit {
                        let units = 31.min((win + self.unit - 1) / self.unit + 8);
                        t.used[(k >> 6) as usize].fetch_or(1u64 << (k & 63), SeqCst);
                        st(&b.ids[i], id);
                        b.put(i, &BuzzEntry { w0, w1, meta: now.max(last) | (units << BUZZ_U_SHIFT), ev: [0; 4] });
                        inc(&t.given);
                        break;
                    }
                }
            }
            return (r, false);
        }
        let (mut want, mut ovf) = (0u64, false);
        if e.is_some() {
            (want, ovf) = self.next_reg(w1, tg, last, now, false); // what the touch below makes of the words read
        }
        let res = self.touch_at(h, window, now);
        if let Some(mut e) = e {
            if ovf {
                // the touch saturates or promotes the key: its words no longer answer for it
                t.leave(k, way as usize);
                inc(&t.dropped);
            } else {
                let stamp = now.max(last);
                e.log(stamp, t.q, self.horizon);
                (e.w0, e.w1) = ((w0 & !TS_MASK) | stamp, want); // checked at the key's next conditional touch
                b.put(way as usize, &e);
            }
        }
        (res, true)
    }

    /// uncertain: a refusal (r) of a key whose words answer for it (plain)
    /// at a limit of buzzCap or less, though the events certainly inside the
    /// window, with the overflow table's part (over), are under the limit.
    #[expect(clippy::too_many_arguments, reason = "the reference's signature, parameter for parameter")]
    pub(crate) fn uncertain(&self, plain: bool, r: &Result, w1: u64, last: u64, now: u64, win: u64, over: i64, limit: u64) -> bool {
        plain && !r.saturated && ((self.certain(w1, last, now, win) + over) as u64) < limit
    }
}
