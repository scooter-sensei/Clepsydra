package temporalfilter

import (
	"math"
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"
)

// Count-min overflow sketch: where a history went when the stash had to drop
// it, before the ghost table (ghost.go) replaced it as the default (it is
// kept for the experiments that compare the two, Filter.cms). It is
// a count-min sketch over (key identity, time slice): every dropped event is
// added to one counter per row. Collisions only add other keys' events, so
// the smallest of a key's counters is never below its true count (Upper),
// and no flood can erase a history before it ages out. Count is the
// count-mean-min estimate (Deng & Rafiei 2007): each counter less the other
// keys' events expected in it, the median over rows. It is unbiased, so a
// flood raises keys that never spilled by noise around 0, not by the
// flood's average load. Slices are Horizon/parts long; a ring of parts+2
// covers the horizon plus the slice being written, and a ring position is
// cleared when a newer slice takes it. The default shape (4 rows, 8-bit
// counters, Horizon/2 slices) packs a column's ring into one word; it beat
// 16-bit counters and Horizon/4 slices at two thirds of the memory
// (verify/claims.md A9).
//
// Adds are lock-free (a CAS per row); only handing a ring position to a new
// slice takes the mutex. Reads are lock-free; counters only grow while
// their slice is live.

const (
	maxRows   = 4
	maxSlices = 6
	// sigmas: a slice's count-mean-min estimate counts only above this many
	// standard deviations of its noise, so keys that never spilled read
	// exact Counts, and a spilled one at most this much low.
	sigmas = 3
)

// sketchShape is the sketch's geometry: rows, counter bits (8 or 16), and
// slices per horizon (the ring holds parts+2).
type sketchShape struct{ rows, bits, parts int }

var defaultShape = sketchShape{rows: 4, bits: 8, parts: 2}

// bytes is the memory of a sketch of width w.
func (sh sketchShape) bytes(w uint64) uint64 {
	per := uint64(32 / sh.bits)
	return uint64(sh.rows) * w * ((uint64(sh.parts) + 2 + per - 1) / per) * 4
}

type sketch struct {
	cells    []uint32 // [row][column][slice / per], bits per slice
	w        uint64   // columns per row
	rows     int
	bits     uint64
	per      uint64 // counters per word
	nsl      uint64 // ring slices
	cw       uint64 // words per column
	cmask    uint32 // counter value mask
	csat     uint32 // counter's saturated flag: its count is a floor
	sliceLen uint64 // ticks
	horizon  uint64
	mu       sync.Mutex
	epochs   [maxSlices]atomic.Uint64 // slice number + 1 held by each ring position
	totals   [maxSlices]atomic.Uint64 // events added to each slice (the same in every row)
	squares  [maxSlices]atomic.Uint64 // sum of each add's n squared: the noise's variance × w
	// until is a tick before which some slice may hold a live count.
	until atomic.Uint64
}

func newSketch(w, horizon uint64, sh sketchShape) *sketch {
	w = max(w, 2)
	per := uint64(32 / sh.bits)
	nsl := uint64(sh.parts + 2)
	cw := (nsl + per - 1) / per
	parts := uint64(sh.parts)
	return &sketch{
		cells:    make([]uint32, uint64(sh.rows)*w*cw),
		w:        w,
		rows:     sh.rows,
		bits:     uint64(sh.bits),
		per:      per,
		nsl:      nsl,
		cw:       cw,
		cmask:    1<<(sh.bits-1) - 1,
		csat:     1 << (sh.bits - 1),
		sliceLen: (horizon + parts - 1) / parts,
		horizon:  horizon,
	}
}

// col is id's counter group in row r.
func (s *sketch) col(id uint64, r int) uint64 {
	c, _ := bits.Mul64(mix64(id+uint64(r)*0x9e3779b97f4a7c15), s.w)
	return (uint64(r)*s.w + c) * s.cw
}

// cell loads the counter of ring position p in the group at base.
func (s *sketch) cell(base, p uint64) uint32 {
	return atomic.LoadUint32(&s.cells[base+p/s.per]) >> (s.bits * (p % s.per)) & (s.csat | s.cmask)
}

// add counts n events of id at tick t (sat: the count is a floor).
func (s *sketch) add(id, t, n uint64, sat bool, now uint64) {
	if n == 0 || age(now, t) > s.horizon {
		return
	}
	sl := t / s.sliceLen
	p := sl % s.nsl
	if e := s.epochs[p].Load(); e != sl+1 {
		if e > sl+1 {
			return // a newer slice took this place: t is past the horizon
		}
		s.renew(p, sl)
	}
	sh := s.bits * (p % s.per)
	full := s.csat | s.cmask
	for r := range s.rows {
		w := &s.cells[s.col(id, r)+p/s.per]
		for {
			old := atomic.LoadUint32(w)
			c := old >> sh & full
			v := uint64(c&s.cmask) + n
			nc := uint32(min(v, uint64(s.cmask))) | c&s.csat
			if sat || v >= uint64(s.cmask) {
				nc |= s.csat
			}
			if atomic.CompareAndSwapUint32(w, old, old&^(full<<sh)|nc<<sh) {
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
	keep := ^((s.csat | s.cmask) << (s.bits * (p % s.per)))
	for i := p / s.per; i < uint64(len(s.cells)); i += s.cw {
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
	if cur >= s.nsl-1 {
		oldest = cur - (s.nsl - 2)
	}
	var c [maxSlices][maxRows]uint64
	var sat [maxSlices]bool
	for i := range sat {
		sat[i] = true
	}
	for row := range s.rows {
		base := s.col(id, row)
		any := false
		for sl := oldest; sl <= cur; sl++ {
			p := sl % s.nsl
			v := s.cell(base, p)
			c[p][row] = uint64(v & s.cmask)
			sat[p] = sat[p] && v&s.csat != 0
			any = any || v != 0 && s.epochs[p].Load() == sl+1
		}
		if !any && !full {
			return Result{}, false
		}
	}
	from := int64(now) - int64(window)
	est := 0.0
	for sl := oldest; sl <= cur; sl++ {
		p := sl % s.nsl
		start, end := int64(sl*s.sliceLen), int64(min(sl*s.sliceLen+s.sliceLen-1, now))
		if s.epochs[p].Load() != sl+1 || end < from || age(now, uint64(end)) > s.horizon {
			continue
		}
		lo := slices.Min(c[p][:s.rows])
		if lo == 0 {
			continue
		}
		r.Upper += int(lo)
		r.Saturated = r.Saturated || sat[p]
		// Count-mean-min: each row less the noise expected in its counter.
		total := float64(s.totals[p].Load())
		var d [maxRows]float64
		for row, v := range c[p][:s.rows] {
			d[row] = float64(v) - (total-float64(v))/float64(s.w-1)
		}
		slices.Sort(d[:s.rows])
		m := min((d[(s.rows-1)/2]+d[s.rows/2])/2, float64(lo))
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
// w1) to the overflow table, or, without one, records it as lost.
func (f *Filter) spill(id, w0, w1, now uint64) {
	last := w0 & tsMask
	if age(now, last) > f.horizon {
		return
	}
	if f.skW == 0 {
		f.dropped(max(now, last)) // its events may be in a window until a horizon after its newest one
		return
	}
	v, ok := f.viewOf(id, w1, last)
	if !ok {
		return // its hot entry is no longer its own: nothing of it is left to spill
	}
	m := f.mass(id, w1, last, now)
	if f.spillMin > 1 && m < f.spillMin {
		f.shed.Add(1)
		return
	}
	sat := w0&satBit != 0 && hot(w1) || v.hot && v.sat != satNone
	if !f.cms {
		f.folded.Add(1)
		f.ghostTable().add(id, m, last, sat, now)
		return
	}
	sk := f.overflow()
	f.folded.Add(1)
	e := v.e
	top := uint64(e+1) * f.unit
	for j := range nBuckets {
		lo := bucketStart(e, j)
		c := v.c[j]
		if c > 0 && top > 0 {
			// The bucket's newest tick (lastTouch for the newest): placing
			// its events as late as possible only widens the windows that
			// see them.
			sk.add(id, min(top-1, last), c, sat, now)
		}
		if lo <= 0 {
			break
		}
		top = uint64(lo) * f.unit
	}
}

// overflow returns the overflow sketch, allocating it on the first spill:
// a filter that never drops a history never pays for it.
func (f *Filter) overflow() *sketch {
	if sk := f.sk.Load(); sk != nil {
		return sk
	}
	f.skMu.Lock()
	defer f.skMu.Unlock()
	if sk := f.sk.Load(); sk != nil {
		return sk
	}
	sk := newSketch(f.skW, f.horizon, f.skShape)
	f.sk.Store(sk)
	return sk
}

// ghostTable returns the ghost overflow table, allocating it on the first
// spill, with the bytes the sketch would take.
func (f *Filter) ghostTable() *ghost {
	if g := f.gh.Load(); g != nil {
		return g
	}
	f.skMu.Lock()
	defer f.skMu.Unlock()
	if g := f.gh.Load(); g != nil {
		return g
	}
	g := newGhost(f.skShape.bytes(f.skW), f.horizon, f.ghChoices, f.ghSub)
	f.gh.Store(g)
	return g
}

// Floored counts histories the overflow table had no room for: each left
// to a floor of its bucket, which then raises Upper for every key sharing
// that floor, so from the first one strict limits turn such keys away. It
// stays 0 below the table's capacity (a heavy flood of 4x the table's slots
// at the defaults). Alert on it.
func (f *Filter) Floored() uint64 {
	if g := f.gh.Load(); g != nil {
		return g.floored.Load()
	}
	return 0
}
