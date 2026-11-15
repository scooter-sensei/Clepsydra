//! batch.go: the batches and their prefetching.

use crate::cpu;
use crate::filter::Core;
use crate::slots::*;

/// batchChunk is how many keys warm overlaps (S48: 32 beat 64 on tables near
/// the last-level cache's size and matched it past them).
pub(crate) const BATCH_CHUNK: usize = 32;

impl Core {
    /// CheckAndTouchBatch is CheckAndTouchAt over many pre-hashed keys, in
    /// the order of hs, a chunk at a time with the chunk's cache misses
    /// overlapped (see warm). out must be at least len(hs).
    pub(crate) fn check_and_touch_batch(&self, hs: &[u64], window: u64, now: u64, out: &mut [bool]) {
        let out = &mut out[..hs.len()];
        let mut at = [0isize; BATCH_CHUNK];
        let mut lo = 0;
        while lo < hs.len() {
            let hi = (lo + BATCH_CHUNK).min(hs.len());
            self.warm(&hs[lo..hi], &mut at);
            for i in lo..hi {
                out[i] = self.touch(hs[i], window, now, false, at[i - lo]).seen;
            }
            lo += BATCH_CHUNK;
        }
    }

    /// TouchAndCountBatch is TouchAndCountAt over many pre-hashed keys, with
    /// the overlapped misses of CheckAndTouchBatch. seen and counts must be
    /// at least len(hs).
    pub(crate) fn touch_and_count_batch(&self, hs: &[u64], window: u64, now: u64, seen: &mut [bool], counts: &mut [i64]) {
        let (seen, counts) = (&mut seen[..hs.len()], &mut counts[..hs.len()]);
        let mut at = [0isize; BATCH_CHUNK];
        let mut lo = 0;
        while lo < hs.len() {
            let hi = (lo + BATCH_CHUNK).min(hs.len());
            self.warm(&hs[lo..hi], &mut at);
            for i in lo..hi {
                let r = self.touch(hs[i], window, now, true, at[i - lo]);
                (seen[i], counts[i]) = (r.seen, r.count);
            }
            lo += BATCH_CHUNK;
        }
    }

    /// VelocityCountBatch is VelocityCountAt over many pre-hashed keys, with
    /// the overlapped misses of CheckAndTouchBatch. counts must be at least
    /// len(hs).
    pub(crate) fn velocity_count_batch(&self, hs: &[u64], window: u64, now: u64, counts: &mut [i64]) {
        let counts = &mut counts[..hs.len()];
        let mut at = [0isize; BATCH_CHUNK];
        let mut lo = 0;
        while lo < hs.len() {
            let hi = (lo + BATCH_CHUNK).min(hs.len());
            self.warm(&hs[lo..hi], &mut at);
            for i in lo..hi {
                counts[i] = self.count(hs[i], window, now, at[i - lo]).count;
            }
            lo += BATCH_CHUNK;
        }
    }

    /// warm overlaps the cache misses of a chunk's keys (memory-level
    /// parallelism). It prefetches every key's primary line, looks for each
    /// key there (at[k]: its slot, or -1), then prefetches the alternate
    /// lines of the keys it didn't find there, so each key's operation, in
    /// order, finds its lines cached or on their way, and a touch or count
    /// starts from its slot. Prefetch instructions, unlike loads, don't hold
    /// up retirement, and the touches' locked CASes don't wait for them. S48:
    /// 1.2x the loads it replaces at 90% load on tables past the last-level
    /// cache; 0.9x on a table that fits it.
    ///
    /// In ConstantTime it looks nothing up and prefetches both lines of
    /// every key: its work, outside the calls' pads, doesn't depend on what
    /// the table holds (touchCT reads both lines anyway).
    pub(crate) fn warm(&self, hs: &[u64], at: &mut [isize; BATCH_CHUNK]) {
        let mut lines = [0u64; 2 * BATCH_CHUNK];
        if self.ct {
            for (k, &h) in hs.iter().enumerate() {
                let p = self.primary(h);
                (lines[2 * k], lines[2 * k + 1], at[k]) = (p, self.alt(p, fingerprint(h)), -1);
            }
            cpu::prefetch_lines(&self.words, &lines[..2 * hs.len()]);
            return;
        }
        for (k, &h) in hs.iter().enumerate() {
            lines[k] = self.primary(h);
        }
        cpu::prefetch_lines(&self.words, &lines[..hs.len()]);
        let mut m = 0;
        for (k, &h) in hs.iter().enumerate() {
            let (p, fp) = (lines[k], fingerprint(h));
            at[k] = self.find(p, fp << 1, tag(h));
            if at[k] < 0 {
                lines[m] = self.alt(p, fp); // m <= k: the entries before k's are used
                m += 1;
            }
        }
        cpu::prefetch_lines(&self.words, &lines[..m]);
    }
}
