// Package temporalfilter implements a Sliding-Window Probabilistic Register:
// a fixed-size, lock-free filter that answers "have we seen key X in the last
// W?" and "how many times in the last W?" for any W up to a configured horizon.
//
// Memory layout: the table is an array of 64-byte cache lines. Each line holds
// four 2-word slots:
//
//	word0: fp(16) | lastTouch(48)          // freshness, CheckAndTouch
//	word1: tag(12) | epoch(4) | b0..b6 (7 buckets, 48 bits) // velocity register
//
// The velocity register keeps 7 buckets whose widths grow with age (1, 1,
// 1-2, 2-4, ... units of Horizon/32): fine resolution near now, coarse in the
// past (register.go). Advancing time re-buckets arithmetically from the clock,
// so there is no background sweep on the correctness path. A key's home is
// its primary line; relocation moves entries to their alternate line to make
// room.
package temporalfilter

import (
	"errors"
	"hash/maphash"
	"math"
	"math/bits"
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
	dispBit   = 1 << 47
	tsMask    = dispBit - 1
	tagBits   = 12
	tagShift  = 64 - tagBits
	epochMask = 0xf
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
	// Resolution is the clock tick. Default 1µs (48-bit timestamps last ~8.9y).
	Resolution time.Duration
	// Load is the target slot occupancy when Capacity keys are live.
	// Default 0.85; cuckoo relocation keeps misses at zero up to ~0.95.
	// Lower values trade memory for headroom above Capacity.
	Load float64
	// Clock, if set, returns the current tick. It must be monotonic.
	// Default: monotonic time since New, in Resolution units.
	Clock func() uint64
	// CoarseClock, if > 0 and Clock is nil, makes the built-in clock a cached
	// value refreshed every CoarseClock by one background goroutine: reading
	// it is an atomic load (~1ns) instead of a monotonic clock read (~25-40ns),
	// and timestamps are accurate to CoarseClock. Call Close to stop it.
	CoarseClock time.Duration
}

// Filter is safe for concurrent use without locks.
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
}

type paddedMutex struct {
	sync.Mutex
	_ [56]byte
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

// Hash hashes a string key for the *At methods.
func (f *Filter) Hash(key string) uint64 { return maphash.String(f.seed, key) }

// HashUint64 hashes an integer key (IP, card BIN, user id) for the *At methods.
func HashUint64(k uint64) uint64 { return mix64(k) }

// CheckAndTouch reports whether key was seen within window, then records an
// occurrence now.
func (f *Filter) CheckAndTouch(key string, window time.Duration) bool {
	seen, _ := f.touch(f.Hash(key), f.Ticks(window), f.clock(), false)
	return seen
}

// VelocityCount estimates how many times key was touched within window.
// It does not record an occurrence.
func (f *Filter) VelocityCount(key string, window time.Duration) int {
	return f.VelocityCountAt(f.Hash(key), f.Ticks(window), f.clock())
}

// TouchAndCount records an occurrence and returns (seen within window before
// this touch, estimated count within window including this touch).
func (f *Filter) TouchAndCount(key string, window time.Duration) (bool, int) {
	return f.touch(f.Hash(key), f.Ticks(window), f.clock(), true)
}

// CheckAndTouchAt is CheckAndTouch on a pre-hashed key with caller-supplied
// window and clock, both in ticks.
func (f *Filter) CheckAndTouchAt(h, window, now uint64) bool {
	seen, _ := f.touch(h, window, now, false)
	return seen
}

// TouchAndCountAt is TouchAndCount on a pre-hashed key, in ticks.
func (f *Filter) TouchAndCountAt(h, window, now uint64) (bool, int) {
	return f.touch(h, window, now, true)
}

// VelocityCountAt is VelocityCount on a pre-hashed key, in ticks.
func (f *Filter) VelocityCountAt(h, window, now uint64) int {
	fp, tg := fingerprint(h), tag(h)
	i := f.locate(h, fp, tg)
	if i < 0 {
		return 0
	}
	w0 := atomic.LoadUint64(&f.words[i])
	if w0>>48 != fp || age(now, w0&tsMask) > f.horizon {
		return 0
	}
	w1 := atomic.LoadUint64(&f.words[i+1])
	if w1>>tagShift != tg {
		return 0
	}
	return f.estimate(w1, w0&tsMask, now, window)
}

// ContainsAt reports whether h was touched within window, without touching it.
func (f *Filter) ContainsAt(h, window, now uint64) bool {
	fp, tg := fingerprint(h), tag(h)
	i := f.locate(h, fp, tg)
	if i < 0 {
		return false
	}
	w0 := atomic.LoadUint64(&f.words[i])
	return w0>>48 == fp && age(now, w0&tsMask) <= min(window, f.horizon)
}

// CheckAndTouchBatch is CheckAndTouchAt over many pre-hashed keys. It first
// issues one independent load per key's primary line so the CPU overlaps the
// cache misses (memory-level parallelism), then runs each operation against
// lines that are already in flight or cached. out must be at least len(hs).
func (f *Filter) CheckAndTouchBatch(hs []uint64, window, now uint64, out []bool) {
	out = out[:len(hs)]
	for lo := 0; lo < len(hs); lo += batchChunk {
		hi := min(lo+batchChunk, len(hs))
		f.warm(hs[lo:hi])
		for i := lo; i < hi; i++ {
			out[i], _ = f.touch(hs[i], window, now, false)
		}
	}
}

// TouchAndCountBatch is TouchAndCountAt over many pre-hashed keys, with the
// overlapped loads of CheckAndTouchBatch. seen and counts must be at least
// len(hs).
func (f *Filter) TouchAndCountBatch(hs []uint64, window, now uint64, seen []bool, counts []int) {
	seen, counts = seen[:len(hs)], counts[:len(hs)]
	for lo := 0; lo < len(hs); lo += batchChunk {
		hi := min(lo+batchChunk, len(hs))
		f.warm(hs[lo:hi])
		for i := lo; i < hi; i++ {
			seen[i], counts[i] = f.touch(hs[i], window, now, true)
		}
	}
}

// VelocityCountBatch is VelocityCountAt over many pre-hashed keys, with the
// overlapped loads of CheckAndTouchBatch. counts must be at least len(hs).
func (f *Filter) VelocityCountBatch(hs []uint64, window, now uint64, counts []int) {
	counts = counts[:len(hs)]
	for lo := 0; lo < len(hs); lo += batchChunk {
		hi := min(lo+batchChunk, len(hs))
		f.warm(hs[lo:hi])
		for i := lo; i < hi; i++ {
			counts[i] = f.VelocityCountAt(hs[i], window, now)
		}
	}
}

// batchChunk is ~ a core's outstanding-miss capacity, with headroom.
const batchChunk = 64

// warm loads each key's primary line without using the result, so the misses
// for a whole chunk are in flight at once.
func (f *Filter) warm(hs []uint64) {
	var sink uint64
	for _, h := range hs {
		sink += atomic.LoadUint64(&f.words[f.primary(h)*wordsPerLine])
	}
	runtime.KeepAlive(sink)
}

// Evictions counts live keys displaced because both candidate lines were full.
func (f *Filter) Evictions() uint64 { return f.evictions.Load() }

// Relocations counts entries moved to their alternate line to make room.
func (f *Filter) Relocations() uint64 { return f.relocations.Load() }

func (f *Filter) touch(h, window, now uint64, wantCount bool) (bool, int) {
	if h&decayEvery == 0 {
		f.decayStep(now, 1)
	}
	fp, tg := fingerprint(h), tag(h)
	for {
		i := f.locate(h, fp, tg)
		if i < 0 {
			// Miss: serialize first touches of the same key so two racers
			// can't each claim a slot (which would split its count). The
			// lock covers only this path; hits never take it.
			mu := &f.claimMu[h>>(64-claimStripeBits)]
			mu.Lock()
			if i = f.locate(h, fp, tg); i < 0 {
				w1 := f.nextReg(0, tg, now, now, true) // first event, published with the key
				i = f.claim(h, fp, w1, now)
				mu.Unlock()
				if i < 0 {
					continue
				}
				if !wantCount {
					return false, 0
				}
				return false, f.estimate(w1, now, now, window)
			}
			mu.Unlock() // another first touch got there first: count as a hit
		}
		w0 := atomic.LoadUint64(&f.words[i])
		if w0>>48 != fp {
			continue // slot was reclaimed between find and load; retry
		}
		last := w0 & tsMask
		stale := age(now, last) > f.horizon
		if now > last && !atomic.CompareAndSwapUint64(&f.words[i], w0, w0&^tsMask|now) {
			continue
		}
		w1, ok := f.bump(i+1, tg, last, now, stale)
		if !ok {
			continue // the slot was handed to another key after our CAS; look again
		}
		seen := !stale && age(now, last) <= window
		if !wantCount {
			return seen, 0
		}
		return seen, f.estimate(w1, max(now, last), now, window)
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
// stripe.
func (f *Filter) claim(h, fp, w1, now uint64) int {
	lines := [2]uint64{f.primary(h), f.alt(f.primary(h), fp)}
	mine := [2]uint64{fp<<48 | now, fp<<48 | dispBit | now}
	for {
		for li, line := range lines {
			base := int(line * wordsPerLine)
			for s := 0; s < wordsPerLine; s += 2 {
				w0 := atomic.LoadUint64(&f.words[base+s])
				if w0 == 0 || age(now, w0&tsMask) > f.horizon {
					if f.install(base+s, w0, atomic.LoadUint64(&f.words[base+s+1]), mine[li], w1) {
						return base + s
					}
					return -1
				}
			}
		}
		// Iterative deepening: all short paths before any long one. The deep
		// budget shrinks as recent searches fail, so an overfull table falls
		// back to cheap eviction instead of searching in vain.
		p := f.pressure.Load()
		budget := 8 + int(uint64(relocBudget-8)*(pressureOne-uint64(p))>>pressureShift)
		for depth := 0; depth <= relocDepth; depth++ {
			for li, line := range lines {
				base := int(line * wordsPerLine)
				for s := 0; s < wordsPerLine; s += 2 {
					if f.relocate(base+s, line, mine[li], w1, now, depth, &budget) {
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
				score := f.mass(vw1, ts, now)<<48 | ts
				if score < victimScore {
					victim, vli, victimScore, victimW0, victimW1 = base+s, li, score, w0, vw1
				}
			}
		}
		if f.install(victim, victimW0, victimW1, mine[vli], w1) {
			f.evictions.Add(1)
			return victim
		}
		return -1
	}
}

// install hands slot i (word index), if it still holds w0/w1, to the entry
// mine0/mine1. word1 goes first: a reader of the old occupant then sees a
// foreign tag and misses rather than matching a half-written slot, and a
// late writer to the old register (bump) sees the tag change and backs off.
func (f *Filter) install(i int, w0, w1, mine0, mine1 uint64) bool {
	if !atomic.CompareAndSwapUint64(&f.words[i+1], w1, mine1) {
		return false
	}
	if atomic.CompareAndSwapUint64(&f.words[i], w0, mine0) {
		return true
	}
	atomic.CompareAndSwapUint64(&f.words[i+1], mine1, w1) // lost the slot; put word1 back
	return false
}

// relocate moves the entry at word index i (in line) to its alternate line
// and installs mine0/mine1 in its place. If that line is full it first
// relocates one of its entries, up to depth more steps (a bounded cuckoo
// path). Copy-then-replace keeps every moved key findable throughout; if the
// source changes mid-move (a touch updated it) the copy is withdrawn.
func (f *Filter) relocate(i int, line, mine0, mine1, now uint64, depth int, budget *int) bool {
	w0 := atomic.LoadUint64(&f.words[i])
	if w0 == 0 {
		return false
	}
	if *budget <= 0 {
		return false
	}
	*budget--
	w1 := atomic.LoadUint64(&f.words[i+1])
	a := f.alt(line, w0>>48)
	base := int(a * wordsPerLine)
	moved := w0 ^ dispBit         // displaced iff it wasn't
	replace := func(d int) bool { // the copy is at d; now take the source
		if f.install(i, w0, w1, mine0, mine1) {
			f.relocations.Add(1)
			return true
		}
		atomic.CompareAndSwapUint64(&f.words[d], moved, 0) // source moved on; withdraw copy
		return false
	}
	for s := 0; s < wordsPerLine; s += 2 {
		d0 := atomic.LoadUint64(&f.words[base+s])
		if d0 != 0 && age(now, d0&tsMask) <= f.horizon {
			continue
		}
		if f.install(base+s, d0, atomic.LoadUint64(&f.words[base+s+1]), moved, w1) {
			return replace(base + s)
		}
	}
	if depth > 0 {
		for s := 0; s < wordsPerLine; s += 2 {
			if f.relocate(base+s, a, moved, w1, now, depth-1, budget) {
				return replace(base + s)
			}
		}
	}
	return false
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
// slot identity is 28 bits.
func tag(h uint64) uint64 { return h >> 36 & (1<<tagBits - 1) }

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
