//! stash.go: the stash and the wait room, the Background placer.

use std::sync::Arc;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering::SeqCst;
use std::thread::yield_now;
use std::time::Duration;

use crate::cpu::mul_hi;
use crate::cuckoo::{CLAIM_FULL, NO_STRIPE};
use crate::filter::Core;
use crate::hooks;
use crate::slots::*;

// Where a key's history was found.
pub(crate) const SRC_TABLE: i32 = 1;
pub(crate) const SRC_STASH: i32 = 2;
/// a waiting key was being committed to the table: look there again
pub(crate) const SRC_RETRY: i32 = 3;
/// a waiting key taken out of the wait room (recall with take): its move is under way
pub(crate) const SRC_ROOM: i32 = 4;

pub(crate) const BG_NAP: Duration = Duration::from_micros(50);
/// naps with nothing waiting before the placer exits
pub(crate) const BG_LINGER: i32 = 20;

// Eviction stash: a 4-way set-associative array of evicted entries, keyed by
// the entry's full identity (primary line, fp, tag: the same bits the table
// compares, so the stash adds no false positives beyond the table's own).
// Evictions are rare and slow already, so it uses plain striped locks.

pub(crate) const STASH_STRIPES: usize = 64;
pub(crate) const STASH_WAYS: u64 = 4;

/// deferMark, the displaced bit of a stash entry's word0 (otherwise clear),
/// marks a key that waits for a slot (ConstantTime) rather than one evicted.
pub(crate) const DEFER_MARK: u64 = DISP_BIT;

/// stashEntry: id 0 = empty; w0's displaced bit is clear.
#[derive(Clone, Copy, Default, PartialEq, Eq, Debug)]
pub(crate) struct StashEntry {
    pub(crate) id: u64,
    pub(crate) w0: u64,
    pub(crate) w1: u64,
}

/// A stashEntry in its table. The reference's entries are plain words under
/// their set's lock; here they are atomics read and written under that lock
/// (the lock is not the entries' owner, so safe Rust needs them shared).
#[derive(Default)]
#[repr(C)]
pub(crate) struct StashCell {
    id: AtomicU64,
    w0: AtomicU64,
    w1: AtomicU64,
}

const _: () = assert!(size_of::<StashCell>() == 24);

/// stashTable: sets of stashWays entries, and a bit per set that holds a key
/// waiting for a slot, set and cleared under the set's lock. The eviction
/// stash is one (its waiting keys are ConstantTime's); the wait room is
/// another (Background's, outside ConstantTime).
pub(crate) struct StashTable {
    pub(crate) e: Box<[StashCell]>,
    pub(crate) wait: Box<[AtomicU64]>,
    pub(crate) n: u64,                                // sets
    pub(crate) mu: Arc<[PaddedMutex; STASH_STRIPES]>, // the filter's, which exist before the table
    pub(crate) at: AtomicU64,                         // the next set to place waiting keys from
}

impl StashTable {
    pub(crate) fn new(sets: u64, mu: &Arc<[PaddedMutex; STASH_STRIPES]>) -> StashTable {
        // Written as it is made: its pages are faulted now (prefault).
        StashTable {
            e: (0..sets * STASH_WAYS).map(|_| StashCell::default()).collect(),
            wait: (0..sets.div_ceil(64)).map(|_| AtomicU64::new(0)).collect(),
            n: sets,
            mu: mu.clone(),
            at: AtomicU64::new(0),
        }
    }

    /// Entry k (the caller holds its set's lock, or the table is quiescent).
    #[inline]
    pub(crate) fn get(&self, k: usize) -> StashEntry {
        let c = &self.e[k];
        StashEntry { id: ld(&c.id), w0: ld(&c.w0), w1: ld(&c.w1) }
    }

    /// Writes entry k (as get).
    #[inline]
    pub(crate) fn put(&self, k: usize, e: StashEntry) {
        let c = &self.e[k];
        st(&c.id, e.id);
        st(&c.w0, e.w0);
        st(&c.w1, e.w1);
    }

    /// `e.w1 = w1` of entry k.
    #[inline]
    pub(crate) fn put_w1(&self, k: usize, w1: u64) {
        st(&self.e[k].w1, w1);
    }

    /// set returns id's set (the index of its first entry: the set is
    /// e[base..base+stashWays]) and its lock.
    #[inline]
    pub(crate) fn set(&self, id: u64) -> (usize, &PaddedMutex) {
        let i = mul_hi(mix64(id), self.n);
        ((i * STASH_WAYS) as usize, &self.mu[(i % STASH_STRIPES as u64) as usize])
    }

    /// waiting: id's set may hold a key waiting for a slot (its bit is set
    /// from the first such key's write until the placer finds the set without
    /// one).
    #[inline]
    pub(crate) fn waiting(&self, id: u64) -> bool {
        let i = mul_hi(mix64(id), self.n);
        (ld(&self.wait[(i / 64) as usize]) & (1 << (i % 64))) != 0
    }
}

impl Core {
    /// recall finds h's evicted history in the stash, or (Background) h
    /// waiting in the wait room, removed if take. The caller holds h's claim
    /// stripe. A key taken out of the wait room (srcRoom) counts as a move of
    /// its stripe from before it leaves until miss has placed it: lock-free
    /// lookups gate on the room's wait bits, which the placer clears once a
    /// set holds no waiting key, so a key in neither the table nor the room
    /// meanwhile would read absent.
    ///
    /// Returns (w0, w1, src).
    pub(crate) fn recall(&self, h: u64, fp: u64, tg: u64, now: u64, take: bool) -> (u64, u64, i32) {
        let id = self.stash_id(h, fp, tg);
        if !self.ct {
            if let Some(p) = self.room.get() {
                if p.waiting(id) {
                    let mv = if take { Some(self.move_of(tg)) } else { None };
                    let (w0, w1, ok, retry) = self.room_get(p, id, now, mv);
                    if retry {
                        return (0, 0, SRC_RETRY);
                    }
                    if ok && take {
                        return (w0, w1, SRC_ROOM);
                    }
                    if ok {
                        return (w0, w1, SRC_STASH);
                    }
                }
            }
            if now > self.stash_until.load(SeqCst) {
                return (0, 0, 0);
            }
        }
        let (w0, w1, ok) = self.stash_get(id, now, take);
        if ok {
            return (w0, w1, SRC_STASH);
        }
        (0, 0, 0)
    }

    /// stashMay: h's history may be outside the table at now: an evicted
    /// entry live in the stash, or (Background) h waiting in the wait room,
    /// whose set's bit is set while it waits.
    #[inline]
    pub(crate) fn stash_may(&self, h: u64, fp: u64, tg: u64, now: u64) -> bool {
        if now <= self.stash_until.load(SeqCst) {
            return true;
        }
        match self.room.get() {
            Some(p) => p.waiting(self.stash_id(h, fp, tg)),
            None => false,
        }
    }

    /// settled: no move of the stripe whose counts are m overlapped a lookup
    /// that read them as b and d before its table scan and its recall. A
    /// waiting key the placer commits to the table between the two
    /// (placeRoomKey, without the key's stripe) counts as such a move, so a
    /// key found in neither is absent only if settled. Without a wait room no
    /// key moves that way.
    #[inline]
    pub(crate) fn settled(&self, m: &MoveCount, b: u64, d: u64) -> bool {
        self.room.get().is_none() || (b == d && m.begun.load(SeqCst) == b)
    }

    /// roomGet is tableGet in the wait room (Background), where an entry
    /// being committed to the table (w1 busy, placeRoomKey) is waited out;
    /// retry then tells the caller to look in the table again if the entry
    /// went there. With mv (the key's move counts) it takes the entry out,
    /// its move begun first.
    ///
    /// Returns (w0, w1, ok, retry).
    pub(crate) fn room_get(&self, p: &StashTable, id: u64, now: u64, mv: Option<&MoveCount>) -> (u64, u64, bool, bool) {
        let take = mv.is_some();
        let (base, mu) = p.set(id);
        let mut retry = false;
        loop {
            let mut busy_now = false;
            let g = mu.lock();
            for k in base..base + STASH_WAYS as usize {
                let e = p.get(k);
                if e.id != id {
                    continue;
                }
                if (e.w1 >> TAG_SHIFT) == TAG_BUSY {
                    busy_now = true;
                    break;
                }
                if age(now, e.w0 & TS_MASK) > self.horizon {
                    if take {
                        // a fresh insert follows: the placer mustn't install this one too
                        self.stash_write(p, k, StashEntry::default());
                    }
                    break;
                }
                let (w0, w1) = (e.w0, e.w1);
                if let Some(mv) = mv {
                    inc(&mv.begun);
                    self.stash_write(p, k, StashEntry::default());
                }
                drop(g);
                if take {
                    hooks::TAKEN.run(());
                }
                return (w0, w1, true, false);
            }
            drop(g);
            if !busy_now {
                return (0, 0, false, retry);
            }
            retry = true;
            yield_now();
        }
    }

    /// lostRecently: without an overflow table, some history was lost within
    /// a horizon.
    #[inline]
    pub(crate) fn lost_recently(&self, now: u64) -> bool {
        let la = self.lost_at.load(SeqCst);
        la != 0 && age(now, la) <= self.horizon
    }

    /// placeRoomKey places waiting key e (as its set held it when read) at
    /// the clock of its last touch, without its claim stripe: the search
    /// blocks no first touch, and the key stays in the room meanwhile, where
    /// its touches and counts find it. put commits it: it marks the room
    /// entry busy (w1 busy; a recall of the key waits that out, then looks in
    /// the table again), installs the entry's words as they are now, and
    /// removes the entry, or restores it if the install lost its race. The
    /// commit counts as a move of the key's stripe, so a lookup that misses
    /// the table meanwhile looks again. If a touch of the key took it out of
    /// the room first, put fails and the key is that touch's to place.
    pub(crate) fn place_room_key(&self, p: &StashTable, e: StashEntry) {
        let id = e.id;
        let fp = (id >> TAG_BITS) & ((1 << 16) - 1);
        let m = self.move_of(id & ((1 << TAG_BITS) - 1));
        let pl = id >> 28;
        let put = |i: isize, w0: u64, w1: u64, mine0: u64, _: u64| -> bool {
            let (base, mu) = p.set(id);
            let g = mu.lock();
            let mut c = None;
            for k in base..base + STASH_WAYS as usize {
                let x = p.get(k);
                if x.id == id && (x.w0 & DEFER_MARK) != 0 && (x.w1 >> TAG_SHIFT) != TAG_BUSY {
                    c = Some(k);
                }
            }
            let Some(c) = c else {
                drop(g);
                return false;
            };
            let StashEntry { w0: c0, w1: c1, .. } = p.get(c);
            p.put_w1(c, BUSY);
            drop(g);
            hooks::PLACING.run(());
            inc(&m.begun);
            let ok = self.install(i, w0, w1, (c0 & !DEFER_MARK) | (mine0 & DISP_BIT), c1);
            let g = mu.lock();
            if ok {
                self.stash_write(p, c, StashEntry::default());
            } else {
                p.put_w1(c, c1);
            }
            drop(g);
            inc(&m.done);
            ok
        };
        self.claim_at([pl, self.alt(pl, fp)], [e.w0 & !DEFER_MARK, e.w0 | DISP_BIT], e.w1, e.w0 & TS_MASK, NO_STRIPE, CLAIM_FULL, Some(&put));
    }

    /// bgKick (Background) starts the placer thread unless it is running.
    pub(crate) fn bg_kick(&self) {
        if !self.bg_manual && !self.placing.load(SeqCst) && self.placing.compare_exchange(false, true, SeqCst, SeqCst).is_ok() {
            // The reference's `go f.bgPlace()`: the thread shares the filter
            // while it runs. A goroutine always starts; a thread the system
            // refuses leaves the keys waiting for the next wait's kick, and
            // first touches search inline once more than one key per 16
            // wait-room entries waits (miss), as when the placer falls behind.
            let me = self.me.upgrade().expect("clepsydra: a filter in use is alive");
            if std::thread::Builder::new().name("clepsydra-placer".into()).spawn(move || me.bg_place()).is_err() {
                self.placing.store(false, SeqCst);
            }
        }
    }

    /// bgPlace is the Background placer: it places waiting keys while any
    /// wait (it never reads the filter's clock: see placeRoomKey), and exits
    /// after bgLinger naps with nothing to place. A key that starts waiting
    /// as it exits sees placing false and starts another, or is seen here
    /// (both flags are sequentially consistent).
    pub(crate) fn bg_place(&self) {
        loop {
            let mut idle = 0;
            while idle < BG_LINGER {
                if self.deferred.load(SeqCst) > 0 && self.place_next(self.room.get(), 0) {
                    idle = 0;
                    continue;
                }
                idle += 1;
                std::thread::sleep(BG_NAP);
            }
            self.placing.store(false, SeqCst);
            if self.deferred.load(SeqCst) == 0 || self.placing.compare_exchange(false, true, SeqCst, SeqCst).is_err() {
                return;
            }
        }
    }

    /// allocStash allocates the stash on the first eviction, so a filter
    /// that never evicts doesn't pay for it.
    pub(crate) fn alloc_stash(&self) -> &StashTable {
        self.stash.get_or_init(|| StashTable::new(self.stash_n, &self.stash_mu))
    }

    /// waitTable is where keys wait for a slot, allocated on first use
    /// (None: nowhere). ConstantTime: the stash, whose set every call reads
    /// anyway. Background: the wait room, a small table of its own (one entry
    /// per 128 slots), so a filter that never evicts holds no stash.
    pub(crate) fn wait_table(&self) -> Option<&StashTable> {
        if self.ct {
            if self.stash_n == 0 {
                return None;
            }
            return Some(self.alloc_stash());
        }
        Some(self.room.get_or_init(|| StashTable::new(self.room_n, &self.room_mu)))
    }

    /// stashPut keeps an evicted entry in its set: in its own old place, a
    /// free or expired way, or else in place of the way with the fewest
    /// events in the horizon if that has fewer than it. Whichever history is
    /// dropped, Lost counts it.
    pub(crate) fn stash_put(&self, id: u64, w0: u64, w1: u64, now: u64) {
        self.stash_put_as(id, w0, w1, now, false)
    }

    /// stashPutAs is stashPut; wait marks the entry as waiting for a slot.
    pub(crate) fn stash_put_as(&self, id: u64, mut w0: u64, w1: u64, now: u64, wait: bool) {
        if self.stash_n == 0 {
            self.spill(id, w0, w1, now);
            return;
        }
        w0 &= !DISP_BIT;
        if wait {
            w0 |= DEFER_MARK;
        }
        let p = self.alloc_stash();
        let (base, mu) = p.set(id);
        let g = mu.lock();
        let (mut way, mut least, mut full) = (usize::MAX, u64::MAX, true); // full: every way holds another live history
        for i in 0..STASH_WAYS as usize {
            let e = p.get(base + i);
            if e.id == id {
                // An evictor holding an older snapshot of the same key (it
                // was evicted, restored and evicted again meanwhile) must not
                // overwrite the newer one.
                if (e.w0 & TS_MASK) > (w0 & TS_MASK)
                    || ((e.w0 & TS_MASK) == (w0 & TS_MASK) && self.mass(e.id, e.w1, e.w0 & TS_MASK, now) >= self.mass(id, w1, w0 & TS_MASK, now))
                {
                    drop(g);
                    return;
                }
                (way, least, full) = (i, 0, false);
                break;
            }
            if e.id == 0 || age(now, e.w0 & TS_MASK) > self.horizon {
                (way, least, full) = (i, 0, false);
                break;
            }
            let m = self.mass(e.id, e.w1, e.w0 & TS_MASK, now);
            if m < least {
                (way, least) = (i, m);
            }
        }
        let mut out = StashEntry::default(); // the history that leaves the stash, if any
        if full {
            if least > self.mass(id, w1, w0 & TS_MASK, now) {
                self.spill(id, w0, w1, now);
                drop(g);
                return;
            }
            // The lightest leaves through spill even with no events in the
            // horizon: a touch of a hot key that has moved word0 adds to its
            // entry by the key's identity, and spill closes the entry, so the
            // add fails and the touch counts afresh. Overwritten in place,
            // the history left its entry the key's, and the add counted where
            // nothing pointed (claims A32).
            out = p.get(base + way);
        }
        if out.id != 0 {
            self.spill(out.id, out.w0 & !DEFER_MARK, out.w1, now);
        }
        self.stash_write(p, base + way, StashEntry { id, w0, w1 });
        if !wait {
            // a waiting key moves no gate: see stashWait
            self.raise_stash_until((w0 & TS_MASK) + self.horizon);
        }
        drop(g);
    }

    /// raiseStashUntil moves stashUntil up to until, if it is below.
    pub(crate) fn raise_stash_until(&self, until: u64) {
        loop {
            let u = self.stash_until.load(SeqCst);
            if u >= until || cas(&self.stash_until, u, until) {
                return;
            }
        }
    }

    /// stashWait puts a key that waits for a slot in a free or expired way
    /// of its set in the wait table (waitTable), or in its own old place, and
    /// reports whether it did: a full set drops nothing for it. No
    /// stashUntil: ConstantTime reads the stash anyway, and Background
    /// lookups read the set's wait bit (stashMay). A way being committed to
    /// the table (word1 busy, placeRoomKey) is never taken, expired or not:
    /// the placer, which released the set's lock to install it, removes it or
    /// puts its word1 back afterwards, and would remove or overwrite the
    /// newcomer instead (claims A24).
    pub(crate) fn stash_wait(&self, id: u64, w0: u64, w1: u64, now: u64) -> bool {
        let Some(p) = self.wait_table() else {
            return false;
        };
        let (base, mu) = p.set(id);
        let _g = mu.lock();
        for k in base..base + STASH_WAYS as usize {
            let e = p.get(k);
            if (e.w1 >> TAG_SHIFT) != TAG_BUSY && (e.id == 0 || e.id == id || age(now, e.w0 & TS_MASK) > self.horizon) {
                self.stash_write(p, k, StashEntry { id, w0: (w0 & !DISP_BIT) | DEFER_MARK, w1 });
                return true;
            }
        }
        false
    }

    /// putBack returns a history recalled for an insert that lost a race for
    /// its slot, for the retry to find: a key that waited back to wait, an
    /// evicted one to the stash.
    pub(crate) fn put_back(&self, id: u64, w0: u64, w1: u64, now: u64) {
        if (w0 & DEFER_MARK) != 0 && self.stash_wait(id, w0, w1, now) {
            return;
        }
        self.stash_put(id, w0, w1, now);
    }

    /// stashWrite replaces entry k of table p (its set's lock held), keeping
    /// the count of entries that wait for a slot.
    pub(crate) fn stash_write(&self, p: &StashTable, k: usize, ne: StashEntry) {
        let e = p.get(k);
        if e.id != 0 && (e.w0 & DEFER_MARK) != 0 {
            self.deferred.fetch_add(-1, SeqCst);
        }
        if ne.id != 0 && (ne.w0 & DEFER_MARK) != 0 {
            self.deferred.fetch_add(1, SeqCst);
            let set = k as u64 / STASH_WAYS;
            let w = &p.wait[(set / 64) as usize];
            if (ld(w) & (1 << (set % 64))) == 0 {
                w.fetch_or(1 << (set % 64), SeqCst);
            }
        }
        p.put(k, ne);
    }

    /// stashGet returns the live stash entry for id; take also removes it.
    pub(crate) fn stash_get(&self, id: u64, now: u64, take: bool) -> (u64, u64, bool) {
        self.table_get(self.stash.get(), id, now, take)
    }

    /// tableGet is stashGet in table p (None: not allocated, so empty).
    pub(crate) fn table_get(&self, p: Option<&StashTable>, id: u64, now: u64, take: bool) -> (u64, u64, bool) {
        let Some(p) = p else {
            return (0, 0, false);
        };
        let (base, mu) = p.set(id);
        let _g = mu.lock();
        for k in base..base + STASH_WAYS as usize {
            let e = p.get(k);
            if e.id != id {
                continue;
            }
            if age(now, e.w0 & TS_MASK) > self.horizon {
                return (0, 0, false);
            }
            if take {
                self.stash_write(p, k, StashEntry::default());
            }
            return (e.w0, e.w1, true);
        }
        (0, 0, false)
    }

    /// dropped records that a live key's history was lost at now.
    pub(crate) fn dropped(&self, now: u64) {
        inc(&self.lost);
        loop {
            let la = self.lost_at.load(SeqCst);
            if la >= now || cas(&self.lost_at, la, now) {
                return;
            }
        }
    }
}
