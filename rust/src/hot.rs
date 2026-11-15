//! hot.go: hot entries.
//!
//! A key whose register would overflow (more than 31 events in its newest
//! Horizon/24 unit, 12.4/s at a 60 s horizon, or a merge past an older
//! bucket's width) is promoted instead: its register moves to a hot entry,
//! the same 7 buckets with 32-bit counters in a side table of one cache line
//! per entry, so its counts stay exact far past any real rate. The slot keeps
//! word0 and the key's tag; word1's 48 register bits hold the entry's index
//! with bucket 0 empty, which no register has (bucket 0 counts the unit of
//! the register's last event, so it holds at least 1). Lookups, the
//! false-positive rate and the slot protocol are unchanged.
//!
//! The table is allocated on the first promotion (Config.Hot entries). An
//! entry belongs to the identity (stashID) it records and is given up when
//! its newest event is past the horizon (then no live slot or stash entry can
//! refer to it), or when its key's rate falls (demote). With every entry in
//! use a key isn't promoted: it saturates as without hot entries, and
//! HotRefused counts it. Writers lock an entry (its version is odd while
//! held); readers read it as a seqlock.

use std::sync::Mutex;
use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicI64, AtomicU32, AtomicU64};
use std::thread::yield_now;

use crate::cpu;
use crate::filter::Core;
use crate::register::{BUCKET_BITS, BUCKET_BITS0, N_BUCKETS, REG_MASK, Register, UNITS_PER_HORIZON, bucket_start, pack, unpack};
use crate::slots::*;

/// the index sits above bucket 0's empty bits
pub(crate) const HOT_IDX_SHIFT: u32 = BUCKET_BITS0;
pub(crate) const HOT_IDX_MASK: u64 = (1 << (48 - HOT_IDX_SHIFT)) - 1;
pub(crate) const SAT_NONE: i64 = i64::MIN;

// Bucket 0's word: a touch in the entry's own unit adds to the count with one
// CAS while hotClosed is clear (add). A writer closes it on taking the entry
// and reopens it with the next generation on leaving, so a CAS that read the
// word before a change of owner or unit fails.
//
// The word's top 4 bits hold bucket 0's onset, as a register's onset field
// does (claims D29), so a reader takes it with the count and a snapshot
// carries it; the generation keeps the 27 bits between.
pub(crate) const HOT_COUNT: u64 = (1 << 32) - 1;
pub(crate) const HOT_CLOSED: u64 = 1 << 32;
pub(crate) const HOT_GEN: u64 = 1 << 33;
pub(crate) const HOT_ON_SHIFT: u32 = 60;
pub(crate) const HOT_ON_MASK: u64 = ONSET_MASK << HOT_ON_SHIFT;

#[repr(C, align(64))]
#[derive(Default)]
pub(crate) struct HotEntry {
    pub(crate) ver: AtomicU64,   // odd while a writer holds the entry
    pub(crate) id: AtomicU64,    // owner's identity; 0: free
    pub(crate) epoch: AtomicI64, // unit of bucket 0
    pub(crate) sat: AtomicI64,   // newest unit whose events may be missing (counts are floors there); satNone
    pub(crate) c0: AtomicU64,    // bucket 0's count (hotCount) | hotClosed | generation (hotGen and up)
    pub(crate) c: [AtomicU32; N_BUCKETS - 1],
}

const _: () = assert!(size_of::<HotEntry>() == 64);

/// isHot reports whether word1 points at a hot entry instead of holding a
/// register.
#[inline(always)]
pub(crate) fn is_hot(w1: u64) -> bool {
    (w1 & ((1 << BUCKET_BITS0) - 1)) == 0 && (w1 & REG_MASK) != 0
}

#[inline(always)]
pub(crate) fn hot_idx(w1: u64) -> u64 {
    (w1 >> HOT_IDX_SHIFT) & HOT_IDX_MASK
}

/// The part of a hotTable its mutex guards.
#[derive(Default)]
pub(crate) struct HotFree {
    pub(crate) free: Vec<u32>,
    pub(crate) next: u64, // entries never handed out: next..len(e)-1
    pub(crate) hand: u64, // reclaim scan position
}

pub(crate) struct HotTable {
    pub(crate) e: Box<[HotEntry]>, // index 0 unused
    pub(crate) mu: Mutex<HotFree>,
    pub(crate) full: AtomicI64, // unit in which an allocation last found no entry
    pub(crate) in_use: AtomicI64,
    pub(crate) demoted: AtomicU64,
}

/// view is a register's state, narrow or hot: bucket counts at unit e, bucket
/// 0's onset in sixteenths of its unit (0 says nothing) and, for a hot entry,
/// the newest unit whose events may be missing.
#[derive(Clone, Copy, Default, Debug)]
pub(crate) struct View {
    pub(crate) e: i64,
    pub(crate) c: [u64; N_BUCKETS],
    pub(crate) on: u64,
    pub(crate) sat: i64,
    pub(crate) hot: bool,
}

impl HotEntry {
    pub(crate) fn lock(&self) -> u64 {
        let mut spins = 0u32;
        loop {
            let v = self.ver.load(SeqCst);
            if (v & 1) == 0 && cas(&self.ver, v, v + 1) {
                self.c0.fetch_or(HOT_CLOSED, SeqCst);
                return v + 1;
            }
            if (spins & 63) == 63 {
                yield_now();
            }
            spins = spins.wrapping_add(1);
        }
    }

    pub(crate) fn unlock(&self, ver: u64) {
        let c = self.c0.load(SeqCst);
        // the generation wraps inside its bits
        self.c0.store((((c & !HOT_CLOSED).wrapping_add(HOT_GEN)) & !HOT_ON_MASK) | (c & HOT_ON_MASK), SeqCst);
        self.ver.store(ver + 1, SeqCst);
    }

    /// load reads the entry's state; the caller holds it or validates the
    /// version around the call.
    pub(crate) fn load(&self) -> View {
        let mut v = View { e: self.epoch.load(SeqCst), sat: self.sat.load(SeqCst), hot: true, ..View::default() };
        let c0 = self.c0.load(SeqCst);
        (v.c[0], v.on) = (c0 & HOT_COUNT, c0 >> HOT_ON_SHIFT);
        for j in 1..N_BUCKETS {
            v.c[j] = self.c[j - 1].load(SeqCst) as u64;
        }
        v
    }

    /// store writes v into a held entry. A count past 32 bits (~1.7e9 events
    /// per second for a unit) is kept as a floor.
    pub(crate) fn store(&self, v: &mut View) {
        for j in 0..N_BUCKETS {
            if v.c[j] > u32::MAX as u64 {
                (v.c[j], v.sat) = (u32::MAX as u64, v.sat.max(v.e));
            }
            if j == 0 {
                self.c0.store((self.c0.load(SeqCst) & !(HOT_COUNT | HOT_ON_MASK)) | v.c[0] | (v.on << HOT_ON_SHIFT), SeqCst);
            } else {
                self.c[j - 1].store(v.c[j] as u32, SeqCst);
            }
        }
        self.epoch.store(v.e, SeqCst);
        self.sat.store(v.sat, SeqCst);
    }
}

impl HotTable {
    /// A table of n entries (index 0 unused), none handed out.
    pub(crate) fn new(n: u64) -> HotTable {
        let t = HotTable {
            e: (0..n + 1).map(|_| HotEntry::default()).collect(),
            mu: Mutex::new(HotFree { free: Vec::new(), next: 1, hand: 0 }),
            full: AtomicI64::new(SAT_NONE),
            in_use: AtomicI64::new(0),
            demoted: AtomicU64::new(0),
        };
        // Written as it is made: its pages are faulted now (prefault).
        t
    }

    #[inline]
    pub(crate) fn guard(&self) -> std::sync::MutexGuard<'_, HotFree> {
        self.mu.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// read returns entry idx's state if it belongs to id.
    pub(crate) fn read(&self, idx: u64, id: u64) -> (View, bool) {
        let e = &self.e[idx as usize];
        let mut spins = 0u32;
        loop {
            let v1 = e.ver.load(SeqCst);
            if (v1 & 1) == 0 {
                let owner = e.id.load(SeqCst);
                let v = e.load();
                if e.ver.load(SeqCst) == v1 {
                    return (v, owner == id);
                }
            }
            if (spins & 63) == 63 {
                yield_now();
            }
            spins = spins.wrapping_add(1);
        }
    }

    /// add counts n events at now (unit u) into entry idx of f's table if it
    /// belongs to id. Events at a unit older than the entry's go to the
    /// bucket that covers it. If slot (a word0 index; -1: none) is the slot
    /// whose word1 points here (old), a touch that starts a new unit with
    /// every bucket at most half the register's width hands the counts back
    /// to the slot as a register and frees the entry (demote).
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn add(&self, f: &Core, idx: u64, id: u64, n: u64, u: i64, now: u64, slot: isize, old: u64) -> bool {
        let e = &self.e[idx as usize];
        loop {
            // the usual case, a touch in the entry's own unit: one CAS
            let c = e.c0.load(SeqCst);
            if (c & HOT_CLOSED) != 0 || e.id.load(SeqCst) != id || e.epoch.load(SeqCst) != u || (c & HOT_COUNT) + n > HOT_COUNT {
                break;
            }
            if cas(&e.c0, c, c + n) {
                return true;
            }
        }
        let ver = e.lock();
        if e.id.load(SeqCst) != id {
            e.unlock(ver);
            return false;
        }
        if u == e.epoch.load(SeqCst) {
            let c = (e.c0.load(SeqCst) & HOT_COUNT) + n;
            if c <= HOT_COUNT {
                e.c0.store((e.c0.load(SeqCst) & !HOT_COUNT) | c, SeqCst);
                e.unlock(ver);
                return true;
            }
        }
        let mut v = e.load();
        if u > v.e {
            advance_wide(&mut v.c, v.e, u);
            (v.e, v.on) = (u, f.onset_of(now, u)); // these events start bucket 0
            v.c[0] += n;
        } else if u == v.e {
            v.c[0] += n;
            v.on = v.on.min(f.onset_of(now, u)); // a caller behind the onset
        } else {
            let mut x = [0u64; N_BUCKETS];
            x[0] = n;
            advance_wide(&mut x, u, v.e);
            for j in 0..N_BUCKETS {
                v.c[j] += x[j];
            }
        }
        if slot >= 0 && v.e == u && demotable(&v, u) {
            let r: Register = v.c;
            let nw = (old & !((1u64 << TAG_SHIFT) - 1)) | (v.on << ONSET_SHIFT) | pack(&r);
            if f.demote_slot(slot, old, nw, u) {
                e.id.store(0, SeqCst);
                e.unlock(ver);
                self.put_free(idx);
                inc(&self.demoted);
                return true;
            }
        }
        e.store(&mut v);
        e.unlock(ver);
        true
    }

    /// putFree puts a closed entry (its owner 0) on the free list.
    pub(crate) fn put_free(&self, idx: u64) {
        self.in_use.fetch_add(-1, SeqCst);
        let mut g = self.guard();
        g.free.push(idx as u32);
        self.full.store(SAT_NONE, SeqCst);
    }

    /// reclaim takes an entry whose newest event is past the horizon at unit
    /// u, so no live slot or stash entry can refer to it (caller holds
    /// t.mu: g).
    pub(crate) fn reclaim(&self, g: &mut HotFree, u: i64) -> u64 {
        let dead = u - UNITS_PER_HORIZON as i64 - 2;
        let n = (self.e.len() - 1) as u64;
        for _ in 0..n.min(256) {
            g.hand = g.hand % n + 1;
            let e = &self.e[g.hand as usize];
            if e.id.load(SeqCst) == 0 || e.epoch.load(SeqCst) >= dead {
                continue; // free ones are on the free list
            }
            let ver = e.lock();
            let ok = e.id.load(SeqCst) != 0 && e.epoch.load(SeqCst) < dead;
            if ok {
                e.id.store(0, SeqCst);
            }
            e.unlock(ver);
            if ok {
                self.in_use.fetch_add(-1, SeqCst);
                return g.hand;
            }
        }
        0
    }
}

/// demotable: v fits a register at half of every bucket's width (so the key
/// isn't promoted again at once), and no event of the register's span is
/// missing.
pub(crate) fn demotable(v: &View, u: i64) -> bool {
    if v.sat != SAT_NONE && v.sat + 2 * UNITS_PER_HORIZON as i64 >= u {
        return false;
    }
    for (j, &c) in v.c.iter().enumerate() {
        if c > ((1u64 << BUCKET_BITS[j]) - 1) / 2 {
            return false;
        }
    }
    true
}

/// advanceWide is register.advance for a hot entry's uncapped counts.
pub(crate) fn advance_wide(r: &mut [u64; N_BUCKETS], u0: i64, u1: i64) {
    if u1 <= u0 {
        return;
    }
    if u1 - u0 > 2 * UNITS_PER_HORIZON as i64 {
        *r = [0; N_BUCKETS];
        return;
    }
    let mut starts = [0i64; N_BUCKETS];
    for (j, s) in starts.iter_mut().enumerate() {
        *s = bucket_start(u1, j);
    }
    let mut out = [0u64; N_BUCKETS];
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
        out[j] += r[k];
    }
    *r = out;
}

impl Core {
    /// hotTable: the table, allocated on the first promotion.
    pub(crate) fn hot_table(&self) -> &HotTable {
        self.hot.get_or_init(|| HotTable::new(self.hot_n))
    }

    /// demoteSlot is a demotion's swap of the word1 of slot i (a word0
    /// index) from the hot pointer p to the register nw, at unit u, taken
    /// only while word0's last touch is in u: by one 16-byte CAS of both
    /// words, word0 compared as loaded, or else under a hold of word1, word0
    /// read under it. A register must never sit beside a last touch of
    /// another unit, and a touch of the hot key into a later unit moves word0
    /// while word1 points here (touch's stamp, which compares word1 or holds
    /// it in turn). A demotion that loses the race is left for a later unit.
    pub(crate) fn demote_slot(&self, i: isize, p: u64, nw: u64, u: i64) -> bool {
        if cpu::has_cas2() {
            let w0 = ld(self.w(i));
            return self.unit_of(w0 & TS_MASK) == u && cpu::cas2(&self.words[i as usize..i as usize + 2], w0, p, w0, nw);
        }
        let m = self.move_of(p >> TAG_SHIFT);
        inc(&m.begun);
        let _done = Defer(|| inc(&m.done));
        if !cas(self.w(i + 1), p, BUSY) {
            return false;
        }
        if self.unit_of(ld(self.w(i)) & TS_MASK) != u {
            st(self.w(i + 1), p);
            return false;
        }
        st(self.w(i + 1), nw);
        true
    }

    /// hotAlloc takes a free entry for id, holding v, or returns 0 if none
    /// is free.
    pub(crate) fn hot_alloc(&self, id: u64, v: &mut View, now: u64) -> u64 {
        let t = self.hot_table();
        let u = self.unit_of(now);
        if t.full.load(SeqCst) == u {
            // found none in this unit already: don't search again
            inc(&self.hot_refused);
            return 0;
        }
        let idx;
        {
            let mut g = t.guard();
            if let Some(x) = g.free.pop() {
                idx = x as u64;
            } else if g.next < t.e.len() as u64 {
                idx = g.next;
                g.next += 1;
            } else {
                idx = t.reclaim(&mut g, u);
            }
            if idx == 0 {
                t.full.store(u, SeqCst);
            }
        }
        if idx == 0 {
            inc(&self.hot_refused);
            return 0;
        }
        let e = &t.e[idx as usize];
        let ver = e.lock();
        e.id.store(id, SeqCst);
        e.store(v);
        e.unlock(ver);
        t.in_use.fetch_add(1, SeqCst);
        idx
    }

    /// hotClose gives up entry idx if it belongs to id and returns its last
    /// state. A writer that loses the race finds the id changed and looks
    /// again.
    pub(crate) fn hot_close(&self, idx: u64, id: u64) -> (View, bool) {
        let Some(t) = self.hot.get() else {
            return (View::default(), false);
        };
        let e = &t.e[idx as usize];
        let ver = e.lock();
        let (mut v, mut ok) = (View::default(), false);
        if e.id.load(SeqCst) == id {
            (v, ok) = (e.load(), true);
            e.id.store(0, SeqCst);
        }
        e.unlock(ver);
        if ok {
            t.put_free(idx);
        }
        (v, ok)
    }

    /// promote moves the register of the key in slot i (word index; w0 and
    /// old: its words, last: its lastTouch before this touch) to a hot entry
    /// with this touch counted, instead of letting the touch spill. It
    /// returns the slot's new word1, or ok false if no entry is free, word1
    /// changed meanwhile, or the slot is another key's (claims A29).
    pub(crate) fn promote(&self, i: isize, w0: u64, old: u64, tg: u64, last: u64, now: u64) -> (u64, bool) {
        let (w1, idx, id) = self.hot_entry_for(i, w0, old, tg, last, now);
        if w1 == 0 {
            return (0, false);
        }
        // Published under a hold, counted as a move, not by a CAS from old: a
        // slot handed to a key with old's tag can hold old bit for bit, and
        // would point to this key's entry. Under the hold, word0 says whose
        // the slot is.
        let m = self.move_of(tg);
        inc(&m.begun);
        let _done = Defer(|| inc(&m.done));
        if !cas(self.w(i + 1), old, BUSY) {
            self.hot_close(idx, id); // never published
            return (0, false);
        }
        if (ld(self.w(i)) >> 47) != (w0 >> 47) {
            st(self.w(i + 1), old);
            self.hot_close(idx, id);
            return (0, false);
        }
        st(self.w(i + 1), w1);
        inc(&self.promoted);
        (w1, true)
    }

    /// promoteHeld is promote for a touch that holds slot i's word1 busy
    /// (realign), which publishes w1 itself.
    pub(crate) fn promote_held(&self, i: isize, w0: u64, old: u64, tg: u64, last: u64, now: u64) -> (u64, bool) {
        let (w1, _, _) = self.hot_entry_for(i, w0, old, tg, last, now);
        if w1 == 0 {
            return (0, false);
        }
        inc(&self.promoted);
        (w1, true)
    }

    /// hotEntryFor takes a free hot entry for the key in slot i (word0 w0,
    /// its register old, last touched at last) holding old's counts and one
    /// event at now: word1 pointing to it (0 if none is free), its index and
    /// identity.
    pub(crate) fn hot_entry_for(&self, i: isize, w0: u64, old: u64, tg: u64, last: u64, now: u64) -> (u64, u64, u64) {
        if self.hot_n == 0 {
            return (0, 0, 0);
        }
        if let Some(t) = self.hot.get() {
            if t.full.load(SeqCst) == self.unit_of(now) {
                inc(&self.hot_refused); // every entry was in use a moment ago: don't search again this unit
                return (0, 0, 0);
            }
        }
        let id = self.slot_id((i as u64) / WORDS_PER_LINE, w0, old);
        let mut v = View { e: self.unit_of(last), on: (old >> ONSET_SHIFT) & ONSET_MASK, sat: SAT_NONE, hot: true, ..View::default() };
        v.c = unpack(old & REG_MASK);
        if (w0 & SAT_BIT) != 0 {
            // it overflowed before: its counts up to now are floors
            v.sat = v.e;
        }
        let u = self.unit_of(now);
        if u > v.e {
            advance_wide(&mut v.c, v.e, u);
            (v.e, v.on) = (u, self.onset_of(now, u)); // this touch starts bucket 0
        } else if u == v.e {
            v.on = v.on.min(self.onset_of(now, u));
        } else {
            v.on = 0; // an event from an earlier unit, counted at v.e
        }
        v.c[0] += 1;
        let idx = self.hot_alloc(id, &mut v, now);
        if idx == 0 {
            return (0, 0, 0);
        }
        ((tg << TAG_SHIFT) | (idx << HOT_IDX_SHIFT), idx, id)
    }

    /// viewOf returns the register state that word1 holds or points to, for
    /// the key with identity id and lastTouch last. ok is false if w1 points
    /// to an entry that is no longer the key's.
    #[inline]
    pub(crate) fn view_of(&self, id: u64, w1: u64, last: u64) -> (View, bool) {
        if is_hot(w1) {
            if let Some(t) = self.hot.get() {
                return t.read(hot_idx(w1), id);
            }
            return (View::default(), false);
        }
        let v = View {
            e: self.unit_of(last),
            on: (w1 >> ONSET_SHIFT) & ONSET_MASK,
            c: [w1 & 0x1F, (w1 >> 5) & 0x1F, (w1 >> 10) & 0x3F, (w1 >> 16) & 0x7F, (w1 >> 23) & 0xFF, (w1 >> 31) & 0xFF, (w1 >> 39) & 0x1FF],
            sat: SAT_NONE,
            hot: false,
        };
        (v, true)
    }

    /// hotGone: w1 points to a hot entry that is no longer the key id's. An
    /// entry is reclaimed once its newest event is past the horizon at the
    /// reclaiming call's clock, which can be ahead of another caller's by
    /// more: a history out of the table (stash, wait room) whose entry went
    /// that way is gone, though its last touch is within a horizon of a
    /// lagging caller's clock. Restored as is, the touch restoring it went
    /// nowhere, and the slot pointed at another key's entry (claims A25).
    pub(crate) fn hot_gone(&self, id: u64, w1: u64) -> bool {
        if !is_hot(w1) {
            return false;
        }
        let (_, ok) = self.view_of(id, w1, 0);
        !ok
    }

    /// slotDrop gives up the hot entry, if any, of a slot's history that the
    /// slot itself discards past the horizon (a touch's reset, the sweep, an
    /// insert over the expired occupant): id is the history's identity, w1
    /// the slot's word1 as taken. A slot is its key's only live reference to
    /// the entry, so nothing else counts in it any more; left held, it stayed
    /// in HotInUse until a reclaim, and a lagging touch's add that came after
    /// landed in it, where nothing pointed, instead of failing and looking
    /// again. The stash's and the wait room's expired ways don't close
    /// theirs: such a way can share its entry with the key's live slot, once
    /// the entry was reclaimed and given to the same key again.
    #[inline]
    pub(crate) fn slot_drop(&self, id: u64, w1: u64) {
        if is_hot(w1) {
            self.hot_close(hot_idx(w1), id);
        }
    }

    /// hotAdd counts one event at now into the entry word1 points to. slot,
    /// if not -1, is the slot (a word0 index; its word1 holds w1) the touch
    /// found the key by: the entry may then be handed back to it as a
    /// register (demote).
    #[inline]
    pub(crate) fn hot_add(&self, w1: u64, id: u64, now: u64, slot: isize) -> bool {
        match self.hot.get() {
            Some(t) => t.add(self, hot_idx(w1), id, 1, self.unit_of(now), now, slot, w1),
            None => false,
        }
    }

    /// Promoted counts keys whose register moved to a hot entry.
    pub(crate) fn promoted(&self) -> u64 {
        self.promoted.load(SeqCst)
    }

    /// HotRefused counts promotions refused because every hot entry was in
    /// use: those keys saturate (Saturated) as they would without hot
    /// entries. Alert on it.
    pub(crate) fn hot_refused(&self) -> u64 {
        self.hot_refused.load(SeqCst)
    }

    /// Demoted counts hot entries handed back to their key's slot once its
    /// rate fell (every bucket at most half the register's width).
    pub(crate) fn demoted(&self) -> u64 {
        self.hot.get().map_or(0, |t| t.demoted.load(SeqCst))
    }

    /// HotInUse is the number of hot entries held now.
    pub(crate) fn hot_in_use(&self) -> i64 {
        self.hot.get().map_or(0, |t| t.in_use.load(SeqCst))
    }
}
