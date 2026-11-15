//! The public `Filter`: each method is the Go reference's exported method of
//! the same name (snake_case), forwarding to `Core` (filter.rs).

use std::io::{Read, Write};
use std::sync::Arc;
use std::sync::atomic::Ordering::SeqCst;
use std::time::Duration;

use crate::filter::{Config, Core};
use crate::{Error, Result};

/// `Filter` is safe for concurrent use: hits never lock; first touches of a
/// key serialize on a striped lock. Dropping it stops its CoarseClock thread,
/// if any.
pub struct Filter {
    pub(crate) c: Arc<Core>,
}

impl Drop for Filter {
    fn drop(&mut self) {
        self.c.close();
    }
}

/// The reference's unexported identifiers, for the white-box tests: as
/// `go/*_test.go` reads `f.words`.
#[cfg(test)]
impl std::ops::Deref for Filter {
    type Target = Core;
    fn deref(&self) -> &Core {
        &self.c
    }
}

impl Filter {
    /// New allocates a `Filter` sized for `cfg.load` slot occupancy at
    /// `cfg.capacity`.
    pub fn new(cfg: Config) -> std::result::Result<Filter, Error> {
        Ok(Filter { c: Core::new(cfg, |_| {})? })
    }

    /// `new`, with the reference's unexported fields set by `tweak` before
    /// the filter is shared (tests: `f.cms = true` after `New`).
    #[cfg(test)]
    pub(crate) fn with(cfg: Config, tweak: impl FnOnce(&mut Core)) -> std::result::Result<Filter, Error> {
        Ok(Filter { c: Core::new(cfg, tweak)? })
    }

    /// Sets the reference's unexported fields on a filter in use (tests:
    /// `f.ct = true` between calls). The filter must be quiescent: no call
    /// under way, no placer or CoarseClock thread running.
    #[cfg(test)]
    pub(crate) fn tweak(&mut self, tweak: impl FnOnce(&mut Core)) {
        // SAFETY: the filter is this test's alone (&mut self) and quiescent,
        // so nothing else reads the Core during the call; the pointer is the
        // Arc's own, to its allocation (as Arc::get_mut_unchecked's). The
        // Core's Weak to itself, which keeps Arc::get_mut from serving, is
        // not dereferenced meanwhile.
        tweak(unsafe { &mut *(Arc::as_ptr(&self.c) as *mut Core) })
    }

    /// Close stops the CoarseClock thread, if any. The filter stays usable;
    /// its clock just stops advancing.
    pub fn close(&self) {
        self.c.close()
    }

    /// Now returns the filter's current tick.
    pub fn now(&self) -> u64 {
        self.c.clock()
    }

    /// Ticks converts a duration to filter ticks.
    pub fn ticks(&self, d: Duration) -> u64 {
        self.c.ticks(d)
    }

    /// Hash hashes a string key for the `*_at` methods, under the filter's
    /// secret key (`Config::key`).
    pub fn hash(&self, key: &str) -> u64 {
        self.c.hash(key)
    }

    /// HashUint64 hashes an integer key (IP, card BIN, user id) for the
    /// `*_at` methods, under the filter's secret key. The `*_at` methods
    /// trust the hash they are given: pass them only `hash` or `hash_uint64`
    /// values of this filter.
    pub fn hash_uint64(&self, k: u64) -> u64 {
        self.c.hash_uint64(k)
    }

    /// HashUint64s sets `out[i] = hash_uint64(keys[i])` for every key (`out`
    /// must be at least `keys.len()`; it panics otherwise).
    pub fn hash_uint64s(&self, keys: &[u64], out: &mut [u64]) {
        self.c.hash_uint64s(keys, out)
    }

    /// Touch records an occurrence of key now and returns the full `Result`
    /// for window: TouchAndCount plus the trust flags.
    pub fn touch(&self, key: &str, window: Duration) -> Result {
        self.c.touch(self.c.hash(key), self.c.ticks(window), self.c.clock(), true, -1)
    }

    /// TouchAt is Touch on a pre-hashed key, in ticks.
    pub fn touch_at(&self, h: u64, window: u64, now: u64) -> Result {
        self.c.touch_at(h, window, now)
    }

    /// Count is VelocityCount with the trust flags of Touch. It does not
    /// record an occurrence.
    pub fn count(&self, key: &str, window: Duration) -> Result {
        self.c.count_at(self.c.hash(key), self.c.ticks(window), self.c.clock())
    }

    /// CountAt is Count on a pre-hashed key, in ticks.
    pub fn count_at(&self, h: u64, window: u64, now: u64) -> Result {
        self.c.count_at(h, window, now)
    }

    /// CheckAndTouch reports whether key was seen within window, then
    /// records an occurrence now.
    pub fn check_and_touch(&self, key: &str, window: Duration) -> bool {
        self.c.touch(self.c.hash(key), self.c.ticks(window), self.c.clock(), false, -1).seen
    }

    /// CheckAndTouchAt is CheckAndTouch on a pre-hashed key with
    /// caller-supplied window and clock, both in ticks.
    pub fn check_and_touch_at(&self, h: u64, window: u64, now: u64) -> bool {
        self.c.touch(h, window, now, false, -1).seen
    }

    /// VelocityCount estimates how many times key was touched within window.
    /// It does not record an occurrence.
    pub fn velocity_count(&self, key: &str, window: Duration) -> i64 {
        self.c.count_at(self.c.hash(key), self.c.ticks(window), self.c.clock()).count
    }

    /// VelocityCountAt is VelocityCount on a pre-hashed key, in ticks.
    pub fn velocity_count_at(&self, h: u64, window: u64, now: u64) -> i64 {
        self.c.count_at(h, window, now).count
    }

    /// TouchAndCount records an occurrence and returns (seen within window
    /// before this touch, estimated count within window including this
    /// touch).
    pub fn touch_and_count(&self, key: &str, window: Duration) -> (bool, i64) {
        let r = self.c.touch(self.c.hash(key), self.c.ticks(window), self.c.clock(), true, -1);
        (r.seen, r.count)
    }

    /// TouchAndCountAt is TouchAndCount on a pre-hashed key, in ticks.
    pub fn touch_and_count_at(&self, h: u64, window: u64, now: u64) -> (bool, i64) {
        let r = self.c.touch(h, window, now, true, -1);
        (r.seen, r.count)
    }

    /// ContainsAt reports whether h was touched within window, without
    /// touching it.
    pub fn contains_at(&self, h: u64, window: u64, now: u64) -> bool {
        self.c.contains_at(h, window, now)
    }

    /// CheckAndTouchBatch is CheckAndTouchAt over many pre-hashed keys, in
    /// the order of `hs`, a chunk at a time with the chunk's cache misses
    /// overlapped. `out` must be at least `hs.len()` (it panics otherwise).
    pub fn check_and_touch_batch(&self, hs: &[u64], window: u64, now: u64, out: &mut [bool]) {
        self.c.check_and_touch_batch(hs, window, now, out)
    }

    /// TouchAndCountBatch is TouchAndCountAt over many pre-hashed keys, with
    /// the overlapped misses of CheckAndTouchBatch. `seen` and `counts` must
    /// be at least `hs.len()`.
    pub fn touch_and_count_batch(&self, hs: &[u64], window: u64, now: u64, seen: &mut [bool], counts: &mut [i64]) {
        self.c.touch_and_count_batch(hs, window, now, seen, counts)
    }

    /// VelocityCountBatch is VelocityCountAt over many pre-hashed keys, with
    /// the overlapped misses of CheckAndTouchBatch. `counts` must be at least
    /// `hs.len()`.
    pub fn velocity_count_batch(&self, hs: &[u64], window: u64, now: u64, counts: &mut [i64]) {
        self.c.velocity_count_batch(hs, window, now, counts)
    }

    /// TouchIfUnder records an occurrence of key now only if the key is
    /// under limit within window by the strict fail-closed recipe (Upper
    /// below limit, not Saturated, and without an overflow table not an
    /// unseen key read Degraded, whose history may have been lost), and
    /// reports whether it did. Refused, the filter is unchanged.
    pub fn touch_if_under(&self, key: &str, window: Duration, limit: u64) -> (Result, bool) {
        self.c.touch_if_under_at(self.c.hash(key), self.c.ticks(window), limit, self.c.clock())
    }

    /// TouchIfUnderAt is TouchIfUnder on a pre-hashed key, in ticks (claims
    /// A45, A46).
    pub fn touch_if_under_at(&self, h: u64, window: u64, limit: u64, now: u64) -> (Result, bool) {
        self.c.touch_if_under_at(h, window, limit, now)
    }

    /// ContinuousDecay reclaims expired slots at a rate set by the clock
    /// (one table pass per horizon, at most 4 lines per call). It never
    /// blocks and never pauses readers. Returns lines swept.
    pub fn continuous_decay(&self) -> i64 {
        self.c.continuous_decay()
    }

    /// Merge adds g's keys to this filter (claims D24, D26, D29). Both
    /// filters must be quiescent and built with the same Config (and Key).
    /// It returns an error when refused (this filter unchanged) or when a key
    /// finds no slot (the keys before it merged).
    pub fn merge(&self, g: &Filter) -> std::result::Result<(), Error> {
        self.c.merge(&g.c)
    }

    /// WriteTo writes the filter's state (a quiescent filter) as a snapshot,
    /// version 3 of the reference's format (claims D27, D29), and returns
    /// the bytes written.
    pub fn write_to<W: Write>(&self, w: &mut W) -> std::result::Result<i64, Error> {
        self.c.write_to(w)
    }

    /// ReadFrom restores a snapshot `write_to` wrote into this filter, built
    /// with the same Config and not used yet, and returns the bytes read. A
    /// filter whose `read_from` failed is unspecified and is to be discarded.
    pub fn read_from<R: Read>(&self, r: &mut R) -> std::result::Result<i64, Error> {
        self.c.read_from(r)
    }

    /// Evictions counts live keys displaced because both candidate lines
    /// were full.
    pub fn evictions(&self) -> u64 {
        self.c.evictions.load(SeqCst)
    }

    /// Relocations counts entries moved to their alternate line to make
    /// room.
    pub fn relocations(&self) -> u64 {
        self.c.relocations.load(SeqCst)
    }

    /// Lost counts live histories dropped because the stash was full and
    /// there is no overflow table: the evictions an attacker could use to
    /// reset a key's count. With the overflow table (the default) it stays 0.
    pub fn lost(&self) -> u64 {
        self.c.lost.load(SeqCst)
    }

    /// Folded counts histories the stash pushed into the overflow table.
    /// Their keys' answers are approximate (Degraded) until they age out.
    pub fn folded(&self) -> u64 {
        self.c.folded.load(SeqCst)
    }

    /// Shed counts dropped histories lighter than `Config::spill_min`,
    /// discarded instead of spilled to the overflow table.
    pub fn shed(&self) -> u64 {
        self.c.shed.load(SeqCst)
    }

    /// Waited counts first touches that found both lines full and waited for
    /// a slot: in the stash for later calls' pads to place them
    /// (ConstantTime), or in the wait room for the placer thread
    /// (Background).
    pub fn waited(&self) -> u64 {
        self.c.waited.load(SeqCst)
    }

    /// Waiting is the number of keys waiting for a slot now. If it stays
    /// high, ConstantTime pads are too short for the insert rate, and calls
    /// also place keys past their pads; with Background, the placer isn't
    /// keeping up, and first touches search inline again.
    pub fn waiting(&self) -> i64 {
        self.c.deferred.load(SeqCst)
    }

    /// Restored counts returning keys whose history came back from the
    /// stash.
    pub fn restored(&self) -> u64 {
        self.c.restored.load(SeqCst)
    }

    /// Promoted counts keys whose register moved to a hot entry.
    pub fn promoted(&self) -> u64 {
        self.c.promoted()
    }

    /// HotRefused counts promotions refused because every hot entry was in
    /// use: those keys saturate (Saturated) as they would without hot
    /// entries. Alert on it.
    pub fn hot_refused(&self) -> u64 {
        self.c.hot_refused()
    }

    /// Demoted counts hot entries handed back to their key's slot once its
    /// rate fell (every bucket at most half the register's width).
    pub fn demoted(&self) -> u64 {
        self.c.demoted()
    }

    /// HotInUse is the number of hot entries held now.
    pub fn hot_in_use(&self) -> i64 {
        self.c.hot_in_use()
    }

    /// Floored counts histories the overflow table had no room for: each
    /// left to a floor of its bucket, which then raises Upper for every key
    /// sharing that floor. It stays 0 below the table's capacity. Alert on
    /// it.
    pub fn floored(&self) -> u64 {
        self.c.floored()
    }

    /// Occupancy returns the number of slots holding a key live at now.
    /// O(table).
    pub fn occupancy(&self, now: u64) -> i64 {
        self.c.occupancy(now)
    }

    /// Slots returns the table's slot count.
    pub fn slots(&self) -> i64 {
        self.c.slots()
    }

    /// Bytes is the memory the filter holds now: its table, and the stash,
    /// overflow table, hot entries and buzz entries (with their blocks'
    /// bitmap) once allocated (each on first use).
    pub fn bytes(&self) -> i64 {
        self.c.bytes()
    }
}
