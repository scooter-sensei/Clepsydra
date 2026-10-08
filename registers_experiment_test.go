package temporalfilter

// Accuracy experiment: three 48-bit per-key velocity encodings, all sharing
// the exact lastTouch from word0. Run with:
//
//	go test -run TestRegisterExperiment -v
//
// Time unit is 1ms; horizon H = 60s.

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

const expH = 60_000.0

type register interface {
	name() string
	add(t float64)                      // record an event at t (non-decreasing)
	count(now, w, last float64) float64 // estimate events in (now-w, now]
}

// gate applies the two exact facts every encoding gets from word0.
func gate(est, now, w, last float64) float64 {
	if now-last > w {
		return 0
	}
	return math.Max(1, est)
}

// --- Linear: 6x8-bit shift register, sub-window H/5 (what filter.go ships) ---

type linearReg struct {
	g    float64
	e    int64
	c    [6]float64
	last float64
}

func newLinear() *linearReg       { return &linearReg{g: expH / 5, e: -1 << 40} }
func (r *linearReg) name() string { return "linear 6x8 (H/5)" }
func (r *linearReg) advance(t float64) {
	e := int64(t / r.g)
	d := e - r.e
	if d <= 0 {
		return
	}
	if d >= 6 {
		r.c = [6]float64{}
	} else {
		copy(r.c[d:], r.c[:6-d])
		clear(r.c[:d])
	}
	r.e = e
}
func (r *linearReg) add(t float64) { r.advance(t); r.c[0] = math.Min(255, r.c[0]+1); r.last = t }
func (r *linearReg) count(now, w, last float64) float64 {
	cp := *r
	cp.advance(now)
	sub := float64(cp.e) * r.g
	from := now - w
	est := cp.c[0]
	if from > sub && last > sub {
		est *= math.Min(1, (last-math.Min(from, last))/(last-sub))
	}
	rem := w/r.g - (now-sub)/r.g
	for k := 1; k < 6 && rem > 0; k++ {
		est += cp.c[k] * math.Min(1, rem)
		rem--
	}
	return gate(est, now, w, last)
}

// --- Dyadic carry-shift: buckets whose widths grow 1,1,1-2,2-4,4-8,... units ---
//
// Boundaries: b0 = u (current unit), bj = floor(u/2^(j-1))*2^(j-1) - 2^(j-1).
// When the unit index becomes u with m trailing zeros, buckets 0..m shift up
// one place and bucket m+1 merges into m+2 (or drops past the top). Recent
// time is kept at unit resolution, older time coarser; n buckets cover
// >= 2^(n-2) units. Counter widths grow with bucket size; they sum to 48.

type dyadicReg struct {
	unit float64
	u    int64
	c    []float64
	max  []float64
}

func newDyadic(widths ...int) *dyadicReg {
	n := len(widths)
	r := &dyadicReg{unit: expH / float64(int(1)<<(n-2)), u: -1 << 40, c: make([]float64, n)}
	for _, w := range widths {
		r.max = append(r.max, float64(int(1)<<w-1))
	}
	return r
}
func (r *dyadicReg) name() string {
	return fmt.Sprintf("dyadic %d buckets (H/%d near now)", len(r.c), 1<<(len(r.c)-2))
}
func (r *dyadicReg) advance(t float64) {
	n := len(r.c)
	u := int64(t / r.unit)
	if u-r.u > 4<<n {
		clear(r.c)
		r.u = u
		return
	}
	for r.u < u {
		r.u++
		top := bitsTrailing(r.u) + 1 // buckets 0..top-1 shift; top merges up
		if top+1 < n {
			r.c[top+1] = math.Min(r.max[top+1], r.c[top+1]+r.c[top])
		} else {
			top = n - 1 // shift everything, oldest drops
		}
		for j := top; j >= 1; j-- {
			r.c[j] = math.Min(r.max[j], r.c[j-1])
		}
		r.c[0] = 0
	}
}
func bitsTrailing(u int64) int {
	m := 0
	for u&1 == 0 && m < 62 {
		u >>= 1
		m++
	}
	return m
}
func (r *dyadicReg) bound(j int) float64 { // start of bucket j, in ms
	if j == 0 {
		return float64(r.u) * r.unit
	}
	s := j - 1
	return float64((r.u>>s)<<s-1<<s) * r.unit
}
func (r *dyadicReg) add(t float64) { r.advance(t); r.c[0] = math.Min(r.max[0], r.c[0]+1) }
func (r *dyadicReg) count(now, w, last float64) float64 {
	cp := *r
	cp.c = slices.Clone(r.c)
	cp.advance(now)
	from := now - w
	est := 0.0
	hi := now
	for j := range cp.c {
		lo := cp.bound(j)
		top := hi
		if j == 0 {
			top = math.Max(lo, math.Min(last, now)) // c0's events lie in [lo, last]
		}
		switch {
		case from <= lo:
			est += cp.c[j]
		case from < top:
			est += cp.c[j] * (top - from) / (top - lo)
		}
		hi = lo
	}
	return gate(est, now, w, last)
}

// --- Laplace: k exponential decays (log-encoded), window synthesized per query ---
//
// Stores S_j = sum_i exp(-(last-t_i)/tau_j) as of lastTouch. At query time
// S_j(now) = S_j * exp(-(now-last)/tau_j): decay is pure clock arithmetic with
// no epochs or shifts. The box window 1[age <= W] is approximated by
// sum_j a_j exp(-age/tau_j) with a_j from a least-squares fit over ages in
// [0, 1.5H]; the count estimate is sum_j a_j S_j(now).

type laplaceReg struct {
	tau  []float64
	s    []float64
	bits int // per-register encoding width (log2(1+S) fixed point)
	last float64
	fit  map[float64][]float64
}

func newLaplace(tau []float64, bits int) *laplaceReg {
	return &laplaceReg{tau: tau, s: make([]float64, len(tau)), bits: bits, last: math.Inf(-1), fit: map[float64][]float64{}}
}
func (r *laplaceReg) name() string { return fmt.Sprintf("laplace %dx%d", len(r.tau), r.bits) }
func (r *laplaceReg) quant(s float64) float64 {
	frac := float64(int(1) << (r.bits - 5)) // 5 integer bits of log2: S < 2^32
	return math.Exp2(math.Round(math.Log2(1+s)*frac)/frac) - 1
}
func (r *laplaceReg) add(t float64) {
	for j, tau := range r.tau {
		r.s[j] = r.quant(r.s[j]*math.Exp(-(t-r.last)/tau) + 1)
	}
	r.last = t
}
func (r *laplaceReg) weights(w float64) []float64 {
	if a, ok := r.fit[w]; ok {
		return a
	}
	k := len(r.tau)
	A := make([][]float64, k)
	b := make([]float64, k)
	L := 1.5 * expH
	for i := range k {
		A[i] = make([]float64, k+1)
		for j := range k {
			s := 1/r.tau[i] + 1/r.tau[j]
			A[i][j] = (1 - math.Exp(-s*L)) / s
		}
		b[i] = r.tau[i] * (1 - math.Exp(-w/r.tau[i]))
		A[i][k] = b[i]
	}
	for c := range k { // Gauss-Jordan, k <= 4
		p := c
		for i := c + 1; i < k; i++ {
			if math.Abs(A[i][c]) > math.Abs(A[p][c]) {
				p = i
			}
		}
		A[c], A[p] = A[p], A[c]
		for i := range k {
			if i != c {
				f := A[i][c] / A[c][c]
				for j := c; j <= k; j++ {
					A[i][j] -= f * A[c][j]
				}
			}
		}
	}
	a := make([]float64, k)
	for i := range k {
		a[i] = A[i][k] / A[i][i]
	}
	r.fit[w] = a
	return a
}
func (r *laplaceReg) count(now, w, last float64) float64 {
	a := r.weights(w)
	est := 0.0
	for j, tau := range r.tau {
		est += a[j] * r.s[j] * math.Exp(-(now-r.last)/tau)
	}
	return gate(math.Max(0, est), now, w, last)
}

// --- traffic ---

type gen struct {
	name string
	f    func(r *rand.Rand, end float64) []float64
}

func poisson(r *rand.Rand, t0, t1, rate float64) []float64 { // rate per ms
	var ev []float64
	for t := t0 + r.ExpFloat64()/rate; t < t1; t += r.ExpFloat64() / rate {
		ev = append(ev, t)
	}
	return ev
}

var gens = []gen{
	{"steady 2/s", func(r *rand.Rand, end float64) []float64 { return poisson(r, 0, end, 2e-3) }},
	{"bursty", func(r *rand.Rand, end float64) []float64 {
		var ev []float64
		for _, c := range poisson(r, 0, end, 1e-4) { // bursts every ~10s
			n := 5 + r.IntN(15)
			for range n {
				ev = append(ev, c+r.Float64()*200)
			}
		}
		slices.Sort(ev)
		return ev
	}},
	{"ramp 0.1->10/s", func(r *rand.Rand, end float64) []float64 {
		var ev []float64
		for t := 0.0; t < end; {
			rate := (0.1 + 9.9*t/end) * 1e-3
			t += r.ExpFloat64() / rate
			ev = append(ev, t)
		}
		return ev
	}},
	{"sparse 1/20s", func(r *rand.Rand, end float64) []float64 { return poisson(r, 0, end, 5e-5) }},
	{"on/off 5s", func(r *rand.Rand, end float64) []float64 {
		var ev []float64
		for t := 0.0; t < end; t += 10_000 {
			ev = append(ev, poisson(r, t, t+5000, 4e-3)...)
		}
		return ev
	}},
}

func TestRegisterExperiment(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	wins := []float64{1000, 5000, 15_000, 30_000, 60_000}
	makers := []func() register{
		func() register { return newLinear() },
		func() register { return newDyadic(6, 6, 7, 8, 9, 12) },
		func() register { return newDyadic(5, 5, 6, 6, 7, 8, 11) },
		func() register { return newLaplace([]float64{expH / 16, expH / 4, expH}, 16) },
		func() register { return newLaplace([]float64{expH / 32, expH / 8, expH / 3, expH}, 12) },
	}
	var out strings.Builder
	fmt.Fprintf(&out, "\nnormalized MAE (sum|est-exact| / sum exact), by window\n%-34s %-15s", "register", "traffic")
	for _, w := range wins {
		fmt.Fprintf(&out, " %6.0fs", w/1000)
	}
	out.WriteString("\n")
	total := map[string][]float64{}
	for _, mk := range makers {
		for _, g := range gens {
			errs := make([]float64, len(wins))
			sums := make([]float64, len(wins))
			name := ""
			for trial := range 40 {
				r := rand.New(rand.NewPCG(uint64(trial), 77))
				ev := g.f(r, 10*expH)
				reg := mk()
				name = reg.name()
				qs := poisson(r, 2*expH, 10*expH, 1/500.0) // query every ~0.5s
				i, last := 0, math.Inf(-1)
				for _, q := range qs {
					for i < len(ev) && ev[i] <= q {
						reg.add(ev[i])
						last = ev[i]
						i++
					}
					for wi, w := range wins {
						exact := 0
						for k := i - 1; k >= 0 && ev[k] > q-w; k-- {
							exact++
						}
						errs[wi] += math.Abs(reg.count(q, w, last) - float64(exact))
						sums[wi] += float64(exact)
					}
				}
			}
			fmt.Fprintf(&out, "%-34s %-15s", name, g.name)
			for wi := range wins {
				e := errs[wi] / math.Max(1, sums[wi])
				fmt.Fprintf(&out, " %7.3f", e)
				total[name] = append(total[name], e)
			}
			out.WriteString("\n")
		}
	}
	out.WriteString("\nmean over all traffic x windows:\n")
	for _, mk := range makers {
		n := mk().name()
		s := 0.0
		for _, e := range total[n] {
			s += e
		}
		fmt.Fprintf(&out, "  %-34s %.3f\n", n, s/float64(len(total[n])))
	}
	t.Log(out.String())
}
