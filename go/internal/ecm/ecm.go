// Package ecm is the ECM-sketch of Papapetrou, Garofalakis and
// Deligiannakis ("Sketch-based Querying of Distributed Sliding-Window Data
// Streams", PVLDB 5(10), 2012): a count-min array of width w and depth d
// whose counters are exponential histograms (Datar, Gionis, Indyk and
// Motwani, SODA 2002) over a time-based sliding window. A point query
// (x, r), for any range r up to the window, is the minimum over rows of each
// histogram's estimate for r (the paper's §4.1).
//
// It is the competitor of the head-to-head comparison
// (docs/artifact/head-to-head.md). No industry implementation exists, so it
// is implemented here from the paper, as the paper states it.
package ecm

import (
	"math"
	"math/bits"
)

// hist is an exponential histogram: buckets of sizes 1, 2, 4, ..., each
// holding the time of its newest event. lv[i] holds the buckets of size 2^i,
// oldest first, and every bucket of level i+1 is older than every bucket of
// level i. A level holds at most min[i]+1 buckets: one more and its two
// oldest merge into a bucket of the next level, stamped with the newer of
// the two, leaving min[i]. Buckets whose newest event left the window are
// dropped on insert (§4: "remove all expired information").
type hist struct {
	lv [][]uint64
}

// Sketch is an ECM-sketch. It is not safe for concurrent use.
type Sketch struct {
	w, d    int
	window  uint64
	min     []int // per level: buckets kept after a merge (levelMins)
	seeds   []uint64
	cells   []hist
	buckets int // buckets held, all cells
	levels  int // levels in use, all cells
	peak    int // most Bytes ever held
}

// levelMins returns, per level, the fewest buckets a level keeps once it has
// merged, so that every bucket satisfies the paper's invariant 1,
// C_j / (2 (1 + sum of the sizes of newer buckets)) <= eps, whatever range
// cuts it. The bucket a range cuts may be the newest of its level, so only
// lower levels are counted as newer: a bucket of size 2^J needs
// sum_{i<J} min[i] 2^i >= 2^(J-1)/eps - 1. That gives about 1/eps buckets at
// level 0 and 1/(2 eps) above it (Datar et al. keep k/2 at every level, which
// bounds the error of the whole-window query, not of every sub-range).
func levelMins(eps float64) []int {
	m := make([]int, 63)
	sum := 0.0 // sum_{i<j} m[i] 2^i
	for j := range m {
		size := math.Ldexp(1, j) // 2^j: the size of level j's buckets
		need := size/eps - 1 - sum
		m[j] = max(1, int(math.Ceil(need/size-1e-9)))
		sum += float64(m[j]) * size
	}
	return m
}

// New returns an ECM-sketch of w columns and d rows whose histograms have
// relative error epsSW over any range up to window (in ticks). seed keys the
// row hashes.
func New(w, d int, epsSW float64, window, seed uint64) *Sketch {
	s := &Sketch{w: w, d: d, window: window, min: levelMins(epsSW), cells: make([]hist, w*d), peak: w * d}
	for range d {
		seed = mix64(seed + 0x9e3779b97f4a7c15)
		s.seeds = append(s.seeds, seed)
	}
	return s
}

// Params returns w, d and the buckets each level keeps after a merge.
func (s *Sketch) Params() (w, d int, mins []int) { return s.w, s.d, s.min }

func (s *Sketch) cell(key uint64, row int) *hist {
	hi, _ := bits.Mul64(mix64(key^s.seeds[row]), uint64(s.w))
	return &s.cells[row*s.w+int(hi)]
}

// Add records one event of key at time t. Times must not decrease.
func (s *Sketch) Add(key, t uint64) {
	for j := range s.d {
		s.add(s.cell(key, j), t)
	}
	s.peak = max(s.peak, s.Bytes())
}

func (s *Sketch) add(h *hist, t uint64) {
	s.expire(h, t)
	if len(h.lv) == 0 {
		h.lv = append(h.lv, nil)
		s.levels++
	}
	h.lv[0] = append(h.lv[0], t)
	s.buckets++
	for i := 0; i < len(h.lv) && len(h.lv[i]) > s.min[i]+1; i++ {
		newer := h.lv[i][1]
		n := copy(h.lv[i], h.lv[i][2:])
		h.lv[i] = h.lv[i][:n]
		if i+1 == len(h.lv) {
			h.lv = append(h.lv, nil)
			s.levels++
		}
		h.lv[i+1] = append(h.lv[i+1], newer)
		s.buckets--
	}
}

// expire drops the buckets whose newest event is older than the window at
// t. The oldest bucket is the first of the highest level.
func (s *Sketch) expire(h *hist, t uint64) {
	for len(h.lv) > 0 {
		top := len(h.lv) - 1
		if len(h.lv[top]) == 0 {
			h.lv = h.lv[:top]
			s.levels--
			continue
		}
		if t-h.lv[top][0] <= s.window {
			return
		}
		n := copy(h.lv[top], h.lv[top][1:])
		h.lv[top] = h.lv[top][:n]
		s.buckets--
	}
}

// count is one histogram's answer for the events in [now-r, now]: est sums
// the buckets the range holds whole and half the oldest one it cuts (§3);
// upper counts every bucket the range reaches, so it is never below the
// truth. A bucket of size 1 is never cut: its one event is at its time.
func (s *Sketch) count(h *hist, now, r uint64) (est, upper int) {
	last := 0 // size of the oldest bucket in range so far
	for i, l := range h.lv {
		for k := len(l) - 1; k >= 0; k-- {
			if now-l[k] > r {
				return finish(upper, last)
			}
			last = 1 << i
			upper += last
		}
	}
	return finish(upper, last)
}

func finish(upper, last int) (est, up int) {
	if last <= 1 {
		return upper, upper
	}
	return upper - last/2, upper
}

// Count answers the point query (key, r) at now: the minimum over rows of
// each histogram's estimate, and of each histogram's upper bound.
func (s *Sketch) Count(key, now, r uint64) (est, upper int) {
	est, upper = math.MaxInt, math.MaxInt
	for j := range s.d {
		e, u := s.count(s.cell(key, j), now, r)
		est, upper = min(est, e), min(upper, u)
	}
	return est, upper
}

// Bytes is the memory the sketch holds, counted compactly: 1 B per counter
// (its number of levels), 1 B per level in use (its bucket count) and 4 B
// per bucket (a 32-bit time; the bucket's size is its level's). Go's slice
// headers are not counted.
func (s *Sketch) Bytes() int { return s.w*s.d + s.levels + 4*s.buckets }

// Peak is the most Bytes the sketch has held.
func (s *Sketch) Peak() int { return s.peak }

// mix64 is the SplitMix64 finalizer.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}
