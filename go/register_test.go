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

// TestCountsLastTouchWhole: the newest bucket's last event lies exactly at
// the slot's lastTouch, so a window that reaches it counts it whole and
// spreads only the bucket's other events. Eleven touches 200 ms apart in one
// 2.5 s unit, the last at 27,000: spreading all eleven over [25,000, 27,000]
// read 200 ms as 1.1 events (truth 2) and 400 ms as 2.2 (truth 3).
func TestCountsLastTouchWhole(t *testing.T) {
	f := newTest(t, 1000, time.Minute) // unit 2,500 ticks: [25,000, 27,500) is one unit
	h := mix64(42)
	for ts := uint64(25_000); ts <= 27_000; ts += 200 {
		f.TouchAt(h, 0, ts)
	}
	for _, c := range []struct{ window, want uint64 }{{200, 2}, {400, 3}, {2_000, 11}} {
		if got := f.CountAt(h, c.window, 27_000); uint64(got.Count) != c.want {
			t.Errorf("window %d ms: Count %d, want %d (%+v)", c.window, got.Count, c.want, got)
		}
	}
}
