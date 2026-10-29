package temporalfilter

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"sort"
	"testing"
	"time"
)

// TestVelocityBound (TF_EXP=1) splits the velocity error (tfbench's vel MAE)
// into what the register's information allows and what the estimator adds.
// It replays tfbench's velocity workload (cmd/tfbench/accuracy.go,
// velocityMAE: 2,000 keys in five traffic shapes, one query per ms for
// four horizons) and scores, per window, sum|estimate - exact| / sum exact:
//
//   - filter: VelocityCountAt (must equal tfbench's figures);
//   - spread: estimate's interpolation as it was until 395f738, every
//     event of the cut bucket spread uniformly;
//   - anchored: the newest bucket's last event, which lies exactly at the
//     slot's lastTouch, counts whole and only the other events are spread
//     (what estimate does now, re-derived from the key's register: it must
//     equal filter);
//   - median: anchored, and the cut bucket's share is the median of
//     Binomial(events, window's share of the bucket), the best guess under
//     absolute error if events are uniform within a bucket;
//   - model floor: the expected |error| of that median under the same
//     model, i.e. what no estimator reading this register can avoid on
//     Poisson traffic;
//   - flat u: exact counts in aligned buckets of width u everywhere (no
//     counter or bucket limits), anchored, uniform in the cut bucket: what
//     a register with resolution u near now could reach.
func TestVelocityBound(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	const capacity, hTicks = 200_000, 60_000
	f, err := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// tfbench's velocity workload, same seed and order of draws.
	r := rand.New(rand.NewPCG(9, 9))
	const nkeys, end = 2000, 5 * hTicks
	type ev struct {
		t uint64
		k int
	}
	var evs []ev
	poisson := func(t0, t1, rate float64, k int) {
		for t := t0 + r.ExpFloat64()/rate; t < t1; t += r.ExpFloat64() / rate {
			evs = append(evs, ev{uint64(t) + 1, k})
		}
	}
	for k := range nkeys {
		switch k % 5 {
		case 0:
			poisson(0, end, (0.2+r.Float64()*4.8)/1000, k)
		case 1:
			for t := r.ExpFloat64() * 10_000; t < end; t += r.ExpFloat64() * 10_000 {
				for range 5 + r.IntN(16) {
					evs = append(evs, ev{uint64(t+r.Float64()*200) + 1, k})
				}
			}
		case 2:
			for t := 0.0; t < end; {
				t += r.ExpFloat64() / ((0.1 + 9.9*t/end) / 1000)
				evs = append(evs, ev{uint64(t) + 1, k})
			}
		case 3:
			poisson(0, end, 1.0/20_000, k)
		case 4:
			for t := 0.0; t < end; t += 10_000 {
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
	wins := []uint64{1_000, 5_000, 15_000, 30_000, 60_000}
	flat := []uint64{2500, 1250, 625, 250, 100} // H/24 (today's unit), H/48, H/96, H/240, H/600
	names := []string{"filter", "spread (until 10-22)", "anchored (re-derived)", "median (anchored)", "model floor"}
	for _, u := range flat {
		names = append(names, "flat "+time.Duration(u*uint64(time.Millisecond)).String())
	}
	errSum := make([][]float64, len(names))
	for i := range errSum {
		errSum[i] = make([]float64, len(wins))
	}
	exactSum := make([]float64, len(wins))
	round := func(est float64, gated bool) float64 { // estimate's own rounding: at least 1 once lastTouch is in the window
		if !gated {
			return 0
		}
		return float64(max(1, int(est+0.5)))
	}
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
		exact := float64(len(ts) - sort.Search(len(ts), func(j int) bool { return ts[j]+w >= now }))
		exactSum[wi] += exact
		got := []float64{float64(f.VelocityCountAt(hs[k], w, now))}
		w0, w1, src := f.read(hs[k], now)
		last := w0 & tsMask
		var v view
		ok := src != 0 && age(now, last) <= f.horizon
		if ok {
			v, ok = f.viewOf(f.stashID(hs[k], fingerprint(hs[k]), tag(hs[k])), w1, last)
		}
		in := ok && age(now, last) <= min(w, f.horizon)
		for _, mode := range []int{0, 1, 2} {
			est, mad := 0.0, 0.0
			if in {
				est, mad = f.regEstimate(v, last, now, w, mode >= 1, mode == 2)
			}
			got = append(got, round(est, in))
			if mode == 2 {
				got = append(got, mad) // the floor is an expected |error|, scored as is
			}
		}
		for _, u := range flat {
			got = append(got, idealEstimate(ts, u, now, w))
		}
		for e, g := range got {
			if names[e] == "model floor" {
				errSum[e][wi] += g
				continue
			}
			errSum[e][wi] += math.Abs(g - exact)
		}
	}
	t.Logf("normalized MAE (sum|est-exact| / sum exact), tfbench's velocity workload")
	t.Logf("%-22s %7s %7s %7s %7s %7s %7s", "estimator", "1s", "5s", "15s", "30s", "60s", "mean")
	for e, name := range names {
		line, mean := "", 0.0
		for wi := range wins {
			x := errSum[e][wi] / exactSum[wi]
			mean += x / float64(len(wins))
			line += " " + pad7(x)
		}
		t.Logf("%-22s%s %s", name, line, pad7(mean))
	}
	if a, b := errSum[0], errSum[2]; !slices.Equal(a, b) {
		t.Errorf("re-derived estimate differs from the filter's: %v vs %v", b, a)
	}
}

func pad7(x float64) string { return fmt.Sprintf("%7.4f", x) }

// regEstimate is estimate's interpolation (register.go) for a register view,
// with two options: anchor (the newest bucket's last event, at lastTouch,
// counts whole) and median (the cut bucket's share is the median of
// Binomial(events, share) instead of its mean). mad is the expected
// |error| of that median under the uniform model.
func (f *Filter) regEstimate(v view, last, now, window uint64, anchor, median bool) (est, mad float64) {
	window = min(window, f.horizon)
	e := v.e
	unit := float64(f.unit)
	from := float64(now) - float64(window)
	top := float64(e+1) * unit
	newest := true
	for j := range nBuckets {
		lo := float64(bucketStart(e, j)) * unit
		c := v.c[j]
		hi := top
		top = lo
		if c == 0 {
			if from >= lo {
				break
			}
			continue
		}
		a := 0.0
		if newest {
			hi = max(lo, min(float64(last), hi))
			newest = false
			if anchor && from < hi {
				a = 1
			}
		}
		if from <= lo {
			est += float64(c)
			continue
		}
		if from < hi {
			p := (hi - from) / (hi - lo)
			if median {
				m, d := binomMedian(int(c)-int(a), p)
				est += a + float64(m)
				mad += d
			} else {
				est += a + (float64(c)-a)*p
			}
		}
		break
	}
	return est, mad
}

// binomMedian returns a median of Binomial(n, p) and E|X - median|.
func binomMedian(n int, p float64) (int, float64) {
	if n <= 0 || p <= 0 {
		return 0, 0
	}
	if p >= 1 {
		return n, 0
	}
	ln, _ := math.Lgamma(float64(n + 1))
	lp, lq := math.Log(p), math.Log1p(-p)
	pmf := make([]float64, n+1)
	for k := range pmf {
		a, _ := math.Lgamma(float64(k + 1))
		b, _ := math.Lgamma(float64(n - k + 1))
		pmf[k] = math.Exp(ln - a - b + float64(k)*lp + float64(n-k)*lq)
	}
	m, cum := n, 0.0
	for k, q := range pmf {
		if cum += q; cum >= 0.5 {
			m = k
			break
		}
	}
	d := 0.0
	for k, q := range pmf {
		d += q * math.Abs(float64(k-m))
	}
	return m, d
}

// idealEstimate counts h's events (ts, sorted) in [now-w, now] from exact
// counts in aligned buckets of width u: whole buckets exactly, the bucket
// the window's edge cuts uniformly, with the newest event counted whole
// when that bucket is the newest. Gated and rounded as estimate is.
func idealEstimate(ts []uint64, u, now, w uint64) float64 {
	if len(ts) == 0 || now-ts[len(ts)-1] > w {
		return 0
	}
	last := ts[len(ts)-1]
	from := now - min(w, now)
	bs := from / u * u
	be := bs + u
	after := len(ts) - sort.Search(len(ts), func(j int) bool { return ts[j] >= be })
	nB := len(ts) - after - sort.Search(len(ts), func(j int) bool { return ts[j] >= bs })
	est := float64(after)
	if nB > 0 {
		hi, a := float64(be), 0.0
		if last < be {
			hi, a = float64(last), 1
		}
		if hi > float64(bs) {
			est += a + (float64(nB)-a)*(hi-float64(from))/(hi-float64(bs))
		} else {
			est += float64(nB)
		}
	}
	return float64(max(1, int(est+0.5)))
}
