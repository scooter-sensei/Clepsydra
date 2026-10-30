package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"sort"
	"sync"
	"time"

	tf "github.com/scooter-sensei/clepsydra/go"
	"github.com/scooter-sensei/clepsydra/go/internal/ecm"
)

// Part 2 of the head-to-head (docs/artifact/head-to-head.md): Clepsydra and
// the ECM-sketch at equal memory, count error over 1-60 s windows, on the
// scorecard's velocity workload.

var velWins = []uint64{1_000, 5_000, 15_000, 30_000, 60_000}

type velEv struct {
	t uint64
	k int
}

type velQuery struct {
	now          uint64
	k, wi, exact int
}

// velocityWorkload is tfbench's velocity workload (cmd/tfbench/accuracy.go,
// velocityMAE; claim D5) with the key count as a parameter: the same shapes,
// the same PCG(9, 9) stream and the same order of draws, so at 2,000 keys it
// is that workload exactly. It returns the events in time order, the keys'
// hashes and every query with its exact count over [now-W, now].
func velocityWorkload(nkeys int) ([]velEv, []uint64, []velQuery) {
	r := rand.New(rand.NewPCG(9, 9))
	end := uint64(5 * horizonMs)
	var evs []velEv
	poisson := func(t0, t1, rate float64, k int) {
		for t := t0 + r.ExpFloat64()/rate; t < t1; t += r.ExpFloat64() / rate {
			evs = append(evs, velEv{uint64(t) + 1, k})
		}
	}
	for k := range nkeys {
		switch k % 5 {
		case 0: // steady 0.2-5/s
			poisson(0, float64(end), (0.2+r.Float64()*4.8)/1000, k)
		case 1: // bursts of 5-20 within 200ms, every ~10s
			for t := r.ExpFloat64() * 10_000; t < float64(end); t += r.ExpFloat64() * 10_000 {
				for range 5 + r.IntN(16) {
					evs = append(evs, velEv{uint64(t+r.Float64()*200) + 1, k})
				}
			}
		case 2: // ramp 0.1 -> 10/s
			for t := 0.0; t < float64(end); {
				t += r.ExpFloat64() / ((0.1 + 9.9*t/float64(end)) / 1000)
				evs = append(evs, velEv{uint64(t) + 1, k})
			}
		case 3: // sparse, ~1 per 20s
			poisson(0, float64(end), 1.0/20_000, k)
		case 4: // 5s on at 4/s, 5s off
			for t := 0.0; t < float64(end); t += 10_000 {
				poisson(t, t+5000, 4.0/1000, k)
			}
		}
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].t < evs[j].t })
	hs := make([]uint64, nkeys)
	for k := range hs {
		hs[k] = mix64(uint64(k) + 1<<50)
	}
	times := make([][]uint64, nkeys)
	var qs []velQuery
	i := 0
	for now := uint64(horizonMs); now < end; now++ {
		for i < len(evs) && evs[i].t <= now {
			times[evs[i].k] = append(times[evs[i].k], evs[i].t)
			i++
		}
		k, wi := r.IntN(nkeys), r.IntN(len(velWins))
		ts := times[k]
		exact := len(ts) - sort.Search(len(ts), func(j int) bool { return ts[j]+velWins[wi] >= now })
		qs = append(qs, velQuery{now, k, wi, exact})
	}
	return evs, hs, qs
}

// velErr is one arm's error per window: sum |estimate - exact| and the same
// for its upper bound, over sum exact.
type velErr struct {
	est, up [5]float64
	exact   [5]float64
}

func (e *velErr) per(i int) float64   { return e.est[i] / e.exact[i] }
func (e *velErr) perUp(i int) float64 { return e.up[i] / e.exact[i] }
func (e *velErr) mean() float64 {
	m := 0.0
	for i := range velWins {
		m += e.per(i) / float64(len(velWins))
	}
	return m
}

// replayVel feeds the events to add and answers every query with count, in
// time order.
func replayVel(evs []velEv, hs []uint64, qs []velQuery, add func(h, t uint64), count func(h, w, now uint64) (int, int)) *velErr {
	e := &velErr{}
	i := 0
	for _, q := range qs {
		for i < len(evs) && evs[i].t <= q.now {
			add(hs[evs[i].k], evs[i].t)
			i++
		}
		if count == nil {
			continue
		}
		est, up := count(hs[q.k], velWins[q.wi], q.now)
		e.est[q.wi] += math.Abs(float64(est - q.exact))
		e.up[q.wi] += math.Abs(float64(up - q.exact))
		e.exact[q.wi] += float64(q.exact)
	}
	return e
}

func runClepVel(capacity int, evs []velEv, hs []uint64, qs []velQuery) (*velErr, int) {
	f, err := tf.New(tf.Config{Capacity: capacity, Horizon: horizonMs * time.Millisecond, Resolution: time.Millisecond})
	if err != nil {
		panic(err)
	}
	e := replayVel(evs, hs, qs, func(h, t uint64) { f.CheckAndTouchAt(h, 0, t) }, func(h, w, now uint64) (int, int) {
		r := f.CountAt(h, w, now)
		return f.VelocityCountAt(h, w, now), r.Upper
	})
	return e, f.Bytes()
}

type ecmCfg struct {
	d    int
	eps  float64
	w    int
	seed uint64
	name string
}

func runECM(c ecmCfg, evs []velEv, hs []uint64, qs []velQuery, score bool) (*velErr, int) {
	s := ecm.New(c.w, c.d, c.eps, horizonMs, c.seed)
	var count func(h, w, now uint64) (int, int)
	if score {
		count = func(h, w, now uint64) (int, int) { return s.Count(h, now, w) }
	}
	e := replayVel(evs, hs, qs, s.Add, count)
	return e, s.Peak()
}

// widest finds the largest w whose peak fits the budget (binary search).
func widest(d int, eps float64, budget int, evs []velEv, hs []uint64, qs []velQuery) int {
	lo, hi := 0, budget/d+1 // every counter costs at least 1 B
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if _, peak := runECM(ecmCfg{d: d, eps: eps, w: mid, seed: 1}, evs, hs, qs, false); peak <= budget {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo
}

// paperCfg is the paper's configuration (§4.1) for a total error eps:
// eps_sw = eps_cm = sqrt(1+eps) - 1, w = ceil(e/eps_cm), d = ceil(ln(1/delta)).
func paperCfg(eps, delta float64) ecmCfg {
	e := math.Sqrt(1+eps) - 1
	return ecmCfg{d: int(math.Ceil(math.Log(1 / delta))), eps: e, w: int(math.Ceil(math.E / e)), seed: 1,
		name: fmt.Sprintf("paper eps=%.3g (eps_sw=eps_cm=%.4f)", eps, e)}
}

// paperFit finds the smallest total eps whose paper configuration fits.
func paperFit(budget int, evs []velEv, hs []uint64, qs []velQuery) (ecmCfg, bool) {
	lo, hi := 1e-4, 64.0 // geometric bisection on eps
	if _, peak := runECM(paperCfg(hi, 0.05), evs, hs, qs, false); peak > budget {
		return ecmCfg{}, false
	}
	for range 40 {
		mid := math.Sqrt(lo * hi)
		if _, peak := runECM(paperCfg(mid, 0.05), evs, hs, qs, false); peak <= budget {
			hi = mid
		} else {
			lo = mid
		}
	}
	return paperCfg(hi, 0.05), true
}

func fmtErr(e *velErr) string {
	var b []string
	for i := range velWins {
		b = append(b, fmt.Sprintf("%.4f", e.per(i)))
	}
	return fmt.Sprintf("%s | %.4f", joinBar(b), e.mean())
}

func fmtUp(e *velErr) string {
	var b []string
	m := 0.0
	for i := range velWins {
		b = append(b, fmt.Sprintf("%.4f", e.perUp(i)))
		m += e.perUp(i) / float64(len(velWins))
	}
	return fmt.Sprintf("%s | %.4f", joinBar(b), m)
}

func joinBar(xs []string) string {
	s := ""
	for i, x := range xs {
		if i > 0 {
			s += " | "
		}
		s += x
	}
	return s
}

// parallelDo runs fn(i) for i in [0, n) on every core.
func parallelDo(n int, fn func(i int)) {
	var wg sync.WaitGroup
	next := make(chan int)
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				fn(i)
			}
		}()
	}
	for i := range n {
		next <- i
	}
	close(next)
	wg.Wait()
}

func cmdECM(nkeys, capacity int, mults []int) {
	evs, hs, qs := velocityWorkload(nkeys)
	fmt.Printf("velocity workload: %d keys, %d events, %d queries; windows 1, 5, 15, 30, 60 s\n", nkeys, len(evs), len(qs))
	// Wiring check: with no memory limit (a column per key, on average many)
	// the sketch's error is its histograms' alone, within eps_sw on every
	// window. A harness that queries it wrongly fails here, before any
	// reading (a first run swapped the query's time and range: every
	// estimate read 0, error 1.0000).
	const wireEps = 0.05
	we, wpeak := runECM(ecmCfg{d: 1, eps: wireEps, w: 1 << 22, seed: 1}, evs, hs, qs, true)
	for i := range velWins {
		if we.per(i) > wireEps {
			fail(fmt.Errorf("wiring check: an unconstrained ECM-sketch (eps_sw %.2f) reads %.4f at %ds", wireEps, we.per(i), velWins[i]/1000))
		}
	}
	fmt.Printf("wiring check: ECM-sketch, d=1 w=%d eps_sw=%.2f (no memory limit; peak %d B, %d B without its empty counters): %s\n",
		1<<22, wireEps, wpeak, wpeak-1<<22, fmtErr(we))
	ce, budget := runClepVel(capacity, evs, hs, qs)
	fmt.Printf("\nClepsydra, Capacity %d: %d bytes (%.1f B/key)\n\n", capacity, budget, float64(budget)/float64(nkeys))
	fmt.Println("| arm | memory | 1 s | 5 s | 15 s | 30 s | 60 s | mean |")
	fmt.Println("|---|---|---|---|---|---|---|---|")
	fmt.Printf("| Clepsydra `Count` | %d B | %s |\n", budget, fmtErr(ce))
	fmt.Printf("| Clepsydra `Upper` | %d B | %s |\n", budget, fmtUp(ce))
	ds := []int{1, 2, 3, 4, 5}
	epss := []float64{0.01, 0.02, 0.05, 0.1, 0.2, 0.3, 0.5}
	for _, m := range mults {
		b := m * budget
		var cfgs []ecmCfg
		for _, d := range ds {
			for _, eps := range epss {
				cfgs = append(cfgs, ecmCfg{d: d, eps: eps, seed: 1})
			}
		}
		parallelDo(len(cfgs), func(i int) { cfgs[i].w = widest(cfgs[i].d, cfgs[i].eps, b, evs, hs, qs) })
		cfgs = slices.DeleteFunc(cfgs, func(c ecmCfg) bool { return c.w == 0 })
		if pc, ok := paperFit(b, evs, hs, qs); ok {
			cfgs = append(cfgs, pc)
		}
		errs := make([]*velErr, len(cfgs))
		peaks := make([]int, len(cfgs))
		parallelDo(len(cfgs), func(i int) { errs[i], peaks[i] = runECM(cfgs[i], evs, hs, qs, true) })
		best := 0
		for i := range cfgs {
			if errs[i].mean() < errs[best].mean() {
				best = i
			}
		}
		// The best configuration per window (the rule's comparison).
		var perWin [5]float64
		var perWinCfg [5]int
		for wi := range velWins {
			perWin[wi] = math.Inf(1)
			for i := range cfgs {
				if v := errs[i].per(wi); v < perWin[wi] {
					perWin[wi], perWinCfg[wi] = v, i
				}
			}
		}
		// Seeds of the best configuration.
		seeds := []uint64{1, 2, 3, 4, 5}
		serrs := make([]*velErr, len(seeds))
		parallelDo(len(seeds), func(i int) {
			c := cfgs[best]
			c.seed = seeds[i]
			serrs[i], _ = runECM(c, evs, hs, qs, true)
		})
		var sMin, sMax [5]float64
		for wi := range velWins {
			sMin[wi], sMax[wi] = math.Inf(1), 0
			for _, e := range serrs {
				sMin[wi], sMax[wi] = min(sMin[wi], e.per(wi)), max(sMax[wi], e.per(wi))
			}
		}
		name := func(c ecmCfg) string {
			if c.name != "" {
				return fmt.Sprintf("%s, d=%d w=%d", c.name, c.d, c.w)
			}
			return fmt.Sprintf("d=%d eps_sw=%.2f w=%d", c.d, c.eps, c.w)
		}
		fmt.Printf("| ECM at %dx: best mean (%s) | %d B | %s |\n", m, name(cfgs[best]), peaks[best], fmtErr(errs[best]))
		fmt.Printf("| ECM at %dx: same, upper bound | %d B | %s |\n", m, peaks[best], fmtUp(errs[best]))
		var cells []string
		for wi := range velWins {
			cells = append(cells, fmt.Sprintf("%.4f", perWin[wi]))
		}
		fmt.Printf("| ECM at %dx: best per window | ≤ %d B | %s | – |\n", m, b, joinBar(cells))
		cells = cells[:0]
		for wi := range velWins {
			cells = append(cells, fmt.Sprintf("%.4f–%.4f", sMin[wi], sMax[wi]))
		}
		fmt.Printf("| ECM at %dx: best mean, 5 seeds | %d B | %s | – |\n", m, peaks[best], joinBar(cells))
		for i, c := range cfgs {
			if c.name != "" {
				fmt.Printf("| ECM at %dx: %s | %d B | %s |\n", m, name(c), peaks[i], fmtErr(errs[i]))
			}
		}
		for wi := range velWins {
			fmt.Printf("<!-- %dx window %ds best: %s -->\n", m, velWins[wi]/1000, name(cfgs[perWinCfg[wi]]))
		}
		for i, c := range cfgs {
			fmt.Printf("<!-- %dx all: %s peak %d: %s -->\n", m, name(c), peaks[i], fmtErr(errs[i]))
		}
	}
}

// mix64 is the SplitMix64 finalizer, tfbench's unkeyed hash for the
// velocity workload's keys.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}
