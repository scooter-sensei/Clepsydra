//! slots.go: slot words, lookups, fingerprint, tag, line arithmetic.

use std::alloc::{Layout, alloc_zeroed, dealloc, handle_alloc_error};
use std::ops::Deref;
use std::sync::atomic::{AtomicI64, AtomicU64, Ordering};
use std::sync::{Mutex, MutexGuard, TryLockError};

use crate::cpu::mul_hi;
use crate::filter::Core;
use crate::register::hot;

pub(crate) const SLOTS_PER_LINE: u64 = 4;
pub(crate) const WORDS_PER_LINE: u64 = 2 * SLOTS_PER_LINE;
/// dispBit marks an entry living in its alternate line ("displaced").
/// Lookups match it too, so a key is compared only with entries that share
/// its primary line, wherever they live: half the false matches.
pub(crate) const DISP_BIT: u64 = 1 << 47;
/// satBit marks a slot whose register overflowed (an event spilled to an
/// older bucket or was dropped) within its span; see estimate.
pub(crate) const SAT_BIT: u64 = 1 << 46;
pub(crate) const TS_MASK: u64 = SAT_BIT - 1;
pub(crate) const TAG_BITS: u32 = 12;
pub(crate) const TAG_SHIFT: u32 = 64 - TAG_BITS;
/// word1's bits 48-51, between the tag and the register: the onset of the
/// register's newest bucket, where in its unit the bucket's first event fell,
/// in sixteenths (0 says nothing). The register itself is always at the unit
/// of word0's last touch: every touch that starts a unit moves both words
/// together (realign), so no unit is stored.
pub(crate) const ONSET_SHIFT: u32 = 48;
pub(crate) const ONSET_MASK: u64 = 0xf;
/// busy is word1 while a slot is being handed over (install), while a touch
/// moves its register and last touch together (realign), and in a
/// relocation's copy until the hand-off (relocate). No key has its tag, so no
/// lookup or touch can match the slot until the new occupant's word1 replaces
/// it.
pub(crate) const TAG_BUSY: u64 = (1 << TAG_BITS) - 1;
pub(crate) const BUSY: u64 = TAG_BUSY << TAG_SHIFT;
/// claimStripeBits sets 256 insert-path lock stripes, picked by the hash's
/// top bits.
pub(crate) const CLAIM_STRIPE_BITS: u32 = 8;

// Go's sync/atomic is sequentially consistent: so is every access here.

/// `atomic.LoadUint64`.
#[inline(always)]
pub(crate) fn ld(a: &AtomicU64) -> u64 {
    a.load(Ordering::SeqCst)
}

/// `atomic.StoreUint64`.
#[inline(always)]
pub(crate) fn st(a: &AtomicU64, v: u64) {
    a.store(v, Ordering::SeqCst)
}

/// `atomic.CompareAndSwapUint64`.
#[inline(always)]
pub(crate) fn cas(a: &AtomicU64, old: u64, new: u64) -> bool {
    a.compare_exchange(old, new, Ordering::SeqCst, Ordering::SeqCst).is_ok()
}

/// `x.Add(1)` of an atomic.Uint64.
#[inline(always)]
pub(crate) fn inc(a: &AtomicU64) {
    a.fetch_add(1, Ordering::SeqCst);
}

/// A Go `defer` at function scope: runs when the function returns. Declared
/// in the order of the reference's defers, so they run in Go's order (last
/// first).
pub(crate) struct Defer<F: FnMut()>(pub(crate) F);

impl<F: FnMut()> Drop for Defer<F> {
    fn drop(&mut self) {
        (self.0)()
    }
}

/// paddedMutex: a `sync.Mutex` on a cache line of its own. A panic while it
/// is held (none in the library: a tick past MaxTick panics before any lock)
/// leaves no poison behind: Go's mutexes have none.
#[repr(align(64))]
#[derive(Default)]
pub(crate) struct PaddedMutex(Mutex<()>);

impl PaddedMutex {
    pub(crate) const fn new() -> Self {
        PaddedMutex(Mutex::new(()))
    }

    /// `mu.Lock()`; dropping the guard is `mu.Unlock()`.
    #[inline]
    pub(crate) fn lock(&self) -> MutexGuard<'_, ()> {
        self.0.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// `mu.TryLock()`.
    #[inline]
    pub(crate) fn try_lock(&self) -> Option<MutexGuard<'_, ()>> {
        match self.0.try_lock() {
            Ok(g) => Some(g),
            Err(TryLockError::Poisoned(e)) => Some(e.into_inner()),
            Err(TryLockError::WouldBlock) => None,
        }
    }
}

/// A value on cache lines of its own: the reference's `_ [64]byte` padding
/// between fields that calls on other cores write.
#[repr(align(64))]
#[derive(Default)]
pub(crate) struct Pad<T>(pub(crate) T);

impl<T> Deref for Pad<T> {
    type Target = T;
    #[inline(always)]
    fn deref(&self) -> &T {
        &self.0
    }
}

/// moveCount: moves of a stripe's keys begun and done, and evictions of them
/// under way. A relocation counts its move, then checks for an eviction; an
/// eviction counts itself, then checks for a move. With both counts
/// sequentially consistent at least one of two that overlap sees the other
/// and backs off, so no eviction takes the source of an entry being moved,
/// which would fail the move's hand-off: the move's work would be lost, not
/// the key's history (relocate).
#[derive(Default)]
pub(crate) struct MoveCount {
    pub(crate) begun: AtomicU64,
    pub(crate) done: AtomicU64,
    pub(crate) evicting: AtomicI64,
}

/// alignedWords: a 64-byte aligned, zeroed array of words. The pages come
/// zeroed from the allocator and fault on first touch, as the reference's.
pub(crate) struct AlignedWords {
    p: *mut AtomicU64,
    n: usize,
}

// SAFETY: the buffer is owned and holds only atomics.
unsafe impl Send for AlignedWords {}
unsafe impl Sync for AlignedWords {}

impl AlignedWords {
    pub(crate) fn new(n: usize) -> Self {
        assert!(n > 0 && n <= isize::MAX as usize / 8);
        let layout = Layout::from_size_align(n * 8, 64).unwrap();
        // SAFETY: the layout's size is not zero.
        let p = unsafe { alloc_zeroed(layout) } as *mut AtomicU64;
        if p.is_null() {
            handle_alloc_error(layout);
        }
        AlignedWords { p, n }
    }

    /// prefault writes a word of every page, so a table allocated while the
    /// filter is in use takes its page faults now rather than one by one in
    /// later calls (a ConstantTime call's timing would show them).
    pub(crate) fn prefault(&self) {
        let mut i = 0;
        while i < self.n {
            st(&self[i], 0);
            i += 4096 / 8;
        }
    }
}

impl Drop for AlignedWords {
    fn drop(&mut self) {
        // SAFETY: allocated in new with this layout.
        unsafe { dealloc(self.p as *mut u8, Layout::from_size_align(self.n * 8, 64).unwrap()) }
    }
}

impl Deref for AlignedWords {
    type Target = [AtomicU64];
    #[inline(always)]
    fn deref(&self) -> &[AtomicU64] {
        // SAFETY: p points to n zero-initialized (or since atomically
        // written) words, alive as long as self; AtomicU64 has u64's layout
        // and all-zero is a valid value.
        unsafe { std::slice::from_raw_parts(self.p, self.n) }
    }
}

impl Core {
    /// Slot word `i` (a word index; a negative one panics, as an index out
    /// of range does in the reference).
    #[inline(always)]
    pub(crate) fn w(&self, i: isize) -> &AtomicU64 {
        &self.words[i as usize]
    }

    /// moveOf is the move counts of entries whose word1 tag is tg. A hold of
    /// a slot takes word1 busy by a CAS from the value it read, so whoever
    /// the slot holds when the CAS succeeds has that value's tag: counted on
    /// the tag's stripe, the hold tells that key's lookups to look again,
    /// even when it is not the occupant whose word0 the holder read (a key
    /// installed between the holder's reads of word0 and word1, or one whose
    /// word1 is bit-identical to the old occupant's: the same tag, epoch bits
    /// and counts).
    #[inline(always)]
    pub(crate) fn move_of(&self, tg: u64) -> &MoveCount {
        &self.moves[(tg & ((1 << CLAIM_STRIPE_BITS) - 1)) as usize]
    }

    /// slotWords loads slot i's words if it still holds fpd/tg: the
    /// fingerprint and displaced bit (word0>>47) and the tag that the lookup
    /// matched there. The displaced bit is part of the key's identity, as in
    /// find: a key with the same fingerprint and tag whose primary line is
    /// this key's alternate lives in this key's primary line displaced, and a
    /// relocation can move it into the slot this key was found in between the
    /// lookup and the load. word0 is read again after word1: a realign
    /// between the two loads would pair the old last touch with the new
    /// register, whose epoch is read against it.
    #[inline]
    pub(crate) fn slot_words(&self, i: isize, fpd: u64, tg: u64) -> (u64, u64, bool) {
        if i < 0 {
            return (0, 0, false);
        }
        let w = &self.words[i as usize..i as usize + 2]; // one bounds check for the three loads
        let w0 = ld(&w[0]);
        let w1 = ld(&w[1]);
        if (w0 >> 47) != fpd || (w1 >> TAG_SHIFT) != tg {
            return (w0, w1, false);
        }
        let a = ld(&w[0]);
        if a != w0 && self.unit_of(a & TS_MASK) != self.unit_of(w0 & TS_MASK) {
            return (w0, w1, false);
        }
        (w0, w1, true)
    }

    /// markSat sets (on) or clears the saturated bit of the slot at word
    /// index i if it still holds fpd (fingerprint and displaced bit:
    /// word0>>47, as bump checks), and returns the bit as left. It clears
    /// only while no bucket is hot, reading word1 after word0: an overflowing
    /// touch writes its register before it sets the bit, so a clear that read
    /// a register from before that touch fails its CAS on the newer word0.
    pub(crate) fn mark_sat(&self, i: isize, fpd: u64, on: bool) -> bool {
        loop {
            let w0 = ld(self.w(i));
            if (w0 >> 47) != fpd {
                return on;
            }
            if ((w0 & SAT_BIT) != 0) == on {
                return on;
            }
            let w1 = ld(self.w(i + 1));
            if !on && ((w1 >> TAG_SHIFT) == TAG_BUSY || hot(w1)) {
                return true; // a busy word1 (realign) may come back with a hot bucket
            }
            if cas(self.w(i), w0, w0 ^ SAT_BIT) {
                return on;
            }
        }
    }

    /// locate returns the word index of h's slot, or -1: an undisplaced
    /// entry in the primary line or a displaced one in the alternate line.
    #[allow(dead_code)] // the tests'
    pub(crate) fn locate(&self, h: u64, fp: u64, tg: u64) -> isize {
        self.locate_at(h, fp, tg).0
    }

    /// locateAt is locate, with the fingerprint and displaced bit it matched
    /// (fpd, word0>>47): what a later load of the slot must still hold.
    #[inline]
    pub(crate) fn locate_at(&self, h: u64, fp: u64, tg: u64) -> (isize, u64) {
        let p = self.primary(h);
        let i = self.find(p, fp << 1, tg);
        if i >= 0 {
            return (i, fp << 1);
        }
        (self.find(self.alt(p, fp), (fp << 1) | 1, tg), (fp << 1) | 1)
    }

    /// locateBoth is locateAt reading both lines every time (ConstantTime).
    pub(crate) fn locate_both(&self, h: u64, fp: u64, tg: u64) -> (isize, u64) {
        let p = self.primary(h);
        let i = self.find(p, fp << 1, tg);
        let j = self.find(self.alt(p, fp), (fp << 1) | 1, tg);
        if i >= 0 {
            return (i, fp << 1);
        }
        (j, (fp << 1) | 1)
    }

    /// find returns the word index of the slot in line whose fingerprint and
    /// displaced bit equal fpd and whose tag is tg, or -1. The tag lives in
    /// word1 of the same cache line, so identity is effectively 28 bits at no
    /// extra memory traffic.
    #[inline]
    pub(crate) fn find(&self, line: u64, fpd: u64, tg: u64) -> isize {
        let base = (line * WORDS_PER_LINE) as usize;
        let w = &self.words[base..base + WORDS_PER_LINE as usize];
        let mut s = 0;
        while s < WORDS_PER_LINE as usize {
            if (ld(&w[s]) >> 47) == fpd && (ld(&w[s + 1]) >> TAG_SHIFT) == tg {
                return (base + s) as isize;
            }
            s += 2;
        }
        -1
    }

    /// primary is h's home line. It uses hash bits 0..35, disjoint from the
    /// fingerprint (48..63) and tag (36..47), so keys sharing a line share no
    /// identity bits. Multiply-shift lets the table be any size.
    #[inline(always)]
    pub(crate) fn primary(&self, h: u64) -> u64 {
        mul_hi(h << 28, self.lines) // bits 0..35 moved to the top
    }

    /// alt is the other candidate line for fp stored in line. It depends
    /// only on (line, fp), as in cuckoo filters, so a stored entry can be
    /// relocated without knowing its key: alt(l) = (x - l) mod lines is its
    /// own inverse. For one or two lines per fp, alt(l) == l; those keys get
    /// one line.
    #[inline(always)]
    pub(crate) fn alt(&self, line: u64, fp: u64) -> u64 {
        let x = mul_hi(mix64(fp.wrapping_mul(0x9e3779b97f4a7c15)), self.lines);
        if x >= line {
            return x - line;
        }
        x + self.lines - line
    }

    /// stashID packs a key's identity: primary line (< 2^36) | fp | tag.
    #[inline(always)]
    pub(crate) fn stash_id(&self, h: u64, fp: u64, tg: u64) -> u64 {
        (self.primary(h) << 28) | (fp << TAG_BITS) | tg
    }

    /// slotID is stashID for the entry in a slot of line, without its key.
    #[inline(always)]
    pub(crate) fn slot_id(&self, mut line: u64, w0: u64, w1: u64) -> u64 {
        let fp = w0 >> 48;
        if (w0 & DISP_BIT) != 0 {
            line = self.alt(line, fp);
        }
        (line << 28) | (fp << TAG_BITS) | (w1 >> TAG_SHIFT)
    }
}

#[inline(always)]
pub(crate) fn fingerprint(h: u64) -> u64 {
    let fp = h >> 48;
    if fp != 0 {
        return fp;
    }
    1
}

/// tag is 12 more fingerprint bits, kept in word1: with the 16-bit fp the
/// slot identity is 28 bits. tagBusy is reserved; keys that would have it
/// share the tag below.
#[inline(always)]
pub(crate) fn tag(h: u64) -> u64 {
    let t = (h >> 36) & ((1 << TAG_BITS) - 1);
    t ^ ((t + 1) >> TAG_BITS)
}

/// age is now-ts, or 0 if ts is ahead (a caller with a slightly stale clock).
#[inline(always)]
pub(crate) fn age(now: u64, ts: u64) -> u64 {
    if now < ts {
        return 0;
    }
    now - ts
}

#[inline(always)]
pub(crate) fn mix64(mut x: u64) -> u64 {
    x ^= x >> 30;
    x = x.wrapping_mul(0xbf58476d1ce4e5b9);
    x ^= x >> 27;
    x = x.wrapping_mul(0x94d049bb133111eb);
    x ^ (x >> 31)
}
