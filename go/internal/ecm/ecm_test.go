package ecm

import (
	"math/rand/v2"
	"sort"
	"testing"
)

// TestHistogramError: one histogram (w = d = 1, so the cell's stream is the
// key's), random streams with bursts and silences, queried at random times
// for ranges from 1 tick to the window. Every answer must be within eps of
// the exact count (the paper's guarantee for any range up to the window),
// the upper bound never below it, and an empty range exactly 0.
func TestHistogramError(t *testing.T) {
	const window = 60_000
	ranges := []uint64{0, 1, 2, 10, 100, 999, 1000, 5000, 15_000, 30_000, 59_999, 60_000}
	for _, eps := range []float64{0.5, 0.3, 0.2, 0.1, 0.05, 0.02, 0.01} {
		var queries, worstAt int
		worst := 0.0
		for seed := range uint64(6) {
			r := rand.New(rand.NewPCG(seed, uint64(eps*1000)))
			s := New(1, 1, eps, window, seed)
			var log []uint64
			now := uint64(1)
			for range 40_000 {
				now += uint64(r.ExpFloat64() * 40)
				if r.IntN(300) == 0 {
					now += uint64(r.IntN(2 * window)) // silence
				}
				n := 1
				if r.IntN(20) == 0 {
					n = 1 + r.IntN(300) // burst in one tick
				}
				for range n {
					s.Add(7, now)
					log = append(log, now)
				}
				if r.IntN(4) != 0 {
					continue
				}
				q := now + uint64(r.IntN(50))
				for _, rg := range append(ranges, r.Uint64N(window+1)) {
					exact := len(log) - sort.Search(len(log), func(i int) bool { return q-log[i] <= rg })
					est, up := s.Count(7, q, rg)
					queries++
					if up < exact {
						t.Fatalf("eps %v: upper %d < exact %d (range %d)", eps, up, exact, rg)
					}
					if exact == 0 && est != 0 {
						t.Fatalf("eps %v: empty range reads %d", eps, est)
					}
					if exact > 0 {
						rel := float64(abs(est-exact)) / float64(exact)
						if rel > worst {
							worst, worstAt = rel, exact
						}
						if rel > eps+1e-12 {
							t.Fatalf("eps %v: range %d at %d reads %d, exact %d (relative error %.4f)", eps, rg, q, est, exact, rel)
						}
					}
				}
			}
			checkInvariant(t, s, eps)
		}
		t.Logf("eps %.2f: %d queries, worst relative error %.4f (exact %d); level minima %v", eps, queries, worst, worstAt, s0(eps))
	}
}

func s0(eps float64) []int { return levelMins(eps)[:6] }

// checkInvariant checks the paper's invariant 1 for every bucket of every
// cell, counting only lower levels as newer (a range can cut any bucket).
// Buckets of size 1 are exempt: a range never cuts them.
func checkInvariant(t *testing.T, s *Sketch, eps float64) {
	t.Helper()
	for c := range s.cells {
		lower := 0
		for i, l := range s.cells[c].lv {
			if i > 0 && len(l) > 0 && float64(int(1)<<i)/(2*float64(1+lower)) > eps+1e-12 {
				t.Fatalf("eps %v: a level-%d bucket has only %d events below it", eps, i, lower)
			}
			lower += len(l) << i
		}
	}
}

// TestSketchBounds: many keys in a narrow sketch, so cells collide. Every
// key's upper bound is at least its count; its estimate is at least
// (1 - eps) times its count (each cell's estimate is within eps of the cell's
// count, which includes the key's); and the bytes counted match a recount.
func TestSketchBounds(t *testing.T) {
	const window, keys, eps = 10_000, 500, 0.1
	r := rand.New(rand.NewPCG(3, 4))
	s := New(64, 3, eps, window, 99)
	logs := make([][]uint64, keys)
	now := uint64(1)
	for range 200_000 {
		now += uint64(r.IntN(3))
		k := r.IntN(keys)
		if k%7 == 0 {
			k = 0 // a heavy key
		}
		s.Add(uint64(k), now)
		logs[k] = append(logs[k], now)
	}
	for k, log := range logs {
		for _, rg := range []uint64{10, 1000, window} {
			exact := len(log) - sort.Search(len(log), func(i int) bool { return now-log[i] <= rg })
			est, up := s.Count(uint64(k), now, rg)
			if up < exact || float64(est) < (1-eps)*float64(exact)-1e-9 {
				t.Fatalf("key %d range %d: est %d upper %d exact %d", k, rg, est, up, exact)
			}
		}
	}
	b, lv := 0, 0
	for _, c := range s.cells {
		for _, l := range c.lv {
			b += len(l)
		}
		lv += len(c.lv)
	}
	if want := len(s.cells) + lv + 4*b; s.Bytes() != want || s.Peak() < s.Bytes() {
		t.Fatalf("bytes %d (peak %d), recount %d", s.Bytes(), s.Peak(), want)
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
