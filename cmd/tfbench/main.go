// Command tfbench is the scorecard for the temporal filter: missed keys,
// false positives, and latency, measured the same way every run so
// iterations are comparable.
//
//	go run ./cmd/tfbench -label "what changed"   # prints and appends to results/history.md
//
// Accuracy is measured against exact ground truth (a map of last-seen times)
// on a churn workload: a stream mixing new keys and repeats, run for 4
// horizons, where every op is CheckAndTouch with a random window.
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tf "github.com/scooter-sensei/temporalfilter"
)

const (
	capacity = 200_000
	horizon  = 60 * time.Second
	hTicks   = 60_000 // ms
)

var windows = []uint64{1_000, 10_000, 60_000}

type churnResult struct{ fnr, fpr, live float64 }

// churn drives a stream whose distinct live keys per horizon ≈ load×capacity.
func churn(load float64, seed uint64) churnResult {
	f, _ := tf.New(tf.Config{Capacity: capacity, Horizon: horizon, Resolution: time.Millisecond})
	r := rand.New(rand.NewPCG(seed, 1))
	const newFrac = 0.6
	ratePerMs := load * capacity / (newFrac * hTicks) / 1.375 // calibrated: live keys per horizon ≈ load×capacity
	type born struct {
		h uint64
		t uint64
	}
	var keys []born
	last := map[uint64]uint64{}
	var next uint64
	var pos, neg, fn, fp int
	for now := uint64(1); now < 4*hTicks; now++ {
		n := int(ratePerMs)
		if r.Float64() < ratePerMs-float64(n) {
			n++
		}
		for range n {
			var h uint64
			if len(keys) == 0 || r.Float64() < newFrac {
				next++
				h = tf.HashUint64(next)
				keys = append(keys, born{h, now})
			} else { // repeat a key born within the last 1.5 horizons
				lo := sort.Search(len(keys), func(i int) bool { return keys[i].t+hTicks*3/2 > now })
				h = keys[lo+r.IntN(len(keys)-lo)].h
			}
			w := windows[r.IntN(len(windows))]
			got := f.CheckAndTouchAt(h, w, now)
			lt, ok := last[h]
			want := ok && now-lt <= w
			last[h] = now
			if now < hTicks { // warm-up
				continue
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
		}
		if now%hTicks == 0 { // keep ground truth bounded
			for k, t := range last {
				if now-t > 2*hTicks {
					delete(last, k)
				}
			}
			cut := 0
			for cut < len(keys) && keys[cut].t+2*hTicks < now {
				cut++
			}
			keys = keys[cut:]
		}
	}
	live := 0
	for _, t := range last {
		if 4*hTicks-t <= hTicks {
			live++
		}
	}
	return churnResult{float64(fn) / float64(max(pos, 1)), float64(fp) / float64(max(neg, 1)), float64(live) / capacity}
}

// velocityMAE: 2000 keys with mixed traffic shapes (steady, bursty, ramp,
// sparse, on/off) over 5 horizons; every ~1ms one random key is queried for a
// random window. Returns the mean over windows of sum|estimate-exact| / sum
// exact, so short windows weigh as much as long ones.
func velocityMAE() float64 {
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
	for wi := range wins {
		perWin := errSum[wi] / exactSum[wi]
		fmt.Fprintf(os.Stderr, "vel MAE %2ds: %.3f\n", wins[wi]/1000, perWin)
		mean += perWin / float64(len(wins))
	}
	return mean
}

// staticFPR: table at capacity, probe never-inserted keys with read-only Contains.
func staticFPR(probes int) float64 {
	f, _ := tf.New(tf.Config{Capacity: capacity, Horizon: horizon, Resolution: time.Millisecond})
	for i := range capacity {
		f.CheckAndTouchAt(tf.HashUint64(uint64(i)), 0, 1)
	}
	fp := 0
	for i := range probes {
		if f.ContainsAt(tf.HashUint64(uint64(capacity+i)), hTicks, 2) {
			fp++
		}
	}
	return float64(fp) / float64(probes)
}

// spray: 100 repeat offenders (20 hits each), then 20x capacity of one-off
// keys inside the window. Returns the fraction of offenders still exact.
func spray() float64 {
	f, _ := tf.New(tf.Config{Capacity: capacity, Horizon: horizon, Resolution: time.Millisecond})
	off := make([]uint64, 100)
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
	return float64(ok) / float64(len(off))
}

const benchKeys = 1 << 20

func filled() (*tf.Filter, []uint64) {
	f, _ := tf.New(tf.Config{Capacity: benchKeys, Horizon: horizon})
	hs := make([]uint64, benchKeys)
	for i := range hs {
		hs[i] = tf.HashUint64(uint64(i))
		f.CheckAndTouchAt(hs[i], 0, 1000)
	}
	return f, hs
}

func nsop(fn func(b *testing.B)) float64 {
	best := 0.0
	for range 3 { // best of 3: least disturbed by noisy neighbours
		r := testing.Benchmark(fn)
		v := float64(r.T.Nanoseconds()) / float64(r.N)
		if best == 0 || v < best {
			best = v
		}
	}
	return best
}

type speeds struct{ hitCold, batch, hitHot, insert, insertSat, velocity, parallel, str float64 }

func speed() speeds {
	f, hs := filled()
	w := f.Ticks(30 * time.Second)
	var s speeds
	s.hitCold = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f.CheckAndTouchAt(hs[i&(benchKeys-1)], w, 2000+uint64(i>>10))
		}
	})
	out := make([]bool, 64)
	s.batch = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i += 64 {
			lo := i & (benchKeys - 1)
			f.CheckAndTouchBatch(hs[lo:lo+64], w, 2000+uint64(i>>10), out)
		}
	})
	s.velocity = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f.VelocityCountAt(hs[i&(benchKeys-1)], w, 2000)
		}
	})
	s.parallel = nsop(func(b *testing.B) {
		var seed atomic.Uint64
		b.RunParallel(func(pb *testing.PB) {
			i := seed.Add(1) * 7919
			for pb.Next() {
				i++
				f.CheckAndTouchAt(hs[i&(benchKeys-1)], w, 2000+i>>10)
			}
		})
	})
	// Steady-state insert: time advances so keys expire as fast as they
	// arrive, holding ~1.0x capacity live.
	var ctr uint64
	perKey := f.Ticks(horizon) / benchKeys
	s.insert = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ctr++
			f.CheckAndTouchAt(tf.HashUint64(benchKeys+ctr), w, 1000+ctr*perKey)
		}
	})
	// Saturated insert: worst case, every insert lands in full lines.
	sat, _ := tf.New(tf.Config{Capacity: benchKeys / 4, Horizon: horizon}) // 512K slots
	for i := range benchKeys {                                             // 2x overfilled
		sat.CheckAndTouchAt(tf.HashUint64(uint64(i)), 0, 1000)
	}
	s.insertSat = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ctr++
			sat.CheckAndTouchAt(tf.HashUint64(1<<40+ctr), w, 1000)
		}
	})
	hot, _ := tf.New(tf.Config{Capacity: 4096, Horizon: horizon})
	for i := range 4096 {
		hot.CheckAndTouchAt(hs[i], 0, 1000)
	}
	s.hitHot = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			hot.CheckAndTouchAt(hs[i&4095], w, 2000+uint64(i>>10))
		}
	})
	keys := make([]string, 1<<16)
	for i := range keys {
		keys[i] = fmt.Sprintf("10.%d.%d.%d", i>>16, i>>8&255, i&255)
		f.CheckAndTouch(keys[i], time.Minute)
	}
	s.str = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f.CheckAndTouch(keys[i&(1<<16-1)], 30*time.Second)
		}
	})
	return s
}

func main() {
	label := flag.String("label", "", "what changed in this iteration")
	quick := flag.Bool("quick", false, "skip speed benchmarks")
	flag.Parse()

	t0 := time.Now()
	commit, _ := exec.Command("git", "-c", "safe.directory=*", "rev-parse", "--short", "HEAD").Output()
	dirty, _ := exec.Command("git", "-c", "safe.directory=*", "status", "--porcelain", "--", "*.go").Output()
	rev := strings.TrimSpace(string(commit))
	if len(dirty) > 0 {
		rev += "+"
	}
	c10, c20, c25 := churn(1.0, 2), churn(1.1, 3), churn(1.15, 4)
	sfpr := staticFPR(50_000_000)
	sp := spray()
	vmae := velocityMAE()
	probe, _ := tf.New(tf.Config{Capacity: capacity, Horizon: horizon})
	bpk := float64(probe.Slots()*16) / capacity
	var s speeds
	if !*quick {
		s = speed()
	}
	row := fmt.Sprintf("| %s | %s | %.3f%% | %.3f%% | %.3f%% | %.2e | %.2e | %.0f%% | %.1f | %.3f | %.0f | %.0f | %.0f | %.0f | %.0f | %.0f | %.1f | %.0f |",
		time.Now().UTC().Format("01-02 15:04"), rev,
		100*c10.fnr, 100*c20.fnr, 100*c25.fnr, c25.fpr, sfpr, 100*sp, bpk, vmae,
		s.hitCold, s.batch, s.hitHot, s.insert, s.insertSat, s.velocity, s.parallel, s.str)
	if *label != "" {
		row += " " + *label + " |"
	} else {
		row += " |"
	}
	fmt.Println(header)
	fmt.Println(row)
	fmt.Fprintf(os.Stderr, "(%.0fs; live keys/capacity: %.2f %.2f %.2f)\n", time.Since(t0).Seconds(), c10.live, c20.live, c25.live)
	if *label != "" && !*quick {
		path := "results/history.md"
		_ = os.MkdirAll("results", 0o755)
		if _, err := os.Stat(path); err != nil {
			os.WriteFile(path, []byte(intro+header+"\n"), 0o644)
		}
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintln(fh, row)
			fh.Close()
		}
	}
}

const intro = `# Temporal filter scorecard history

Produced by ` + "`go run ./cmd/tfbench -label ...`" + `. Capacity 200k, horizon 60s.
FN = missed keys on the churn workload at 1.0x / 2.0x / 2.5x the configured Capacity (the table is sized for ~38% slot load at 1.0x, ~95% at 2.5x) (ground truth: exact map).
FP churn = false "seen" on the churn workload at the last load point. Static FPR = 50M never-inserted probes at capacity.
Spray = repeat offenders keeping exact counts after 20x-capacity spray. Latencies in ns/op (best of 3),
1M-key table; batch = CheckAndTouchBatch per key; insert = steady state (keys expire as fast as they arrive), insert sat = every line full; parallel is per-op throughput with GOMAXPROCS goroutines; str = string key + built-in clock.

`

const header = `| when (UTC) | rev | FN 1.0x | FN 1.1x | FN 1.15x | FP churn 1.15x | static FPR | spray | B/key | vel MAE | hit cold | batch | hit hot | insert | insert sat | velocity | parallel | str | change |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|`
