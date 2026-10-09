package temporalfilter

import (
	"math/rand/v2"
	"os"
	"slices"
	"testing"
	"time"
)

// TestTimingLeak measures how well one request's response time tells a seen
// key from an unseen one, without ConstantTime and with a 3 µs pad. A table at design
// load (85%) larger than the last-level cache; seen and unseen probes in
// random order, each timed alone. It reports each side's median and the
// Kolmogorov-Smirnov distance D between the two latency distributions: an
// observer who classifies by a threshold on one timing is right at most
// (1+D)/2 of the time. Set TF_TIMING=1 to run.
func TestTimingLeak(t *testing.T) {
	if os.Getenv("TF_TIMING") == "" {
		t.Skip("set TF_TIMING=1")
	}
	const capacity, probes = 1 << 21, 40_000
	for _, ct := range []time.Duration{0, 3 * time.Microsecond} {
		for _, op := range []string{"Count", "Touch"} {
			now := uint64(1)
			f, err := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond,
				Clock: func() uint64 { return now }, ConstantTime: ct})
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(3, 4))
			seen := make([]uint64, capacity)
			for i := range seen {
				seen[i] = f.HashUint64(rng.Uint64())
				f.TouchAt(seen[i], 0, now)
			}
			now += 1000
			// Probes are drawn up front and read in order, so fetching one
			// doesn't add a cache miss to the timed call.
			kind := make([]int, probes)
			hs := make([]uint64, probes)
			for i := range hs {
				kind[i] = rng.IntN(2)
				hs[i] = f.HashUint64(rng.Uint64())
				if kind[i] == 1 {
					hs[i] = seen[rng.IntN(len(seen))]
				}
			}
			var ts [2][]float64 // [unseen, seen]
			for i, h := range hs {
				t0 := time.Now()
				if op == "Count" {
					f.CountAt(h, 60_000, now)
				} else {
					f.TouchAt(h, 60_000, now)
				}
				ts[kind[i]] = append(ts[kind[i]], float64(time.Since(t0).Nanoseconds()))
			}
			slices.Sort(ts[0])
			slices.Sort(ts[1])
			q := func(x []float64, p float64) float64 { return x[int(p*float64(len(x)-1))] }
			t.Logf("ConstantTime %-5v %-5s: unseen p50/p99/p99.9 %4.0f/%5.0f/%5.0f ns, seen %4.0f/%5.0f/%5.0f ns; KS D = %.3f (best single-timing guess right %.1f%%)",
				ct, op, q(ts[0], .5), q(ts[0], .99), q(ts[0], .999), q(ts[1], .5), q(ts[1], .99), q(ts[1], .999), ks(ts[0], ts[1]), 50+50*ks(ts[0], ts[1]))
		}
	}
}

// ks is the Kolmogorov-Smirnov distance between two sorted samples.
func ks(a, b []float64) float64 {
	d := 0.0
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		x := min(a[i], b[j])
		for i < len(a) && a[i] <= x {
			i++
		}
		for j < len(b) && b[j] <= x {
			j++
		}
		d = max(d, abs(float64(i)/float64(len(a))-float64(j)/float64(len(b))))
	}
	return d
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
