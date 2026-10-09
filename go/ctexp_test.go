package temporalfilter

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

// TestCTPads compares ConstantTime pads on fresh 2M-key tables at 85%:
// fixed pads for both operations, a fixed Touch pad with Count's tracked
// (the default), and AutoPad. 80,000 untimed calls (seen and unseen mixed)
// let the estimates settle, then 40,000 timed ones, each alone; "D" is the
// Kolmogorov-Smirnov distance between seen and unseen timings, so the best
// single-timing guess is right (1+D)/2 of the time (TF_CTEXP=1).
func TestCTPads(t *testing.T) {
	if os.Getenv("TF_CTEXP") == "" {
		t.Skip("set TF_CTEXP=1")
	}
	const capacity, probes = 1 << 21, 40_000
	modes := []struct {
		name string
		pad  time.Duration
		auto bool // Count's pad tracks its p99.5 (capped at pad)
	}{{"off", 0, false}, {"fixed 1us", time.Microsecond, false}, {"fixed 3us", 3 * time.Microsecond, false},
		{"3us, Count auto", 3 * time.Microsecond, true}, {"AutoPad", AutoPad, true}}
	for _, m := range modes {
		line := ""
		for _, op := range []string{"Count", "Touch"} {
			now := uint64(1)
			f, err := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond,
				Clock: func() uint64 { return now }})
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
			if m.pad != 0 {
				f.ct, f.pad = true, m.pad
				f.auto = m.pad == AutoPad
				if !m.auto {
					f.padQ[opCount].q.Store(1 << 62) // Count waits out the fixed pad
				}
			}
			var ts [2][]float64
			var cpu float64
			for round := range 3 {
				kind := make([]int, probes)
				hs := make([]uint64, probes)
				for i := range hs {
					kind[i] = rng.IntN(2)
					hs[i] = f.HashUint64(rng.Uint64())
					if kind[i] == 1 {
						hs[i] = seen[rng.IntN(len(seen))]
					}
				}
				start := time.Now()
				for i, h := range hs {
					t0 := time.Now()
					if op == "Count" {
						f.CountAt(h, 60_000, now)
					} else {
						f.TouchAt(h, 60_000, now)
					}
					if round == 2 {
						ts[kind[i]] = append(ts[kind[i]], float64(time.Since(t0).Nanoseconds()))
					}
				}
				cpu = float64(time.Since(start).Nanoseconds()) / probes
			}
			slices.Sort(ts[0])
			slices.Sort(ts[1])
			q := func(x []float64, p float64) float64 { return x[int(p*float64(len(x)-1))] }
			d := ks(ts[0], ts[1])
			pad := ""
			if f.ct {
				p := time.Duration(f.padQ[map[string]int{"Touch": opTouch, "Count": opCount}[op]].q.Load() / 1000)
				if !f.auto && (op == "Touch" || !m.auto) {
					p = f.pad
				}
				pad = fmt.Sprintf(" pad %4dns", min(p, f.pad).Nanoseconds())
				if f.auto {
					pad = fmt.Sprintf(" pad %4dns", p.Nanoseconds())
				}
			}
			line += fmt.Sprintf(" | %s D %.3f (%4.1f%%) p50 %5.0f/%5.0f, %5.0f ns/op%s", op, d, 50+50*d, q(ts[0], .5), q(ts[1], .5), cpu, pad)
		}
		t.Logf("%-16s%s", m.name, line)
	}
	t.Logf("p50 is unseen/seen; ns/op is the mean wall time per call on one goroutine")
}

// TestCTParallel: 16 goroutines of ConstantTime Counts on 4 cores, plus one
// goroutine timing seen and unseen probes (TF_CTEXP=1).
func TestCTParallel(t *testing.T) {
	if os.Getenv("TF_CTEXP") == "" {
		t.Skip("set TF_CTEXP=1")
	}
	const capacity, workers, per, probes = 1 << 21, 16, 40_000, 20_000
	for _, m := range []struct {
		name string
		pad  time.Duration
		auto bool
	}{{"fixed 3us", 3 * time.Microsecond, false}, {"3us, auto Count", 3 * time.Microsecond, true}} {
		now := uint64(1)
		f, _ := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond, Clock: func() uint64 { return now }})
		rng := rand.New(rand.NewPCG(3, 4))
		seen := make([]uint64, capacity)
		for i := range seen {
			seen[i] = f.HashUint64(rng.Uint64())
			f.TouchAt(seen[i], 0, now)
		}
		now += 1000
		f.ct, f.pad = true, m.pad
		if !m.auto {
			f.padQ[opCount].q.Store(1 << 62) // Count waits out the fixed pad
		}
		var wg sync.WaitGroup
		t0 := time.Now()
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := rand.New(rand.NewPCG(uint64(w), 5))
				for range per {
					h := seen[r.IntN(len(seen))]
					if r.IntN(2) == 0 {
						h = r.Uint64()
					}
					f.CountAt(h, 60_000, now)
				}
			}()
		}
		var ts [2][]float64
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(9, 9))
			for range probes {
				k := r.IntN(2)
				h := f.HashUint64(r.Uint64())
				if k == 1 {
					h = seen[r.IntN(len(seen))]
				}
				s := time.Now()
				f.CountAt(h, 60_000, now)
				ts[k] = append(ts[k], float64(time.Since(s).Nanoseconds()))
			}
		}()
		wg.Wait()
		el := time.Since(t0)
		slices.Sort(ts[0])
		slices.Sort(ts[1])
		t.Logf("%-16s %5.2f M ops/s; probe p50 unseen %6.1f us seen %6.1f us; KS D %.3f (%.1f%%); Count pad %d ns", m.name,
			float64(workers*per+probes)/el.Seconds()/1e6, ts[0][len(ts[0])/2]/1000, ts[1][len(ts[1])/2]/1000, ks(ts[0], ts[1]), 50+50*ks(ts[0], ts[1]), min(f.padQ[opCount].q.Load()/1000, int64(f.pad)))
	}
}
