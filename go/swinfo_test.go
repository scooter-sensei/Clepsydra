package temporalfilter

// lab/sw-2026-11, arm W1: what information the short windows need (a model;
// no library change). See SW-RESULTS.md.

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// swWorkload is velWorkload (tfbench's velocity workload) with its seed as
// parameters; (9, 9) is tfbench's own.
func swWorkload(s1, s2 uint64, touch func(k int, t uint64), visit func(k int, ts []uint64, wi int, now uint64)) {
	const hTicks = 60_000
	r := rand.New(rand.NewPCG(s1, s2))
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

// swQuery is one query of the replay: key, how many of its events precede
// it, window index, now, the exact count and the filter's Count.
type swQuery struct {
	k, n, wi      int
	now           uint64
	exact, filter float64
}

type swRun struct {
	times [][]uint64
	qs    []swQuery
}

// swRecord replays the workload once through the filter and records every
// query, so models can be scored in parallel afterwards.
func swRecord(t testing.TB, s1, s2 uint64) *swRun {
	f, err := New(Config{Capacity: 200_000, Horizon: time.Minute, Resolution: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	hs := make([]uint64, 2000)
	for k := range hs {
		hs[k] = mix64(uint64(k) + 1<<50)
	}
	run := &swRun{times: make([][]uint64, 2000)}
	swWorkload(s1, s2, func(k int, tk uint64) {
		f.CheckAndTouchAt(hs[k], 0, tk)
		run.times[k] = append(run.times[k], tk)
	}, func(k int, ts []uint64, wi int, now uint64) {
		w := velWins[wi]
		exact := float64(len(ts) - sort.Search(len(ts), func(j int) bool { return ts[j]+w >= now }))
		run.qs = append(run.qs, swQuery{k, len(ts), wi, now, exact, float64(f.VelocityCountAt(hs[k], w, now))})
	})
	return run
}

// swModel estimates a key's count in [now-w, now] from its events ts
// (non-empty, sorted, the last at the slot's last touch); bits is what the
// model stores beyond the register and its onset (-1: exact values, not
// priced).
type swModel struct {
	name string
	bits int
	est  func(ts []uint64, now, w uint64) float64
}

type swScore struct {
	err, exact [5]float64
}

func (s swScore) win(wi int) float64 { return s.err[wi] / s.exact[wi] }
func (s swScore) mean() float64 {
	m := 0.0
	for wi := range 5 {
		m += s.win(wi) / 5
	}
	return m
}

// swScoreAll scores every model over run's queries, in parallel; the
// result's last entry is the filter itself.
func swScoreAll(run *swRun, models []swModel) []swScore {
	out := make([]swScore, len(models)+1)
	for _, q := range run.qs {
		out[len(models)].err[q.wi] += math.Abs(q.filter - q.exact)
		out[len(models)].exact[q.wi] += q.exact
	}
	var wg sync.WaitGroup
	next := make(chan int)
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range next {
				var s swScore
				for _, q := range run.qs {
					s.exact[q.wi] += q.exact
					if q.n == 0 {
						continue
					}
					s.err[q.wi] += math.Abs(models[m].est(run.times[q.k][:q.n], q.now, velWins[q.wi]) - q.exact)
				}
				out[m] = s
			}
		}()
	}
	for m := range models {
		next <- m
	}
	close(next)
	wg.Wait()
	return out
}

// swSpan is the part of ts the register holds: events within two horizons
// of the last one, after the last gap of more than a horizon (bump resets a
// stale slot's register). geom.buckets of it equals geom.buckets of ts.
func swSpan(ts []uint64) []uint64 {
	L := ts[len(ts)-1]
	i0 := sort.Search(len(ts), func(i int) bool { return ts[i]+120_000 >= L })
	for i := len(ts) - 1; i > i0; i-- {
		if ts[i]-ts[i-1] > 60_000 {
			return ts[i:]
		}
	}
	return ts[i0:]
}

var swCur = geom{2500, []uint{0, 0, 1, 2, 3, 3, 3}, []int64{0, 1, 2, 4, 8, 16, 24}}

// swIval is c events spread uniformly over [lo, hi] (lo == hi: all at lo).
type swIval struct{ lo, hi, c float64 }

func swCount(iv []swIval, from float64) float64 {
	est := 0.0
	for _, b := range iv {
		switch {
		case b.c <= 0:
		case from <= b.lo:
			est += b.c
		case from < b.hi:
			est += b.c * (b.hi - from) / (b.hi - b.lo)
		}
	}
	return est
}

// swOnset is where bucket 0's first event lies, quantized to 2^bits parts
// of the unit (the filter: 4 bits), or bucket 0's start with bits 0.
func swOnset(span []uint64, b0 simBucket, unit float64, bits int) float64 {
	if bits <= 0 {
		return b0.lo
	}
	s := unit / float64(int(1)<<bits)
	for _, x := range span {
		if float64(x) >= b0.lo {
			return b0.lo + math.Floor((float64(x)-b0.lo)/s)*s
		}
	}
	return b0.lo
}

// swLogged is one event of a log of recent events: its true time (which
// bucket holds it) and the interval its stored form places it in.
type swLogged struct {
	t      uint64
	lo, hi float64
}

// swIvals turns the register (its buckets bs over span) into intervals:
// the event at the last touch as a point; bucket 0's other events over
// [onset, last]; logged events as intervals of one event, taken out of
// their buckets, whose other events then lie below hiCap; sub > 1 splits
// the newest unit (and with two, the one before) into sub aligned parts
// with their own counts.
func swIvals(span []uint64, bs []simBucket, unit float64, onBits int, logged []swLogged, hiCap float64, sub int, two bool) []swIval {
	L := float64(span[len(span)-1])
	iv := []swIval{{L, L, 1}}
	for _, e := range logged {
		iv = append(iv, swIval{e.lo, e.hi, 1})
	}
	on := swOnset(span, bs[0], unit, onBits)
	for j, b := range bs {
		lo, hi, c := b.lo, b.hi, float64(b.c)
		if j == 0 {
			c--
			hi = L
			lo = on
		}
		for _, e := range logged {
			if float64(e.t) >= b.lo && float64(e.t) < b.hi {
				c--
			}
		}
		if c <= 0 {
			continue
		}
		if (j == 0 || (j == 1 && two)) && sub > 1 {
			// count bucket j's events (but the last touch) per part
			w := (b.hi - b.lo) / float64(sub)
			cnt := make([]float64, sub)
			for _, x := range span {
				if fx := float64(x); fx >= b.lo && fx < b.hi {
					cnt[min(sub-1, int((fx-b.lo)/w))]++
				}
			}
			if j == 0 {
				cnt[min(sub-1, int((L-b.lo)/w))]-- // the last touch is a point already
			}
			for p := range sub {
				plo, phi := b.lo+float64(p)*w, b.lo+float64(p+1)*w
				plo = max(plo, lo)
				phi = min(phi, hi)
				if phi < plo {
					phi = plo
				}
				iv = append(iv, swIval{plo, phi, cnt[p]})
			}
			continue
		}
		if len(logged) > 0 || !math.IsInf(hiCap, 1) {
			hi = min(hi, hiCap)
			if hi < lo {
				hi = lo
			}
		}
		iv = append(iv, swIval{lo, hi, c})
	}
	return iv
}

// swLog logs the events before the last touch as a stored log would: m of
// them at most, each as floor(t/r). abs (d == 0): kept while younger than
// a; delta (d > 0): gaps of d bits between quantized times, a full gap ends
// the log. hiCap bounds the events the log does not hold.
func swLog(span []uint64, m int, r, a float64, d int) (logged []swLogged, hiCap float64) {
	L := span[len(span)-1]
	hiCap = math.Inf(1)
	qPrev := math.Floor(float64(L) / r)
	for i := len(span) - 2; i >= 0; i-- {
		t := span[i]
		q := math.Floor(float64(t) / r)
		if d == 0 {
			if float64(L-t) >= a {
				return logged, float64(L) - a
			}
		} else if g := qPrev - q; g >= float64(int(1)<<d-1) {
			return logged, (qPrev - float64(int(1)<<d-1) + 1) * r
		}
		if len(logged) == m {
			o := logged[m-1]
			return logged, o.hi
		}
		hi := min((q+1)*r, float64(L))
		logged = append(logged, swLogged{t, q * r, hi})
		qPrev = q
	}
	return logged, hiCap
}

// swG is the mean of exp(-lambda (L - t)) over t uniform in [lo, hi].
func swG(lo, hi, L, lambda float64) float64 {
	if hi-lo < 1e-9 {
		return math.Exp(-lambda * (L - lo))
	}
	return (math.Exp(-lambda*(L-hi)) - math.Exp(-lambda*(L-lo))) / (lambda * (hi - lo))
}

// swTraceEst reads traces s[a] = sum over events of exp(-(L-t)/taus[a])
// jointly with the intervals: the linear minimum mean-square-error estimate
// of how many of the cut interval's events lie in the window, under the
// intervals' uniform model (the cut's split binomial, positions uniform).
func swTraceEst(iv []swIval, L, from float64, taus, s []float64) float64 {
	return swTraceEstNoise(iv, L, from, taus, s, nil)
}

// swTraceEstNoise is swTraceEst with qv[a] added to trace a's variance (a
// stored trace's rounding), if qv is not nil.
func swTraceEstNoise(iv []swIval, L, from float64, taus, s, qv []float64) float64 {
	cut := -1
	base := 0.0
	for k, b := range iv {
		switch {
		case b.c <= 0:
		case from <= b.lo:
			base += b.c
		case from < b.hi:
			cut = k
		}
	}
	if cut < 0 {
		return base
	}
	c, lo, hi := iv[cut].c, iv[cut].lo, iv[cut].hi
	p := (hi - from) / (hi - lo)
	mu, v := c*p, c*p*(1-p)
	if v <= 0 {
		return base + mu
	}
	n := len(taus)
	lam := make([]float64, n)
	for a := range n {
		lam[a] = 1 / taus[a]
	}
	D := make([]float64, n)
	resid := make([]float64, n)
	cov := make([][]float64, n)
	for a := range n {
		cov[a] = make([]float64, n)
		gin, gout := swG(from, hi, L, lam[a]), swG(lo, from, L, lam[a])
		D[a] = gin - gout
		exp := c * gout
		for k, b := range iv {
			if k != cut && b.c > 0 {
				exp += b.c * swG(b.lo, b.hi, L, lam[a])
			}
		}
		resid[a] = s[a] - exp - D[a]*mu
	}
	for a := range n {
		for bb := range n {
			cv := func(l0, h0 float64) float64 {
				return swG(l0, h0, L, lam[a]+lam[bb]) - swG(l0, h0, L, lam[a])*swG(l0, h0, L, lam[bb])
			}
			e := mu*cv(from, hi) + (c-mu)*cv(lo, from)
			for k, b := range iv {
				if k != cut && b.c > 0 {
					e += b.c * cv(b.lo, b.hi)
				}
			}
			cov[a][bb] = e + v*D[a]*D[bb]
		}
		cov[a][a] += 1e-12
		if qv != nil {
			cov[a][a] += qv[a]
		}
	}
	var gain float64
	switch n {
	case 1:
		gain = v * D[0] / cov[0][0] * resid[0]
	case 2:
		det := cov[0][0]*cov[1][1] - cov[0][1]*cov[1][0]
		i00, i01, i10, i11 := cov[1][1]/det, -cov[0][1]/det, -cov[1][0]/det, cov[0][0]/det
		gain = v * (D[0]*(i00*resid[0]+i01*resid[1]) + D[1]*(i10*resid[0]+i11*resid[1]))
	default:
		panic("swTraceEst: at most two traces")
	}
	return base + max(0, min(c, mu+gain))
}

// swTraces are the exact traces at the last touch.
func swTraces(span []uint64, taus []float64) []float64 {
	L := span[len(span)-1]
	s := make([]float64, len(taus))
	for a, tau := range taus {
		for i := len(span) - 1; i >= 0 && float64(L-span[i]) < 30*tau; i-- {
			s[a] += math.Exp(-float64(L-span[i]) / tau)
		}
	}
	return s
}

func swRound(est float64, in bool) float64 {
	if !in {
		return 0
	}
	return float64(max(1, int(est+0.5)))
}

func swBits(n float64) int { return int(math.Ceil(math.Log2(n))) }

func swW1Models() []swModel {
	g := swCur
	unit := float64(g.unit)
	ivModel := func(name string, bits, onBits int, side func(span []uint64) ([]swLogged, float64), sub int, two bool) swModel {
		return swModel{name, bits, func(ts []uint64, now, w uint64) float64 {
			span := swSpan(ts)
			L := span[len(span)-1]
			bs := g.buckets(span)
			var logged []swLogged
			hiCap := math.Inf(1)
			if side != nil {
				logged, hiCap = side(span)
			}
			iv := swIvals(span, bs, unit, onBits, logged, hiCap, sub, two)
			return swRound(swCount(iv, float64(now)-float64(w)), now-L <= w)
		}}
	}
	onName := map[int]string{0: "", 4: " +on4"}
	var ms []swModel
	for _, on := range []int{0, 4} {
		ms = append(ms, ivModel("register"+onName[on], 0, on, nil, 0, false))
	}
	// reference: D19's exact last-m rows (m counts the last touch)
	for _, m := range []int{2, 3, 4, 5, 6, 8} {
		for _, on := range []int{0, 4} {
			ms = append(ms, ivModel(fmt.Sprintf("last %d exact%s", m, onName[on]), -1, on, func(span []uint64) ([]swLogged, float64) {
				return swLog(span, m-1, 1e-6, math.Inf(1), 0)
			}, 0, false))
		}
	}
	// W1a: logs of recent events, absolute (age limit a) or delta-coded
	for _, m := range []int{3, 4, 5, 6, 8} {
		for _, r := range []float64{40, 80, 160, 300} {
			for _, on := range []int{0, 4} {
				for _, a := range []float64{1250, 2500, 5000} {
					k := swBits(a/r + 2)
					ms = append(ms, ivModel(fmt.Sprintf("log abs m%d r%.0f A%.0f%s", m, r, a, onName[on]), (m-1)*k, on, func(span []uint64) ([]swLogged, float64) {
						return swLog(span, m-1, r, a, 0)
					}, 0, false))
				}
				for _, d := range []int{2, 3, 4} {
					ms = append(ms, ivModel(fmt.Sprintf("log delta m%d r%.0f d%d%s", m, r, d, onName[on]), (m-1)*d, on, func(span []uint64) ([]swLogged, float64) {
						return swLog(span, m-1, r, 0, d)
					}, 0, false))
				}
			}
		}
	}
	// W1b: sub-unit counts of the newest unit (and of the one before)
	for _, sub := range []int{2, 4, 8} {
		for _, two := range []bool{false, true} {
			for _, on := range []int{0, 4} {
				bits := (sub - 1) * 5
				name := fmt.Sprintf("sub %d", sub)
				if two {
					bits *= 2
					name += " x2 units"
				}
				ms = append(ms, ivModel(name+onName[on], bits, on, nil, sub, two))
			}
		}
	}
	// W1c: exact traces anchored at the last touch
	traceModel := func(name string, on int, taus []float64) swModel {
		return swModel{name, -1, func(ts []uint64, now, w uint64) float64 {
			span := swSpan(ts)
			L := span[len(span)-1]
			bs := g.buckets(span)
			iv := swIvals(span, bs, unit, on, nil, math.Inf(1), 0, false)
			return swRound(swTraceEst(iv, float64(L), float64(now)-float64(w), taus, swTraces(span, taus)), now-L <= w)
		}}
	}
	taus := []float64{250, 500, 1000, 2000, 4000}
	for _, on := range []int{0, 4} {
		for i, a := range taus {
			ms = append(ms, traceModel(fmt.Sprintf("trace %.2gs%s", a/1000, onName[on]), on, []float64{a}))
			for _, b := range taus[i+1:] {
				ms = append(ms, traceModel(fmt.Sprintf("traces %.2gs+%.2gs%s", a/1000, b/1000, onName[on]), on, []float64{a, b}))
			}
		}
	}
	return ms
}

func swPad(x float64) string { return fmt.Sprintf("%7.4f", x) }

// TestShortWindowInformation (TF_EXP=1) is lab/sw's W1: see SW-RESULTS.md.
// TF_SW_FILTER=substring keeps only the models whose name holds it.
func TestShortWindowInformation(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	models := swW1Models()
	if f := os.Getenv("TF_SW_FILTER"); f != "" {
		var keep []swModel
		for _, m := range models {
			if strings.Contains(m.name, f) || m.name == "register +on4" {
				keep = append(keep, m)
			}
		}
		models = keep
	}
	seeds := [][2]uint64{{9, 9}, {3, 7}}
	scores := make([][]swScore, len(seeds))
	for si, sd := range seeds {
		run := swRecord(t, sd[0], sd[1])
		scores[si] = swScoreAll(run, models)
		// the interval model of the register with its 4-bit onset is the filter
		var reg swScore
		for m, md := range models {
			if md.name == "register +on4" {
				reg = scores[si][m]
			}
		}
		fil := scores[si][len(models)]
		for wi := range 5 {
			if math.Abs(reg.err[wi]-fil.err[wi]) > 1e-9 {
				t.Errorf("seed %v: the register +on4 model differs from the filter at window %d: %v vs %v", sd, wi, reg.err[wi], fil.err[wi])
			}
		}
	}
	t.Logf("normalized MAE per window (1 5 15 30 60 s) and mean; seed (9,9) then held-out (3,7); bits beyond the register and onset (-1: exact values); pass = W1's rule on both seeds")
	t.Logf("%-34s %5s | %7s %7s %7s %7s %7s %7s | %7s %7s | %s", "model", "bits", "1s", "5s", "15s", "30s", "60s", "mean", "1s'", "mean'", "rule")
	line := func(name string, bits int, si0, si1 swScore) string {
		s := ""
		for wi := range 5 {
			s += " " + swPad(si0.win(wi))
		}
		return fmt.Sprintf("%-34s %5d |%s %s | %s %s", name, bits, s, swPad(si0.mean()), swPad(si1.win(0)), swPad(si1.mean()))
	}
	nf := len(models)
	t.Logf("%s", line("filter", 0, scores[0][nf], scores[1][nf]))
	for m, md := range models {
		pass := true
		for si := range seeds {
			if scores[si][m].win(0) > 0.082 {
				pass = false
			}
			for wi := 1; wi < 5; wi++ {
				if scores[si][m].win(wi) > 1.01*scores[si][nf].win(wi) {
					pass = false
				}
			}
		}
		verdict := ""
		if pass {
			verdict = "PASS"
		}
		t.Logf("%s | %s", line(md.name, md.bits, scores[0][m], scores[1][m]), verdict)
	}
	// the frontier: for priced models, the lowest 1 s error at each bit count or fewer
	type pt struct {
		bits int
		e1   float64
		mean float64
		name string
	}
	var pts []pt
	for m, md := range models {
		if md.bits > 0 {
			pts = append(pts, pt{md.bits, max(scores[0][m].win(0), scores[1][m].win(0)), max(scores[0][m].mean(), scores[1][m].mean()), md.name})
		}
	}
	sort.Slice(pts, func(i, j int) bool {
		return pts[i].bits < pts[j].bits || pts[i].bits == pts[j].bits && pts[i].e1 < pts[j].e1
	})
	t.Logf("frontier (worse of the two seeds): bits, 1 s, mean, model")
	best := math.Inf(1)
	for _, p := range pts {
		if p.e1 < best {
			best = p.e1
			t.Logf("%5d %s %s %s", p.bits, swPad(p.e1), swPad(p.mean), p.name)
		}
	}
}
