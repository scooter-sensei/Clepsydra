package temporalfilter

import (
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"testing"
	"time"
)

// velWorkload replays tfbench's velocity workload (cmd/tfbench/accuracy.go,
// velocityMAE): same seed, same order of draws. visit is called once per
// query with the key's event times so far (sorted), the query's window
// index and now; touch once per event, before the queries that see it.
func velWorkload(touch func(k int, t uint64), visit func(k int, ts []uint64, wi int, now uint64)) {
	const hTicks = 60_000
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
	times := make([][]uint64, nkeys)
	i := 0
	for now := uint64(hTicks); now < end; now++ {
		for i < len(evs) && evs[i].t <= now {
			touch(evs[i].k, evs[i].t)
			times[evs[i].k] = append(times[evs[i].k], evs[i].t)
			i++
		}
		k, wi := r.IntN(nkeys), r.IntN(5)
		visit(k, times[k], wi, now)
	}
}

var velWins = []uint64{1_000, 5_000, 15_000, 30_000, 60_000}

// geom is an aligned register geometry: unit in ticks, and per bucket
// (newest first) its alignment step 2^logStep and offset, in units, as
// register.go's bucketLogStep and bucketOff.
type geom struct {
	unit    uint64
	logStep []uint
	off     []int64
}

func (g geom) start(u int64, j int) int64 { return u&^(1<<g.logStep[j]-1) - g.off[j] }

// simBucket is one bucket of a model's state: [lo, hi) in ticks, c events.
type simBucket struct {
	lo, hi float64
	c      int
}

// buckets lays ts (events up to last) into g's buckets at the epoch of
// last's unit, as the register holds them (no counter limits). A touch more
// than a horizon after the one before starts the register afresh (bump's
// reset of a stale slot), so only events since the last such gap count.
func (g geom) buckets(ts []uint64) []simBucket {
	for i := len(ts) - 1; i > 0; i-- {
		if ts[i]-ts[i-1] > 60_000 {
			ts = ts[i:]
			break
		}
	}
	last := ts[len(ts)-1]
	e := int64(last / g.unit)
	out := make([]simBucket, len(g.logStep))
	top := float64(e+1) * float64(g.unit)
	for j := range out {
		lo := float64(g.start(e, j)) * float64(g.unit)
		out[j] = simBucket{lo: lo, hi: top}
		top = lo
	}
	for _, t := range ts {
		for j := range out {
			if float64(t) >= out[j].lo {
				out[j].c++
				break
			}
		}
	}
	return out
}

// simEstimate is register.go's estimate over a bucket list (newest first):
// the newest non-empty bucket ends at last, which holds one event counted
// whole; the cut bucket is spread uniformly. known lists exact event times
// (newest first) taken out of their buckets' counts and counted exactly;
// onsets[j] >= 0 is a lower bound on bucket j's events (its first one lies
// in [onsets[j], onsets[j]+onStep)).
func simEstimate(bs []simBucket, last, now, w uint64, known []uint64, onsets []float64, onStep float64) float64 {
	if now-last > w {
		return 0
	}
	from := float64(now) - float64(w)
	est := 0.0
	for _, t := range known {
		if float64(t) >= from {
			est++
		}
	}
	newest := true
	for j, b := range bs {
		c := float64(b.c)
		lo, hi := b.lo, b.hi
		nk := 0.0
		for _, t := range known {
			if float64(t) >= lo && float64(t) < hi {
				nk++
			}
		}
		c -= nk
		if b.c == 0 {
			if from >= lo {
				break
			}
			continue
		}
		a := 0.0
		if newest {
			newest = false
			hi = math.Max(lo, math.Min(float64(last), hi))
			if nk == 0 && from < hi {
				a = 1 // the event at last, counted whole
			}
		}
		if len(known) > 0 {
			hi = math.Min(hi, float64(known[len(known)-1])) // older events lie before the oldest known one
			if hi < lo {
				hi = lo
			}
		}
		if c-a > 0 && onsets != nil && onsets[j] >= 0 {
			// one event in [on, on+step), the others in [on, hi]
			on := math.Max(lo, onsets[j])
			if from <= on {
				est += c
				if from <= lo {
					continue
				}
				break
			}
			rest := c - a
			p := 0.0
			if hi > on {
				p = math.Max(0, math.Min(1, (hi-from)/(hi-on)))
			}
			est += a + rest*p
			if from > lo {
				break
			}
			continue
		}
		if from <= lo {
			est += c
			continue
		}
		if from < hi {
			est += a + (c-a)*(hi-from)/(hi-lo)
		}
		break
	}
	return est
}

func simRound(est float64, in bool) float64 {
	if !in {
		return 0
	}
	return float64(max(1, int(est+0.5)))
}

// TestVelocityInformation (TF_EXP=1) asks what information about a key's
// recent events target 7 (velocity error <= 0.05, tfbench's vel MAE) needs,
// beyond TestVelocityBound's model floor. It replays tfbench's velocity
// workload and scores models built from each key's exact event times: the
// register as laid out (no counter limits; it must equal the filter), the
// register plus side information a few more bits could carry (the offset of
// each bucket's first event, the exact times of the key's last m events),
// and wider geometries. Each model is estimate's interpolation (register.go)
// with the side information used where it applies. Then the filter's error
// by traffic shape, as a share of each window's total.
func TestVelocityInformation(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	f, err := New(Config{Capacity: 200_000, Horizon: time.Minute, Resolution: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	cur := geom{2500, []uint{0, 0, 1, 2, 3, 3, 3}, []int64{0, 1, 2, 4, 8, 16, 24}}
	type model struct {
		name string
		est  func(ts []uint64, now, w uint64) float64
	}
	onset := func(g geom, bits int, all bool) func(ts []uint64, now, w uint64) float64 {
		return func(ts []uint64, now, w uint64) float64 {
			last := ts[len(ts)-1]
			bs := g.buckets(ts)
			on := make([]float64, len(bs))
			step := float64(g.unit) / float64(int(1)<<bits)
			for j := range on {
				on[j] = -1
				if j > 0 && !all {
					continue
				}
				for _, x := range ts { // first event in bucket j
					if float64(x) >= bs[j].lo && float64(x) < bs[j].hi {
						width := bs[j].hi - bs[j].lo
						s := step * width / float64(g.unit)
						if bits >= 30 {
							on[j] = float64(x)
						} else {
							on[j] = bs[j].lo + math.Floor((float64(x)-bs[j].lo)/s)*s
						}
						break
					}
				}
			}
			s := step
			if bits >= 30 {
				s = 0
			}
			return simRound(simEstimate(bs, last, now, w, nil, on, s), now-last <= w)
		}
	}
	lastM := func(g geom, m int) func(ts []uint64, now, w uint64) float64 {
		return func(ts []uint64, now, w uint64) float64 {
			last := ts[len(ts)-1]
			var known []uint64
			for i := len(ts) - 1; i >= 0 && len(known) < m; i-- {
				known = append(known, ts[i])
			}
			return simRound(simEstimate(g.buckets(ts), last, now, w, known, nil, 0), now-last <= w)
		}
	}
	plain := func(g geom) func(ts []uint64, now, w uint64) float64 {
		return func(ts []uint64, now, w uint64) float64 {
			last := ts[len(ts)-1]
			return simRound(simEstimate(g.buckets(ts), last, now, w, nil, nil, 0), now-last <= w)
		}
	}
	// H/48 near now, 8 buckets to cover the horizon (sized as register.go
	// sizes its own, at 15 events per 1.25 s: 4, 4, 5, 6, 7, 8, 8, 8 = 50
	// bits); and H/24 with two buckets per width (5, 5, 5, 6, 6, 7, 7, 8, 8
	// = 57 bits).
	h48 := geom{1250, []uint{0, 0, 1, 2, 3, 4, 4, 4}, []int64{0, 1, 2, 4, 8, 16, 32, 48}}
	h24x9 := geom{2500, []uint{0, 0, 0, 1, 1, 2, 2, 3, 3}, []int64{0, 1, 2, 3, 5, 7, 11, 15, 23}}
	models := []model{
		{"register", plain(cur)},
		{"+onset newest 2b", onset(cur, 2, false)},
		{"+onset newest 4b", onset(cur, 4, false)},
		{"+onset each 3b", onset(cur, 3, true)},
		{"+onset each exact", onset(cur, 30, true)},
		{"+last 2 exact", lastM(cur, 2)},
		{"+last 4 exact", lastM(cur, 4)},
		{"+last 8 exact", lastM(cur, 8)},
		{"H/48, 8 buckets", plain(h48)},
		{"H/24, 9 buckets", plain(h24x9)},
	}
	const nshapes = 5
	nm := len(models) + 1
	errSum := make([][nshapes][5]float64, nm)
	var exactSum [nshapes][5]float64
	hs := make([]uint64, 2000)
	for k := range hs {
		hs[k] = mix64(uint64(k) + 1<<50)
	}
	velWorkload(func(k int, tk uint64) { f.CheckAndTouchAt(hs[k], 0, tk) }, func(k int, ts []uint64, wi int, now uint64) {
		w := velWins[wi]
		exact := float64(len(ts) - sort.Search(len(ts), func(j int) bool { return ts[j]+w >= now }))
		sh := k % 5
		exactSum[sh][wi] += exact
		errSum[0][sh][wi] += math.Abs(float64(f.VelocityCountAt(hs[k], w, now)) - exact)
		if len(ts) == 0 {
			return
		}
		for m, md := range models {
			errSum[m+1][sh][wi] += math.Abs(md.est(ts, now, w) - exact)
		}
	})
	names := []string{"filter"}
	for _, m := range models {
		names = append(names, m.name)
	}
	t.Logf("normalized MAE (sum|est-exact| / sum exact), tfbench's velocity workload")
	t.Logf("%-20s %7s %7s %7s %7s %7s %7s", "model", "1s", "5s", "15s", "30s", "60s", "mean")
	for m, name := range names {
		line, mean := "", 0.0
		for wi := range 5 {
			e, x := 0.0, 0.0
			for sh := range nshapes {
				e += errSum[m][sh][wi]
				x += exactSum[sh][wi]
			}
			mean += e / x / 5
			line += " " + pad7(e/x)
		}
		t.Logf("%-20s%s %s", name, line, pad7(mean))
	}
	t.Logf("the filter's error by traffic shape, share of each window's total")
	shapes := []string{"steady", "bursts", "ramp", "sparse", "on/off"}
	for sh := range nshapes {
		line := ""
		for wi := range 5 {
			tot := 0.0
			for s := range nshapes {
				tot += errSum[0][s][wi]
			}
			line += " " + pad7(errSum[0][sh][wi]/tot)
		}
		t.Logf("%-20s%s", shapes[sh], line)
	}
	for wi := range 5 {
		for sh := range nshapes {
			if errSum[0][sh][wi] != errSum[1][sh][wi] {
				t.Errorf("the register model differs from the filter: shape %d window %d: %v vs %v", sh, wi, errSum[1][sh][wi], errSum[0][sh][wi])
			}
		}
	}
}
