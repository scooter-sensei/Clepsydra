package temporalfilter

import (
	"math/bits"
	"sync/atomic"
)

// Velocity register: 7 counters packed into the low 48 bits of word1.
//
// In units of u = Horizon/unitsPerHorizon, with U the current unit index,
// bucket j covers [b_j, b_{j-1}) where b_{-1} = +inf, b_0 = U and, for j >= 1,
//
//	b_j = floor(U / s_j) * s_j - m_j     (s_j a power of two, nondecreasing)
//
// Every boundary is a multiple of its own s_j and moves only when U crosses
// one, so as U advances each old bucket lands whole inside one new bucket.
// Advancing any distance is therefore a single re-bucketing pass keyed by
// each old bucket's start: no per-tick work and no sweep. Widths grow with
// age (fine resolution near now) and then stay flat so long windows keep
// their resolution. Counter widths grow with bucket width.
const (
	nBuckets        = 7
	unitsPerHorizon = geomUnits
	regMask         = 1<<48 - 1
)

var bucketShift = func() (sh [nBuckets]uint) {
	for j := 1; j < nBuckets; j++ {
		sh[j] = sh[j-1] + bucketBits[j-1]
	}
	return sh
}()

// bucketBase maps a register bit to the shift of the bucket holding it.
var bucketBase = func() (b [48]uint8) {
	for j := range nBuckets {
		for k := bucketShift[j]; k < bucketShift[j]+bucketBits[j]; k++ {
			b[k] = uint8(bucketShift[j])
		}
	}
	return b
}()

type register [nBuckets]uint64

func unpack(w1 uint64) (r register) {
	for j := range r {
		r[j] = w1 >> bucketShift[j] & (1<<bucketBits[j] - 1)
	}
	return r
}

func (r *register) pack() (w uint64) {
	for j, c := range r {
		w |= c << bucketShift[j]
	}
	return w
}

// add puts n events into bucket j, spilling into older buckets when it is
// full: the count is preserved and the event is aged at most one bucket.
// Reports whether any of the n didn't fit in bucket j (spilled or, with
// every older bucket full too, dropped).
func (r *register) add(j int, n uint64) (overflow bool) {
	overflow = n > 1<<bucketBits[j]-1-r[j]
	for ; j < nBuckets && n > 0; j++ {
		room := uint64(1)<<bucketBits[j] - 1 - r[j]
		take := min(room, n)
		r[j] += take
		n -= take
	}
	return overflow
}

// bucketStart is b_j in units (may be negative near U = 0).
func bucketStart(u int64, j int) int64 {
	return u&bucketStepMask[j] - bucketOff[j]
}

// advance re-buckets r from unit u0 to unit u1 >= u0, reporting whether a
// merge overflowed its new bucket.
func (r *register) advance(u0, u1 int64) (overflow bool) {
	if u1 <= u0 {
		return false
	}
	if u1-u0 > 2*unitsPerHorizon {
		*r = register{}
		return false
	}
	var starts [nBuckets]int64
	for j := range starts {
		starts[j] = bucketStart(u1, j)
	}
	var out register
	j := 0
	for k := range r {
		if r[k] == 0 {
			continue
		}
		start := bucketStart(u0, k)
		for j < nBuckets && start < starts[j] {
			j++
		}
		if j == nBuckets {
			break // older buckets only start earlier: all dropped
		}
		overflow = out.add(j, r[k]) || overflow
	}
	*r = out
	return overflow
}

// regEpoch recovers the register's full unit index from its 4 stored bits,
// using a reference unit known to be within 8 of it: the unit of the slot's
// lastTouch, which the register was last aligned to (a racing touch may
// leave it one unit either side).
func regEpoch(w1 uint64, ref int64) int64 {
	d := (ref - int64(w1>>48)) & epochMask
	if d > epochMask/2 {
		d -= epochMask + 1
	}
	return ref - d
}

// nextReg returns word1 after counting one event at now in old, the
// register of key tg last touched at last. reset (or a tag mismatch) starts
// a fresh register. A full register in the current unit comes back as is.
// overflow reports that an event spilled out of its bucket or was dropped
// (the slot's saturated bit must then be set).
func (f *Filter) nextReg(old, tg, last, now uint64, reset bool) (w1 uint64, overflow bool) {
	nowU := f.unitOf(now)
	if !reset && old>>tagShift == tg && old>>48&epochMask == uint64(nowU)&epochMask &&
		regEpoch(old, f.unitOf(last)) == nowU {
		// Fast path, same unit: increment the first bucket with room in
		// place (spilling as add does). Buckets are contiguous from bit 0,
		// so the lowest zero bit of the register lies in that bucket.
		free := ^old & regMask
		if free == 0 {
			return old, true // every bucket full
		}
		b := bits.TrailingZeros64(free)
		return old + 1<<bucketBase[b], b >= bucketBits0
	}
	var r register
	e := nowU
	if !reset && old>>tagShift == tg {
		r = unpack(old & regMask)
		e = regEpoch(old, f.unitOf(last))
		if nowU > e {
			overflow = r.advance(e, nowU)
			e = nowU
		} // else a caller with a slightly stale clock: count at e
	}
	overflow = r.add(0, 1) || overflow
	return tg<<tagShift | uint64(e)&epochMask<<48 | r.pack(), overflow
}

// hot reports whether some bucket of w1 holds satPerUnit or more events.
// A bucket that overflowed (or took a spill-causing merge) holds at least
// that many, and its count only grows until it ages out of the register, so
// while a slot's saturated bit is set some bucket is hot.
func hot(w1 uint64) bool {
	if isHot(w1) {
		return false // a hot entry has no saturation of its own (its sat unit says what is missing)
	}
	for j := range nBuckets {
		if w1>>bucketShift[j]&(1<<bucketBits[j]-1) >= satPerUnit {
			return true
		}
	}
	return false
}

// bump counts one event at now in the register of the slot at word index i,
// which must still hold the caller's key: word0's fingerprint and displaced
// bit fpd, word1's tag tg. It reports false, writing nothing, if the slot has
// been handed to another key since the caller found it. word0 is checked
// after word1 is read: a hand-over makes word1 busy before it changes word0,
// so an old value that passes both checks still belongs to the caller's key
// (a newcomer with the same tag can't be mistaken for it). A key whose
// register would overflow is promoted to a hot entry instead (hot.go); a
// promoted key's events go to its entry and word1 stays as it is.
func (f *Filter) bump(i int, fpd, tg, last, now uint64, reset bool) (w1 uint64, overflow, ok bool) {
	for {
		old := atomic.LoadUint64(&f.words[i+1])
		w0 := atomic.LoadUint64(&f.words[i])
		if old>>tagShift != tg || w0>>47 != fpd {
			return 0, false, false
		}
		if isHot(old) && !reset {
			if f.hotAdd(old, f.slotID(uint64(i/wordsPerLine), w0, old), now, &f.words[i+1]) {
				return old, false, true
			}
			if atomic.LoadUint64(&f.words[i+1]) != old {
				continue // the entry was taken back with the slot: look again
			}
			reset = true // the entry aged out with the key's history: start afresh
		}
		nw, over := f.nextReg(old, tg, last, now, reset)
		if over {
			if pw, ok := f.promote(i, w0, old, tg, last, now); ok {
				return pw, false, true
			}
		}
		if nw == old || atomic.CompareAndSwapUint64(&f.words[i+1], old, nw) {
			return nw, over, true
		}
		reset = false // a concurrent touch already reset it; merge
	}
}

// mass is the event count of the key with identity id (register or hot
// entry in w1, lastTouch last) in buckets that still reach inside the
// horizon (for eviction scoring).
func (f *Filter) mass(id, w1, last, now uint64) uint64 {
	if age(now, last) > f.horizon {
		return 0
	}
	v, ok := f.viewOf(id, w1, last)
	if !ok {
		return 0
	}
	oldest := int64(now-min(now, f.horizon)) / int64(f.unit) // unit of now-H, rounded down
	var n uint64
	top := v.e + 1
	for j := range nBuckets {
		if top <= oldest {
			break
		}
		n += v.c[j]
		top = bucketStart(v.e, j)
	}
	return n
}

// estimate interpolates the count in (now-window, now] straight from the
// buckets as they were laid out at the register's own epoch: no re-bucketing
// to now, so no resolution is lost to merging. Two facts from word0 make it
// exact at the extremes: nothing happened after last, so a window ending
// before it holds zero and one reaching it holds at least one; and the
// newest non-empty bucket's events all lie in [its start, last]. The one
// bucket the window's trailing edge cuts is assumed uniform for Count and
// counted whole for Upper.
//
// sat is the slot's saturated bit: some event overflowed its bucket within
// the register's span. The bucket it overflowed from still covers its time
// and is hot (see hot), so if no bucket the window reaches is hot, no event
// in the window was moved or dropped and Upper bounds the true count. A hot
// entry (w1 points to one; id is the key's identity) never overflows; it is
// Saturated only for windows reaching back to the unit it was promoted in,
// if the register it came from had overflowed. ok is false if w1 points to
// an entry that is no longer the key's (an abandoned move took it back).
func (f *Filter) estimate(id, w1, last, now, window uint64, sat bool) (r Result, ok bool) {
	window = min(window, f.horizon)
	if age(now, last) > window {
		return Result{}, true
	}
	v, ok := f.viewOf(id, w1, last)
	if !ok {
		return Result{}, false
	}
	e := v.e
	unit := float64(f.unit)
	from := float64(now) - float64(window)
	top := float64(e+1) * unit
	newest := true
	est, upper, hit := 0.0, uint64(0), false
	for j := range nBuckets {
		lo := float64(bucketStart(e, j)) * unit
		c := v.c[j]
		hi := top
		top = lo
		if c == 0 {
			if from >= lo {
				break
			}
			continue
		}
		hit = hit || c >= satPerUnit
		upper += c
		if newest {
			hi = max(lo, min(float64(last), hi))
			newest = false
		}
		if from <= lo {
			est += float64(c)
			continue
		}
		if from < hi {
			est += float64(c) * (hi - from) / (hi - lo)
		}
		break
	}
	n := max(1, int(est+0.5))
	r = Result{Count: n, Upper: max(n, int(upper)), Saturated: sat && hit}
	if v.hot {
		r.Saturated = v.sat != satNone && from < float64(v.sat+1)*unit
	}
	return r, true
}

// satPerUnit is the newest bucket's capacity: a unit with more events
// overflows, and every bucket that overflows holds at least this many.
const satPerUnit = 1<<bucketBits0 - 1

// Geometry (see the comment at the top): unit = Horizon/24; widths 1, 1,
// 1-2, 2-4, 4-8, 8, 8 units; coverage >= 24 units.
const (
	geomUnits   = 24
	bucketBits0 = 5
)

var (
	bucketLogStep = [nBuckets]uint{0, 0, 1, 2, 3, 3, 3}
	bucketOff     = [nBuckets]int64{0, 1, 2, 4, 8, 16, 24}
	bucketBits    = [nBuckets]uint{bucketBits0, 5, 6, 7, 8, 8, 9}
)

var bucketStepMask = func() (m [nBuckets]int64) {
	for j := range m {
		m[j] = ^(int64(1)<<bucketLogStep[j] - 1)
	}
	return m
}()

// unitOf is t / f.unit via a precomputed reciprocal: one multiply instead of
// a 64-bit divide on every touch.
func (f *Filter) unitOf(t uint64) int64 {
	q, _ := bits.Mul64(t, f.unitInv)
	if q*f.unit > t {
		q--
	}
	return int64(q)
}
