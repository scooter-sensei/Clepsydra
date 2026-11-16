//! filter.go: construction and defaults, the clock, hashing, ContainsAt,
//! counters.
//!
//! The table is an array of 64-byte cache lines. Each line holds four 2-word
//! slots:
//!
//! ```text
//! word0: fp(16) | displaced(1) | saturated(1) | lastTouch(46) // freshness, CheckAndTouch
//! word1: tag(12) | onset(4) | b0..b6 (7 buckets, 48 bits)    // velocity register, at word0's unit
//! ```
//!
//! The velocity register keeps 7 buckets whose widths grow with age (1, 1,
//! 1-2, 2-4, 4-8, 8, 8 units of Horizon/24): fine resolution near now, coarse
//! in the past (register.rs). Advancing time re-buckets arithmetically from
//! the clock, so there is no background sweep on the correctness path. A
//! key's home is its primary line; relocation moves entries to their
//! alternate line to make room.

use std::collections::hash_map::RandomState;
use std::fmt;
use std::hash::{BuildHasher, Hasher};
use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU32, AtomicU64};
use std::sync::{Arc, OnceLock, Weak};
use std::time::{Duration, Instant};

use crate::buzz::BuzzTable;
use crate::ct::{AUTO_START_NS, QuantilePad};
use crate::ghost::Ghost;
use crate::hooks;
use crate::hot::{HOT_IDX_MASK, HotTable};
use crate::overflow::{DEFAULT_SHAPE, Sketch, SketchShape};
use crate::register::UNITS_PER_HORIZON;
use crate::siphash;
use crate::slots::*;
use crate::stash::{STASH_STRIPES, STASH_WAYS, StashTable};
use crate::{Error, Result};

/// `AUTO_PAD` as [`Config::constant_time`] pads each operation to a running
/// estimate of its own work's 99.5th-percentile latency instead of a fixed
/// duration.
pub const AUTO_PAD: Duration = Duration::MAX;

/// `MAX_TICK` is the latest tick a filter can record: a slot keeps its last
/// touch in 46 bits, and a later tick would run into the slot's flag and
/// fingerprint bits (its key would read unseen from then on).
/// [`Filter::new`](crate::Filter::new) rejects a clock already past it, and a
/// first touch (an insert), a touch that realigns its register (one that
/// starts a unit) and `continuous_decay` panic past it. A hit or a count
/// doesn't check, to keep those paths as they are: a clock that passes
/// `MAX_TICK` after its keys were inserted gets wrong answers for them until
/// the next insert panics.
pub const MAX_TICK: u64 = TS_MASK;

pub(crate) const ERR_TICK: &str = "temporalfilter: tick past MaxTick (2^46-1): see Config.Resolution";

/// The most lines a table can have: its bytes (64 a line) fit an allocation.
const MAX_LINES: u64 = isize::MAX as u64 / (WORDS_PER_LINE * 8);

/// checkTick panics if now is past MaxTick.
#[inline(always)]
pub(crate) fn check_tick(now: u64) {
    if now > MAX_TICK {
        tick_panic();
    }
}

#[cold]
#[inline(never)]
fn tick_panic() -> ! {
    panic!("{}", ERR_TICK)
}

/// A caller's clock: returns the current tick ([`Config::clock`]).
pub type ClockFn = Arc<dyn Fn() -> u64 + Send + Sync>;

/// `Config` sizes a [`Filter`](crate::Filter). Fields left at their zero
/// value (`..Default::default()`) take the defaults described on each.
#[derive(Clone, Default)]
pub struct Config {
    /// The expected number of distinct keys live within `horizon`.
    pub capacity: i64,
    /// The largest window any query may use.
    pub horizon: Duration,
    /// The clock tick. Default 1µs. A slot keeps its last touch in 46 bits,
    /// so ticks must stay at most [`MAX_TICK`] (2^46-1): 2.2 years of the
    /// built-in clock at 1µs, 19.5 hours at 1ns. An insert at a later tick
    /// panics.
    pub resolution: Duration,
    /// The target slot occupancy when `capacity` keys are live. Default
    /// 0.85. The scorecard misses no keys at 94% slot load and 0.051% at
    /// 97.8%. Lower values trade memory for headroom above `capacity`.
    pub load: f64,
    /// If set, returns the current tick. It must be monotonic and stay at
    /// most [`MAX_TICK`] (a clock from an epoch, Unix microseconds say, is
    /// past it: `new` rejects a clock that reads past it already). Default:
    /// monotonic time since `new`, in `resolution` units.
    pub clock: Option<ClockFn>,
    /// If > 0 and `clock` is `None`, makes the built-in clock a cached value
    /// refreshed every `coarse_clock` by one background thread: reading it is
    /// an atomic load instead of a monotonic clock read, and timestamps are
    /// accurate to `coarse_clock`. `close` (or dropping the filter) stops it.
    pub coarse_clock: Duration,
    /// If set, makes `hash` and `hash_uint64` SipHash-2-4 under this 128-bit
    /// key: a keyed PRF, the same in every process that uses the key.
    /// Default: a random per-filter seed, also secret but not a PRF. Either
    /// way an attacker who can't learn the key can't pick inputs that land on
    /// a chosen line, identity or lock stripe (docs/paper/06-security.md).
    pub key: Option<[u8; 16]>,
    /// The number of eviction-stash entries (24 B each). A live key evicted
    /// from a full table is kept there, and its freshness and counts come
    /// back if it returns, so flooding the table doesn't reset an offender's
    /// history. When the stash must drop a key it keeps the one with more
    /// events; the other goes to the overflow table. It is allocated on the
    /// first eviction. Default (0): one entry per 16 slots (1.5 B per slot).
    /// Negative: none.
    pub stash: i64,
    /// Sizes the overflow table in units of 16 B. A history the stash has to
    /// drop is kept there as a 21-bit entry (fingerprint, event count, time
    /// slice) in the less loaded of two buckets, so no flood can erase it
    /// within the horizon: `upper` never reads below a key's true count,
    /// except for a key that left more than one history in a full bucket's
    /// floor (docs/paper/06-security.md). A key's answer adds its own entries
    /// (`degraded` then set), so other keys' histories add nothing unless
    /// their fingerprint matches (about 0.3% of keys while the table is
    /// full). When both buckets are full, the lightest history leaves and one
    /// of the bucket's three floors (picked by fingerprint) rises to its
    /// count, which the keys sharing that floor then add to `upper`;
    /// `floored` counts these. The table is allocated on the first drop, so a
    /// filter that never overflows its stash doesn't pay for it. Default (0):
    /// 5 units per 9 slots, 8.9 B per slot once allocated, room for 3.2
    /// histories per slot: a heavy flood of 4x the table's slots raises no
    /// floor. Negative: none; dropped histories are then lost and every
    /// unseen key reads `degraded` for a horizon after a drop. A key that
    /// comes back after its history was lost is held again and reads low
    /// without a flag until the lost events leave the window: `upper` is then
    /// a bound only while `lost` hasn't moved within a horizon.
    pub overflow: i64,
    /// A dropped history with fewer events than this in the horizon is shed
    /// (`shed` counts it) instead of going to the overflow table, so a flood
    /// of cheap keys doesn't fill it, and inserts into a full table stay
    /// cheap. A key can then lose up to `spill_min`-1 events per eviction.
    /// Default (0): 4. 1 or less: keep every history (dedup of one-event keys
    /// then survives floods; inserts into a full table cost ~30% more).
    pub spill_min: i64,
    /// The number of hot entries (64 B each). A key that sends more than its
    /// register holds in one Horizon/24 unit (31 events: 12.4/s at a 60 s
    /// horizon) moves its register to one, with 32-bit counters, so its
    /// counts stay exact (`saturated` is not set) at any realistic rate. The
    /// table is allocated on the first promotion. With every entry in use, a
    /// key saturates as without them, and `hot_refused` counts it. Default
    /// (0): one entry per 128 slots, at least 16. Negative: none.
    pub hot: i64,
    /// If > 0 or [`AUTO_PAD`], hides from response times whether a key was
    /// seen. Every operation does the same lookups whatever the key's state
    /// (both candidate lines, the stash set and the overflow table, under the
    /// key's lock stripe) and returns no sooner than a pad after it started.
    /// A first touch that finds both lines full doesn't search: the key waits
    /// in the stash, where every call looks anyway, and calls place waiting
    /// keys (search, or evict as usual) before their own work, in the part of
    /// the pad their own work's estimate leaves, so how much they do depends
    /// on the stash, not on the caller's key. Touch's pad is `constant_time`;
    /// Count's, and with `AUTO_PAD` Touch's too, is a running estimate of its
    /// own work's 99.5th percentile, capped at `constant_time`. If keys wait
    /// faster than pads place them (more than one per 16 stash entries),
    /// every call places one set past its pad. `waited` and `waiting` report
    /// it. docs/paper/06-security.md has the measurements. Each call costs at
    /// least its pad in CPU.
    pub constant_time: Duration,
    /// If set, keeps cuckoo searches off the call path: a first touch that
    /// finds both its lines full waits in the wait room (24 B per 128 slots,
    /// allocated on the first wait), where its touches and counts find it,
    /// and a thread places it (path search, or eviction as usual). The thread
    /// runs while keys wait and exits soon after; it never reads the clock. A
    /// lookup that misses the table reads one bit of its wait-room set. If
    /// more than one key per 16 wait-room entries waits, the placer isn't
    /// keeping up and first touches search inline again. No effect with
    /// `constant_time`, whose pads place waiting keys.
    pub background: bool,
}

impl fmt::Debug for Config {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Config")
            .field("capacity", &self.capacity)
            .field("horizon", &self.horizon)
            .field("resolution", &self.resolution)
            .field("load", &self.load)
            .field("clock", &self.clock.as_ref().map(|_| "Fn() -> u64"))
            .field("coarse_clock", &self.coarse_clock)
            .field("key", &self.key.map(|_| "[secret]"))
            .field("stash", &self.stash)
            .field("overflow", &self.overflow)
            .field("spill_min", &self.spill_min)
            .field("hot", &self.hot)
            .field("constant_time", &self.constant_time)
            .field("background", &self.background)
            .finish()
    }
}

/// A `time.Duration` in nanoseconds. Go's range ends at 2^63-1 ns (292
/// years): longer durations saturate to it.
#[inline]
pub(crate) fn dur_ns(d: Duration) -> i64 {
    d.as_nanos().min(i64::MAX as u128) as i64
}

/// Nanoseconds since t, as `time.Since`.
#[inline(always)]
pub(crate) fn since_ns(t: Instant) -> i64 {
    t.elapsed().as_nanos().min(i64::MAX as u128) as i64
}

/// The filter's clock (the reference's `clock func() uint64`).
pub(crate) enum Clock {
    /// Monotonic time since `new`, in Resolution units, plus 1.
    Own,
    /// The cached tick a background thread refreshes (CoarseClock).
    Coarse,
    /// Config.Clock.
    User(ClockFn),
}

/// The reference's `Filter` struct, field for field. The public
/// [`Filter`](crate::Filter) owns one behind an `Arc`, which the Background
/// placer's thread shares while it runs. It is safe for concurrent use: hits
/// never lock; first touches of a key serialize on a striped lock.
#[repr(C)]
#[allow(
    unreachable_pub,
    missing_debug_implementations,
    reason = "public only as the white-box tests' Deref target (cfg(test)), which a crate-private type can't be; not exported"
)]
pub struct Core {
    pub(crate) words: AlignedWords, // 64-byte aligned, len = lines*wordsPerLine
    pub(crate) lines: u64,          // any size; indexed by multiply-shift, not masking
    pub(crate) horizon: u64,        // ticks
    pub(crate) unit: u64,           // ticks per velocity register unit (Horizon/unitsPerHorizon)
    pub(crate) unit_inv: u64,       // floor(2^64 / unit) + 1, for unitOf
    pub(crate) res: u64,            // ns per tick
    pub(crate) clock: Clock,
    pub(crate) seed: RandomState,
    // Read by calls, written only by New (hot: by the first promotion): kept
    // apart from the counters below, which calls on other cores write. On a
    // counter's line, ct cost every core's lookups 1.68x while one core added
    // to waited (claims S60).
    pub(crate) ct: bool,                // Config.ConstantTime > 0
    pub(crate) pad: i64,                // Config.ConstantTime, ns (-1: AutoPad)
    pub(crate) hot: OnceLock<HotTable>, // unset until the first promotion
    pub(crate) hot_n: u64,              // hot entries; 0: none
    pub(crate) bg: bool,                // Config.Background

    pub(crate) cursor: Pad<AtomicU64>, // ContinuousDecay progress, in lines
    pub(crate) coarse: Pad<AtomicU64>, // cached tick for CoarseClock
    pub(crate) stop: AtomicBool,
    pub(crate) evictions: AtomicU64,
    pub(crate) relocations: AtomicU64,
    pub(crate) pressure: Pad<AtomicU32>,                        // EWMA of relocation-search failure, 0..pressureOne
    pub(crate) claim_mu: [PaddedMutex; 1 << CLAIM_STRIPE_BITS], // serializes first touches per key
    // moves counts, per stripe of word1 tags (moveOf), moves of a key's entry
    // under way: a relocation from before its hidden copy goes in to the
    // copy's publication or clearing, an eviction from taking the slot to
    // stashing the history, and every other hold of an occupied slot. A
    // lookup's miss is trusted only if none overlapped it (lookupStable), and
    // an evictor leaves a key with one under way alone (claim).
    pub(crate) moves: [MoveCount; 1 << CLAIM_STRIPE_BITS],

    pub(crate) sip: siphash::Key,
    pub(crate) keyed: bool,                 // Config.Key set: hash with sip
    pub(crate) stash: OnceLock<StashTable>, // unset until the first eviction
    pub(crate) stash_n: u64,                // sets of stashWays entries
    // stashUntil is a tick before which the stash may hold a live entry; past
    // it, misses skip the stash.
    pub(crate) stash_until: Pad<AtomicU64>,
    pub(crate) lost_at: AtomicU64, // tick of the latest dropped history, 0 if none
    pub(crate) lost: AtomicU64,
    pub(crate) restored: AtomicU64,
    pub(crate) folded: AtomicU64,
    pub(crate) stash_mu: Arc<[PaddedMutex; STASH_STRIPES]>,
    pub(crate) sk: OnceLock<Sketch>,    // count-min overflow (cms), unset until the first spill
    pub(crate) gh: OnceLock<Ghost>,     // ghost overflow, unset until the first spill
    pub(crate) cms: bool,               // overflow to the count-min sketch, not the ghost table (experiments)
    pub(crate) gh_choices: i64,         // candidate buckets per dropped history in the ghost table (2; experiments: 1)
    pub(crate) gh_sub: i64,             // floors per ghost bucket, picked by fingerprint (3; experiments: 1)
    pub(crate) auto: bool,              // ConstantTime == AutoPad
    pub(crate) pad_q: [QuantilePad; 2], // AutoPad: Touch's and Count's
    pub(crate) sk_w: u64,               // overflow size in 16 B units; 0: no overflow
    pub(crate) sk_shape: SketchShape,
    pub(crate) spill_min: u64, // histories lighter than this aren't spilled (Shed)
    pub(crate) shed: AtomicU64,
    pub(crate) deferred: AtomicI64, // keys waiting for a slot (deferMark): in the stash (ConstantTime) or the wait room (Background)
    pub(crate) waited: AtomicU64,   // inserts that waited for a slot
    pub(crate) place_ns: AtomicI64, // ConstantTime: running mean of a placement step, ns
    pub(crate) promoted: AtomicU64,
    pub(crate) hot_refused: AtomicU64,
    pub(crate) placing: AtomicBool,             // Background: the placer thread is running
    pub(crate) bg_manual: bool,                 // tests: Background without the thread (the test places)
    pub(crate) room: Pad<OnceLock<StashTable>>, // Background: keys waiting for a slot, unset until the first waits
    pub(crate) room_n: u64,                     // sets of the wait room
    pub(crate) own_clock: bool,                 // the built-in clock: ReadFrom re-anchors it (claims D27)
    pub(crate) clock_add: AtomicU64,            // ticks the built-in clock is moved on by
    pub(crate) room_mu: Arc<[PaddedMutex; STASH_STRIPES]>,
    pub(crate) buzz: OnceLock<BuzzTable>, // conditional touches' exact logs (buzz.rs), unset until the first uncertain refusal

    pub(crate) base: Instant,  // the built-in clock's start
    pub(crate) coarse_ns: i64, // Config.CoarseClock, ns
    pub(crate) me: Weak<Core>, // for the threads this filter starts (the reference's goroutines hold f)
}

impl Core {
    /// New allocates a filter sized for cfg.Load slot occupancy at
    /// cfg.Capacity. `tweak` edits it before it is shared (tests set the
    /// reference's unexported fields there).
    pub(crate) fn new(mut cfg: Config, tweak: impl FnOnce(&mut Core)) -> std::result::Result<Arc<Core>, Error> {
        #[cfg(test)]
        crate::tests::check::init(); // the suite's TF_CT and TF_BG
        let horizon_ns = dur_ns(cfg.horizon);
        if cfg.capacity <= 0 || horizon_ns <= 0 {
            return Err(Error::new("temporalfilter: Capacity and Horizon must be positive"));
        }
        let mut res_ns = dur_ns(cfg.resolution);
        if res_ns <= 0 {
            res_ns = 1000;
        }
        let horizon = (horizon_ns / res_ns) as u64;
        if horizon < 2 * UNITS_PER_HORIZON {
            // unit >= 2 ticks
            return Err(Error::new("temporalfilter: Horizon too small for Resolution"));
        }
        if horizon >= MAX_TICK {
            // no tick could age a key past it
            return Err(Error::new("temporalfilter: Horizon too large for Resolution (MaxTick ticks or more)"));
        }
        if cfg.load <= 0.0 {
            cfg.load = 0.85;
        }
        if cfg.load.is_nan() || cfg.load > 1.0 {
            // NaN too: it passes both of the reference's checks
            return Err(Error::new("temporalfilter: Load must be <= 1"));
        }
        if let Some(c) = &cfg.clock {
            if c() > MAX_TICK {
                return Err(Error::new(ERR_TICK));
            }
        }
        // A table of 2^57 lines or more has more bytes than an allocation can
        // (isize::MAX): the reference wraps its word count there and fails at
        // its make or its first index, and a build without overflow checks
        // would wrap it too, and build the filter on a short table. It is
        // refused before any size is computed from it.
        let want = (cfg.capacity as f64 / (cfg.load * SLOTS_PER_LINE as f64)).ceil();
        if want >= MAX_LINES as f64 {
            return Err(Error::new("temporalfilter: Capacity too large for Load"));
        }
        let lines = 2u64.max(want as u64);
        let words = AlignedWords::try_new((lines * WORDS_PER_LINE) as usize)
            .ok_or_else(|| Error::new("temporalfilter: no memory for the table (Capacity too large for Load)"))?;
        let (sip, keyed) = match &cfg.key {
            Some(k) => (siphash::Key::new(k), true),
            None => (siphash::Key::default(), false),
        };
        if cfg.stash == 0 {
            cfg.stash = 1.max(lines * SLOTS_PER_LINE / 16) as i64;
        }
        let mut stash_n = 0;
        if cfg.stash > 0 {
            stash_n = (cfg.stash as u64 + STASH_WAYS - 1) / STASH_WAYS;
        }
        let room_n = 16.max(lines * SLOTS_PER_LINE / (128 * STASH_WAYS)); // Background: one entry per 128 slots
        if cfg.overflow == 0 {
            cfg.overflow = 64.max(lines * SLOTS_PER_LINE * 5 / 9) as i64;
        }
        let (mut sk_w, mut sk_shape, mut gh_choices, mut gh_sub) = (0, SketchShape::default(), 0, 0);
        if cfg.overflow > 0 {
            sk_w = cfg.overflow as u64;
            (sk_shape, gh_choices, gh_sub) = (DEFAULT_SHAPE, 2, 3);
        }
        if cfg.spill_min == 0 {
            cfg.spill_min = 4;
        }
        let spill_min = cfg.spill_min.max(1) as u64;
        if cfg.hot == 0 {
            cfg.hot = 16.max(lines * SLOTS_PER_LINE / 128) as i64;
        }
        let hot_n = (cfg.hot.max(0) as u64).min(HOT_IDX_MASK);
        let auto = cfg.constant_time == AUTO_PAD;
        let pad = if auto { -1 } else { dur_ns(cfg.constant_time) };
        let ct = pad > 0 || auto || hooks::FORCE_CT.load(SeqCst);
        let bg = (cfg.background || hooks::FORCE_BG.load(SeqCst)) && !ct;
        let unit = (horizon + UNITS_PER_HORIZON - 1) / UNITS_PER_HORIZON;
        let coarse_ns = dur_ns(cfg.coarse_clock);
        let (clock, own_clock) = match cfg.clock.take() {
            Some(c) => (Clock::User(c), false),
            None if coarse_ns > 0 => (Clock::Coarse, true),
            None => (Clock::Own, true),
        };
        let mut f = Core {
            words,
            lines,
            horizon,
            unit,
            unit_inv: (u64::MAX / unit).wrapping_add(1),
            res: res_ns as u64,
            clock,
            seed: RandomState::new(),
            ct,
            pad,
            hot: OnceLock::new(),
            hot_n,
            bg,
            cursor: Pad::default(),
            coarse: Pad::default(),
            stop: AtomicBool::new(false),
            evictions: AtomicU64::new(0),
            relocations: AtomicU64::new(0),
            pressure: Pad::default(),
            claim_mu: std::array::from_fn(|_| PaddedMutex::new()),
            moves: std::array::from_fn(|_| MoveCount::default()),
            sip,
            keyed,
            stash: OnceLock::new(),
            stash_n,
            stash_until: Pad::default(),
            lost_at: AtomicU64::new(0),
            lost: AtomicU64::new(0),
            restored: AtomicU64::new(0),
            folded: AtomicU64::new(0),
            stash_mu: Arc::new(std::array::from_fn(|_| PaddedMutex::new())),
            sk: OnceLock::new(),
            gh: OnceLock::new(),
            cms: false,
            gh_choices,
            gh_sub,
            auto,
            pad_q: std::array::from_fn(|_| QuantilePad::default()),
            sk_w,
            sk_shape,
            spill_min,
            shed: AtomicU64::new(0),
            deferred: AtomicI64::new(0),
            waited: AtomicU64::new(0),
            place_ns: AtomicI64::new(0),
            promoted: AtomicU64::new(0),
            hot_refused: AtomicU64::new(0),
            placing: AtomicBool::new(false),
            bg_manual: false,
            room: Pad(OnceLock::new()),
            room_n,
            own_clock,
            clock_add: AtomicU64::new(0),
            room_mu: Arc::new(std::array::from_fn(|_| PaddedMutex::new())),
            buzz: OnceLock::new(),
            base: Instant::now(),
            coarse_ns,
            me: Weak::new(),
        };
        for q in &f.pad_q {
            q.q.store(AUTO_START_NS * 1000, SeqCst);
        }
        tweak(&mut f);
        let f = Arc::new_cyclic(|me| {
            f.me = me.clone();
            f
        });
        if matches!(f.clock, Clock::Coarse) {
            f.coarse.store(f.read_own(), SeqCst);
            let me = Arc::downgrade(&f);
            // The reference's ticker goroutine: it holds the filter only
            // while it refreshes the tick, and ends when the filter is closed
            // or gone. thread::sleep waits on a high-resolution timer where
            // the platform has one (the reference's runtime does too).
            let period = Duration::from_nanos(f.coarse_ns.max(10_000) as u64);
            std::thread::Builder::new()
                .name("clepsydra-coarse-clock".into())
                .spawn(move || {
                    loop {
                        std::thread::sleep(period);
                        let Some(f) = me.upgrade() else { return };
                        if f.stop.load(SeqCst) {
                            return;
                        }
                        f.coarse.store(f.read_own(), SeqCst);
                    }
                })
                .map_err(|e| Error::new(format!("temporalfilter: the CoarseClock thread did not start: {e}")))?;
        }
        Ok(f)
    }

    /// The built-in clock: ticks since the filter was built, plus 1, plus
    /// what a restore moved it on by.
    #[inline]
    pub(crate) fn read_own(&self) -> u64 {
        (self.base.elapsed().as_nanos() / u128::from(self.res)) as u64 + 1 + self.clock_add.load(SeqCst)
    }

    /// `f.clock()`.
    #[inline]
    pub(crate) fn clock(&self) -> u64 {
        match &self.clock {
            Clock::Own => self.read_own(),
            Clock::Coarse => self.coarse.load(SeqCst),
            Clock::User(c) => c(),
        }
    }

    /// Close stops the CoarseClock thread, if any. The filter stays usable;
    /// its clock just stops advancing.
    pub(crate) fn close(&self) {
        self.stop.store(true, SeqCst);
    }

    /// Ticks converts a duration to filter ticks.
    #[inline]
    pub(crate) fn ticks(&self, d: Duration) -> u64 {
        (dur_ns(d) as u64) / self.res
    }

    /// Hash hashes a string key for the *At methods, under the filter's
    /// secret key (Config.Key).
    #[inline]
    pub(crate) fn hash(&self, key: &str) -> u64 {
        if self.keyed {
            return self.sip.hash_string(key.as_bytes());
        }
        let mut h = self.seed.build_hasher();
        h.write(key.as_bytes());
        h.finish()
    }

    /// HashUint64 hashes an integer key (IP, card BIN, user id) for the *At
    /// methods, under the filter's secret key.
    #[inline]
    pub(crate) fn hash_uint64(&self, k: u64) -> u64 {
        if self.keyed {
            return self.sip.hash_uint64(k);
        }
        let mut h = self.seed.build_hasher();
        h.write_u64(k);
        h.finish()
    }

    /// HashUint64s sets out[i] = f.HashUint64(keys[i]) for every key (out
    /// must be at least len(keys)).
    pub(crate) fn hash_uint64s(&self, keys: &[u64], out: &mut [u64]) {
        if self.keyed {
            self.sip.hash_uint64s(keys, out);
            return;
        }
        let out = &mut out[..keys.len()];
        for (o, &k) in out.iter_mut().zip(keys) {
            *o = self.hash_uint64(k);
        }
    }

    /// TouchAt is Touch on a pre-hashed key, in ticks.
    #[inline]
    pub(crate) fn touch_at(&self, h: u64, window: u64, now: u64) -> Result {
        self.touch(h, window, now, true, -1)
    }

    /// CountAt is Count on a pre-hashed key, in ticks.
    #[inline]
    pub(crate) fn count_at(&self, h: u64, window: u64, now: u64) -> Result {
        self.count(h, window, now, -1)
    }

    /// ContainsAt reports whether h was touched within window, without
    /// touching it.
    pub(crate) fn contains_at(&self, h: u64, window: u64, now: u64) -> bool {
        if self.ct {
            return self.count_at(h, window, now).seen;
        }
        let (w0, _, src) = self.read(h, now, -1);
        if src != 0 && age(now, w0 & TS_MASK) <= window.min(self.horizon) {
            return true;
        }
        if !self.sketch_live(now) {
            return false;
        }
        let mut r = Result::default();
        self.add_sketch(&mut r, h, window, now);
        r.seen
    }

    /// Occupancy returns the number of slots holding a key live at now.
    /// O(table).
    pub(crate) fn occupancy(&self, now: u64) -> i64 {
        let mut n = 0;
        let mut i = 0;
        while i < self.words.len() {
            let w0 = ld(&self.words[i]);
            if w0 != 0 && age(now, w0 & TS_MASK) <= self.horizon {
                n += 1;
            }
            i += 2;
        }
        n
    }

    /// Slots returns the table's slot count.
    pub(crate) fn slots(&self) -> i64 {
        (self.words.len() / 2) as i64
    }

    /// Bytes is the memory the filter holds now: its table, and the stash,
    /// overflow table, hot entries and buzz entries (with their blocks'
    /// bitmap) once allocated (each on first use).
    pub(crate) fn bytes(&self) -> i64 {
        let mut n = 8 * self.words.len();
        for s in [self.stash.get(), self.room.get()].into_iter().flatten() {
            n += s.e.len() * size_of::<crate::stash::StashCell>() + 8 * s.wait.len();
        }
        if let Some(g) = self.gh.get() {
            n += 8 * g.words.len();
        }
        if let Some(sk) = self.sk.get() {
            n += 4 * sk.cells.len();
        }
        if let Some(t) = self.hot.get() {
            n += t.e.len() * size_of::<crate::hot::HotEntry>();
        }
        if let Some(t) = self.buzz.get() {
            n += t.b.len() * size_of::<crate::buzz::BuzzBlock>() + 8 * t.used.len(); // the blocks and their bitmap
        }
        n as i64
    }
}
