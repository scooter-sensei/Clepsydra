//! snapshot.go: WriteTo, ReadFrom (version 3, reads version 2).
//!
//! Snapshot format, version 3 (claims D27, D29; version 1 was E38's
//! unversioned stream, never in the library; version 2 held each register's
//! unit in word1's bits 48-51, which version 3 gives to the onset). A stream
//! of little-endian 64-bit words in seven sections (six in version 2), each
//! ended by a check word:
//!
//! ```text
//! 0 header:   magic "CLEPSNP1", version (3), the Config's shape (11 words:
//!             lines, horizon, unit, resolution, stash sets, hot entries,
//!             overflow bytes, overflow choices, floors per bucket, SpillMin,
//!             count-min bytes; with the overflow table off all four
//!             overflow words are 0), the clock anchor (tick, wall-clock ns)
//! 1 table:    lines x 8 words, as the table holds them
//! 2 counters: decay cursor, evictions, relocations, pressure, stash gate,
//!             lostAt, lost, restored, folded, shed, waited, promoted,
//!             hot refused
//! 3 stash:    presence (0/1); if 1: sets x 4 entries (id, word0, word1),
//!             the wait bitmap's words, the placement cursor
//! 4 hot:      presence; if 1: entries (version, id, epoch, saturated unit,
//!             bucket-0 word, buckets 1-6 two to a word), the free list
//!             (length, then one index per word), next, hand, full, in use,
//!             demoted
//! 5 overflow: presence; if 1: the table's words, epoch, until, saturated
//!             mark, floored
//! 6 buzz:     presence; if 1: entries (id, word0, word1, meta, the ring's
//!             four words); version 3 only (claims A46)
//! ```
//!
//! A check word holds the CRC32C (Castagnoli) of its section's bytes (every
//! word since the previous check word) in bits 0-31 and the section number in
//! bits 32-39; nothing follows section 6 (5 in version 2). ReadFrom refuses a
//! bad magic, another version or Config, a short stream, a check word that
//! doesn't match and trailing bytes; the filter it read into is then
//! unspecified and is to be discarded. The check words catch accidental
//! damage (every truncation and single-bit flip tried), not a crafted stream:
//! a snapshot is trusted input.
//!
//! Snapshots (claims D27): WriteTo and ReadFrom save and restore a quiescent
//! filter's state (the table, stash, hot entries and overflow table, the
//! decay cursor and counters) into a filter New made with the same Config.
//! With the built-in clock the restored filter's clock continues the old
//! timeline (the old tick plus the wall time since the snapshot), so stored
//! ticks stay valid; a caller's Clock must continue it itself. String keys
//! need Config.Key: the default seed is per process. ConstantTime, Background
//! and the count-min overflow are refused.

use std::io::{BufReader, BufWriter, Read, Write};
use std::sync::atomic::Ordering::SeqCst;
use std::time::{SystemTime, UNIX_EPOCH};

use crate::Error;
use crate::buzz::{BUZZ_WAYS, BuzzEntry};
use crate::crc32c;
use crate::filter::{Clock, Core, MAX_TICK};
use crate::hot::{HOT_ON_MASK, hot_idx, is_hot};
use crate::register::{REG_MASK, pack, reg_advance, unpack};
use crate::slots::*;
use crate::stash::StashEntry;

pub(crate) const SNAP_VERSION: u64 = 3;
pub(crate) const SNAP_MAGIC: u64 = 0x31504e5350454c43; // "CLEPSNP1"

const ERR_SNAP_MODE: &str = "temporalfilter: snapshots of ConstantTime, Background or count-min filters are not supported";

struct SnapW<'a, W: Write> {
    w: BufWriter<&'a mut W>,
    n: i64,
    err: Option<Error>,
    crc: u32,
    sec: u32,
}

impl<W: Write> SnapW<'_, W> {
    /// end writes the section's check word.
    fn end(&mut self) {
        let x = self.crc as u64 | ((self.sec as u64) << 32);
        self.u(x);
        (self.crc, self.sec) = (0, self.sec + 1);
    }

    fn u(&mut self, x: u64) {
        if self.err.is_none() {
            let b = x.to_le_bytes();
            self.crc = crc32c::update(self.crc, &b);
            match self.w.write_all(&b) {
                Ok(()) => self.n += 8,
                Err(e) => self.err = Some(Error::new(e.to_string())),
            }
        }
    }
}

struct SnapR<'a, R: Read> {
    r: BufReader<&'a mut R>,
    n: i64,
    err: Option<Error>,
    crc: u32,
    sec: u32,
}

impl<R: Read> SnapR<'_, R> {
    /// end reads and checks the section's check word.
    fn end(&mut self) {
        let want = self.crc as u64 | ((self.sec as u64) << 32);
        let got = self.u();
        if self.err.is_none() && got != want {
            self.fail(&format!("section {}: check word {:#x}, want {:#x}", self.sec, got, want));
        }
        (self.crc, self.sec) = (0, self.sec + 1);
    }

    fn u(&mut self) -> u64 {
        if self.err.is_some() {
            return 0;
        }
        let mut b = [0u8; 8];
        let mut m = 0;
        while m < 8 {
            match self.r.read(&mut b[m..]) {
                Ok(0) => break,
                Ok(k) => m += k,
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
                Err(_) => break,
            }
        }
        self.n += m as i64;
        if m < 8 {
            self.err = Some(Error::new("temporalfilter: snapshot: short stream"));
            return 0;
        }
        self.crc = crc32c::update(self.crc, &b);
        u64::from_le_bytes(b)
    }

    fn fail(&mut self, msg: &str) {
        if self.err.is_none() {
            self.err = Some(Error::new(format!("temporalfilter: snapshot: {msg}")));
        }
    }
}

/// `time.Now().UnixNano()`.
fn unix_nano() -> i64 {
    match SystemTime::now().duration_since(UNIX_EPOCH) {
        Ok(d) => d.as_nanos().min(i64::MAX as u128) as i64,
        Err(_) => 0,
    }
}

impl Core {
    pub(crate) fn snap_shape(&self) -> [u64; 11] {
        [
            self.lines,
            self.horizon,
            self.unit,
            self.res,
            self.stash_n,
            self.hot_n,
            self.sk_w,
            self.gh_choices as u64,
            self.gh_sub as u64,
            self.spill_min,
            self.sk_shape.bytes(self.sk_w),
        ]
    }

    /// WriteTo writes f's state to w. f must be quiescent. Returns the bytes
    /// written.
    pub(crate) fn write_to<W: Write>(&self, w: &mut W) -> std::result::Result<i64, Error> {
        if self.ct || self.bg || self.cms {
            return Err(Error::new(ERR_SNAP_MODE));
        }
        let mut s = SnapW { w: BufWriter::new(w), n: 0, err: None, crc: 0, sec: 0 };
        s.u(SNAP_MAGIC);
        s.u(SNAP_VERSION);
        for x in self.snap_shape() {
            s.u(x);
        }
        s.u(self.clock());
        s.u(unix_nano() as u64);
        s.end();
        for w in self.words.iter() {
            s.u(ld(w));
        }
        s.end();
        for x in [
            self.cursor.load(SeqCst),
            self.evictions.load(SeqCst),
            self.relocations.load(SeqCst),
            self.pressure.load(SeqCst) as u64,
            self.stash_until.load(SeqCst),
            self.lost_at.load(SeqCst),
            self.lost.load(SeqCst),
            self.restored.load(SeqCst),
            self.folded.load(SeqCst),
            self.shed.load(SeqCst),
            self.waited.load(SeqCst),
            self.promoted.load(SeqCst),
            self.hot_refused.load(SeqCst),
        ] {
            s.u(x);
        }
        s.end();
        if let Some(st) = self.stash.get() {
            s.u(1);
            for i in 0..st.e.len() {
                let e = st.get(i);
                s.u(e.id);
                s.u(e.w0);
                s.u(e.w1);
            }
            for w in st.wait.iter() {
                s.u(ld(w));
            }
            s.u(st.at.load(SeqCst));
        } else {
            s.u(0);
        }
        s.end();
        if let Some(t) = self.hot.get() {
            s.u(1);
            for e in t.e.iter() {
                s.u(e.ver.load(SeqCst));
                s.u(e.id.load(SeqCst));
                s.u(e.epoch.load(SeqCst) as u64);
                s.u(e.sat.load(SeqCst) as u64);
                s.u(e.c0.load(SeqCst));
                let mut j = 0;
                while j < e.c.len() {
                    s.u(e.c[j].load(SeqCst) as u64 | ((e.c[j + 1].load(SeqCst) as u64) << 32));
                    j += 2;
                }
            }
            let g = t.guard();
            s.u(g.free.len() as u64);
            for &x in &g.free {
                s.u(x as u64);
            }
            s.u(g.next);
            s.u(g.hand);
            s.u(t.full.load(SeqCst) as u64);
            s.u(t.in_use.load(SeqCst) as u64);
            s.u(t.demoted.load(SeqCst));
        } else {
            s.u(0);
        }
        s.end();
        if let Some(g) = self.gh.get() {
            s.u(1);
            for w in g.words.iter() {
                s.u(ld(w));
            }
            s.u(g.epoch.load(SeqCst));
            s.u(g.until.load(SeqCst));
            s.u(g.sat.load(SeqCst) as u64);
            s.u(g.floored.load(SeqCst));
        } else {
            s.u(0);
        }
        s.end();
        if let Some(t) = self.buzz.get() {
            s.u(1);
            for b in t.b.iter() {
                for j in 0..BUZZ_WAYS {
                    let e = b.entry(j);
                    for x in [ld(&b.ids[j]), e.w0, e.w1, e.meta, e.ev[0], e.ev[1], e.ev[2], e.ev[3]] {
                        s.u(x);
                    }
                }
            }
        } else {
            s.u(0);
        }
        s.end();
        if s.err.is_none() {
            if let Err(e) = s.w.flush() {
                s.err = Some(Error::new(e.to_string()));
            }
        }
        match s.err {
            Some(e) => Err(e),
            None => Ok(s.n),
        }
    }

    /// continued is the built-in clock's tick for a snapshot written at tick
    /// oldNow and wall-clock wall (Unix ns): the old tick plus the wall time
    /// since.
    pub(crate) fn continued(&self, old_now: u64, wall: i64) -> u64 {
        old_now.wrapping_add((0.max(unix_nano().wrapping_sub(wall)) as u64) / self.res)
    }

    /// ReadFrom restores a snapshot WriteTo wrote into f, which New made
    /// with the same Config and nothing has used yet. With the built-in clock
    /// it refuses a snapshot whose continued clock would be past MaxTick.
    /// Returns the bytes read.
    ///
    /// A stream whose check words are right but whose words no quiescent
    /// filter writes (an index past a table, a held hot entry) is refused
    /// too (snapSane), where the reference panics or hangs at the value's
    /// first use; a refused restore leaves the filter empty (snapScrub). The
    /// C++ port's deviation, kept here.
    pub(crate) fn read_from<R: Read>(&self, r: &mut R) -> std::result::Result<i64, Error> {
        if self.ct || self.bg || self.cms {
            return Err(Error::new(ERR_SNAP_MODE));
        }
        let mut s = SnapR { r: BufReader::new(r), n: 0, err: None, crc: 0, sec: 0 };
        let mut wrote = false; // a section has been stored into the filter
        self.read_sections(&mut s, &mut wrote);
        match s.err {
            Some(e) => {
                if wrote {
                    self.snap_scrub(); // unspecified, but nothing in it points past a table
                }
                Err(e)
            }
            None => Ok(s.n),
        }
    }

    fn read_sections<R: Read>(&self, s: &mut SnapR<'_, R>, wrote: &mut bool) {
        let f = self;
        if s.u() != SNAP_MAGIC {
            s.fail("bad magic");
        }
        let v = s.u();
        if v != SNAP_VERSION && v != 2 {
            s.fail(&format!("version {v}, want {SNAP_VERSION} or 2"));
        }
        for x in f.snap_shape() {
            if s.u() != x {
                s.fail("another Config");
            }
        }
        let (old_now, wall) = (s.u(), s.u() as i64);
        s.end();
        // The built-in clock continues the old timeline (below), so a lineage
        // of snapshots can carry it past MaxTick, where hits read wrong until
        // an insert panics (claims A35): refuse it before anything is
        // written.
        if s.err.is_none() && f.own_clock && f.continued(old_now, wall) > MAX_TICK {
            s.fail("the continued clock is past MaxTick");
        }
        if s.err.is_some() {
            return;
        }
        *wrote = true;
        for w in f.words.iter() {
            st(w, s.u());
        }
        if v == 2 {
            let mut i = 0;
            while i < f.words.len() {
                let (w0, w1) = f.from_v2(ld(&f.words[i]), ld(&f.words[i + 1]));
                st(&f.words[i], w0);
                st(&f.words[i + 1], w1);
                i += 2;
            }
        }
        s.end();
        f.cursor.store(s.u(), SeqCst);
        f.evictions.store(s.u(), SeqCst);
        f.relocations.store(s.u(), SeqCst);
        f.pressure.store(s.u() as u32, SeqCst);
        f.stash_until.store(s.u(), SeqCst);
        f.lost_at.store(s.u(), SeqCst);
        f.lost.store(s.u(), SeqCst);
        f.restored.store(s.u(), SeqCst);
        f.folded.store(s.u(), SeqCst);
        f.shed.store(s.u(), SeqCst);
        f.waited.store(s.u(), SeqCst);
        f.promoted.store(s.u(), SeqCst);
        f.hot_refused.store(s.u(), SeqCst);
        s.end();
        if s.u() == 1 {
            if f.stash_n == 0 {
                s.fail("stash entries in a filter without them");
                return;
            }
            let p = f.alloc_stash();
            for i in 0..p.e.len() {
                let mut e = StashEntry { id: s.u(), w0: s.u(), w1: s.u() };
                if v == 2 {
                    (e.w0, e.w1) = f.from_v2(e.w0, e.w1);
                }
                p.put(i, e);
            }
            for w in p.wait.iter() {
                st(w, s.u());
            }
            p.at.store(s.u(), SeqCst);
        }
        s.end();
        if s.u() == 1 {
            if f.hot_n == 0 {
                s.fail("hot entries in a filter without them");
                return;
            }
            let t = f.hot_table();
            for e in t.e.iter() {
                e.ver.store(s.u(), SeqCst);
                e.id.store(s.u(), SeqCst);
                e.epoch.store(s.u() as i64, SeqCst);
                e.sat.store(s.u() as i64, SeqCst);
                let mut c0 = s.u();
                if v == 2 {
                    c0 &= !HOT_ON_MASK; // generation bits there; an onset of 0 says nothing
                }
                e.c0.store(c0, SeqCst);
                let mut j = 0;
                while j < e.c.len() {
                    let x = s.u();
                    e.c[j].store(x as u32, SeqCst);
                    e.c[j + 1].store((x >> 32) as u32, SeqCst);
                    j += 2;
                }
            }
            let n = s.u();
            if n > t.e.len() as u64 {
                s.fail("free list");
                return;
            }
            let mut g = t.guard();
            g.free = (0..n).map(|_| s.u() as u32).collect();
            (g.next, g.hand) = (s.u(), s.u());
            t.full.store(s.u() as i64, SeqCst);
            t.in_use.store(s.u() as i64, SeqCst);
            t.demoted.store(s.u(), SeqCst);
        }
        s.end();
        if s.u() == 1 {
            let g = f.ghost_table();
            for w in g.words.iter() {
                st(w, s.u());
            }
            g.epoch.store(s.u(), SeqCst);
            g.until.store(s.u(), SeqCst);
            g.sat.store(s.u() == 1, SeqCst);
            g.floored.store(s.u(), SeqCst);
        }
        s.end();
        if v >= 3 && s.u() == 1 {
            let t = f.buzz_table();
            for (i, b) in t.b.iter().enumerate() {
                for j in 0..BUZZ_WAYS {
                    st(&b.ids[j], s.u());
                    let mut e = BuzzEntry { w0: s.u(), w1: s.u(), meta: s.u(), ev: [0; 4] };
                    for k in 0..4 {
                        e.ev[k] = s.u();
                    }
                    b.put(j, &e);
                }
                t.mark(i as u64);
            }
        }
        if v >= 3 {
            s.end();
        }
        if s.err.is_none() {
            let mut b = [0u8; 1];
            loop {
                match s.r.read(&mut b) {
                    Ok(0) => break,
                    Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
                    _ => {
                        s.fail("trailing bytes");
                        break;
                    }
                }
            }
        }
        if s.err.is_none() {
            if let Some(why) = f.snap_sane() {
                s.fail(why);
            }
        }
        if s.err.is_none() && f.own_clock {
            let target = f.continued(old_now, wall);
            if target > MAX_TICK {
                // crossed while the sections were read
                s.fail("the continued clock is past MaxTick");
            } else {
                let cur = f.clock();
                if target > cur {
                    f.clock_add.fetch_add(target - cur, SeqCst);
                    if matches!(f.clock, Clock::Coarse) {
                        // CoarseClock (closed or not): the cached tick moves on too
                        f.coarse.fetch_add(target - cur, SeqCst);
                    }
                }
            }
        }
    }

    /// snapSane checks, once every section is read, the restored values that
    /// a later call uses as an index or waits on, and says what is wrong
    /// (None if nothing): a slot's or a stash entry's word1 pointing past the
    /// hot entries, a free hot entry or the next one out of range, a hot
    /// entry held (an odd version: readers would wait it out forever), a wait
    /// bit past the stash's last set. A quiescent filter writes none of
    /// these. Check words catch damage, not a crafted stream (a snapshot is
    /// trusted input, docs/paper/06-security.md).
    pub(crate) fn snap_sane(&self) -> Option<&'static str> {
        let t = self.hot.get();
        let hn = t.map_or(0, |t| t.e.len() as u64);
        let points = |w1: u64| is_hot(w1) && (hot_idx(w1) == 0 || hot_idx(w1) >= hn);
        let mut i = 1;
        while i < self.words.len() {
            if points(ld(&self.words[i])) {
                return Some("a slot points past the hot entries");
            }
            i += 2;
        }
        if let Some(st) = self.stash.get() {
            for i in 0..st.e.len() {
                if points(st.get(i).w1) {
                    return Some("a stash entry points past the hot entries");
                }
            }
            let tail = st.n % 64;
            if tail != 0 && !st.wait.is_empty() && (ld(&st.wait[st.wait.len() - 1]) >> tail) != 0 {
                return Some("a wait bit past the stash's last set");
            }
        }
        if let Some(t) = t {
            let g = t.guard();
            for &x in &g.free {
                if x == 0 || x as u64 >= hn {
                    return Some("a free hot entry out of range");
                }
            }
            if g.next == 0 || g.next > hn {
                return Some("the next hot entry out of range");
            }
            for e in t.e.iter() {
                if (e.ver.load(SeqCst) & 1) != 0 {
                    return Some("a hot entry held");
                }
            }
        }
        None
    }

    /// snapScrub empties what a refused restore stored: the table's words,
    /// the stash and the hot entries, so a filter whose ReadFrom failed,
    /// unspecified, holds nothing that points past a table.
    pub(crate) fn snap_scrub(&self) {
        for w in self.words.iter() {
            st(w, 0);
        }
        if let Some(p) = self.stash.get() {
            for i in 0..p.e.len() {
                p.put(i, StashEntry::default());
            }
            for w in p.wait.iter() {
                st(w, 0);
            }
            p.at.store(0, SeqCst);
        }
        if let Some(t) = self.hot.get() {
            for e in t.e.iter() {
                e.ver.store(0, SeqCst);
                e.id.store(0, SeqCst);
                e.epoch.store(0, SeqCst);
                e.sat.store(0, SeqCst);
                e.c0.store(0, SeqCst);
                for x in &e.c {
                    x.store(0, SeqCst);
                }
            }
            let mut g = t.guard();
            g.free.clear();
            g.next = 1;
            g.hand = 0;
            t.full.store(crate::hot::SAT_NONE, SeqCst); // as a new table: no unit found it full
            t.in_use.store(0, SeqCst);
        }
    }

    /// fromV2 is a version 2 snapshot's slot pair (w0, w1) as version 3
    /// holds it. A register there kept 4 bits of its unit, read against its
    /// last touch's (as regEpoch did): it is moved to that unit, where a
    /// quiescent filter's registers are already, and its onset is 0, which
    /// says nothing, so it answers as it did. One ahead of its last touch
    /// (never at rest) keeps its unit, its last touch moved to that unit's
    /// start.
    #[allow(clippy::wrong_self_convention)] // the reference's fromV2, a method of the filter
    pub(crate) fn from_v2(&self, mut w0: u64, w1: u64) -> (u64, u64) {
        if w0 == 0 || w1 == 0 || (w1 >> TAG_SHIFT) == TAG_BUSY || is_hot(w1) {
            return (w0, w1);
        }
        let u = self.unit_of(w0 & TS_MASK);
        let mut d = u.wrapping_sub((w1 >> ONSET_SHIFT) as i64) & ONSET_MASK as i64;
        if d > (ONSET_MASK / 2) as i64 {
            d -= (ONSET_MASK + 1) as i64;
        }
        let mut r = unpack(w1 & REG_MASK);
        let e = u - d;
        if e < u {
            if reg_advance(&mut r, e, u) {
                w0 |= SAT_BIT;
            }
        } else if e > u {
            w0 = (w0 & !TS_MASK) | (e as u64).wrapping_mul(self.unit);
        }
        (w0, (w1 & !((ONSET_MASK << ONSET_SHIFT) | REG_MASK)) | pack(&r))
    }
}
