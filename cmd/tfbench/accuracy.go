package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	tf "github.com/scooter-sensei/temporalfilter"
	"github.com/scooter-sensei/temporalfilter/internal/workload"
)

// The accuracy workloads use only the API every commit has had (New,
// CheckAndTouchAt, ContainsAt, VelocityCountAt, HashUint64, Slots), so
// verify/replay.sh can run this file against older commits.

const (
	capacity = 200_000
	horizon  = 60 * time.Second
	hTicks   = 60_000 // ms
)

type churnResult struct {
	fnr, fpr, live          float64
	pos, fn, neg, fp, nlive int
	evictions, relocations  uint64
}

// churn replays the churn stream (internal/workload) through CheckAndTouch
// and scores every answer after the first horizon against exact ground truth.
func churn(load float64, seed uint64) churnResult {
	f, _ := tf.New(tf.Config{Capacity: capacity, Horizon: horizon, Resolution: time.Millisecond})
	last := map[uint64]uint64{}
	var pos, neg, fn, fp int
	var pruned uint64
	workload.Churn(load, seed, capacity, hTicks, tf.HashUint64, func(h, w, now uint64) {
		if now%hTicks == 0 && now != pruned { // keep ground truth bounded
			pruned = now
			for k, t := range last {
				if now-t > 2*hTicks {
					delete(last, k)
				}
			}
		}
		got := f.CheckAndTouchAt(h, w, now)
		lt, ok := last[h]
		want := ok && now-lt <= w
		last[h] = now
		if now < hTicks { // warm-up
			return
		}
		if want {
			pos++
			if !got {
				fn++
			}
		} else {
			neg++
			if got {
				fp++
			}
		}
	})
	live := 0
	for _, t := range last {
		if 4*hTicks-t <= hTicks {
			live++
		}
	}
	return churnResult{float64(fn) / float64(max(pos, 1)), float64(fp) / float64(max(neg, 1)), float64(live) / capacity,
		pos, fn, neg, fp, live, f.Evictions(), relocations(f)}
}

// velocityMAE: 2000 keys with mixed traffic shapes (steady, bursty, ramp,
// sparse, on/off) over 5 horizons; every ~1ms one random key is queried for a
// random window. Returns the mean over windows of sum|estimate-exact| / sum
// exact, so short windows weigh as much as long ones.
func velocityMAE() (float64, []velWin) {
	f, _ := tf.New(tf.Config{Capacity: capacity, Horizon: horizon, Resolution: time.Millisecond})
	r := rand.New(rand.NewPCG(9, 9))
	const nkeys, end = 2000, 5 * hTicks
	type ev struct {
		t uint64
		k int
	}
	var evs []ev
	times := make([][]uint64, nkeys)
	poisson := func(t0, t1, rate float64, k int) {
		for t := t0 + r.ExpFloat64()/rate; t < t1; t += r.ExpFloat64() / rate {
			evs = append(evs, ev{uint64(t) + 1, k})
		}
	}
	for k := range nkeys {
		switch k % 5 {
		case 0: // steady 0.2-5/s
			poisson(0, end, (0.2+r.Float64()*4.8)/1000, k)
		case 1: // bursts of 5-20 within 200ms, every ~10s
			for t := r.ExpFloat64() * 10_000; t < end; t += r.ExpFloat64() * 10_000 {
				for range 5 + r.IntN(16) {
					evs = append(evs, ev{uint64(t+r.Float64()*200) + 1, k})
				}
			}
		case 2: // ramp 0.1 -> 10/s
			for t := 0.0; t < end; {
				t += r.ExpFloat64() / ((0.1 + 9.9*t/end) / 1000)
				evs = append(evs, ev{uint64(t) + 1, k})
			}
		case 3: // sparse, ~1 per 20s
			poisson(0, end, 1.0/20_000, k)
		case 4: // 5s on at 4/s, 5s off
			for t := 0.0; t < end; t += 10_000 {
				poisson(t, t+5000, 4.0/1000, k)
			}
		}
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].t < evs[j].t })
	hs := make([]uint64, nkeys)
	for k := range hs {
		hs[k] = tf.HashUint64(uint64(k) + 1<<50)
	}
	wins := []uint64{1_000, 5_000, 15_000, 30_000, 60_000}
	errSum, exactSum := make([]float64, len(wins)), make([]float64, len(wins))
	i := 0
	for now := uint64(hTicks); now < end; now++ {
		for i < len(evs) && evs[i].t <= now {
			f.CheckAndTouchAt(hs[evs[i].k], 0, evs[i].t)
			times[evs[i].k] = append(times[evs[i].k], evs[i].t)
			i++
		}
		k, wi := r.IntN(nkeys), r.IntN(len(wins))
		w := wins[wi]
		ts := times[k]
		exact := len(ts) - sort.Search(len(ts), func(j int) bool { return ts[j]+w > now })
		got := f.VelocityCountAt(hs[k], w, now)
		errSum[wi] += math.Abs(float64(got - exact))
		exactSum[wi] += float64(exact)
	}
	mean := 0.0
	var per []velWin
	for wi := range wins {
		perWin := errSum[wi] / exactSum[wi]
		mean += perWin / float64(len(wins))
		per = append(per, velWin{wins[wi], errSum[wi], exactSum[wi]})
	}
	return mean, per
}

// velWin is one window's velocity error: sum |estimate-exact| and sum exact
// over its queries (both integers).
type velWin struct {
	w             uint64
	errSum, exact float64
}

// staticFPR: table at capacity, probe never-inserted keys with read-only Contains.
func staticFPR(probes int) int64 {
	f, _ := tf.New(tf.Config{Capacity: capacity, Horizon: horizon, Resolution: time.Millisecond})
	for i := range capacity {
		f.CheckAndTouchAt(tf.HashUint64(uint64(i)), 0, 1)
	}
	// FPR is ~1e-8, so it takes ~1e9 probes to see more than a handful;
	// split them across cores.
	var fp atomic.Int64
	var wg sync.WaitGroup
	workers := runtime.GOMAXPROCS(0)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := 0
			for i := w; i < probes; i += workers {
				if f.ContainsAt(tf.HashUint64(uint64(capacity+i)), hTicks, 2) {
					n++
				}
			}
			fp.Add(int64(n))
		}()
	}
	wg.Wait()
	return fp.Load()
}

// spray: 100 repeat offenders (20 hits each), then 20x capacity of one-off
// keys inside the window. Returns the fraction of offenders still exact.
func spray() int {
	f, _ := tf.New(tf.Config{Capacity: capacity, Horizon: horizon, Resolution: time.Millisecond})
	off := make([]uint64, sprayOffenders)
	for i := range off {
		off[i] = tf.HashUint64(uint64(1<<40 + i))
		for range 20 {
			f.CheckAndTouchAt(off[i], 0, 1)
		}
	}
	for i := range 20 * capacity {
		f.CheckAndTouchAt(tf.HashUint64(uint64(i)), 0, 1+uint64(i%30_000))
	}
	ok := 0
	for _, h := range off {
		if f.VelocityCountAt(h, hTicks, 30_001) == 20 {
			ok++
		}
	}
	return ok
}

const (
	fprProbes      = 1_000_000_000
	sprayOffenders = 100
)

func bytesPerKey() float64 {
	probe, _ := tf.New(tf.Config{Capacity: capacity, Horizon: horizon})
	return float64(probe.Slots()*16) / capacity
}

// printExact prints every deterministic scorecard metric as the raw counts
// it is computed from. The workloads use seeded PCG streams and the unkeyed
// HashUint64, and run single-threaded (static FPR sums per-worker counts), so
// the output is a pure function of the code: two runs, on any amd64 machine
// with the same Go version, must match byte for byte (verify/expected.txt).
func printExact() {
	for _, c := range []struct {
		load float64
		seed uint64
	}{{1.0, 2}, {1.1, 3}, {1.15, 4}} {
		r := churn(c.load, c.seed)
		fmt.Printf("churn load=%.2f seed=%d positives=%d missed=%d negatives=%d false_seen=%d live_at_end=%d evictions=%d relocations=%d  => FN %.3f%% FP %.2e live/capacity %.2f\n",
			c.load, c.seed, r.pos, r.fn, r.neg, r.fp, r.nlive, r.evictions, r.relocations, 100*r.fnr, r.fpr, r.live)
	}
	n := staticFPR(fprProbes)
	fmt.Printf("static_fpr probes=%d false_positives=%d  => %.2e\n", fprProbes, n, float64(n)/fprProbes)
	ok := spray()
	fmt.Printf("spray offenders=%d exact=%d  => %.0f%%\n", sprayOffenders, ok, 100*float64(ok)/sprayOffenders)
	mean, per := velocityMAE()
	for _, v := range per {
		fmt.Printf("velocity window=%ds abs_error_sum=%.0f exact_sum=%.0f  => %.4f\n", v.w/1000, v.errSum, v.exact, v.errSum/v.exact)
	}
	fmt.Printf("velocity mean_of_windows => %.4f\n", mean)
	for _, n := range []int{100, 500, 725, 750, 775, 1000, 1273, 1274, 2617, 5000} {
		b, s := heavyHitter(n)
		fmt.Printf("heavy_hitter touches=%d count_burst=%d count_spread_over_horizon=%d\n", n, b, s)
	}
	probe, _ := tf.New(tf.Config{Capacity: capacity, Horizon: horizon})
	fmt.Printf("memory capacity=%d slots=%d bytes=%d  => %.1f B/key\n", capacity, probe.Slots(), probe.Slots()*16, bytesPerKey())
}

// relocations is f.Relocations(), or 0 on commits that predate it.
func relocations(f any) uint64 {
	if r, ok := f.(interface{ Relocations() uint64 }); ok {
		return r.Relocations()
	}
	return 0
}

// heavyHitter returns what VelocityCount(Horizon) reports for one key after
// n touches: all in one tick (burst), and spread evenly over one horizon
// (queried at the last touch).
func heavyHitter(n int) (burst, spread int) {
	b, _ := tf.New(tf.Config{Capacity: 1000, Horizon: horizon, Resolution: time.Millisecond})
	s, _ := tf.New(tf.Config{Capacity: 1000, Horizon: horizon, Resolution: time.Millisecond})
	h := tf.HashUint64(42)
	var last uint64
	for i := range n {
		b.CheckAndTouchAt(h, 0, hTicks)
		last = hTicks + uint64(i)*(hTicks-1)/uint64(max(n-1, 1))
		s.CheckAndTouchAt(h, 0, last)
	}
	return b.VelocityCountAt(h, hTicks, hTicks), s.VelocityCountAt(h, hTicks, last)
}
