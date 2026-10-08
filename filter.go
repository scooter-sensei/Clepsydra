// Package temporalfilter implements a Sliding-Window Probabilistic Register:
// a fixed-size, lock-free filter that answers "have we seen key X in the last
// W?" and "how many times in the last W?" for any W up to a configured horizon.
//
// Memory layout: the table is an array of 64-byte cache lines. Each line holds
// four 2-word slots:
//
//	word0: fp(16) | lastTouch(48)          // freshness, CheckAndTouch
//	word1: tag(12) | epoch(4) | c0..c5(6x8) // velocity shift register
//
// c0 counts the newest sub-window, c5 the oldest. Advancing time by d
// sub-windows is a left shift by 8*d bits, so decay is pure arithmetic on the
// clock: there is no background sweep on the correctness path. A key's home is
// its primary line; it overflows to a second line only when the primary holds
// four live keys.
package temporalfilter

import (
	"errors"
	"hash/maphash"
	"math"
	"math/bits"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	slotsPerLine = 8
	wordsPerLine = 2 * slotsPerLine
	subWindows   = 6
	tsMask       = 1<<48 - 1
	ctrMask      = 1<<48 - 1
	ctrMax       = 0xff
	tagBits      = 12
	tagShift     = 64 - tagBits
	epochMask    = 0xf
	// decayEvery gates the inline decay step to 1/64 of operations (by key
	// hash) so the shared cursor's cache line is not touched on every call.
	decayEvery = 63
	// relocDepth bounds the cuckoo path an insert into two full lines may
	// take before evicting.
	relocDepth = 4
	// relocBudget caps the alternate lines one insert may scan. The usable
	// part above the first 8 shrinks with pressure (see claim).
	relocBudget   = 1000
	pressureShift = 16
	pressureOne   = 1 << pressureShift
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
}

// Filter is safe for concurrent use without locks.
type Filter struct {
	words   []uint64 // 64-byte aligned, len = lines*wordsPerLine
	lines   uint64   // any size; indexed by multiply-shift, not masking
	horizon uint64   // ticks
	gran    uint64   // ticks per velocity sub-window
	res     time.Duration
	clock   func() uint64
	seed    maphash.Seed

	_           [64]byte
	cursor      atomic.Uint64 // ContinuousDecay progress, in lines
	_           [56]byte
	evictions   atomic.Uint64
	relocations atomic.Uint64
	_           [48]byte
	pressure    atomic.Uint32 // EWMA of relocation-search failure, 0..pressureOne
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
	if horizon < subWindows-1 {
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
		gran:    (horizon + subWindows - 2) / (subWindows - 1), // ceil(H/5): 5 full + 1 partial >= H
		res:     cfg.Resolution,
		clock:   cfg.Clock,
		seed:    maphash.MakeSeed(),
	}
	f.words = alignedWords(int(lines * wordsPerLine))
	if f.clock == nil {
		base, res := time.Now(), cfg.Resolution
		f.clock = func() uint64 { return uint64(time.Since(base)/res) + 1 }
	}
	return f, nil
}

func alignedWords(n int) []uint64 {
	buf := make([]uint64, n+wordsPerLine)
	const align = wordsPerLine * 8 // bytes per bucket
	off := int((align-uintptr(unsafe.Pointer(&buf[0]))%align)%align) / 8
	return buf[off : off+n : off+n]
}

// Now returns the filter's current tick.
func (f *Filter) Now() uint64 { return f.clock() }

// Ticks converts a duration to filter ticks.
func (f *Filter) Ticks(d time.Duration) uint64 { return uint64(d / f.res) }

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
	i := f.find(f.primary(h), fp, tg)
	if i < 0 {
		if i = f.find(f.alt(f.primary(h), fp), fp, tg); i < 0 {
			return 0
		}
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
	i := f.find(f.primary(h), fp, tg)
	if i < 0 {
		if i = f.find(f.alt(f.primary(h), fp), fp, tg); i < 0 {
			return false
		}
	}
	w0 := atomic.LoadUint64(&f.words[i])
	return w0>>48 == fp && age(now, w0&tsMask) <= min(window, f.horizon)
}

// CheckAndTouchBatch is CheckAndTouchAt over many pre-hashed keys. It first
// issues one independent load per key's primary line so the CPU overlaps the
// cache misses (memory-level parallelism), then runs each operation against
// lines that are already in flight or cached. out must be at least len(hs).
func (f *Filter) CheckAndTouchBatch(hs []uint64, window, now uint64, out []bool) {
	_ = out[len(hs)-1]
	const chunk = 64 // ~ the core's outstanding-miss capacity, with headroom
	for lo := 0; lo < len(hs); lo += chunk {
		hi := min(lo+chunk, len(hs))
		var sink uint64
		for _, h := range hs[lo:hi] {
			b := f.primary(h) * wordsPerLine
			for l := uint64(0); l < wordsPerLine; l += 8 { // each 64-byte line of the bucket
				sink += atomic.LoadUint64(&f.words[b+l])
			}
		}
		runtime.KeepAlive(sink) // keep the prefetch loads
		for i := lo; i < hi; i++ {
			out[i], _ = f.touch(hs[i], window, now, false)
		}
	}
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
		i := f.find(f.primary(h), fp, tg)
		if i < 0 {
			i = f.find(f.alt(f.primary(h), fp), fp, tg)
		}
		if i < 0 {
			i = f.claim(h, fp, now)
			w1 := f.bump(i+1, tg, now, true)
			if !wantCount {
				return false, 0
			}
			return false, f.estimate(w1, now, now, window)
		}
		w0 := atomic.LoadUint64(&f.words[i])
		if w0>>48 != fp {
			continue // slot was reclaimed between find and load; retry
		}
		last := w0 & tsMask
		if now > last && !atomic.CompareAndSwapUint64(&f.words[i], w0, fp<<48|now) {
			continue
		}
		stale := age(now, last) > f.horizon
		w1 := f.bump(i+1, tg, now, stale)
		seen := !stale && age(now, last) <= window
		if !wantCount {
			return seen, 0
		}
		return seen, f.estimate(w1, max(now, last), now, window)
	}
}

// find returns the word index of the slot matching fp and tag in line, or -1.
// The tag lives in word1 of the same cache line, so identity is effectively a
// 24-bit fingerprint at no extra memory traffic.
func (f *Filter) find(line, fp, tg uint64) int {
	base := int(line * wordsPerLine)
	w := f.words[base : base+wordsPerLine : base+wordsPerLine]
	for s := 0; s < wordsPerLine; s += 2 {
		if atomic.LoadUint64(&w[s])>>48 == fp && atomic.LoadUint64(&w[s+1])>>tagShift == tg {
			return base + s
		}
	}
	return -1
}

// claim installs fp in an empty or expired slot of the primary line, then the
// alternate line. If all eight are live it tries to relocate one of them to
// its own alternate line (one cuckoo step). Only then does it evict, taking
// the slot with the fewest events in the horizon (oldest on ties), so a spray
// of fresh keys cannot flush a repeat offender. Returns word0 index.
func (f *Filter) claim(h, fp, now uint64) int {
	lines := [2]uint64{f.primary(h), f.alt(f.primary(h), fp)}
	for {
		victim, victimScore, victimW0 := -1, uint64(1<<64-1), uint64(0)
		for _, line := range lines {
			base := int(line * wordsPerLine)
			for s := 0; s < wordsPerLine; s += 2 {
				w0 := atomic.LoadUint64(&f.words[base+s])
				ts := w0 & tsMask
				if w0 == 0 || age(now, ts) > f.horizon {
					if atomic.CompareAndSwapUint64(&f.words[base+s], w0, fp<<48|now) {
						return base + s
					}
					continue
				}
				score := f.mass(atomic.LoadUint64(&f.words[base+s+1]), now)<<48 | ts
				if score < victimScore {
					victim, victimScore, victimW0 = base+s, score, w0
				}
			}
		}
		if victim < 0 {
			continue
		}
		// Iterative deepening: all short paths before any long one. The deep
		// budget shrinks as recent searches fail, so an overfull table falls
		// back to cheap eviction instead of searching in vain.
		p := f.pressure.Load()
		budget := 8 + int(uint64(relocBudget-8)*(pressureOne-uint64(p))>>pressureShift)
		for depth := 0; depth <= relocDepth; depth++ {
			for _, line := range lines {
				base := int(line * wordsPerLine)
				for s := 0; s < wordsPerLine; s += 2 {
					if f.relocate(base+s, line, fp<<48|now, now, depth, &budget) {
						f.pressure.Store(p - p>>3) // EWMA toward "searches succeed"
						return base + s
					}
				}
			}
		}
		f.pressure.Store(p - p>>3 + pressureOne>>3) // EWMA toward "searches fail"
		if atomic.CompareAndSwapUint64(&f.words[victim], victimW0, fp<<48|now) {
			f.evictions.Add(1)
			return victim
		}
	}
}

// relocate moves the entry at word index i (in line) to its alternate line
// and installs mine in its place. If that line is full it first relocates one
// of its entries, up to depth more steps (a bounded cuckoo path).
// Copy-then-replace keeps every moved key findable throughout; if the source
// changes mid-move the copy is withdrawn.
func (f *Filter) relocate(i int, line, mine, now uint64, depth int, budget *int) bool {
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
	place := func(d int) bool { // d's word0 already holds w0
		if old := atomic.LoadUint64(&f.words[d+1]); !atomic.CompareAndSwapUint64(&f.words[d+1], old, w1) {
			atomic.CompareAndSwapUint64(&f.words[d], w0, 0)
			return false
		}
		if atomic.CompareAndSwapUint64(&f.words[i], w0, mine) {
			f.relocations.Add(1)
			return true
		}
		atomic.CompareAndSwapUint64(&f.words[d], w0, 0) // source moved on; withdraw copy
		return false
	}
	for s := 0; s < wordsPerLine; s += 2 {
		d0 := atomic.LoadUint64(&f.words[base+s])
		if d0 != 0 && age(now, d0&tsMask) <= f.horizon {
			continue
		}
		if atomic.CompareAndSwapUint64(&f.words[base+s], d0, w0) {
			return place(base + s)
		}
	}
	if depth > 0 {
		for s := 0; s < wordsPerLine; s += 2 {
			if f.relocate(base+s, a, w0, now, depth-1, budget) {
				return place(base + s)
			}
		}
	}
	return false
}

// lag is how many sub-windows the register in w1 trails now. The stored
// epoch is only 4 bits: callers only consult registers whose lastTouch is
// within the horizon (< subWindows sub-windows), so mod 16 is unambiguous,
// and lags of 8..15 mean the caller's clock is slightly behind (treated as 0).
// ok is false once every counter has shifted out.
func (f *Filter) lag(w1, now uint64) (d uint64, ok bool) {
	d = (now/f.gran - w1>>48) & epochMask
	if d > epochMask/2 {
		return 0, true
	}
	return d, d < subWindows
}

// mass is the register's total event count still inside the horizon.
func (f *Filter) mass(w1, now uint64) uint64 {
	d, ok := f.lag(w1, now)
	if !ok {
		return 0
	}
	ctr := w1 & ctrMask << (8 * d) & ctrMask
	ctr = ctr&0x00ff00ff00ff + ctr>>8&0x00ff00ff00ff // pairwise byte sums
	return ctr&0xffff + ctr>>16&0xffff + ctr>>32&0xffff
}

// bump advances the velocity shift register to now and increments c0.
// reset discards counts left by a previous occupant or an expired run.
func (f *Filter) bump(i int, tg, now uint64, reset bool) uint64 {
	nowE := now / f.gran
	for {
		old := atomic.LoadUint64(&f.words[i])
		e := old >> 48 & epochMask
		ctr := old & ctrMask
		if reset || old>>tagShift != tg {
			ctr, e = 0, nowE
		} else if d := (nowE - e) & epochMask; d <= epochMask/2 { // else caller's clock is behind; count in c0
			if d >= subWindows {
				ctr = 0
			} else {
				ctr = ctr << (8 * d) & ctrMask
			}
			e = nowE
		}
		if ctr&ctrMax != ctrMax {
			ctr++
		}
		nw := tg<<tagShift | (e&epochMask)<<48 | ctr
		if atomic.CompareAndSwapUint64(&f.words[i], old, nw) {
			return nw
		}
		reset = false // a concurrent writer already re-tagged it; merge
	}
}

// estimate interpolates the count in (now-window, now] from the register.
// Two facts from word0 make it exact at the extremes: nothing happened after
// last, so a window ending before it holds zero and one reaching it holds at
// least one; and c0's events all lie in [start of current sub-window, last].
// Older sub-windows assume events are uniform inside the one partially covered.
func (f *Filter) estimate(w1, last, now, window uint64) int {
	if window > f.horizon {
		window = f.horizon
	}
	if age(now, last) > window {
		return 0
	}
	nowE := now / f.gran
	d, ok := f.lag(w1, now)
	if !ok {
		return 0
	}
	ctr := w1 & ctrMask << (8 * d) & ctrMask
	g := float64(f.gran)
	subStart := nowE * f.gran
	from := now - min(window, now) // window start
	est := float64(ctr & 0xff)
	if from > subStart && last > subStart {
		est *= min(1, float64(last-min(from, last))/float64(last-subStart))
	}
	r := float64(window)/g - float64(now-subStart)/g // sub-windows still to cover
	for k := 1; k < subWindows && r > 0; k++ {
		c := float64(ctr >> (8 * k) & 0xff)
		if r >= 1 {
			est += c
		} else {
			est += c * r
		}
		r--
	}
	return max(1, int(est+0.5))
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
