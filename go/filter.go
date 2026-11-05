// Package temporalfilter implements a Sliding-Window Probabilistic Register:
// a fixed-size filter that answers "have we seen key X in the last W?" and
// "how many times in the last W?" for any W up to a configured horizon. Hits
// are lock-free (CAS on the slot's words); a key's first touch takes one of
// 256 striped locks so racing inserts can't give it two slots.
//
// Memory layout: the table is an array of 64-byte cache lines. Each line holds
// four 2-word slots:
//
//	word0: fp(16) | displaced(1) | saturated(1) | lastTouch(46) // freshness, CheckAndTouch
//	word1: tag(12) | epoch(4) | b0..b6 (7 buckets, 48 bits)    // velocity register
//
// The velocity register keeps 7 buckets whose widths grow with age (1, 1,
// 1-2, 2-4, 4-8, 8, 8 units of Horizon/24): fine resolution near now, coarse
// in the past (register.go). Advancing time re-buckets arithmetically from
// the clock, so there is no background sweep on the correctness path. A key's
// home is its primary line; relocation moves entries to their alternate line
// to make room.
package temporalfilter

import (
	"errors"
	"hash/maphash"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/scooter-sensei/clepsydra/go/internal/siphash"
)

// Config sizes a Filter.
type Config struct {
	// Capacity is the expected number of distinct keys live within Horizon.
	Capacity int
	// Horizon is the largest window any query may use.
	Horizon time.Duration
	// Resolution is the clock tick. Default 1µs. A slot keeps its last touch
	// in 46 bits, so ticks must stay at most MaxTick (2^46-1): 2.2 years of
	// the built-in clock at 1µs, 19.5 hours at 1ns. An insert at a later
	// tick panics (MaxTick).
	Resolution time.Duration
	// Load is the target slot occupancy when Capacity keys are live.
	// Default 0.85. The scorecard (cmd/tfbench) misses no keys at 94% slot
	// load and 0.051% at 97.8%. Lower values trade memory for headroom above
	// Capacity.
	Load float64
	// Clock, if set, returns the current tick. It must be monotonic and
	// stay at most MaxTick (a clock from an epoch, Unix microseconds say, is
	// past it: New rejects a Clock that reads past it already).
	// Default: monotonic time since New, in Resolution units.
	Clock func() uint64
	// CoarseClock, if > 0 and Clock is nil, makes the built-in clock a cached
	// value refreshed every CoarseClock by one background goroutine: reading
	// it is an atomic load instead of a monotonic clock read (BenchmarkClock*:
	// ~1.4 vs 30-42 ns on the reference host, docs/artifact/speed-runs.md),
	// and timestamps are accurate to CoarseClock. Call Close to stop it.
	CoarseClock time.Duration
	// Key, if set, makes Hash and HashUint64 SipHash-2-4 under this 128-bit
	// key: a keyed PRF, the same in every process that uses the key. Default:
	// a random per-filter maphash seed, also secret but not a PRF. Either
	// way an attacker who can't learn the key can't pick inputs that land on
	// a chosen line, identity or lock stripe (docs/paper/06-security.md).
	Key *[16]byte
	// Stash is the number of eviction-stash entries (24 B each). A live key
	// evicted from a full table is kept there, and its freshness and counts
	// come back if it returns, so flooding the table doesn't reset an
	// offender's history. When the stash must drop a key it keeps the one
	// with more events; the other goes to the overflow table. It is
	// allocated on the first eviction. Default (0): one entry per 16 slots
	// (1.5 B per slot). Negative: none.
	Stash int
	// Overflow sizes the overflow table in units of 16 B. A history the stash
	// has to drop is kept there as a 21-bit entry (fingerprint, event count,
	// time slice) in the less loaded of two buckets, so no flood can erase
	// it within the horizon: Upper never reads below a key's true count,
	// except for a key that left more than one history in a full bucket's
	// floor (docs/paper/06-security.md). A key's answer adds its own entries (Degraded
	// then set), so other keys' histories add nothing unless their
	// fingerprint matches (about 0.3% of keys while the table is full). When
	// both buckets are full, the lightest history leaves and one of the
	// bucket's three floors (picked by fingerprint) rises to its count, which
	// the keys sharing that floor then add to Upper; Floored counts these.
	// The table is allocated on the first drop, so a filter that never
	// overflows its stash doesn't pay for it. Default (0): 5 units per 9
	// slots, 8.9 B per slot once allocated, room for 3.2 histories per slot:
	// a heavy flood of 4x the table's slots raises no floor. Negative: none;
	// dropped histories are then lost and every unseen key reads Degraded
	// for a horizon after a drop. A key that comes back after its history
	// was lost is held again and reads low without a flag until the lost
	// events leave the window: Upper is then a bound only while Lost hasn't
	// moved within a horizon.
	Overflow int
	// SpillMin: a dropped history with fewer events than this in the horizon
	// is shed (Shed counts it) instead of going to the overflow table, so a
	// flood of cheap keys doesn't fill it, and inserts into a full table
	// stay cheap. A key can then lose up to SpillMin-1 events per eviction.
	// Default (0): 4. 1 or less: keep every history (dedup of one-event keys
	// then survives floods; inserts into a full table cost ~30% more).
	SpillMin int
	// Hot is the number of hot entries (64 B each). A key that sends more
	// than its register holds in one Horizon/24 unit (31 events: 12.4/s at a
	// 60 s horizon) moves its register to one, with 32-bit counters, so its
	// counts stay exact (Saturated is not set) at any realistic rate. The
	// table is allocated on the first promotion. With every entry in use, a
	// key saturates as without them, and HotRefused counts it. Default (0):
	// one entry per 128 slots, at least 16. Negative: none.
	Hot int
	// ConstantTime, if > 0 or AutoPad, hides from response times whether a
	// key was seen. Every operation does the same lookups whatever the key's
	// state (both candidate lines, the stash set and the overflow table,
	// under the key's lock stripe) and returns no sooner than a pad after it
	// started. A first touch that finds both lines full doesn't search: the
	// key waits in the stash, where every call looks anyway, and calls place
	// waiting keys (search, or evict as usual) before their own work, in the
	// part of the pad their own work's estimate leaves, so how much they do
	// depends on the stash, not on the caller's key. Touch's pad is
	// ConstantTime; Count's, and with AutoPad Touch's too, is a running
	// estimate of its own work's 99.5th percentile, capped at ConstantTime.
	// If keys wait faster than pads place them (more than one per 16 stash
	// entries), every call places one set past its pad. Waited and Waiting
	// report it. docs/paper/06-security.md has the measurements. Each call costs at
	// least its pad in CPU.
	ConstantTime time.Duration
	// Background, if set, keeps cuckoo searches off the call path: a first
	// touch that finds both its lines full waits in the wait room (24 B per
	// 128 slots, allocated on the first wait), where its touches and counts
	// find it, and a goroutine places it (path search, or eviction as
	// usual). The goroutine runs while keys wait and exits soon after; it
	// never reads Clock. A lookup that misses the table reads one bit of its
	// wait-room set. If more than one key per 16 wait-room entries waits, the
	// placer isn't keeping up and first touches search inline again. No
	// effect with ConstantTime, whose pads place waiting keys.
	Background bool
}

// Filter is safe for concurrent use. Hits never lock; first touches of a key
// serialize on a striped lock.
type Filter struct {
	words   []uint64 // 64-byte aligned, len = lines*wordsPerLine
	lines   uint64   // any size; indexed by multiply-shift, not masking
	horizon uint64   // ticks
	unit    uint64   // ticks per velocity register unit (Horizon/unitsPerHorizon)
	unitInv uint64   // floor(2^64 / unit) + 1, for unitOf
	res     time.Duration
	clock   func() uint64
	seed    maphash.Seed
	// Read by calls, written only by New (hot: by the first promotion):
	// kept apart from the counters below, which calls on other cores write.
	// On a counter's line, ct cost every core's lookups 1.68x while one
	// core added to waited (claims S60).
	ct   bool                     // Config.ConstantTime > 0
	pad  time.Duration            // Config.ConstantTime
	hot  atomic.Pointer[hotTable] // nil until the first promotion
	hotN int                      // hot entries; 0: none
	bg   bool                     // Config.Background

	_           [64]byte
	cursor      atomic.Uint64 // ContinuousDecay progress, in lines
	_           [56]byte
	coarse      atomic.Uint64 // cached tick for CoarseClock
	stop        chan struct{}
	stopOnce    sync.Once
	evictions   atomic.Uint64
	relocations atomic.Uint64
	_           [48]byte
	pressure    atomic.Uint32 // EWMA of relocation-search failure, 0..pressureOne
	_           [60]byte
	claimMu     [1 << claimStripeBits]paddedMutex // serializes first touches per key
	// moves counts, per stripe of word1 tags (moveOf), moves of a key's
	// entry under way: a relocation from before its hidden copy goes in to
	// the copy's publication or clearing, an eviction from taking the slot
	// to stashing the history, and every other hold of an occupied slot. A
	// lookup's miss is trusted only if none overlapped it (lookupStable),
	// and an evictor leaves a key with one under way alone (claim).
	moves [1 << claimStripeBits]moveCount

	sip    siphash.Key
	keyed  bool                       // Config.Key set: hash with sip
	stash  atomic.Pointer[stashTable] // nil until the first eviction
	stashN uint64                     // sets of stashWays entries
	_      [64]byte
	// stashUntil is a tick before which the stash may hold a live entry;
	// past it, misses skip the stash.
	stashUntil atomic.Uint64
	lostAt     atomic.Uint64 // tick of the latest dropped history, 0 if none
	lost       atomic.Uint64
	restored   atomic.Uint64
	folded     atomic.Uint64
	_          [24]byte
	stashMu    [stashStripes]paddedMutex
	sk         atomic.Pointer[sketch] // count-min overflow (cms), nil until the first spill
	gh         atomic.Pointer[ghost]  // ghost overflow, nil until the first spill
	cms        bool                   // overflow to the count-min sketch, not the ghost table (experiments)
	ghChoices  int                    // candidate buckets per dropped history in the ghost table (2; experiments: 1)
	ghSub      int                    // floors per ghost bucket, picked by fingerprint (3; experiments: 1)
	shadow     bool                   // ConstantTime: each path also runs the other's work, read-only
	auto       bool                   // ConstantTime == AutoPad
	padQ       [2]quantilePad         // AutoPad: Touch's and Count's
	skMu       sync.Mutex
	skW        uint64 // overflow size in 16 B units; 0: no overflow
	skShape    sketchShape
	spillMin   uint64 // histories lighter than this aren't spilled (Shed)
	shed       atomic.Uint64
	deferred   atomic.Int64  // keys waiting for a slot (deferMark): in the stash (ConstantTime) or the wait room (Background)
	waited     atomic.Uint64 // inserts that waited for a slot
	placeNs    atomic.Int64  // ConstantTime: running mean of a placement step, ns
	promoted   atomic.Uint64
	hotRefused atomic.Uint64
	placing    atomic.Bool // Background: the placer goroutine is running
	bgManual   bool        // tests: Background without the goroutine (the test places)
	_          [64]byte
	room       atomic.Pointer[stashTable] // Background: keys waiting for a slot, nil until the first waits
	roomN      uint64                     // sets of the wait room
	ownClock   bool                       // the built-in clock: ReadFrom re-anchors it (claims D27)
	clockAdd   atomic.Uint64              // ticks the built-in clock is moved on by
	roomMu     [stashStripes]paddedMutex
}

// Test hooks, if set: testHookCopied runs between a relocation's copy and
// its hand-off, testHookVictim between an eviction's choice of victim and
// its install, testHookTaken after a touch takes a waiting key out of the
// wait room, testHookStamped after a touch moves its slot's last touch to a
// later unit, before the register follows (realign, word1 busy);
// testHookPicked after a relocation reads the entry at word index i, before
// it copies it; testHookPlacing after the Background placer marks a waiting
// key's wait-room entry busy, before it installs the key; testHookSweeping
// after the decay sweep's first write to an expired slot, before its last;
// testHookInstalling after an install takes slot i's word1 busy, before
// its word0; testHookBumping after bump checks slot i's words, before its
// count's CAS; testHookBumped after that CAS succeeds, before bump reads
// word0 again; testHookOpenSeen after an insert reads slot i's word0 free
// or expired, before its word1; testHookSweepSeen after the decay sweep
// reads slot i's word0, before its word1; testHookSweepRead after it reads
// word1, before its hold; testHookHotAdding after bump checks slot i's
// words and finds a hot pointer, before it adds to the entry.
var testHookCopied, testHookVictim, testHookTaken, testHookStamped, testHookPlacing, testHookSweeping func()
var testHookPicked, testHookInstalling, testHookBumping, testHookBumped, testHookOpenSeen, testHookSweepSeen, testHookSweepRead, testHookHotAdding func(i int)

// forceCT turns ConstantTime on in every filter (tests: TF_CT=1).
var forceCT bool

// forceBG turns Background on in every filter (tests: TF_BG=1).
var forceBG bool

// New allocates a Filter sized for cfg.Load slot occupancy at cfg.Capacity.
func New(cfg Config) (*Filter, error) {
	if cfg.Capacity <= 0 || cfg.Horizon <= 0 {
		return nil, errors.New("temporalfilter: Capacity and Horizon must be positive")
	}
	if cfg.Resolution <= 0 {
		cfg.Resolution = time.Microsecond
	}
	horizon := uint64(cfg.Horizon / cfg.Resolution)
	if horizon < 2*unitsPerHorizon { // unit >= 2 ticks
		return nil, errors.New("temporalfilter: Horizon too small for Resolution")
	}
	if horizon >= MaxTick { // no tick could age a key past it
		return nil, errors.New("temporalfilter: Horizon too large for Resolution (MaxTick ticks or more)")
	}
	if cfg.Load <= 0 {
		cfg.Load = 0.85
	}
	if cfg.Load > 1 {
		return nil, errors.New("temporalfilter: Load must be <= 1")
	}
	if cfg.Clock != nil && cfg.Clock() > MaxTick {
		return nil, errTick
	}
	lines := max(2, uint64(math.Ceil(float64(cfg.Capacity)/(cfg.Load*slotsPerLine))))
	f := &Filter{
		lines:   lines,
		horizon: horizon,
		unit:    (horizon + unitsPerHorizon - 1) / unitsPerHorizon,
		res:     cfg.Resolution,
		clock:   cfg.Clock,
		seed:    maphash.MakeSeed(),
	}
	if cfg.Key != nil {
		f.sip, f.keyed = siphash.NewKey(cfg.Key), true
	}
	if cfg.Stash == 0 {
		cfg.Stash = int(max(1, lines*slotsPerLine/16))
	}
	if cfg.Stash > 0 {
		f.stashN = uint64(cfg.Stash+stashWays-1) / stashWays
	}
	f.roomN = max(16, lines*slotsPerLine/(128*stashWays)) // Background: one entry per 128 slots
	if cfg.Overflow == 0 {
		cfg.Overflow = int(max(64, lines*slotsPerLine*5/9))
	}
	if cfg.Overflow > 0 {
		f.skW = uint64(cfg.Overflow)
		f.skShape, f.ghChoices, f.ghSub = defaultShape, 2, 3
	}
	if cfg.SpillMin == 0 {
		cfg.SpillMin = 4
	}
	f.spillMin = uint64(max(cfg.SpillMin, 1))
	if cfg.Hot == 0 {
		cfg.Hot = int(max(16, lines*slotsPerLine/128))
	}
	f.hotN = int(min(uint64(max(cfg.Hot, 0)), hotIdxMask)) // through uint64: hotIdxMask overflows a 32-bit int
	f.ct, f.pad = cfg.ConstantTime > 0 || cfg.ConstantTime == AutoPad || forceCT, cfg.ConstantTime
	f.auto = cfg.ConstantTime == AutoPad
	f.bg = (cfg.Background || forceBG) && !f.ct
	for i := range f.padQ {
		f.padQ[i].q.Store(int64(autoStart) * 1000)
	}
	f.unitInv = ^uint64(0)/f.unit + 1
	f.words = alignedWords(int(lines * wordsPerLine))
	if f.clock == nil {
		base, res := time.Now(), cfg.Resolution
		f.ownClock = true
		read := func() uint64 { return uint64(time.Since(base)/res) + 1 + f.clockAdd.Load() }
		f.clock = read
		if cfg.CoarseClock > 0 {
			f.coarse.Store(read())
			f.stop = make(chan struct{})
			go func(t *time.Ticker) {
				defer t.Stop()
				for {
					select {
					case <-t.C:
						f.coarse.Store(read())
					case <-f.stop:
						return
					}
				}
			}(time.NewTicker(max(cfg.CoarseClock, 10*time.Microsecond)))
			f.clock = f.coarse.Load
		}
	}
	return f, nil
}

// prefault writes a word of every page of w, so a table allocated while
// the filter is in use takes its page faults now rather than one by one in
// later calls (a ConstantTime call's timing would show them).
func prefault(w []uint64) {
	for i := 0; i < len(w); i += 4096 / 8 {
		w[i] = 0
	}
}

func alignedWords(n int) []uint64 {
	buf := make([]uint64, n+wordsPerLine)
	const align = wordsPerLine * 8 // bytes per bucket
	off := int((align-uintptr(unsafe.Pointer(&buf[0]))%align)%align) / 8
	return buf[off : off+n : off+n]
}

// Close stops the CoarseClock goroutine, if any. The filter stays usable;
// its clock just stops advancing.
func (f *Filter) Close() {
	if f.stop != nil {
		f.stopOnce.Do(func() { close(f.stop) })
	}
}

// Now returns the filter's current tick.
func (f *Filter) Now() uint64 { return f.clock() }

// MaxTick is the latest tick a filter can record: a slot keeps its last
// touch in 46 bits, and a later tick would run into the slot's flag and
// fingerprint bits (its key would read unseen from then on). New rejects a
// Clock already past it, and a first touch (an insert), a touch that
// realigns its register (8 or more units after the last) and
// ContinuousDecay panic past it. A hit or a count doesn't check, to keep
// those paths as they are: a clock that passes MaxTick after its keys were
// inserted gets wrong answers for them until the next insert panics.
const MaxTick = tsMask

var errTick = errors.New("temporalfilter: tick past MaxTick (2^46-1): see Config.Resolution")

// checkTick panics if now is past MaxTick.
func checkTick(now uint64) {
	if now > MaxTick {
		panic(errTick)
	}
}

// Ticks converts a duration to filter ticks.
func (f *Filter) Ticks(d time.Duration) uint64 { return uint64(max(d, 0) / f.res) }

// Hash hashes a string key for the *At methods, under the filter's secret
// key (Config.Key).
func (f *Filter) Hash(key string) uint64 {
	if f.keyed {
		return f.sip.HashString(key)
	}
	return maphash.String(f.seed, key)
}

// HashUint64 hashes an integer key (IP, card BIN, user id) for the *At
// methods, under the filter's secret key. The *At methods trust the hash they
// are given: pass them only Hash or HashUint64 values of this filter.
func (f *Filter) HashUint64(k uint64) uint64 {
	if f.keyed {
		return f.sip.HashUint64(k)
	}
	return maphash.Comparable(f.seed, k)
}

// HashUint64s sets out[i] = f.HashUint64(keys[i]) for every key (out must
// be at least len(keys)). Under Config.Key it hashes 8 keys at once where
// the CPU has AVX-512 (siphash.Key.HashUint64s).
func (f *Filter) HashUint64s(keys, out []uint64) {
	if f.keyed {
		f.sip.HashUint64s(keys, out)
		return
	}
	out = out[:len(keys)]
	for i, k := range keys {
		out[i] = maphash.Comparable(f.seed, k)
	}
}

// Result is the full answer of Touch and Count, including how far it can be
// trusted. A fail-closed caller (rate limit, abuse check) should treat
// Saturated as "over any limit above Count" and, while Degraded, a key not
// Seen as possibly a returning one. docs/paper/06-security.md has the threat model.
type Result struct {
	// Seen: the key was touched within the window (before this touch).
	Seen bool
	// Count estimates the key's events within the window (including this
	// touch, for Touch). It interpolates the one bucket the window's edge
	// cuts, so it can read a little high or low.
	Count int
	// Upper counts that cut bucket whole, so it is >= the true count unless
	// Saturated.
	Upper int
	// Saturated: within the window the key ran at or above the register's
	// capacity (31 events per Horizon/24), so events may have spilled to
	// older buckets or been dropped. Count and Upper are then lower bounds.
	Saturated bool
	// Restored: the key had been evicted; its history came back from the
	// stash.
	Restored bool
	// Degraded: the key's history had been pushed out of the stash, and
	// this answer comes from the overflow table: Seen, Count and Upper
	// cover the key's real events but may include another key's (a
	// fingerprint match). (Without an overflow table: the key was not found and the filter lost some history within
	// the last Horizon, so "not seen" may be a key whose history was lost.)
	Degraded bool
}

// Touch records an occurrence of key now and returns the full Result for
// window: TouchAndCount plus the trust flags.
func (f *Filter) Touch(key string, window time.Duration) Result {
	return f.touch(f.Hash(key), f.Ticks(window), f.clock(), true, -1)
}

// TouchAt is Touch on a pre-hashed key, in ticks.
func (f *Filter) TouchAt(h, window, now uint64) Result { return f.touch(h, window, now, true, -1) }

// Count is VelocityCount with the trust flags of Touch. It does not record an
// occurrence.
func (f *Filter) Count(key string, window time.Duration) Result {
	return f.CountAt(f.Hash(key), f.Ticks(window), f.clock())
}

// CheckAndTouch reports whether key was seen within window, then records an
// occurrence now.
func (f *Filter) CheckAndTouch(key string, window time.Duration) bool {
	return f.touch(f.Hash(key), f.Ticks(window), f.clock(), false, -1).Seen
}

// VelocityCount estimates how many times key was touched within window.
// It does not record an occurrence.
func (f *Filter) VelocityCount(key string, window time.Duration) int {
	return f.VelocityCountAt(f.Hash(key), f.Ticks(window), f.clock())
}

// TouchAndCount records an occurrence and returns (seen within window before
// this touch, estimated count within window including this touch).
func (f *Filter) TouchAndCount(key string, window time.Duration) (bool, int) {
	r := f.touch(f.Hash(key), f.Ticks(window), f.clock(), true, -1)
	return r.Seen, r.Count
}

// CheckAndTouchAt is CheckAndTouch on a pre-hashed key with caller-supplied
// window and clock, both in ticks.
func (f *Filter) CheckAndTouchAt(h, window, now uint64) bool {
	return f.touch(h, window, now, false, -1).Seen
}

// TouchAndCountAt is TouchAndCount on a pre-hashed key, in ticks.
func (f *Filter) TouchAndCountAt(h, window, now uint64) (bool, int) {
	r := f.touch(h, window, now, true, -1)
	return r.Seen, r.Count
}

// VelocityCountAt is VelocityCount on a pre-hashed key, in ticks.
func (f *Filter) VelocityCountAt(h, window, now uint64) int {
	return f.CountAt(h, window, now).Count
}

// CountAt is Count on a pre-hashed key, in ticks.
func (f *Filter) CountAt(h, window, now uint64) Result { return f.count(h, window, now, -1) }

// ContainsAt reports whether h was touched within window, without touching it.
func (f *Filter) ContainsAt(h, window, now uint64) bool {
	if f.ct {
		return f.CountAt(h, window, now).Seen
	}
	w0, _, src := f.read(h, now, -1)
	if src != 0 && age(now, w0&tsMask) <= min(window, f.horizon) {
		return true
	}
	if !f.sketchLive(now) {
		return false
	}
	var r Result
	f.addSketch(&r, h, window, now)
	return r.Seen
}

// Evictions counts live keys displaced because both candidate lines were full.
func (f *Filter) Evictions() uint64 { return f.evictions.Load() }

// Relocations counts entries moved to their alternate line to make room.
func (f *Filter) Relocations() uint64 { return f.relocations.Load() }

// Lost counts live histories dropped because the stash was full and there
// is no overflow table: the evictions an attacker could use to reset a
// key's count. With the overflow table (the default) it stays 0.
func (f *Filter) Lost() uint64 { return f.lost.Load() }

// Folded counts histories the stash pushed into the overflow table. Their
// keys' answers are approximate (Degraded) until they age out.
func (f *Filter) Folded() uint64 { return f.folded.Load() }

// Shed counts dropped histories lighter than Config.SpillMin, discarded
// instead of spilled to the overflow table.
func (f *Filter) Shed() uint64 { return f.shed.Load() }

// Waited counts first touches that found both lines full and waited for a
// slot: in the stash for later calls' pads to place them (ConstantTime), or
// in the wait room for the placer goroutine (Background).
func (f *Filter) Waited() uint64 { return f.waited.Load() }

// Waiting is the number of keys waiting for a slot now. If it stays high,
// ConstantTime pads are too short for the insert rate, and calls also place
// keys past their pads (see place); with Background, the placer isn't
// keeping up, and first touches search inline again.
func (f *Filter) Waiting() int { return int(f.deferred.Load()) }

// Restored counts returning keys whose history came back from the stash.
func (f *Filter) Restored() uint64 { return f.restored.Load() }

// Occupancy returns the number of slots holding a key live at now. O(table).
func (f *Filter) Occupancy(now uint64) int {
	n := 0
	for i := 0; i < len(f.words); i += 2 {
		w0 := atomic.LoadUint64(&f.words[i])
		if w0 != 0 && age(now, w0&tsMask) <= f.horizon {
			n++
		}
	}
	return n
}

// Slots returns the table's slot count.
func (f *Filter) Slots() int { return len(f.words) / 2 }

// Bytes is the memory the filter holds now: its table, and the stash,
// overflow table and hot entries once allocated (each on first use).
func (f *Filter) Bytes() int {
	n := 8 * len(f.words)
	for _, s := range [2]*stashTable{f.stash.Load(), f.room.Load()} {
		if s != nil {
			n += len(s.e)*int(unsafe.Sizeof(stashEntry{})) + 8*len(s.wait)
		}
	}
	if g := f.gh.Load(); g != nil {
		n += 8 * len(g.words)
	}
	if sk := f.sk.Load(); sk != nil {
		n += 4 * len(sk.cells)
	}
	if t := f.hot.Load(); t != nil {
		n += len(t.e) * int(unsafe.Sizeof(hotEntry{}))
	}
	return n
}
