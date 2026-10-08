// Package temporalfilter implements a Sliding-Window Probabilistic Register:
// a fixed-size, lock-free filter that answers "have we seen key X in the last
// W?" and "how many times in the last W?" for any W up to a configured horizon.
//
// Memory layout: the table is an array of 64-byte cache lines. Each line holds
// four 2-word slots:
//
//	word0: fp(16) | lastTouch(48)          // freshness, CheckAndTouch
//	word1: tag(8) | epoch(8) | c0..c5(6x8) // velocity shift register
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
	"math/bits"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	slotsPerLine = 4
	wordsPerLine = 2 * slotsPerLine
	subWindows   = 6
	tsMask       = 1<<48 - 1
	ctrMask      = 1<<48 - 1
	ctrMax       = 0xff
	// decayEvery gates the inline decay step to 1/64 of operations (by key
	// hash) so the shared cursor's cache line is not touched on every call.
	decayEvery = 63
)

// Config sizes a Filter.
type Config struct {
	// Capacity is the expected number of distinct keys live within Horizon.
	Capacity int
	// Horizon is the largest window any query may use.
	Horizon time.Duration
	// Resolution is the clock tick. Default 1µs (48-bit timestamps last ~8.9y).
	Resolution time.Duration
	// Clock, if set, returns the current tick. It must be monotonic.
	// Default: monotonic time since New, in Resolution units.
	Clock func() uint64
}

// Filter is safe for concurrent use without locks.
type Filter struct {
	words   []uint64 // 64-byte aligned, len = lines*wordsPerLine
	mask    uint64   // lines-1
	lines   uint64
	horizon uint64 // ticks
	gran    uint64 // ticks per velocity sub-window
	res     time.Duration
	clock   func() uint64
	seed    maphash.Seed

	_         [64]byte
	cursor    atomic.Uint64 // ContinuousDecay progress, in lines
	_         [56]byte
	evictions atomic.Uint64
}

// New allocates a Filter sized for ~50% slot load at cfg.Capacity.
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
	lines := uint64(2)
	for lines*slotsPerLine < uint64(cfg.Capacity)*2 {
		lines <<= 1
	}
	f := &Filter{
		words:   alignedWords(int(lines * wordsPerLine)),
		mask:    lines - 1,
		lines:   lines,
		horizon: horizon,
		gran:    (horizon + subWindows - 2) / (subWindows - 1), // ceil(H/5): 5 full + 1 partial >= H
		res:     cfg.Resolution,
		clock:   cfg.Clock,
		seed:    maphash.MakeSeed(),
	}
	if f.clock == nil {
		base, res := time.Now(), cfg.Resolution
		f.clock = func() uint64 { return uint64(time.Since(base)/res) + 1 }
	}
	return f, nil
}

func alignedWords(n int) []uint64 {
	buf := make([]uint64, n+wordsPerLine)
	off := int((64-uintptr(unsafe.Pointer(&buf[0]))&63)&63) / 8
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
	i := f.find(h&f.mask, fp, tg)
	if i < 0 {
		if i = f.find(f.alt(h), fp, tg); i < 0 {
			return 0
		}
	}
	w0 := atomic.LoadUint64(&f.words[i])
	if w0>>48 != fp || age(now, w0&tsMask) > f.horizon {
		return 0
	}
	w1 := atomic.LoadUint64(&f.words[i+1])
	if uint8(w1>>56) != tg {
		return 0
	}
	return f.estimate(w1, w0&tsMask, now, window)
}

// ContainsAt reports whether h was touched within window, without touching it.
func (f *Filter) ContainsAt(h, window, now uint64) bool {
	fp, tg := fingerprint(h), tag(h)
	i := f.find(h&f.mask, fp, tg)
	if i < 0 {
		if i = f.find(f.alt(h), fp, tg); i < 0 {
			return false
		}
	}
	w0 := atomic.LoadUint64(&f.words[i])
	return w0>>48 == fp && age(now, w0&tsMask) <= min(window, f.horizon)
}

// Evictions counts live keys displaced because both candidate lines were full.
func (f *Filter) Evictions() uint64 { return f.evictions.Load() }

func (f *Filter) touch(h, window, now uint64, wantCount bool) (bool, int) {
	if h&decayEvery == 0 {
		f.decayStep(now, 1)
	}
	fp, tg := fingerprint(h), tag(h)
	for {
		i := f.find(h&f.mask, fp, tg)
		if i < 0 {
			i = f.find(f.alt(h), fp, tg)
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
func (f *Filter) find(line, fp uint64, tg uint8) int {
	base := int(line * wordsPerLine)
	w := f.words[base : base+wordsPerLine : base+wordsPerLine]
	for s := 0; s < wordsPerLine; s += 2 {
		if atomic.LoadUint64(&w[s])>>48 == fp && uint8(atomic.LoadUint64(&w[s+1])>>56) == tg {
			return base + s
		}
	}
	return -1
}

// claim installs fp in an empty or expired slot of the primary line, then the
// alternate line, else evicts the stalest of the eight. Returns word0 index.
func (f *Filter) claim(h, fp, now uint64) int {
	for {
		victim, victimTs, victimW0 := -1, uint64(1<<64-1), uint64(0)
		for _, line := range [2]uint64{h & f.mask, f.alt(h)} {
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
				if ts < victimTs {
					victim, victimTs, victimW0 = base+s, ts, w0
				}
			}
		}
		if victim >= 0 && atomic.CompareAndSwapUint64(&f.words[victim], victimW0, fp<<48|now) {
			f.evictions.Add(1)
			return victim
		}
	}
}

// bump advances the velocity shift register to now and increments c0.
// reset discards counts left by a previous occupant or an expired run.
func (f *Filter) bump(i int, tg uint8, now uint64, reset bool) uint64 {
	nowE := now / f.gran
	for {
		old := atomic.LoadUint64(&f.words[i])
		e := uint64(uint8(old >> 48))
		ctr := old & ctrMask
		if reset || uint8(old>>56) != tg {
			ctr, e = 0, nowE
		} else if d := (nowE - e) & 0xff; d < 0x80 { // d >= 0x80: caller's clock is behind; count in c0
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
		nw := uint64(tg)<<56 | (e&0xff)<<48 | ctr
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
	d := (nowE - uint64(uint8(w1>>48))) & 0xff
	if d >= 0x80 {
		d = 0
	} else if d >= subWindows {
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
		f.sweepLine(l&f.mask, now)
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

func (f *Filter) alt(h uint64) uint64 {
	a := mix64(h^0x9e3779b97f4a7c15) & f.mask
	if a == h&f.mask {
		a ^= 1
	}
	return a
}

func fingerprint(h uint64) uint64 {
	if fp := h >> 48; fp != 0 {
		return fp
	}
	return 1
}

func tag(h uint64) uint8 { return uint8(h >> 40) }

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
