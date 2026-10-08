package temporalfilter

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// Reference: keep an exact per-unit histogram and bucket it directly at the
// end. The register, driven like bump drives it (advance, then add), must
// match whenever nothing saturates.
func TestAdvanceMatchesHistogram(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for range 3000 {
		u := int64(rng.IntN(1 << 20))
		start := u
		var r register
		hist := map[int64]uint64{}
		for range 1 + rng.IntN(40) {
			next := u + int64(rng.IntN(12))
			if rng.IntN(8) == 0 {
				next += int64(rng.IntN(3 * unitsPerHorizon))
			}
			r.advance(u, next)
			u = next
			r.add(0, 1)
			hist[u]++
		}
		end := u + int64(rng.IntN(2*unitsPerHorizon+5))
		r.advance(u, end)
		var want register
		if end-u <= 2*unitsPerHorizon {
			for t, n := range hist {
				for j := range nBuckets {
					hi := int64(1 << 62)
					if j > 0 {
						hi = bucketStart(end, j-1)
					}
					if t >= bucketStart(end, j) && t < hi {
						want[j] += n
					}
				}
			}
			// history older than the last reset gap was legitimately dropped
			if lastGap(hist, end) {
				continue
			}
		}
		if r != want {
			t.Fatalf("start %d end %d: register %v, histogram %v", start, end, r, want)
		}
	}
}

// lastGap reports whether some gap between consecutive events exceeded the
// reset threshold (the register then forgets everything before it).
func lastGap(hist map[int64]uint64, end int64) bool {
	var ts []int64
	for t := range hist {
		ts = append(ts, t)
	}
	slices.Sort(ts)
	ts = append(ts, end)
	for i := 1; i < len(ts); i++ {
		if ts[i]-ts[i-1] > 2*unitsPerHorizon {
			return true
		}
	}
	return false
}

func TestBucketsCoverHorizon(t *testing.T) {
	for u := int64(64); u < 4096; u++ {
		if span := u - bucketStart(u, nBuckets-1); span < unitsPerHorizon {
			t.Fatalf("u=%d: buckets cover only %d units", u, span)
		}
	}
}

func TestPackRoundTripAndSpill(t *testing.T) {
	var r register
	r.add(0, 100) // bucket 0 holds 31; the rest spills to older buckets
	var sum uint64
	for _, c := range r {
		sum += c
	}
	if sum != 100 || r[0] != 31 {
		t.Fatalf("spill: %v", r)
	}
	if got := unpack(r.pack()); got != r {
		t.Fatalf("round trip %v -> %v", r, got)
	}
}

func TestUnitOfMatchesDivision(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	for _, h := range []time.Duration{48 * time.Microsecond, time.Second, time.Minute, 7 * time.Hour} {
		f, err := New(Config{Capacity: 10, Horizon: h})
		if err != nil {
			t.Fatal(err)
		}
		for range 100_000 {
			x := rng.Uint64() & tsMask
			if rng.IntN(2) == 0 {
				x = x/f.unit*f.unit + uint64(rng.IntN(3)) - 1 // around boundaries
			}
			if got, want := f.unitOf(x), int64(x/f.unit); got != want {
				t.Fatalf("unit %d: unitOf(%d) = %d, want %d", f.unit, x, got, want)
			}
		}
	}
}

func TestTicksClampsNegative(t *testing.T) {
	f := newTest(t, 10, time.Minute)
	if f.Ticks(-time.Second) != 0 {
		t.Fatal("negative duration should be 0 ticks")
	}
}
