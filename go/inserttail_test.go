package temporalfilter

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
	"time"
)

// TestInsertTail (TF_EXP=1) times first touches of unseen keys, each alone,
// in a 2M-key table larger than the last-level cache filled to a given slot
// load, and splits them by what the insert did: took a free slot, moved
// entries along a cuckoo path (by how many entries moved), or evicted. It
// shows which path makes the unseen-key tail (docs/paper/06-security.md, Timing).
// Each load point pools TF_TABLES (default 1) fresh tables of 40,000 probes,
// and "noise" times an empty call in the same loop: the host's own tail.
func TestInsertTail(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	const capacity, probes = 1 << 21, 40_000
	tables := 1
	fmt.Sscan(os.Getenv("TF_TABLES"), &tables)
	names := []string{"free slot", "moved 1", "moved 2", "moved 3", "moved 4+", "evicted", "noise", "waited", "spin 0.5us"}
	for _, load := range []float64{0.85, 0.90, 0.95} {
		ts := make([][]float64, len(names))
		var all []float64
		for tb := range tables {
			now := uint64(1)
			f, err := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond,
				Clock: func() uint64 { return now }})
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(5+uint64(tb), 6))
			for range int(float64(f.Slots()) * load) {
				f.TouchAt(f.HashUint64(rng.Uint64()), 0, now)
			}
			now += 1000
			hs := make([]uint64, probes)
			for i := range hs {
				hs[i] = f.HashUint64(rng.Uint64())
			}
			for _, h := range hs {
				r0, e0, w0 := f.Relocations(), f.Evictions(), f.Waited()
				t0 := time.Now()
				f.TouchAt(h, 60_000, now)
				d := float64(time.Since(t0).Nanoseconds())
				k := 0
				switch moved := f.Relocations() - r0; {
				case f.Waited() != w0:
					k = 7
				case f.Evictions() != e0:
					k = 5
				case moved > 0:
					k = int(min(moved, 4))
				}
				ts[k] = append(ts[k], d)
				all = append(all, d)
				t0 = time.Now()
				noop()
				ts[6] = append(ts[6], float64(time.Since(t0).Nanoseconds()))
				t0 = time.Now()
				for time.Since(t0) < 500*time.Nanosecond {
				}
				ts[8] = append(ts[8], float64(time.Since(t0).Nanoseconds()))
			}
		}
		for k := range ts {
			slices.Sort(ts[k])
		}
		slices.Sort(all)
		q := func(x []float64, p float64) float64 {
			if len(x) == 0 {
				return 0
			}
			return x[int(p*float64(len(x)-1))]
		}
		over := func(x []float64) float64 { // share over 2 µs, %
			i, _ := slices.BinarySearch(x, 2000)
			return 100 * float64(len(x)-i) / float64(max(1, len(x)))
		}
		t.Logf("load %.2f, %d tables, all: p50/p99/p99.9/max %5.0f/%6.0f/%6.0f/%7.0f ns, over 2 us %.3f%% (spin 0.5us: %.3f%%)", load, tables, q(all, .5), q(all, .99), q(all, .999), q(all, 1), over(all), over(ts[8]))
		for k, name := range names {
			share := 100 * float64(len(ts[k])) / float64(len(all))
			if k == 6 || k == 8 {
				share = 100
			}
			t.Logf("  %-10s %6.2f%%: p50/p99/p99.9/max %5.0f/%6.0f/%6.0f/%7.0f ns, over 2 us %.3f%%", name, share,
				q(ts[k], .5), q(ts[k], .99), q(ts[k], .999), q(ts[k], 1), over(ts[k]))
		}
	}
}

//go:noinline
func noop() {}
