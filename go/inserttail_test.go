package temporalfilter

import (
	"math/rand/v2"
	"os"
	"slices"
	"testing"
	"time"
)

// TestInsertTail (TF_EXP=1) times first touches of unseen keys, each alone,
// in a 2M-key table larger than the last-level cache filled to a given slot
// load, and splits them by what the insert did: took a free slot, moved
// entries along a cuckoo path, or evicted. It shows which path makes the
// unseen-key tail (docs/SECURITY.md, Timing).
func TestInsertTail(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	const capacity, probes = 1 << 21, 40_000
	for _, load := range []float64{0.85, 0.90, 0.95} {
		now := uint64(1)
		f, err := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond,
			Clock: func() uint64 { return now }})
		if err != nil {
			t.Fatal(err)
		}
		rng := rand.New(rand.NewPCG(5, 6))
		for range int(float64(f.Slots()) * load) {
			f.TouchAt(f.HashUint64(rng.Uint64()), 0, now)
		}
		now += 1000
		hs := make([]uint64, probes)
		for i := range hs {
			hs[i] = f.HashUint64(rng.Uint64())
		}
		var ts [3][]float64 // free slot, moved, evicted
		for _, h := range hs {
			r0, e0 := f.Relocations(), f.Evictions()
			t0 := time.Now()
			f.TouchAt(h, 60_000, now)
			d := float64(time.Since(t0).Nanoseconds())
			k := 0
			if f.Evictions() != e0 {
				k = 2
			} else if f.Relocations() != r0 {
				k = 1
			}
			ts[k] = append(ts[k], d)
		}
		var all []float64
		for k := range ts {
			all = append(all, ts[k]...)
			slices.Sort(ts[k])
		}
		slices.Sort(all)
		q := func(x []float64, p float64) float64 {
			if len(x) == 0 {
				return 0
			}
			return x[int(p*float64(len(x)-1))]
		}
		t.Logf("load %.2f all: p50/p99/p99.9/max %5.0f/%6.0f/%6.0f/%7.0f ns", load, q(all, .5), q(all, .99), q(all, .999), q(all, 1))
		for k, name := range []string{"free slot", "moved", "evicted"} {
			t.Logf("  %-9s %5.2f%%: p50/p99/p99.9/max %5.0f/%6.0f/%6.0f/%7.0f ns", name, 100*float64(len(ts[k]))/probes,
				q(ts[k], .5), q(ts[k], .99), q(ts[k], .999), q(ts[k], 1))
		}
	}
}
