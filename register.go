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
func (r *register) add(j int, n uint64) {
	for ; j < nBuckets && n > 0; j++ {
		room := uint64(1)<<bucketBits[j] - 1 - r[j]
		take := min(room, n)
		r[j] += take
		n -= take
	}
}

// bucketStart is b_j in units (may be negative near U = 0).
func bucketStart(u int64, j int) int64 {
	return u&bucketStepMask[j] - bucketOff[j]
}

// advance re-buckets r from unit u0 to unit u1 >= u0.
func (r *register) advance(u0, u1 int64) {
	if u1 <= u0 {
		return
	}
	if u1-u0 > 2*unitsPerHorizon {
		*r = register{}
		return
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
		out.add(j, r[k])
	}
	*r = out
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

// bump advances the register to now and counts one event. last is the
// slot's lastTouch before this touch (the epoch reference); reset discards
// counts left by a previous occupant or an expired run.
func (f *Filter) bump(i int, tg, last, now uint64, reset bool) uint64 {
	nowU := f.unitOf(now)
	for {
		old := atomic.LoadUint64(&f.words[i])
		if !reset && old>>tagShift == tg && old>>48&epochMask == uint64(nowU)&epochMask &&
			regEpoch(old, f.unitOf(last)) == nowU {
			// Fast path, same unit: increment the first bucket with room in
			// place (spilling as add does). A full register stays as is.
			nw := old
			for j := range nBuckets {
				if old>>bucketShift[j]&(1<<bucketBits[j]-1) != 1<<bucketBits[j]-1 {
					nw = old + 1<<bucketShift[j]
					break
				}
			}
			if nw == old || atomic.CompareAndSwapUint64(&f.words[i], old, nw) {
				return nw
			}
			continue
		}
		var r register
		e := nowU
		if !reset && old>>tagShift == tg {
			r = unpack(old & regMask)
			e = regEpoch(old, f.unitOf(last))
			if nowU > e {
				r.advance(e, nowU)
				e = nowU
			} // else a caller with a slightly stale clock: count at e
		}
		r.add(0, 1)
		nw := tg<<tagShift | uint64(e)&epochMask<<48 | r.pack()
		if atomic.CompareAndSwapUint64(&f.words[i], old, nw) {
			return nw
		}
		reset = false // a concurrent writer already re-tagged it; merge
	}
}

// mass is the register's event count in buckets that still reach inside
// the horizon (for eviction scoring).
func (f *Filter) mass(w1, last, now uint64) uint64 {
	if age(now, last) > f.horizon {
		return 0
	}
	e := regEpoch(w1, f.unitOf(last))
	oldest := int64(now-min(now, f.horizon)) / int64(f.unit) // unit of now-H, rounded down
	var n uint64
	top := e + 1
	for j := range nBuckets {
		if top <= oldest {
			break
		}
		n += w1 >> bucketShift[j] & (1<<bucketBits[j] - 1)
		top = bucketStart(e, j)
	}
	return n
}

// estimate interpolates the count in (now-window, now] straight from the
// buckets as they were laid out at the register's own epoch: no re-bucketing
// to now, so no resolution is lost to merging. Two facts from word0 make it
// exact at the extremes: nothing happened after last, so a window ending
// before it holds zero and one reaching it holds at least one; and the
// newest non-empty bucket's events all lie in [its start, last]. The one
// bucket the window's trailing edge cuts is assumed uniform.
func (f *Filter) estimate(w1, last, now, window uint64) int {
	window = min(window, f.horizon)
	if age(now, last) > window {
		return 0
	}
	e := regEpoch(w1, f.unitOf(last))
	unit := float64(f.unit)
	from := float64(now) - float64(window)
	top := float64(e+1) * unit
	newest := true
	est := 0.0
	for j := range nBuckets {
		lo := float64(bucketStart(e, j)) * unit
		c := float64(w1 >> bucketShift[j] & (1<<bucketBits[j] - 1))
		hi := top
		top = lo
		if c == 0 {
			if from >= lo {
				break
			}
			continue
		}
		if newest {
			hi = max(lo, min(float64(last), hi))
			newest = false
		}
		if from <= lo {
			est += c
			continue
		}
		if from < hi {
			est += c * (hi - from) / (hi - lo)
		}
		break
	}
	return max(1, int(est+0.5))
}

// Geometry (see the comment at the top): unit = Horizon/24; widths 1, 1,
// 1-2, 2-4, 4-8, 8, 8 units; coverage >= 24 units.
const geomUnits = 24

var (
	bucketLogStep = [nBuckets]uint{0, 0, 1, 2, 3, 3, 3}
	bucketOff     = [nBuckets]int64{0, 1, 2, 4, 8, 16, 24}
	bucketBits    = [nBuckets]uint{5, 5, 6, 7, 8, 8, 9}
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
