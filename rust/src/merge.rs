//! merge.go: Merge.

use crate::Error;
use crate::cpu;
use crate::cuckoo::CLAIM_FULL;
use crate::filter::Core;
use crate::hot::View;
use crate::register::{N_BUCKETS, Register, pack, reg_add, reg_advance};
use crate::slots::*;
use crate::stash::SRC_STASH;

impl Core {
    /// Merge adds g's keys to f: for a key in both, the two registers are
    /// re-bucketed to the newer one's unit and summed bucket by bucket (an
    /// overflow sets the saturated bit), and the last touch is the newer one;
    /// a key only g's table holds goes in as a first touch would (which may
    /// evict), with f's own stashed history of it, if f evicted it, summed in
    /// the same way (as a first touch restores it: installed without it, f's
    /// reads would find the key in the table and skip the stashed history).
    /// Both filters must be quiescent and built with the same Config (and
    /// Key). g's stash and overflow table (evicted histories) are not merged.
    /// ConstantTime, Background and hot entries in use are refused with an
    /// error, f unchanged. A key that can't get a slot (a writer racing the
    /// merge) stops it with an error, the keys before it merged.
    pub(crate) fn merge(&self, g: &Core) -> std::result::Result<(), Error> {
        let f = self;
        if std::ptr::eq(f, g) {
            return Err(Error::new("temporalfilter: Merge with itself"));
        }
        if f.lines != g.lines || f.horizon != g.horizon || f.unit != g.unit || f.res != g.res {
            return Err(Error::new("temporalfilter: Merge needs the same Config"));
        }
        if f.ct || g.ct || f.bg || g.bg {
            return Err(Error::new("temporalfilter: Merge of ConstantTime or Background filters"));
        }
        if f.hot.get().is_some() || g.hot.get().is_some() {
            return Err(Error::new("temporalfilter: Merge with hot entries in use"));
        }
        let mut now = 0u64;
        for x in [f, g] {
            let mut i = 0;
            while i < x.words.len() {
                now = now.max(ld(&x.words[i]) & TS_MASK);
                i += 2;
            }
        }
        let mut i = 0isize;
        while (i as usize) < g.words.len() {
            let (w0, w1) = (ld(g.w(i)), ld(g.w(i + 1)));
            let gi = i;
            i += 2;
            let last = w0 & TS_MASK;
            if w0 == 0 || (w1 >> TAG_SHIFT) == TAG_BUSY || age(now, last) > g.horizon {
                continue;
            }
            let id = g.slot_id((gi as u64) / WORDS_PER_LINE, w0, w1);
            let (line, fp, tg) = (id >> 28, w0 >> 48, w1 >> TAG_SHIFT);
            let mut lo = cpu::div64(line >> 28, line << 36, f.lines); // ceil(line * 2^36 / lines): f.primary(h) == line
            if lo.wrapping_mul(f.lines) < (line << 36) {
                lo += 1;
            }
            let h = (fp << 48) | (tg << 36) | lo;
            if f.primary(h) != line || fingerprint(h) != fp || tag(h) != tg {
                return Err(Error::new("temporalfilter: Merge: identity does not round-trip"));
            }
            let (vg, _) = g.view_of(id, w1, last);
            let _mu = f.claim_mu[(h >> (64 - CLAIM_STRIPE_BITS)) as usize].lock();
            let (j, fw0, fw1) = f.lookup_stable(h, fp, tg, false);
            if j >= 0 {
                let (vf, _) = f.view_of(id, fw1, fw0 & TS_MASK);
                let (nw1, over) = sum_registers(vf, vg, tg);
                let mut sat = (fw0 | w0) & SAT_BIT;
                if over {
                    sat = SAT_BIT;
                }
                st(f.w(j + 1), nw1);
                st(f.w(j), (fw0 & !(TS_MASK | SAT_BIT)) | sat | last.max(fw0 & TS_MASK));
            } else {
                // f may hold the key's evicted history in its stash: take it
                // and sum it in. Background is refused, so recall finds
                // nothing or the stash (srcStash), and with hot entries
                // refused its word1 is a register.
                let (mut nw1, mut sat, mut stamp) = (w1, w0 & SAT_BIT, last);
                let (s0, s1, src) = f.recall(h, fp, tg, now, true);
                if src == SRC_STASH {
                    let (vf, _) = f.view_of(id, s1, s0 & TS_MASK);
                    let over;
                    (nw1, over) = sum_registers(vf, vg, tg);
                    sat = (s0 | w0) & SAT_BIT;
                    if over {
                        sat = SAT_BIT;
                    }
                    stamp = last.max(s0 & TS_MASK);
                }
                let mut placed = false;
                for _ in 0..8 {
                    if f.claim(h, fp, nw1, now, stamp, sat, CLAIM_FULL) >= 0 {
                        placed = true;
                        break;
                    }
                }
                if !placed {
                    if src == SRC_STASH {
                        // put it back, as miss does for its retry
                        f.put_back(f.stash_id(h, fp, tg), s0, s1, now);
                    }
                    return Err(Error::new("temporalfilter: Merge: a key found no slot in 8 tries (both filters must be quiescent)"));
                }
            }
        }
        Ok(())
    }
}

/// sumRegisters re-buckets views a and b to the newer unit and adds them
/// bucket by bucket: the merged register word for tag tg, and whether a
/// bucket overflowed (the caller sets the saturated bit).
pub(crate) fn sum_registers(a: View, b: View, tg: u64) -> (u64, bool) {
    let e = a.e.max(b.e);
    let (mut ra, mut rb): (Register, Register) = (a.c, b.c);
    let mut over = reg_advance(&mut ra, a.e, e);
    over = reg_advance(&mut rb, b.e, e) || over;
    for i in 0..N_BUCKETS {
        over = reg_add(&mut ra, i, rb[i]) || over;
    }
    // Bucket 0 is the newer register's (the older one's events advance out
    // of it): its onset, or the earlier of two in one unit (0 says nothing).
    let mut on = a.on;
    if a.e == b.e {
        on = a.on.min(b.on);
    } else if b.e > a.e {
        on = b.on;
    }
    ((tg << TAG_SHIFT) | (on << ONSET_SHIFT) | pack(&ra), over)
}
