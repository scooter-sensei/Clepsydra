package temporalfilter

import (
	"math"
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"
)

// Overflow sketch: where a history goes when the stash has to drop it. It is
// a count-min sketch over (key identity, time slice): every dropped event is
// added to sketchRows counters, one per row. Collisions only add other keys'
// events, so the smallest of a key's counters is never below its true count
// (Upper), and no flood can erase a history before it ages out. Count is the
// count-mean-min estimate (Deng & Rafiei 2007): each counter less the other
// keys' events expected in it, the median over rows. It is unbiased, so a
// flood raises keys that never spilled by noise around 0, not by the
// flood's average load. Slices are Horizon/sliceParts long; a ring of
// sketchSlices covers the horizon plus the slice being written, and a ring
// position is cleared when a newer slice takes it.
//
// Adds are lock-free (a CAS per row); only handing a ring position to a new
// slice takes the mutex. Reads are lock-free; counters only grow while
// their slice is live.

const (
	sketchRows   = 4
	sliceParts   = 4
	sketchSlices = sliceParts + 2
	// Counters are 16 bits, two slices to a word. cellSat marks a counter
	// that took a saturated history or reached its cap: its count is a floor.
	// A key reads saturated only if all its counters carry it.
	cellSat   = 1 << 15
	cellMask  = cellSat - 1
	cellWords = sketchSlices / 2
	// sigmas: a slice's count-mean-min estimate counts only above this many
	// standard deviations of its noise, so keys that never spilled read
	// exact Counts, and a spilled one at most this much low.
	sigmas = 3
)

type sketch struct {
	cells    []uint32 // [row][column][slice/2], 16 bits per slice
	w        uint64   // columns per row
	sliceLen uint64   // ticks
	horizon  uint64
	mu       sync.Mutex
	epochs   [sketchSlices]atomic.Uint64 // slice number + 1 held by each ring position
	totals   [sketchSlices]atomic.Uint64 // events added to each slice (the same in every row)
	squares  [sketchSlices]atomic.Uint64 // sum of each add's n squared: the noise's variance × w
	// until is a tick before which some slice may hold a live count.
	until atomic.Uint64
}

func newSketch(w, horizon uint64) *sketch {
	return &sketch{
		cells:    make([]uint32, sketchRows*max(w, 2)*cellWords),
		w:        max(w, 2),
		sliceLen: (horizon + sliceParts - 1) / sliceParts,
		horizon:  horizon,
	}
}

// col is id's counter group in row r.
func (s *sketch) col(id uint64, r int) uint64 {
	c, _ := bits.Mul64(mix64(id+uint64(r)*0x9e3779b97f4a7c15), s.w)
	return (uint64(r)*s.w + c) * cellWords
}

// cell loads the counter of ring position p in the group at base.
func (s *sketch) cell(base, p uint64) uint32 {
	return atomic.LoadUint32(&s.cells[base+p/2]) >> (16 * (p & 1)) & 0xffff
}

// add counts n events of id at tick t (sat: the count is a floor).
func (s *sketch) add(id, t, n uint64, sat bool, now uint64) {
	if n == 0 || age(now, t) > s.horizon {
		return
	}
	sl := t / s.sliceLen
	p := sl % sketchSlices
	if e := s.epochs[p].Load(); e != sl+1 {
		if e > sl+1 {
			return // a newer slice took this place: t is past the horizon
		}
		s.renew(p, sl)
	}
	sh := 16 * (p & 1)
	for r := range sketchRows {
		w := &s.cells[s.col(id, r)+p/2]
		for {
			old := atomic.LoadUint32(w)
			c := old >> sh & 0xffff
			v := uint64(c&cellMask) + n
			nc := uint32(min(v, cellMask)) | c&cellSat
			if sat || v >= cellMask {
				nc |= cellSat
			}
			if atomic.CompareAndSwapUint32(w, old, old&^(0xffff<<sh)|nc<<sh) {
				break
			}
		}
	}
	s.totals[p].Add(n)
	s.squares[p].Add(n * n)
	if u := sl*s.sliceLen + s.sliceLen - 1 + s.horizon; s.until.Load() < u {
		for {
			cur := s.until.Load()
			if cur >= u || s.until.CompareAndSwap(cur, u) {
				break
			}
		}
	}
}

// renew hands ring position p to slice sl, clearing it. Readers skip p
// meanwhile. An add that checked p's epoch just before can still land after
// the clear: an expired slice's events then count in the new one, which only
// raises answers.
func (s *sketch) renew(p, sl uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.epochs[p].Load() >= sl+1 {
		return
	}
	s.epochs[p].Store(0)
	keep := ^uint32(0xffff << (16 * (p & 1)))
	for i := p / 2; i < uint64(len(s.cells)); i += cellWords {
		atomic.AndUint32(&s.cells[i], keep)
	}
	s.totals[p].Store(0)
	s.squares[p].Store(0)
	s.epochs[p].Store(sl + 1)
}

// estimate is id's events in the window ending at now, as Result fields:
// Upper is the count-min bound, with the slice at the window's edge whole;
// Count the count-mean-min estimate where it clears the noise, with that slice
// pro rata; Seen if Count
// rounds to at least 1. ok is false if no slice in the window has a count.
// full reads every row whatever it finds (ConstantTime); otherwise the first
// row is read alone and is usually enough to say no.
func (s *sketch) estimate(id, window, now uint64, full bool) (r Result, ok bool) {
	cur := now / s.sliceLen
	oldest := uint64(0)
	if cur >= sketchSlices-1 {
		oldest = cur - (sketchSlices - 2)
	}
	var c [sketchSlices][sketchRows]uint64
	var sat [sketchSlices]bool
	for i := range sat {
		sat[i] = true
	}
	for row := range sketchRows {
		base := s.col(id, row)
		any := false
		for sl := oldest; sl <= cur; sl++ {
			p := sl % sketchSlices
			v := s.cell(base, p)
			c[p][row] = uint64(v & cellMask)
			sat[p] = sat[p] && v&cellSat != 0
			any = any || v != 0 && s.epochs[p].Load() == sl+1
		}
		if !any && !full {
			return Result{}, false
		}
	}
	from := int64(now) - int64(window)
	est := 0.0
	for sl := oldest; sl <= cur; sl++ {
		p := sl % sketchSlices
		start, end := int64(sl*s.sliceLen), int64(min(sl*s.sliceLen+s.sliceLen-1, now))
		if s.epochs[p].Load() != sl+1 || end < from || age(now, uint64(end)) > s.horizon {
			continue
		}
		lo := slices.Min(c[p][:])
		if lo == 0 {
			continue
		}
		r.Upper += int(lo)
		r.Saturated = r.Saturated || sat[p]
		// Count-mean-min: each row less the noise expected in its counter.
		total := float64(s.totals[p].Load())
		var d [sketchRows]float64
		for row, v := range c[p] {
			d[row] = float64(v) - (total-float64(v))/float64(s.w-1)
		}
		slices.Sort(d[:])
		m := min((d[sketchRows/2-1]+d[sketchRows/2])/2, float64(lo))
		// Other keys' events arrive in lumps (a history at a time), so the
		// noise's variance is the sum of the lumps' squares over w.
		if m < sigmas*math.Sqrt(float64(s.squares[p].Load())/float64(s.w)) {
			m = 0 // within the noise: other keys' events could explain it
		}
		if start >= from {
			est += m
		} else {
			est += m * float64(end-from+1) / float64(end-start+1)
		}
	}
	if r.Upper == 0 {
		return Result{}, false
	}
	r.Count = int(min(max(est, 0), float64(r.Upper)) + 0.5)
	r.Seen = r.Count > 0
	return r, true
}

// spill sends a history the stash is dropping (identity id, slot words w0,
// w1) to the overflow sketch, or, without one, records it as lost.
func (f *Filter) spill(id, w0, w1, now uint64) {
	last := w0 & tsMask
	if age(now, last) > f.horizon {
		return
	}
	if f.sk == nil {
		f.dropped(now)
		return
	}
	f.folded.Add(1)
	sat := w0&satBit != 0 && hot(w1)
	e := regEpoch(w1, f.unitOf(last))
	top := uint64(e+1) * f.unit
	for j := range nBuckets {
		lo := bucketStart(e, j)
		c := w1 >> bucketShift[j] & (1<<bucketBits[j] - 1)
		if c > 0 && top > 0 {
			// The bucket's newest tick (lastTouch for the newest): placing
			// its events as late as possible only widens the windows that
			// see them.
			f.sk.add(id, min(top-1, last), c, sat, now)
		}
		if lo <= 0 {
			break
		}
		top = uint64(lo) * f.unit
	}
}
