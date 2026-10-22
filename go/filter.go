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
	"math/bits"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	slotsPerLine = 4
	wordsPerLine = 2 * slotsPerLine
	// dispBit marks an entry living in its alternate line ("displaced").
	// Lookups match it too, so a key is compared only with entries that
	// share its primary line, wherever they live: half the false matches.
	dispBit = 1 << 47
	// satBit marks a slot whose register overflowed (an event spilled to an
	// older bucket or was dropped) within its span; see estimate.
	satBit    = 1 << 46
	tsMask    = satBit - 1
	tagBits   = 12
	tagShift  = 64 - tagBits
	epochMask = 0xf
	// busy is word1 while a slot is being handed over (install), while a
	// touch moves its register and last touch together (realign), and in a
	// relocation's copy until the hand-off (relocate).
	// No key has its tag, so no lookup or touch can match the slot until
	// the new occupant's word1 replaces it.
	tagBusy = 1<<tagBits - 1
	busy    = tagBusy << tagShift
	// decayEvery gates the inline decay step to 1/64 of operations (by key
	// hash) so the shared cursor's cache line is not touched on every call.
	decayEvery = 63
	// relocDepth bounds the cuckoo path an insert into two full lines may
	// take before evicting.
	relocDepth = 3
	// relocBudget caps the alternate lines one insert may scan. The usable
	// part above the first 8 shrinks with pressure (see claim).
	relocBudget   = 500
	pressureShift = 16
	pressureOne   = 1 << pressureShift
	// claimStripeBits sets 256 insert-path lock stripes, picked by the
	// hash's top bits.
	claimStripeBits = 8
)

// Config sizes a Filter.
type Config struct {
	// Capacity is the expected number of distinct keys live within Horizon.
	Capacity int
	// Horizon is the largest window any query may use.
	Horizon time.Duration
	// Resolution is the clock tick. Default 1µs (46-bit timestamps last ~2.2y).
	Resolution time.Duration
	// Load is the target slot occupancy when Capacity keys are live.
	// Default 0.85. The scorecard (cmd/tfbench) misses no keys at 94% slot
	// load and 0.051% at 97.8%. Lower values trade memory for headroom above
	// Capacity.
	Load float64
	// Clock, if set, returns the current tick. It must be monotonic.
	// Default: monotonic time since New, in Resolution units.
	Clock func() uint64
	// CoarseClock, if > 0 and Clock is nil, makes the built-in clock a cached
	// value refreshed every CoarseClock by one background goroutine: reading
	// it is an atomic load instead of a monotonic clock read (BenchmarkClock*:
	// ~1.4 vs 30-42 ns on the reference host, verify/speed.md),
	// and timestamps are accurate to CoarseClock. Call Close to stop it.
	CoarseClock time.Duration
	// Key, if set, makes Hash and HashUint64 SipHash-2-4 under this 128-bit
	// key: a keyed PRF, the same in every process that uses the key. Default:
	// a random per-filter maphash seed, also secret but not a PRF. Either
	// way an attacker who can't learn the key can't pick inputs that land on
	// a chosen line, identity or lock stripe (docs/SECURITY.md).
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
	// floor (docs/SECURITY.md). A key's answer adds its own entries (Degraded
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
	// report it. docs/SECURITY.md has the measurements. Each call costs at
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
	// moves counts, per claim stripe, moves of a key's entry under way: a
	// relocation from before its hidden copy goes in to the copy's
	// publication or clearing, an eviction from taking the slot to stashing
	// the history. A lookup's miss is trusted only if none overlapped it
	// (lookupStable), and an evictor leaves a key with one under way alone
	// (claim).
	moves [1 << claimStripeBits]moveCount

	sip    sipKey
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
	ct         bool                     // Config.ConstantTime > 0
	deferred   atomic.Int64             // keys waiting for a slot (deferMark): in the stash (ConstantTime) or the wait room (Background)
	waited     atomic.Uint64            // inserts that waited for a slot
	placeNs    atomic.Int64             // ConstantTime: running mean of a placement step, ns
	pad        time.Duration            // Config.ConstantTime
	hot        atomic.Pointer[hotTable] // nil until the first promotion
	hotN       int                      // hot entries; 0: none
	promoted   atomic.Uint64
	hotRefused atomic.Uint64
	bg         bool        // Config.Background
	placing    atomic.Bool // Background: the placer goroutine is running
	bgManual   bool        // tests: Background without the goroutine (the test places)
	_          [64]byte
	room       atomic.Pointer[stashTable] // Background: keys waiting for a slot, nil until the first waits
	roomN      uint64                     // sets of the wait room
	roomMu     [stashStripes]paddedMutex
}

// Test hooks, if set: testHookCopied runs between a relocation's copy and
// its hand-off, testHookVictim between an eviction's choice of victim and
// its install, testHookTaken after a touch takes a waiting key out of the
// wait room, testHookStamped after a touch moves its slot's last touch to a
// later unit, before the register follows (realign, word1 busy);
// testHookPicked after a relocation reads the entry at word index i, before
// it copies it.
var testHookCopied, testHookVictim, testHookTaken, testHookStamped func()
var testHookPicked func(i int)

// forceCT turns ConstantTime on in every filter (tests: TF_CT=1).
var forceCT bool

// forceBG turns Background on in every filter (tests: TF_BG=1).
var forceBG bool

type paddedMutex struct {
	sync.Mutex
	_ [56]byte
}

// moveCount: moves of a stripe's keys begun and done, and evictions of
// them under way. A relocation counts its move, then checks for an
// eviction; an eviction counts itself, then checks for a move. With both
// counts sequentially consistent at least one of two that overlap sees the
// other and backs off, so no eviction takes the source of an entry being
// moved, which would fail the move's hand-off: the move's work would be
// lost, not the key's history (relocate).
type moveCount struct {
	begun, done atomic.Uint64
	evicting    atomic.Int64
}

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
	if cfg.Load <= 0 {
		cfg.Load = 0.85
	}
	if cfg.Load > 1 {
		return nil, errors.New("temporalfilter: Load must be <= 1")
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
		f.sip, f.keyed = newSipKey(cfg.Key), true
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
	f.hotN = min(max(cfg.Hot, 0), hotIdxMask)
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
		read := func() uint64 { return uint64(time.Since(base)/res) + 1 }
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

// Ticks converts a duration to filter ticks.
func (f *Filter) Ticks(d time.Duration) uint64 { return uint64(max(d, 0) / f.res) }

// Hash hashes a string key for the *At methods, under the filter's secret
// key (Config.Key).
func (f *Filter) Hash(key string) uint64 {
	if f.keyed {
		return f.sip.string(key)
	}
	return maphash.String(f.seed, key)
}

// HashUint64 hashes an integer key (IP, card BIN, user id) for the *At
// methods, under the filter's secret key. The *At methods trust the hash they
// are given: pass them only Hash or HashUint64 values of this filter.
func (f *Filter) HashUint64(k uint64) uint64 {
	if f.keyed {
		return f.sip.uint64(k)
	}
	return maphash.Comparable(f.seed, k)
}

// Result is the full answer of Touch and Count, including how far it can be
// trusted. A fail-closed caller (rate limit, abuse check) should treat
// Saturated as "over any limit above Count" and, while Degraded, a key not
// Seen as possibly a returning one. docs/SECURITY.md has the threat model.
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
func (f *Filter) CountAt(h, window, now uint64) Result {
	if f.ct {
		return f.countCT(h, window, now)
	}
	w0, w1, src := f.read(h, now)
	var r Result
	if src != 0 && age(now, w0&tsMask) <= f.horizon {
		r = f.estimateAt(h, w1, w0&tsMask, now, window, w0&satBit != 0)
		r.Seen = r.Count > 0
	} else {
		r.Degraded = f.lostRecently(now)
	}
	if f.sketchLive(now) {
		f.addSketch(&r, h, window, now)
	}
	return r
}

// estimateAt is estimate for key h, whose register (or hot entry) is in w1
// and whose lastTouch is last. If w1 points to an entry the key gave up
// meanwhile (a touch demoted it), the key's slot is looked up again,
// without its stripe (the caller may hold it). If it can't be found, the
// answer fails closed.
func (f *Filter) estimateAt(h, w1, last, now, window uint64, sat bool) Result {
	fp, tg := fingerprint(h), tag(h)
	id := f.stashID(h, fp, tg)
	for range 8 {
		if r, ok := f.estimate(id, w1, last, now, window, sat); ok {
			return r
		}
		i, w0, nw1 := f.lookupStable(h, fp, tg, false)
		if i < 0 {
			break
		}
		w1, last, sat = nw1, max(last, w0&tsMask), w0&satBit != 0
	}
	return Result{Count: 1, Upper: 1, Saturated: true}
}

// countCT is CountAt in ConstantTime mode.
func (f *Filter) countCT(h, window, now uint64) Result {
	t0, pad := f.ctBegin(opCount, now)
	s := time.Now()
	w0, w1, src := f.read(h, now)
	var r Result
	if src != 0 && age(now, w0&tsMask) <= f.horizon {
		r = f.estimateAt(h, w1, w0&tsMask, now, window, w0&satBit != 0)
		r.Seen = r.Count > 0
	} else {
		r.Degraded = f.lostRecently(now)
	}
	if f.sk.Load() != nil || f.gh.Load() != nil {
		f.addSketch(&r, h, window, now)
	}
	f.ctEnd(opCount, h, t0, s, pad)
	return r
}

// ContainsAt reports whether h was touched within window, without touching it.
func (f *Filter) ContainsAt(h, window, now uint64) bool {
	if f.ct {
		return f.CountAt(h, window, now).Seen
	}
	w0, _, src := f.read(h, now)
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

// Where a key's history was found.
const (
	srcTable = iota + 1
	srcStash
	srcRetry // a waiting key was being committed to the table: look there again
	srcRoom  // a waiting key taken out of the wait room (recall with take): its move is under way
)

// read returns h's slot words from the table or, if it was evicted, the
// stash, and where they came from (0: neither).
func (f *Filter) read(h, now uint64) (w0, w1 uint64, src int) {
	fp, tg := fingerprint(h), tag(h)
	if !f.ct {
		// lookupStable, inline: one scan whether the key is found or not.
		// The stash gate is read between the scan and the stability check:
		// a waiting key that leaves the wait room for the table counts as a
		// move of its stripe (placeRoomKey), so if no move began by the
		// check, the key was still waiting when the gate was read.
		m := &f.moves[h>>(64-claimStripeBits)]
		for try := 0; ; try++ {
			b, d := m.begun.Load(), m.done.Load()
			i := f.locate(h, fp, tg)
			if w0, w1, ok := f.slotWords(i, fp, tg); ok {
				return w0, w1, srcTable
			}
			may := f.stashMay(h, fp, tg, now)
			if i < 0 && b == d && m.begun.Load() == b {
				if !may {
					return 0, 0, 0
				}
				break
			}
			if try > 0 {
				runtime.Gosched()
			}
		}
	}
	// Under the key's stripe: an eviction moves the key to the stash while
	// holding it, so the key is found in one of the two.
	mu := &f.claimMu[h>>(64-claimStripeBits)]
	mu.Lock()
	defer mu.Unlock()
	m := &f.moves[h>>(64-claimStripeBits)]
	for {
		b, d := m.begun.Load(), m.done.Load()
		i, w0, w1 := f.lookupStable(h, fp, tg, true)
		s0, s1, src := f.recall(h, fp, tg, now, false)
		if src == srcRetry {
			continue
		}
		if i >= 0 {
			return w0, w1, srcTable
		}
		if src == 0 && !f.settled(m, b, d) {
			continue
		}
		return s0, s1, src
	}
}

// lookupStable returns the index and words of h's slot, or -1 if h has
// none, reading both lines if both (ConstantTime). A lookup scans one line
// and then the other, so an entry that a relocation moves into the line
// already scanned and out of the other in between is missed by both scans;
// an eviction likewise takes a key out of the table before its history
// reaches the stash. A miss is trusted only if no such move of a key on h's
// claim stripe began or ended around the scan. Moves never wait on a
// lookup, so the retry loop ends.
func (f *Filter) lookupStable(h, fp, tg uint64, both bool) (i int, w0, w1 uint64) {
	m := &f.moves[h>>(64-claimStripeBits)]
	for {
		b, d := m.begun.Load(), m.done.Load()
		if both {
			i = f.locateBoth(h, fp, tg)
		} else {
			i = f.locate(h, fp, tg)
		}
		if i >= 0 {
			if w0, w1, ok := f.slotWords(i, fp, tg); ok {
				return i, w0, w1
			}
		} else if b == d && m.begun.Load() == b {
			return -1, 0, 0
		}
		runtime.Gosched()
	}
}

// slotWords loads slot i's words if it still holds fp/tg. word0 is read
// again after word1: a realign between the two loads would pair the old
// last touch with the new register, whose epoch is read against it.
func (f *Filter) slotWords(i int, fp, tg uint64) (w0, w1 uint64, ok bool) {
	if i < 0 {
		return 0, 0, false
	}
	w0 = atomic.LoadUint64(&f.words[i])
	w1 = atomic.LoadUint64(&f.words[i+1])
	if w0>>48 != fp || w1>>tagShift != tg {
		return w0, w1, false
	}
	if a := atomic.LoadUint64(&f.words[i]); a != w0 && f.unitOf(a&tsMask) != f.unitOf(w0&tsMask) {
		return w0, w1, false
	}
	return w0, w1, true
}

// sketchLive: the overflow table may hold a count at now. ConstantTime
// reads it whatever it holds.
func (f *Filter) sketchLive(now uint64) bool {
	if !f.cms {
		g := f.gh.Load()
		return g != nil && (f.ct || now <= g.until.Load())
	}
	sk := f.sk.Load()
	return sk != nil && (f.ct || now <= sk.until.Load())
}

// addSketch adds h's events in the overflow table to r, the answer from
// the table or stash. The overflow holds only histories the stash dropped,
// and a key that comes back starts a fresh slot, so the two never count the
// same event. Upper takes the overflow's bound, Count the key's own entry
// (the count-min sketch: the part that clears the noise), and Degraded is
// set when there is one: the key likely has a dropped history.
func (f *Filter) addSketch(r *Result, h, window, now uint64) {
	var s Result
	var ok bool
	if !f.cms {
		s, ok = f.gh.Load().estimate(f.stashID(h, fingerprint(h), tag(h)), min(window, f.horizon), now)
	} else {
		s, ok = f.sk.Load().estimate(f.stashID(h, fingerprint(h), tag(h)), min(window, f.horizon), now, f.ct)
	}
	if !ok {
		return
	}
	r.Count += s.Count
	r.Upper += s.Upper
	r.Saturated = r.Saturated || s.Saturated
	r.Seen = r.Seen || s.Seen
	r.Degraded = r.Degraded || s.Count > 0
}

// recall finds h's evicted history in the stash, or (Background) h waiting
// in the wait room, removed if take. The caller holds h's claim stripe. A
// key taken out of the wait room (srcRoom) counts as a move of its stripe
// from before it leaves until miss has placed it: lock-free lookups gate
// on the room's wait bits, which the placer clears once a set holds no
// waiting key, so a key in neither the table nor the room meanwhile would
// read absent.
func (f *Filter) recall(h, fp, tg, now uint64, take bool) (w0, w1 uint64, src int) {
	id := f.stashID(h, fp, tg)
	if !f.ct {
		if p := f.room.Load(); p != nil && p.waiting(id) {
			var mv *moveCount
			if take {
				mv = &f.moves[h>>(64-claimStripeBits)]
			}
			w0, w1, ok, retry := f.roomGet(p, id, now, mv)
			if retry {
				return 0, 0, srcRetry
			}
			if ok && take {
				return w0, w1, srcRoom
			}
			if ok {
				return w0, w1, srcStash
			}
		}
		if now > f.stashUntil.Load() {
			return 0, 0, 0
		}
	}
	if w0, w1, ok := f.stashGet(id, now, take); ok {
		return w0, w1, srcStash
	}
	return 0, 0, 0
}

// stashMay: h's history may be outside the table at now: an evicted entry
// live in the stash, or (Background) h waiting in the wait room, whose set's
// bit is set while it waits.
func (f *Filter) stashMay(h, fp, tg, now uint64) bool {
	if now <= f.stashUntil.Load() {
		return true
	}
	p := f.room.Load()
	return p != nil && p.waiting(f.stashID(h, fp, tg))
}

// settled: no move of the stripe whose counts are m overlapped a lookup that
// read them as b and d before its table scan and its recall. A waiting key
// the placer commits to the table between the two (placeRoomKey, without
// the key's stripe) counts as such a move, so a key found in neither is
// absent only if settled. Without a wait room no key moves that way.
func (f *Filter) settled(m *moveCount, b, d uint64) bool {
	return f.room.Load() == nil || b == d && m.begun.Load() == b
}

// roomGet is tableGet in the wait room (Background), where an entry being
// committed to the table (w1 busy, placeRoomKey) is waited out; retry then
// tells the caller to look in the table again if the entry went there. With
// mv (the key's move counts) it takes the entry out, its move begun first.
func (f *Filter) roomGet(p *stashTable, id, now uint64, mv *moveCount) (w0, w1 uint64, ok, retry bool) {
	take := mv != nil
	set, mu := p.set(id)
	for {
		busyNow := false
		mu.Lock()
		for i := range set {
			e := &set[i]
			if e.id != id {
				continue
			}
			if e.w1>>tagShift == tagBusy {
				busyNow = true
				break
			}
			if age(now, e.w0&tsMask) > f.horizon {
				if take { // a fresh insert follows: the placer mustn't install this one too
					f.stashWrite(p, e, stashEntry{})
				}
				break
			}
			w0, w1 = e.w0, e.w1
			if take {
				mv.begun.Add(1)
				f.stashWrite(p, e, stashEntry{})
			}
			mu.Unlock()
			if take && testHookTaken != nil {
				testHookTaken()
			}
			return w0, w1, true, false
		}
		mu.Unlock()
		if !busyNow {
			return 0, 0, false, retry
		}
		retry = true
		runtime.Gosched()
	}
}

// lostRecently: without an overflow table, some history was lost within a horizon.
func (f *Filter) lostRecently(now uint64) bool {
	la := f.lostAt.Load()
	return la != 0 && age(now, la) <= f.horizon
}

// CheckAndTouchBatch is CheckAndTouchAt over many pre-hashed keys, in the
// order of hs, a chunk at a time with the chunk's cache misses overlapped
// (see warm). out must be at least len(hs).
func (f *Filter) CheckAndTouchBatch(hs []uint64, window, now uint64, out []bool) {
	out = out[:len(hs)]
	var at [batchChunk]int
	for lo := 0; lo < len(hs); lo += batchChunk {
		hi := min(lo+batchChunk, len(hs))
		f.warm(hs[lo:hi], &at)
		for i := lo; i < hi; i++ {
			out[i] = f.touch(hs[i], window, now, false, at[i-lo]).Seen
		}
	}
}

// TouchAndCountBatch is TouchAndCountAt over many pre-hashed keys, with the
// overlapped misses of CheckAndTouchBatch. seen and counts must be at least
// len(hs).
func (f *Filter) TouchAndCountBatch(hs []uint64, window, now uint64, seen []bool, counts []int) {
	seen, counts = seen[:len(hs)], counts[:len(hs)]
	var at [batchChunk]int
	for lo := 0; lo < len(hs); lo += batchChunk {
		hi := min(lo+batchChunk, len(hs))
		f.warm(hs[lo:hi], &at)
		for i := lo; i < hi; i++ {
			r := f.touch(hs[i], window, now, true, at[i-lo])
			seen[i], counts[i] = r.Seen, r.Count
		}
	}
}

// VelocityCountBatch is VelocityCountAt over many pre-hashed keys, with the
// overlapped misses of CheckAndTouchBatch. counts must be at least len(hs).
func (f *Filter) VelocityCountBatch(hs []uint64, window, now uint64, counts []int) {
	counts = counts[:len(hs)]
	var at [batchChunk]int
	for lo := 0; lo < len(hs); lo += batchChunk {
		hi := min(lo+batchChunk, len(hs))
		f.warm(hs[lo:hi], &at)
		for i := lo; i < hi; i++ {
			counts[i] = f.VelocityCountAt(hs[i], window, now)
		}
	}
}

// batchChunk is how many keys warm overlaps (S48: 32 beat 64 on tables
// near the last-level cache's size and matched it past them).
const batchChunk = 32

// warm overlaps the cache misses of a chunk's keys (memory-level
// parallelism). It prefetches every key's primary line, looks for each key
// there (at[k]: its slot, or -1), then prefetches the alternate lines of the
// keys it didn't find there, so each key's operation, in order, finds its
// lines cached or on their way, and a touch starts from its slot. Prefetch
// instructions, unlike loads, don't hold up retirement, and the touches'
// locked CASes don't wait for them. S48: 1.2x the loads it replaces at 90%
// load on tables past the last-level cache; 0.9x on a table that fits it.
//
// In ConstantTime it looks nothing up and prefetches both lines of every
// key: its work, outside the calls' pads, doesn't depend on what the table
// holds (touchCT reads both lines anyway).
func (f *Filter) warm(hs []uint64, at *[batchChunk]int) {
	var lines [2 * batchChunk]uint64
	w := unsafe.Pointer(&f.words[0])
	if f.ct {
		for k, h := range hs {
			p := f.primary(h)
			lines[2*k], lines[2*k+1], at[k] = p, f.alt(p, fingerprint(h)), -1
		}
		prefetchLines(w, &lines[0], 2*len(hs))
		return
	}
	for k, h := range hs {
		lines[k] = f.primary(h)
	}
	prefetchLines(w, &lines[0], len(hs))
	m := 0
	for k, h := range hs {
		p, fp := lines[k], fingerprint(h)
		if at[k] = f.find(p, fp<<1, tag(h)); at[k] < 0 {
			lines[m], m = f.alt(p, fp), m+1 // m <= k: the entries before k's are used
		}
	}
	prefetchLines(w, &lines[0], m)
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

// touch is TouchAt. i0, if not -1, is a slot where a lookup just found h
// (warm): its first look starts there if the slot still holds h.
func (f *Filter) touch(h, window, now uint64, wantCount bool, i0 int) Result {
	if h&decayEvery == 0 {
		f.decayStep(now, 1)
	}
	if f.ct {
		return f.touchCT(h, window, now, wantCount)
	}
	fp, tg := fingerprint(h), tag(h)
	for first := true; ; first = false {
		i := -1
		if first {
			if _, _, ok := f.slotWords(i0, fp, tg); ok {
				i = i0
			}
		}
		if i < 0 {
			i = f.locate(h, fp, tg)
		}
		if i < 0 {
			// Miss: serialize first touches of the same key so two racers
			// can't each claim a slot (which would split its count). The
			// lock covers only this path; hits never take it.
			mu := &f.claimMu[h>>(64-claimStripeBits)]
			mu.Lock()
			m := &f.moves[h>>(64-claimStripeBits)]
			b, d := m.begun.Load(), m.done.Load()
			if i, _, _ = f.lookupStable(h, fp, tg, false); i < 0 {
				s0, s1, src := f.recall(h, fp, tg, now, true)
				if src == srcRetry || src == 0 && !f.settled(m, b, d) {
					mu.Unlock()
					continue
				}
				r := f.miss(h, fp, tg, window, now, wantCount, s0, s1, src)
				mu.Unlock()
				if r.Count < 0 {
					continue
				}
				if f.sketchLive(now) {
					f.addSketch(&r, h, window, now)
				}
				return r
			}
			mu.Unlock() // another first touch got there first: count as a hit
		}
		w0 := atomic.LoadUint64(&f.words[i])
		if w0>>48 != fp {
			continue // slot was reclaimed between find and load; retry
		}
		last := w0 & tsMask
		stale := age(now, last) > f.horizon
		var w1, ref uint64
		var over, ok bool
		nu, lu := f.unitOf(now), f.unitOf(last)
		if old, far, wait := f.farAhead(i, nu, lu); wait {
			runtime.Gosched()
			continue // another touch is realigning the slot (see hit)
		} else if far {
			w1, over, ok = f.realign(h, i, w0, old, tg, now)
			ref = now
		} else {
			if now > last && !atomic.CompareAndSwapUint64(&f.words[i], w0, w0&^tsMask|now) {
				continue
			}
			if nu > lu && testHookStamped != nil {
				testHookStamped()
			}
			w1, ref, over, ok = f.bump(i, w0>>47, tg, now, nu, last, lu, stale)
		}
		if !ok {
			continue // the slot was handed to another key, or moved on, meanwhile; look again
		}
		sat := w0&satBit != 0
		if over != sat {
			sat = f.markSat(i, w0>>48, over)
		}
		var r Result
		if wantCount {
			r = f.estimateAt(h, w1, max(now, ref), now, window, sat)
		}
		r.Seen = !stale && age(now, last) <= window
		if f.sketchLive(now) {
			f.addSketch(&r, h, window, now)
		}
		return r
	}
}

// AutoPad as Config.ConstantTime pads each operation to a running estimate
// of its own work's 99.5th-percentile latency instead of a fixed duration.
const AutoPad time.Duration = -1

const (
	opTouch = iota
	opCount
	// The estimate (in ps) moves up autoUp when a sampled call's own work
	// ran past it and down autoDown otherwise, so it settles where 0.5% of calls run
	// past it (autoDown/(autoUp+autoDown)). One slow call (a GC pause)
	// moves it 100 ns.
	autoUp    = 99_500
	autoDown  = 500
	autoStart = 4 * time.Microsecond
	autoMax   = 50 * time.Microsecond
)

type quantilePad struct {
	q atomic.Int64 // ps
	_ [56]byte
}

// ctBegin starts a ConstantTime call of kind op and returns its start and
// pad. Touch's pad is ConstantTime; Count's, whose paths differ far less
// (no insert), is a running estimate of its own work's 99.5th percentile,
// at most ConstantTime; with AutoPad both are. Before the call's own work,
// so that how much it does can't depend on the caller's key, it spends what
// the pad leaves beyond that estimate placing keys that wait in the stash.
func (f *Filter) ctBegin(op int, now uint64) (t0 time.Time, pad time.Duration) {
	t0 = time.Now()
	own := time.Duration(f.padQ[op].q.Load() / 1000)
	switch {
	case op == opTouch && !f.auto:
		pad = f.pad
	case f.auto:
		pad = own
	default:
		pad = min(own, f.pad)
	}
	f.place(t0, pad-own, now)
	return t0, pad
}

// ctEnd moves op's own-work estimate with the work done since s (a sample
// of calls, 1 in 4 by key, moves it) and spins until the pad, plus a random
// dither, has passed since t0. The spin reads the clock every ~37 ns (2026-10-21
// host), so a pad ending at a fixed time ends at a phase of those reads
// that follows the call's own work, and keys whose work time is narrow show
// it (Count at a fixed 1 µs pad: D 0.019-0.041; with the dither 0.007-0.014,
// claims S41). The dither spans several reads and doesn't depend on the key.
func (f *Filter) ctEnd(op int, h uint64, t0, s time.Time, pad time.Duration) {
	if h>>8&3 == 0 {
		qp := &f.padQ[op].q
		if est := time.Duration(qp.Load() / 1000); time.Since(s) > est {
			if est < autoMax {
				qp.Add(autoUp)
			}
		} else if est > 0 {
			qp.Add(-autoDown)
		}
	}
	pad += time.Duration(rand.Uint64N(uint64(padDither)))
	for time.Since(t0) < pad {
	}
}

// padDither is the span over which a ConstantTime pad's end is drawn: a mean
// cost of 64 ns per call.
const padDither = 128 * time.Nanosecond

// place spends up to budget since t0 on keys that wait in the stash for a
// slot (an insert that found its two lines full defers the cuckoo search to
// here, so no call's own work includes one). A step takes the next stash
// set with a waiting key and inserts its waiting keys as a first touch
// outside ConstantTime would: free slot, cuckoo path, or eviction of the
// slot with the fewest events into the stash. A step starts only if the
// rest of the budget covers a step's running mean cost. If more than one
// key per 16 stash entries waits (pads too short for the insert rate),
// every call places one set regardless: calls then run longer for every key
// alike, which says the table is under insert pressure, not which keys were
// seen, and the stash's sets stay far from full (a waiting key whose set is
// full is inserted at once, which takes time).
func (f *Filter) place(t0 time.Time, budget time.Duration, now uint64) {
	p := f.stash.Load()
	if p == nil {
		return
	}
	forced := f.deferred.Load() > int64(len(p.e)/16)
	for f.deferred.Load() > 0 {
		est := time.Duration(f.placeNs.Load())
		if est == 0 {
			est = time.Microsecond
		}
		if !forced && budget-time.Since(t0) < est {
			f.placeNs.Store(int64(est - est/64)) // a step that ran long once mustn't stop placing for good
			return
		}
		forced = false
		s := time.Now()
		if !f.placeNext(p, now) {
			return
		}
		f.placeNs.Store(int64(est + (time.Since(s)-est)/8))
	}
}

// placeNext places the waiting keys of table p's next set (after the last
// one visited) whose wait bit is set; false if no bit is set.
func (f *Filter) placeNext(p *stashTable, now uint64) bool {
	if p == nil {
		return false
	}
	nw := uint64(len(p.wait))
	start := p.at.Load()
	for k := range nw + 1 {
		wi := (start/64 + k) % nw
		b := p.wait[wi].Load()
		if k == 0 {
			b &^= 1<<(start%64) - 1 // bits before the cursor wait for the wrap-around
		}
		if b == 0 {
			continue
		}
		set := wi*64 + uint64(bits.TrailingZeros64(b))
		p.at.Store(set + 1)
		if p == f.room.Load() {
			f.placeRoomSet(p, set)
		} else {
			f.placeSet(p, set, now)
		}
		return true
	}
	return false
}

// placeSet moves the waiting keys of table p's set i into the table. A key
// whose stripe is busy, or that loses a race for its slot, is left for a
// later step.
func (f *Filter) placeSet(p *stashTable, i, now uint64) {
	set, mu := p.e[i*stashWays:(i+1)*stashWays], &p.mu[i%stashStripes]
	var ids [stashWays]uint64
	n := 0
	mu.Lock()
	for k := range set {
		if e := &set[k]; e.id != 0 && e.w0&deferMark != 0 {
			if age(now, e.w0&tsMask) > f.horizon {
				f.stashWrite(p, e, stashEntry{}) // expired while it waited
				continue
			}
			ids[n], n = e.id, n+1
		}
	}
	if n == 0 {
		p.wait[i/64].And(^(1 << (i % 64)))
	}
	mu.Unlock()
	for _, id := range ids[:n] {
		fp, tg := id>>tagBits&(1<<16-1), id&(1<<tagBits-1)
		stripe := fp >> (16 - claimStripeBits)
		cm := &f.claimMu[stripe]
		if !cm.TryLock() {
			continue
		}
		if w0, w1, ok := f.tableGet(p, id, now, true); ok {
			pl := id >> 28
			al := f.alt(pl, fp)
			switch {
			case f.find(pl, fp<<1, tg) >= 0 || f.find(al, fp<<1|1, tg) >= 0:
				f.stashPut(id, w0, w1, now) // the table holds the key: no longer waiting
			case f.claimAt([2]uint64{pl, al}, [2]uint64{w0 &^ dispBit, w0 | dispBit}, w1, now, stripe, claimFull, nil) < 0:
				f.stashPutAs(id, w0, w1, now, true)
			}
		}
		cm.Unlock()
	}
}

// placeRoomSet places the waiting keys of the wait room's set i
// (Background).
func (f *Filter) placeRoomSet(p *stashTable, i uint64) {
	set, mu := p.e[i*stashWays:(i+1)*stashWays], &p.mu[i%stashStripes]
	var waiting [stashWays]stashEntry
	n := 0
	mu.Lock()
	for k := range set {
		if e := set[k]; e.id != 0 && e.w0&deferMark != 0 {
			waiting[n], n = e, n+1
		}
	}
	if n == 0 {
		p.wait[i/64].And(^(1 << (i % 64)))
	}
	mu.Unlock()
	for _, e := range waiting[:n] {
		f.placeRoomKey(p, e)
	}
}

// placeRoomKey places waiting key e (as its set held it when read) at the
// clock of its last touch, without its claim stripe: the search blocks no
// first touch, and the key stays in the room meanwhile, where its touches
// and counts find it. put commits it: it marks the room entry busy (w1
// busy; a recall of the key waits that out, then looks in the table
// again), installs the entry's words as they are now, and removes the entry,
// or restores it if the install lost its race. The commit counts as a move
// of the key's stripe, so a lookup that misses the table meanwhile looks
// again. If a touch of the key took it out of the room first, put fails and
// the key is that touch's to place.
func (f *Filter) placeRoomKey(p *stashTable, e stashEntry) {
	id := e.id
	fp := id >> tagBits & (1<<16 - 1)
	m := &f.moves[fp>>(16-claimStripeBits)]
	pl := id >> 28
	put := func(i int, w0, w1, mine0, _ uint64) bool {
		set, mu := p.set(id)
		mu.Lock()
		var c *stashEntry
		for k := range set {
			if set[k].id == id && set[k].w0&deferMark != 0 && set[k].w1>>tagShift != tagBusy {
				c = &set[k]
			}
		}
		if c == nil {
			mu.Unlock()
			return false
		}
		c0, c1 := c.w0, c.w1
		c.w1 = busy
		mu.Unlock()
		m.begun.Add(1)
		ok := f.install(i, w0, w1, c0&^deferMark|mine0&dispBit, c1)
		mu.Lock()
		if ok {
			f.stashWrite(p, c, stashEntry{})
		} else {
			c.w1 = c1
		}
		mu.Unlock()
		m.done.Add(1)
		return ok
	}
	f.claimAt([2]uint64{pl, f.alt(pl, fp)}, [2]uint64{e.w0 &^ deferMark, e.w0 | dispBit}, e.w1, e.w0&tsMask, noStripe, claimFull, put)
}

// noStripe, as claimAt's stripe: the caller holds no claim stripe.
const noStripe = 1 << claimStripeBits

// bgKick (Background) starts the placer goroutine unless it is running.
func (f *Filter) bgKick() {
	if !f.bgManual && !f.placing.Load() && f.placing.CompareAndSwap(false, true) {
		go f.bgPlace()
	}
}

const (
	bgNap    = 50 * time.Microsecond
	bgLinger = 20 // naps with nothing waiting before the placer exits
)

// bgPlace is the Background placer: it places waiting keys while any wait
// (it never reads the filter's clock: see placeRoomKey), and exits after
// bgLinger naps with nothing to place. A key that starts waiting as it
// exits sees placing false and starts another, or is seen here (both flags
// are sequentially consistent).
func (f *Filter) bgPlace() {
	for {
		for idle := 0; idle < bgLinger; {
			if f.deferred.Load() > 0 && f.placeNext(f.room.Load(), 0) {
				idle = 0
				continue
			}
			idle++
			time.Sleep(bgNap)
		}
		f.placing.Store(false)
		if f.deferred.Load() == 0 || !f.placing.CompareAndSwap(false, true) {
			return
		}
	}
}

// hit counts a touch in slot i, which held the key when it was found. ok is
// false if the slot changed hands meanwhile and the caller must look again.
//
// A register keeps 4 bits of its unit and is read against the slot's last
// touch (word0), so word0 must never run 8 units or more ahead of it where
// a touch or a read can see the pair. A touch that moves word0 first and
// the register after keeps that only for a jump of under 8 units from the
// register's own unit: it then moves word0 lock-free and counts with bump,
// which reads the register against word0 as it is then. A touch 8 or more
// units past the register's unit (a key idle a third of the horizon, or
// past it) moves word0 and the register together under word1 busy
// (realign). Before realign, such a touch moved word0 first: a second touch
// of the key between its two writes read the old register as the new
// unit's, and the first then aged that touch's event by the gap, or, past
// the horizon, reset it away (claims A21). A hot entry keeps its own full
// unit: its key's touches all count lock-free.
func (f *Filter) hit(h uint64, i int, fp, tg, window, now uint64, wantCount bool) (Result, bool) {
	w0 := atomic.LoadUint64(&f.words[i])
	if w0>>48 != fp {
		return Result{}, false // slot was reclaimed between find and load
	}
	last := w0 & tsMask
	stale := age(now, last) > f.horizon
	var w1, ref uint64
	var over, ok bool
	nu, lu := f.unitOf(now), f.unitOf(last)
	if old, far, wait := f.farAhead(i, nu, lu); wait {
		return Result{}, false
	} else if far {
		w1, over, ok = f.realign(h, i, w0, old, tg, now)
		ref = now
	} else {
		if now > last && !atomic.CompareAndSwapUint64(&f.words[i], w0, w0&^tsMask|now) {
			return Result{}, false
		}
		if nu > lu && testHookStamped != nil {
			testHookStamped()
		}
		w1, ref, over, ok = f.bump(i, w0>>47, tg, now, nu, last, lu, stale)
	}
	if !ok {
		return Result{}, false // the slot was handed to another key, or moved on, meanwhile
	}
	sat := w0&satBit != 0
	if over != sat {
		sat = f.markSat(i, w0>>48, over)
	}
	seen := !stale && age(now, last) <= window
	if !wantCount {
		return Result{Seen: seen}, true
	}
	r := f.estimateAt(h, w1, max(now, ref), now, window, sat)
	r.Seen = seen
	return r, true
}

// farAhead: a touch in unit nu would move slot i's last touch (in unit lu)
// 8 or more units past its register's unit (old: the register read, or a
// hot entry's word1, which counts lock-free whatever the gap). wait: word1
// is busy, so its unit can't be read; a touch that moved word0 on now could
// leave it 8 units past the register the holder publishes.
func (f *Filter) farAhead(i int, nu, lu int64) (old uint64, far, wait bool) {
	if nu <= lu {
		return 0, false, false
	}
	old = atomic.LoadUint64(&f.words[i+1])
	if old>>tagShift == tagBusy {
		return old, false, true
	}
	return old, !isHot(old) && nu-regEpoch(old, lu) > epochMask/2, false
}

// realign is hit's count of a touch at now 8 or more units past the unit of
// slot i's register (old, read against word0 w0): word1 goes busy, word0
// moves to now, then the register, advanced to now's unit with the touch
// counted, is published in word1. While word1 is busy, a lookup that
// misses the slot looks again: the hold counts as a move of the key's
// stripe. ok is false if the slot changed meanwhile.
func (f *Filter) realign(h uint64, i int, w0, old, tg, now uint64) (w1 uint64, over, ok bool) {
	if old>>tagShift != tg || isHot(old) {
		return 0, false, false
	}
	m := &f.moves[h>>(64-claimStripeBits)]
	m.begun.Add(1)
	defer m.done.Add(1)
	if !atomic.CompareAndSwapUint64(&f.words[i+1], old, busy) {
		return 0, false, false
	}
	if !atomic.CompareAndSwapUint64(&f.words[i], w0, w0&^tsMask|now) {
		atomic.StoreUint64(&f.words[i+1], old)
		return 0, false, false
	}
	if testHookStamped != nil {
		testHookStamped()
	}
	last := w0 & tsMask
	w1, over = f.nextReg(old, tg, last, now, age(now, last) > f.horizon)
	if over {
		if pw, ok := f.promoteHeld(i, w0, old, tg, last, now); ok {
			w1, over = pw, false
		}
	}
	atomic.StoreUint64(&f.words[i+1], w1)
	return w1, over, true
}

// touchCT is touch in ConstantTime mode: under the key's stripe, the same
// lookups whatever the key's state (both lines, stash, overflow), then the
// hit or the insert.
func (f *Filter) touchCT(h, window, now uint64, wantCount bool) Result {
	t0, pad := f.ctBegin(opTouch, now)
	s := time.Now()
	fp, tg := fingerprint(h), tag(h)
	mu := &f.claimMu[h>>(64-claimStripeBits)]
	var r Result
	for {
		// The stripe is released between attempts: a claim that fails on
		// another stripe's TryLock must not hold its own while it retries.
		mu.Lock()
		var ok bool
		r, ok = f.touchCTOnce(h, fp, tg, window, now, wantCount)
		mu.Unlock()
		if ok {
			break
		}
	}
	if f.sk.Load() != nil || f.gh.Load() != nil {
		f.addSketch(&r, h, window, now)
	}
	f.ctEnd(opTouch, h, t0, s, pad)
	return r
}

func (f *Filter) touchCTOnce(h, fp, tg, window, now uint64, wantCount bool) (Result, bool) {
	i, _, _ := f.lookupStable(h, fp, tg, true)
	s0, s1, src := f.recall(h, fp, tg, now, true)
	if i >= 0 {
		if src == srcStash { // a stale copy: the table's entry is newer
			f.stashPut(f.stashID(h, fp, tg), s0, s1, now)
		}
		return f.hit(h, i, fp, tg, window, now, wantCount)
	}
	r := f.miss(h, fp, tg, window, now, wantCount, s0, s1, src)
	return r, r.Count >= 0
}

// miss installs h, absent from the table, with this touch counted: a fresh
// register, or its evicted history from the stash. Count < 0 means it lost a
// race for a slot and the caller should look again. The caller holds h's
// claim stripe, so no other first touch of h runs meanwhile.
func (f *Filter) miss(h, fp, tg, window, now uint64, wantCount bool, s0, s1 uint64, src int) Result {
	var r Result
	// Taken out of the wait room (srcRoom): its move ends once it is placed.
	room := src == srcRoom
	w1, _ := f.nextReg(0, tg, now, now, true) // first event, published with the key
	last, sat := now, uint64(0)
	hotRestore := false
	if src != 0 && age(now, s0&tsMask) <= f.horizon {
		last = s0 & tsMask
		if isHot(s1) { // it keeps its hot entry; the touch is counted once it is installed
			w1, hotRestore = s1, true
		} else {
			var over bool
			w1, over = f.nextReg(s1, tg, last, now, false)
			if over || s0&satBit != 0 && hot(w1) {
				sat = satBit
			}
		}
		r.Seen = age(now, last) <= window
		r.Restored = s0&deferMark == 0 // a key that waited for a slot was never evicted
	} else if f.lostRecently(now) {
		r.Degraded = true
	}
	// A recalled history keeps its last touch even when this caller's clock is
	// behind it (the register counted this touch in that unit too): stamped
	// with the older clock, a later window starting between the two would
	// miss the history's newest event.
	stamp := max(now, last)
	ci := f.claim(h, fp, w1, now, stamp, sat)
	if ci == claimWait {
		// No free slot: wait for one, in the stash (ConstantTime; pads place
		// it) or the wait room (Background; the placer goroutine places it).
		// If more than one key per 16 wait-room entries waits, the placer
		// isn't keeping up: search now.
		if (f.ct || f.deferred.Load() <= int64(f.roomN*stashWays/16)) && f.stashWait(f.stashID(h, fp, tg), fp<<48|sat|stamp, w1, now) {
			f.waited.Add(1)
			src, r.Restored = 0, false
			if f.bg {
				f.bgKick()
			}
		} else { // its set is full: search (and evict) now rather than drop a history
			ci = f.claimAt([2]uint64{f.primary(h), f.alt(f.primary(h), fp)},
				[2]uint64{fp<<48 | sat | stamp, fp<<48 | dispBit | sat | stamp}, w1, now, h>>(64-claimStripeBits), claimFull, nil)
		}
	}
	if ci == -1 && (src == srcStash || room) { // put it back for the retry
		f.putBack(f.stashID(h, fp, tg), s0, s1, now)
	}
	if room { // in the table, or back in the room or the stash
		f.moves[h>>(64-claimStripeBits)].done.Add(1)
	}
	if ci == -1 {
		return Result{Count: -1}
	}
	if hotRestore && !f.hotAdd(w1, f.stashID(h, fp, tg), now, nil) {
		return Result{Count: 1, Upper: 1, Saturated: true, Seen: r.Seen, Restored: r.Restored} // can't happen while its history is live
	}
	if src == srcStash && r.Restored {
		f.restored.Add(1)
	}
	if !wantCount {
		return r
	}
	e := f.estimateAt(h, w1, stamp, now, window, sat != 0)
	e.Seen, e.Restored, e.Degraded = r.Seen, r.Restored, r.Degraded
	return e
}

// markSat sets (on) or clears the saturated bit of the slot at word index i
// if it still holds fp, and returns the bit as left. It clears only while no
// bucket is hot, reading word1 after word0: an overflowing touch writes its
// register before it sets the bit, so a clear that read a register from
// before that touch fails its CAS on the newer word0.
func (f *Filter) markSat(i int, fp uint64, on bool) bool {
	for {
		w0 := atomic.LoadUint64(&f.words[i])
		if w0>>48 != fp {
			return on
		}
		if w0&satBit != 0 == on {
			return on
		}
		if w1 := atomic.LoadUint64(&f.words[i+1]); !on && (w1>>tagShift == tagBusy || hot(w1)) {
			return true // a busy word1 (realign) may come back with a hot bucket
		}
		if atomic.CompareAndSwapUint64(&f.words[i], w0, w0^satBit) {
			return on
		}
	}
}

// locate returns the word index of h's slot, or -1: an undisplaced entry in
// the primary line or a displaced one in the alternate line.
func (f *Filter) locate(h, fp, tg uint64) int {
	p := f.primary(h)
	if i := f.find(p, fp<<1, tg); i >= 0 {
		return i
	}
	return f.find(f.alt(p, fp), fp<<1|1, tg)
}

// locateBoth is locate reading both lines every time (ConstantTime).
func (f *Filter) locateBoth(h, fp, tg uint64) int {
	p := f.primary(h)
	i := f.find(p, fp<<1, tg)
	j := f.find(f.alt(p, fp), fp<<1|1, tg)
	if i >= 0 {
		return i
	}
	return j
}

// find returns the word index of the slot in line whose fingerprint and
// displaced bit equal fpd and whose tag is tg, or -1. The tag lives in word1
// of the same cache line, so identity is effectively 28 bits at no extra
// memory traffic.
func (f *Filter) find(line, fpd, tg uint64) int {
	base := int(line * wordsPerLine)
	w := f.words[base : base+wordsPerLine : base+wordsPerLine]
	for s := 0; s < wordsPerLine; s += 2 {
		if atomic.LoadUint64(&w[s])>>47 == fpd && atomic.LoadUint64(&w[s+1])>>tagShift == tg {
			return base + s
		}
	}
	return -1
}

// claim installs fp in an empty or expired slot of the primary line, then the
// alternate line. If all eight are live it tries to relocate one of them to
// its own alternate line (one cuckoo step). Only then does it evict, taking
// the slot with the fewest events in the horizon (oldest on ties), so a spray
// of fresh keys cannot flush a repeat offender. w1 is the new key's tagged
// register, published together with its word0. Returns word0 index, or -1
// if it lost a race for a slot and the caller should look the key up again
// (a racing writer may have completed it). The caller holds the key's claim
// stripe. In ConstantTime and Background it searches and evicts nothing:
// with no free slot it returns claimWait, and the key waits for a slot
// (miss). The entry's last touch is stamp; now is the caller's clock
// (expiry, eviction scores).
func (f *Filter) claim(h, fp, w1, now, stamp, sat uint64) int {
	mode := claimFull
	if f.ct || f.bg {
		mode = claimDefer
	}
	return f.claimAt([2]uint64{f.primary(h), f.alt(f.primary(h), fp)},
		[2]uint64{fp<<48 | sat | stamp, fp<<48 | dispBit | sat | stamp}, w1, now, h>>(64-claimStripeBits), mode, nil)
}

// claimAt modes: free slot, path or eviction (claimFull); free slot only,
// else claimWait (claimDefer).
const (
	claimFull = iota
	claimDefer

	claimWait = -2
)

// claimAt is claim for an entry with lines and words mine (primary,
// alternate), on claim stripe stripe, which the caller holds (noStripe: none).
// put installs the entry (nil: install; see putNew).
func (f *Filter) claimAt(lines, mine [2]uint64, w1, now, stripe uint64, mode int, put putFunc) int {
	for {
		for li, line := range lines {
			base := int(line * wordsPerLine)
			for s := 0; s < wordsPerLine; s += 2 {
				w0 := atomic.LoadUint64(&f.words[base+s])
				if w0 == 0 || age(now, w0&tsMask) > f.horizon {
					if f.putNew(put, base+s, w0, atomic.LoadUint64(&f.words[base+s+1]), mine[li], w1) {
						return base + s
					}
					return -1
				}
			}
		}
		if mode == claimDefer {
			return claimWait
		}
		// Iterative deepening: all short paths before any long one. The deep
		// budget shrinks as recent searches fail, so an overfull table falls
		// back to cheap eviction instead of searching in vain.
		p := f.pressure.Load()
		budget := 8 + int(uint64(relocBudget-8)*(pressureOne-uint64(p))>>pressureShift)
		f.warmAlts(lines)
		for depth := 0; depth <= relocDepth; depth++ {
			for li, line := range lines {
				base := int(line * wordsPerLine)
				for s := 0; s < wordsPerLine; s += 2 {
					if f.relocate(base+s, line, mine[li], w1, now, depth, &budget, nil, put) {
						f.pressure.Store(p - p>>3) // EWMA toward "searches succeed"
						return base + s
					}
				}
			}
		}
		f.pressure.Store(p - p>>3 + pressureOne>>3) // EWMA toward "searches fail"
		victim, vli, victimScore, victimW0, victimW1 := -1, 0, uint64(1<<64-1), uint64(0), uint64(0)
		for li, line := range lines {
			base := int(line * wordsPerLine)
			for s := 0; s < wordsPerLine; s += 2 {
				w0 := atomic.LoadUint64(&f.words[base+s])
				ts := w0 & tsMask
				vw1 := atomic.LoadUint64(&f.words[base+s+1])
				// Read after the slot: a copy goes in only once its move is
				// counted, so a key whose move is under way is left alone.
				if mv := &f.moves[w0>>(64-claimStripeBits)]; mv.begun.Load() != mv.done.Load() {
					continue
				}
				score := min(f.mass(f.slotID(line, w0, vw1), vw1, ts, now), 1<<16-1)<<48 | ts
				if score < victimScore {
					victim, vli, victimScore, victimW0, victimW1 = base+s, li, score, w0, vw1
				}
			}
		}
		if victim < 0 {
			return -1 // every candidate is mid-move: look again
		}
		if testHookVictim != nil {
			testHookVictim()
		}
		// The victim's history goes to the stash under the victim's own claim
		// stripe (its hash's top bits are its fp's), so a first touch of the
		// victim, which takes that stripe, finds it either still here or
		// already stashed. TryLock: the caller holds its own stripe.
		vs := victimW0 >> (64 - claimStripeBits)
		if vs != stripe {
			mu := &f.claimMu[vs]
			if !mu.TryLock() {
				return -1
			}
			defer mu.Unlock()
		}
		// A move of the victim's stripe that began after the scan may have
		// copied the victim, and its hand-off would then fail: back off (see
		// moveCount).
		m := &f.moves[vs]
		m.evicting.Add(1)
		defer m.evicting.Add(-1)
		if m.begun.Load() != m.done.Load() {
			return -1
		}
		// Between the install and the stash a lock-free lookup finds the
		// victim in neither; the move count tells it to look again.
		m.begun.Add(1)
		defer m.done.Add(1)
		if !f.putNew(put, victim, victimW0, victimW1, mine[vli], w1) {
			return -1
		}
		f.evictions.Add(1)
		f.stashPut(f.slotID(lines[vli], victimW0, victimW1), victimW0, victimW1, now)
		return victim
	}
}

// putFunc installs a new key's entry in slot i (word index) if the slot still
// holds w0/w1, as install does; a Background placement's put also takes the
// key out of the wait room as it goes in.
type putFunc func(i int, w0, w1, mine0, mine1 uint64) bool

// putNew installs a new key's entry with put, or install if put is nil.
func (f *Filter) putNew(put putFunc, i int, w0, w1, mine0, mine1 uint64) bool {
	if put != nil {
		return put(i, w0, w1, mine0, mine1)
	}
	return f.install(i, w0, w1, mine0, mine1)
}

// warmAlts loads one word of the alternate line of every entry in lines
// without using it: the misses are in flight together, and the search that
// follows finds the lines on their way or cached.
func (f *Filter) warmAlts(lines [2]uint64) {
	f.warmLine(lines[0])
	f.warmLine(lines[1])
}

// warmLine loads one word of the alternate line of each entry in line.
// Four misses at once is about what this host keeps in flight (claims S42);
// a whole level of the search at once (32) is not.
func (f *Filter) warmLine(line uint64) {
	var sink uint64
	base := int(line * wordsPerLine)
	for s := 0; s < wordsPerLine; s += 2 {
		if w0 := atomic.LoadUint64(&f.words[base+s]); w0 != 0 {
			sink += atomic.LoadUint64(&f.words[f.alt(line, w0>>48)*wordsPerLine])
		}
	}
	runtime.KeepAlive(sink)
}

// install hands slot i (word index), if it still holds w0/w1, to the entry
// mine0/mine1. word1 goes busy first: a reader of the old occupant then
// misses rather than matching a half-written slot, and a late writer to the
// old register (bump) backs off. Nobody else writes a busy word1, so it is
// then set with plain stores.
func (f *Filter) install(i int, w0, w1, mine0, mine1 uint64) bool {
	if w1>>tagShift == tagBusy || !atomic.CompareAndSwapUint64(&f.words[i+1], w1, busy) {
		return false
	}
	if !atomic.CompareAndSwapUint64(&f.words[i], w0, mine0) {
		atomic.StoreUint64(&f.words[i+1], w1) // lost the slot; put word1 back
		return false
	}
	atomic.StoreUint64(&f.words[i+1], mine1)
	return true
}

// relocate moves the entry at word index i (in line) to its alternate line
// and installs mine0/mine1 in its place. If that line is full it first
// relocates one of its entries, up to depth more steps (a bounded cuckoo
// path).
//
// The copy is installed hidden (word1 busy): no lookup finds it and no
// touch counts in it. The hand-off takes the source only if its words are
// still those copied (install's CASes), and only then is the copy's word1
// published, so a key is never live in two slots. Before, the copy was
// visible from its install: a touch that had found the source a moment
// before counted there while reads found the copy (they scan its line
// first), and a copy of words read before the key was evicted came back as
// a second history whose touches the failed hand-off then dropped (claims
// A23). If the source changed (a touch reached it, or the key was evicted)
// the move is abandoned and the copy, which counted nothing, cleared.
//
// The move counts in its key's stripe (f.moves) from before the copy is
// installed to its publication or clearing: a lookup that misses meanwhile
// (between the hand-off and the publication the key is in no slot a lookup
// matches) looks again, and an evictor leaves the source alone. parent is
// the move whose copy (mine0; mine1 is busy, the parent publishes it) this
// one installs, nil for a new key, which put installs.
func (f *Filter) relocate(i int, line, mine0, mine1, now uint64, depth int, budget *int, parent *moveCount, put putFunc) bool {
	w0 := atomic.LoadUint64(&f.words[i])
	if w0 == 0 {
		return false
	}
	if *budget <= 0 {
		return false
	}
	*budget--
	w1 := atomic.LoadUint64(&f.words[i+1])
	if w1>>tagShift == tagBusy || atomic.LoadUint64(&f.words[i]) != w0 {
		return false // being installed or realigned, or touched since: a copy would pair stale words
	}
	if testHookPicked != nil {
		testHookPicked(i)
	}
	a := f.alt(line, w0>>48)
	base := int(a * wordsPerLine)
	moved := w0 ^ dispBit                   // displaced iff it wasn't
	m := &f.moves[w0>>(64-claimStripeBits)] // the entry's key's stripe (its hash's top bits are its fp's)
	replace := func(d int) bool {           // the hidden copy is at d; now take the source
		defer m.done.Add(1)
		if parent != nil {
			parent.begun.Add(1) // the parent's copy goes in (hidden) with this install
			if parent.evicting.Load() != 0 {
				parent.done.Add(1)
				f.abandon(d)
				return false
			}
		}
		if f.putNew(put, i, w0, w1, mine0, mine1) { // deeper levels pass no put: install
			atomic.StoreUint64(&f.words[d+1], w1) // the source held w0/w1 until the hand-off: publish
			f.relocations.Add(1)
			return true
		}
		if parent != nil {
			parent.done.Add(1) // ...or doesn't
		}
		f.abandon(d) // a touch reached the source first, or the key was evicted
		return false
	}
	for s := 0; s < wordsPerLine; s += 2 {
		d0 := atomic.LoadUint64(&f.words[base+s])
		if d0 != 0 && age(now, d0&tsMask) <= f.horizon {
			continue
		}
		m.begun.Add(1)
		if m.evicting.Load() != 0 { // an eviction of this stripe is under way: it may take the source
			m.done.Add(1)
			return false
		}
		if f.install(base+s, d0, atomic.LoadUint64(&f.words[base+s+1]), moved, busy) {
			if testHookCopied != nil {
				testHookCopied()
			}
			return replace(base + s)
		}
		m.done.Add(1)
	}
	if depth > 0 {
		f.warmLine(a) // the next lines of a's entries together, not one miss at a time
		for s := 0; s < wordsPerLine; s += 2 {
			if f.relocate(base+s, a, moved, busy, now, depth-1, budget, m, nil) {
				return replace(base + s)
			}
		}
	}
	return false
}

// abandon clears the hidden copy at word index d of a move whose source
// changed before the hand-off, word0 first (a slot is never a fingerprint
// with an empty register). Nobody else writes a busy word1.
func (f *Filter) abandon(d int) {
	atomic.StoreUint64(&f.words[d], 0)
	atomic.StoreUint64(&f.words[d+1], 0)
}

// ContinuousDecay reclaims expired slots at a rate set by the clock: one full
// pass of the table per Horizon of elapsed ticks, spread across callers in
// steps of at most a few cache lines. It never blocks and never pauses
// readers. Expiry correctness does not depend on it (ages are evaluated
// arithmetically on every access); it only frees slots and keeps occupancy
// honest. touch calls it inline on 1/64 of operations; calling it from a
// ticker as well is optional. Returns lines swept.
func (f *Filter) ContinuousDecay() int { return f.decayStep(f.clock(), 4) }

func (f *Filter) decayStep(now uint64, budget uint64) int {
	hi, lo := bits.Mul64(now, f.lines)
	if hi >= f.horizon {
		return 0
	}
	target, _ := bits.Div64(hi, lo, f.horizon)
	c := f.cursor.Load()
	if c >= target {
		return 0
	}
	start := c
	if target-start > f.lines { // fell behind by over a lap: skip ahead, lazy expiry covered it
		start = target - f.lines
	}
	n := min(target-start, budget)
	if !f.cursor.CompareAndSwap(c, start+n) {
		return 0 // another caller took this step
	}
	for l := start; l < start+n; l++ {
		f.sweepLine(l%f.lines, now)
	}
	return int(n)
}

func (f *Filter) sweepLine(line, now uint64) {
	base := int(line * wordsPerLine)
	for s := 0; s < wordsPerLine; s += 2 {
		w0 := atomic.LoadUint64(&f.words[base+s])
		if w0 == 0 || age(now, w0&tsMask) <= f.horizon {
			continue
		}
		w1 := atomic.LoadUint64(&f.words[base+s+1])
		if w1>>tagShift == tagBusy {
			continue // being handed over
		}
		if atomic.CompareAndSwapUint64(&f.words[base+s], w0, 0) {
			atomic.CompareAndSwapUint64(&f.words[base+s+1], w1, 0)
		}
	}
}

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

// primary is h's home line. It uses hash bits 0..35, disjoint from the
// fingerprint (48..63) and tag (36..47), so keys sharing a line share no
// identity bits. Multiply-shift lets the table be any size.
func (f *Filter) primary(h uint64) uint64 {
	hi, _ := bits.Mul64(h<<28, f.lines) // bits 0..35 moved to the top
	return hi
}

// alt is the other candidate line for fp stored in line. It depends only on
// (line, fp), as in cuckoo filters, so a stored entry can be relocated
// without knowing its key: alt(l) = (x - l) mod lines is its own inverse.
// For one or two lines per fp, alt(l) == l; those keys get one line.
func (f *Filter) alt(line, fp uint64) uint64 {
	x, _ := bits.Mul64(mix64(fp*0x9e3779b97f4a7c15), f.lines)
	if x >= line {
		return x - line
	}
	return x + f.lines - line
}

func fingerprint(h uint64) uint64 {
	if fp := h >> 48; fp != 0 {
		return fp
	}
	return 1
}

// tag is 12 more fingerprint bits, kept in word1: with the 16-bit fp the
// slot identity is 28 bits. tagBusy is reserved; keys that would have it
// share the tag below.
func tag(h uint64) uint64 {
	t := h >> 36 & (1<<tagBits - 1)
	return t ^ (t+1)>>tagBits
}

// age is now-ts, or 0 if ts is ahead (a caller with a slightly stale clock).
func age(now, ts uint64) uint64 {
	if now < ts {
		return 0
	}
	return now - ts
}

func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}

// Eviction stash: a 4-way set-associative array of evicted entries, keyed
// by the entry's full identity (primary line, fp, tag: the same bits the
// table compares, so the stash adds no false positives beyond the table's
// own). Evictions are rare and slow already, so it uses plain striped locks.

const (
	stashStripes = 64
	stashWays    = 4
)

type stashEntry struct {
	id, w0, w1 uint64 // id 0 = empty; w0's displaced bit is clear
}

// stashID packs a key's identity: primary line (< 2^36) | fp | tag.
func (f *Filter) stashID(h, fp, tg uint64) uint64 {
	return f.primary(h)<<28 | fp<<tagBits | tg
}

// slotID is stashID for the entry in a slot of line, without its key.
func (f *Filter) slotID(line, w0, w1 uint64) uint64 {
	fp := w0 >> 48
	if w0&dispBit != 0 {
		line = f.alt(line, fp)
	}
	return line<<28 | fp<<tagBits | w1>>tagShift
}

// stashSet is id's set (none before the stash is allocated) and its lock.
func (f *Filter) stashSet(id uint64) ([]stashEntry, *paddedMutex) {
	i, _ := bits.Mul64(mix64(id), f.stashN)
	mu := &f.stashMu[i%stashStripes]
	p := f.stash.Load()
	if p == nil {
		return nil, mu
	}
	return p.e[i*stashWays : (i+1)*stashWays : (i+1)*stashWays], mu
}

// stashTable: sets of stashWays entries, and a bit per set that holds a key
// waiting for a slot, set and cleared under the set's lock. The eviction
// stash is one (its waiting keys are ConstantTime's); the wait room is
// another (Background's, outside ConstantTime).
type stashTable struct {
	e    []stashEntry
	wait []atomic.Uint64
	n    uint64                     // sets
	mu   *[stashStripes]paddedMutex // the filter's, which exist before the table
	at   atomic.Uint64              // the next set to place waiting keys from
}

func newStashTable(sets uint64, mu *[stashStripes]paddedMutex) *stashTable {
	e := make([]stashEntry, sets*stashWays)
	prefault(unsafe.Slice((*uint64)(unsafe.Pointer(&e[0])), len(e)*3))
	return &stashTable{e: e, wait: make([]atomic.Uint64, (sets+63)/64), n: sets, mu: mu}
}

// set returns id's set and its lock.
func (p *stashTable) set(id uint64) ([]stashEntry, *paddedMutex) {
	i, _ := bits.Mul64(mix64(id), p.n)
	return p.e[i*stashWays : (i+1)*stashWays : (i+1)*stashWays], &p.mu[i%stashStripes]
}

// waiting: id's set may hold a key waiting for a slot (its bit is set from
// the first such key's write until the placer finds the set without one).
func (p *stashTable) waiting(id uint64) bool {
	i, _ := bits.Mul64(mix64(id), p.n)
	return p.wait[i/64].Load()&(1<<(i%64)) != 0
}

// allocStash allocates the stash on the first eviction, so a filter that
// never evicts doesn't pay for it.
func (f *Filter) allocStash() {
	if f.stash.Load() != nil {
		return
	}
	f.skMu.Lock()
	defer f.skMu.Unlock()
	if f.stash.Load() == nil {
		f.stash.Store(newStashTable(f.stashN, &f.stashMu))
	}
}

// waitTable is where keys wait for a slot, allocated on first use (nil:
// nowhere). ConstantTime: the stash, whose set every call reads anyway.
// Background: the wait room, a small table of its own (one entry per 128
// slots), so a filter that never evicts holds no stash.
func (f *Filter) waitTable() *stashTable {
	if f.ct {
		if f.stashN == 0 {
			return nil
		}
		f.allocStash()
		return f.stash.Load()
	}
	if p := f.room.Load(); p != nil {
		return p
	}
	f.skMu.Lock()
	defer f.skMu.Unlock()
	if f.room.Load() == nil {
		f.room.Store(newStashTable(f.roomN, &f.roomMu))
	}
	return f.room.Load()
}

// stashPut keeps an evicted entry in its set: in its own old place, a free
// or expired way, or else in place of the way with the fewest events in the
// horizon if that has fewer than it. Whichever history is dropped, Lost
// counts it. Reports whether the entry was kept.
func (f *Filter) stashPut(id, w0, w1, now uint64) { f.stashPutAs(id, w0, w1, now, false) }

// deferMark, the displaced bit of a stash entry's word0 (otherwise clear),
// marks a key that waits for a slot (ConstantTime) rather than one evicted.
const deferMark = dispBit

// stashPutAs is stashPut; wait marks the entry as waiting for a slot.
func (f *Filter) stashPutAs(id, w0, w1, now uint64, wait bool) {
	if f.stashN == 0 {
		f.spill(id, w0, w1, now)
		return
	}
	w0 &^= dispBit
	if wait {
		w0 |= deferMark
	}
	f.allocStash()
	set, mu := f.stashSet(id)
	mu.Lock()
	way, least := -1, uint64(1<<64-1)
	for i := range set {
		e := &set[i]
		if e.id == id {
			// An evictor holding an older snapshot of the same key (it was
			// evicted, restored and evicted again meanwhile) must not
			// overwrite the newer one.
			if e.w0&tsMask > w0&tsMask || e.w0&tsMask == w0&tsMask && f.mass(e.id, e.w1, e.w0&tsMask, now) >= f.mass(id, w1, w0&tsMask, now) {
				mu.Unlock()
				return
			}
			way, least = i, 0
			break
		}
		if e.id == 0 || age(now, e.w0&tsMask) > f.horizon {
			way, least = i, 0
			break
		}
		if m := f.mass(e.id, e.w1, e.w0&tsMask, now); m < least {
			way, least = i, m
		}
	}
	var out stashEntry // the history that leaves the stash, if any
	if least > 0 {
		if least > f.mass(id, w1, w0&tsMask, now) {
			f.spill(id, w0, w1, now)
			mu.Unlock()
			return
		}
		out = set[way]
	}
	if out.id != 0 {
		f.spill(out.id, out.w0&^deferMark, out.w1, now)
	}
	f.stashWrite(f.stash.Load(), &set[way], stashEntry{id, w0, w1})
	for until := (w0 & tsMask) + f.horizon; !wait; { // a waiting key: see stashWait
		u := f.stashUntil.Load()
		if u >= until || f.stashUntil.CompareAndSwap(u, until) {
			break
		}
	}
	mu.Unlock()
}

// stashWait puts a key that waits for a slot in a free or expired way of its
// set in the wait table (waitTable), or in its own old place, and reports
// whether it did: a full set drops nothing for it. No stashUntil:
// ConstantTime reads the stash anyway, and Background lookups read the
// set's wait bit (stashMay).
func (f *Filter) stashWait(id, w0, w1, now uint64) bool {
	p := f.waitTable()
	if p == nil {
		return false
	}
	set, mu := p.set(id)
	mu.Lock()
	defer mu.Unlock()
	for i := range set {
		if e := &set[i]; e.id == 0 || e.id == id || age(now, e.w0&tsMask) > f.horizon {
			f.stashWrite(p, e, stashEntry{id, w0&^dispBit | deferMark, w1})
			return true
		}
	}
	return false
}

// putBack returns a history recalled for an insert that lost a race for
// its slot, for the retry to find: a key that waited back to wait, an
// evicted one to the stash.
func (f *Filter) putBack(id, w0, w1, now uint64) {
	if w0&deferMark != 0 && f.stashWait(id, w0, w1, now) {
		return
	}
	f.stashPut(id, w0, w1, now)
}

// stashWrite replaces entry e of table p (its set's lock held), keeping the
// count of entries that wait for a slot.
func (f *Filter) stashWrite(p *stashTable, e *stashEntry, ne stashEntry) {
	if e.id != 0 && e.w0&deferMark != 0 {
		f.deferred.Add(-1)
	}
	if ne.id != 0 && ne.w0&deferMark != 0 {
		f.deferred.Add(1)
		set := uint64((uintptr(unsafe.Pointer(e)) - uintptr(unsafe.Pointer(&p.e[0]))) / unsafe.Sizeof(stashEntry{}) / stashWays)
		if w := &p.wait[set/64]; w.Load()&(1<<(set%64)) == 0 {
			w.Or(1 << (set % 64))
		}
	}
	*e = ne
}

// stashGet returns the live stash entry for id; take also removes it.
func (f *Filter) stashGet(id, now uint64, take bool) (w0, w1 uint64, ok bool) {
	return f.tableGet(f.stash.Load(), id, now, take)
}

// tableGet is stashGet in table p (nil: not allocated, so empty).
func (f *Filter) tableGet(p *stashTable, id, now uint64, take bool) (w0, w1 uint64, ok bool) {
	if p == nil {
		return 0, 0, false
	}
	set, mu := p.set(id)
	mu.Lock()
	defer mu.Unlock()
	for i := range set {
		e := &set[i]
		if e.id != id {
			continue
		}
		if age(now, e.w0&tsMask) > f.horizon {
			return 0, 0, false
		}
		w0, w1 = e.w0, e.w1
		if take {
			f.stashWrite(p, e, stashEntry{})
		}
		return w0, w1, true
	}
	return 0, 0, false
}

// dropped records that a live key's history was lost at now.
func (f *Filter) dropped(now uint64) {
	f.lost.Add(1)
	for {
		la := f.lostAt.Load()
		if la >= now || f.lostAt.CompareAndSwap(la, now) {
			return
		}
	}
}
