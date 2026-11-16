package temporalfilter

// lab/sw-2026-11, arm W2: encodings of the short windows' information at a
// fixed budget (a model; no library change). See SW-RESULTS.md.

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
)

// Variable-length codes of a gap g >= 0 (in quanta), newest first in a
// field of B bits that each event shifts its code into; bits past B fall
// off. A code the field's end cuts gives a lower bound on its gap. A fresh
// field is filled so that it decodes as a cut code: ones for Rice codes,
// zeros for gamma codes.
type swCode struct {
	name  string
	gamma bool
	k     uint // Rice parameter
}

func (c swCode) len(g uint64) int {
	if c.gamma {
		n := 63 - leadingZeros64(g+1)
		return 2*n + 1
	}
	return int(g>>c.k) + 1 + int(c.k)
}

func leadingZeros64(x uint64) int {
	n := 0
	for i := 63; i >= 0 && x>>uint(i)&1 == 0; i-- {
		n++
	}
	return n
}

// lb is the lower bound on g that its code's first avail bits give
// (avail < c.len(g)).
func (c swCode) lb(g uint64, avail int) uint64 {
	if c.gamma {
		x := g + 1
		n := 63 - leadingZeros64(x)
		if avail <= n {
			return 1<<uint(avail) - 1
		}
		p := avail - n // bits of x read, MSB first, p <= n
		sh := uint(n + 1 - p)
		return (x>>sh)<<sh - 1
	}
	q := int(g >> c.k)
	if avail <= q {
		return uint64(avail) << c.k
	}
	p := avail - q - 1 // remainder bits read
	if p <= 0 {
		return uint64(q) << c.k
	}
	sh := c.k - uint(p)
	rem := g & (1<<c.k - 1)
	return uint64(q)<<c.k + (rem>>sh)<<sh
}

// fill is the lower bound a fresh field's last avail bits give.
func (c swCode) fill(avail int) uint64 {
	if avail <= 0 {
		return 0
	}
	if c.gamma {
		return 1<<uint(min(avail, 62)) - 1
	}
	return uint64(avail) << c.k
}

// swVarLog decodes the field a B-bit shift register of codes holds after
// span's events: the events it places (newest first) and the bound on the
// events it does not hold.
func swVarLog(span []uint64, B int, code swCode, r float64) (logged []swLogged, hiCap float64) {
	L := span[len(span)-1]
	qPrev := math.Floor(float64(L) / r)
	used := 0
	for i := len(span) - 2; ; i-- {
		avail := B - used
		if i < 0 {
			return logged, (qPrev - float64(code.fill(avail)) + 1) * r
		}
		q := math.Floor(float64(span[i]) / r)
		g := uint64(qPrev - q)
		if n := code.len(g); n <= avail {
			used += n
			logged = append(logged, swLogged{span[i], q * r, min((q+1)*r, float64(L))})
			qPrev = q
			continue
		}
		return logged, (qPrev - float64(code.lb(g, avail)) + 1) * r
	}
}

// swTraceQ replays the key's recent events into traces held as b-bit codes
// on a log scale (code c is base^c, base = smax^(1/(2^b-1)), so 0 is 1.0),
// each event rounding stochastically by a hash of its tick. It returns the
// decoded traces and the rounding's variance.
func swTraceQ(span []uint64, taus []float64, b int, smax float64) (vals, qv []float64) {
	L := span[len(span)-1]
	tmax := 0.0
	for _, t := range taus {
		tmax = max(tmax, t)
	}
	i0 := sort.Search(len(span), func(i int) bool { return float64(L-span[i]) < 20*tmax })
	levels := float64(int(1)<<b - 1)
	lb := math.Log(smax) / levels
	vals, qv = make([]float64, len(taus)), make([]float64, len(taus))
	for a, tau := range taus {
		c := 0.0
		prev := span[i0]
		for _, t := range span[i0+1:] {
			s := math.Exp(c*lb)*math.Exp(-float64(t-prev)/tau) + 1
			x := math.Log(s) / lb
			u := float64(mix64(t^uint64(a+1)<<56)>>11) / (1 << 53)
			c = min(levels, math.Floor(x+u))
			prev = t
		}
		vals[a] = math.Exp(c * lb)
		qv[a] = vals[a] * vals[a] * lb * lb / 6
	}
	return vals, qv
}

func swW2Models() []swModel {
	g := swCur
	unit := float64(g.unit)
	logModel := func(name string, bits int, side func(span []uint64) ([]swLogged, float64)) swModel {
		return swModel{name, bits, func(ts []uint64, now, w uint64) float64 {
			span := swSpan(ts)
			L := span[len(span)-1]
			logged, hiCap := side(span)
			iv := swIvals(span, g.buckets(span), unit, 4, logged, hiCap, 0, false)
			return swRound(swCount(iv, float64(now)-float64(w)), now-L <= w)
		}}
	}
	var ms []swModel
	ms = append(ms, swModel{"register +on4", 0, func(ts []uint64, now, w uint64) float64 {
		span := swSpan(ts)
		L := span[len(span)-1]
		iv := swIvals(span, g.buckets(span), unit, 4, nil, math.Inf(1), 0, false)
		return swRound(swCount(iv, float64(now)-float64(w)), now-L <= w)
	}})
	rs := []int{64, 32, 16, 8} // quantum = unit / rs
	for _, B := range []int{12, 14, 15, 16} {
		for _, rd := range rs {
			r := unit / float64(rd)
			// W2a: fixed-width delta logs
			for _, d := range []int{2, 3, 4, 5} {
				m := B / d
				ms = append(ms, logModel(fmt.Sprintf("W2a B%d fixed d%d x%d r=u/%d", B, d, m, rd), m*d, func(span []uint64) ([]swLogged, float64) {
					return swLog(span, m, r, 0, d)
				}))
			}
			// W2b: variable-length shift registers
			for _, code := range []swCode{{"rice0", false, 0}, {"rice1", false, 1}, {"rice2", false, 2}, {"gamma", true, 0}} {
				ms = append(ms, logModel(fmt.Sprintf("W2b B%d %s r=u/%d", B, code.name, rd), B, func(span []uint64) ([]swLogged, float64) {
					return swVarLog(span, B, code, r)
				}))
			}
		}
	}
	// W2c: two traces of b bits each
	for _, b := range []int{6, 7, 8} {
		for _, taus := range [][]float64{{250, 500}, {250, 1000}, {500, 1000}} {
			ms = append(ms, swModel{fmt.Sprintf("W2c B%d traces %.2gs+%.2gs", 2*b, taus[0]/1000, taus[1]/1000), 2 * b, func(ts []uint64, now, w uint64) float64 {
				span := swSpan(ts)
				L := span[len(span)-1]
				iv := swIvals(span, g.buckets(span), unit, 4, nil, math.Inf(1), 0, false)
				s, qv := swTraceQ(span, taus, b, 64)
				return swRound(swTraceEstNoise(iv, float64(L), float64(now)-float64(w), taus, s, qv), now-L <= w)
			}})
		}
	}
	return ms
}

// TestShortWindowEncodings (TF_EXP=1) is lab/sw's W2: see SW-RESULTS.md.
func TestShortWindowEncodings(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	models := swW2Models()
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
		scores[si] = swScoreAll(swRecord(t, sd[0], sd[1]), models)
		fil := scores[si][len(models)]
		for wi := range 5 {
			if math.Abs(scores[si][0].err[wi]-fil.err[wi]) > 1e-9 {
				t.Errorf("seed %v: the register +on4 model differs from the filter at window %d", sd, wi)
			}
		}
	}
	nf := len(models)
	t.Logf("normalized MAE per window (1 5 15 30 60 s) and mean on (9,9); 1 s and mean on the held-out (3,7); rule: B <= 15, mean <= 0.050 on both, no window over 1.01x the filter's")
	t.Logf("%-36s %4s | %7s %7s %7s %7s %7s %7s | %7s %7s | %s", "model", "bits", "1s", "5s", "15s", "30s", "60s", "mean", "1s'", "mean'", "rule")
	type row struct {
		name string
		bits int
		worst float64
		pass bool
	}
	var rows []row
	for m, md := range models {
		pass := md.bits <= 15
		worst := 0.0
		for si := range seeds {
			worst = max(worst, scores[si][m].mean())
			if scores[si][m].mean() > 0.050 {
				pass = false
			}
			for wi := range 5 {
				if scores[si][m].win(wi) > 1.01*scores[si][nf].win(wi) {
					pass = false
				}
			}
		}
		s := ""
		for wi := range 5 {
			s += " " + swPad(scores[0][m].win(wi))
		}
		v := ""
		if pass {
			v = "PASS"
		}
		t.Logf("%-36s %4d |%s %s | %s %s | %s", md.name, md.bits, s, swPad(scores[0][m].mean()), swPad(scores[1][m].win(0)), swPad(scores[1][m].mean()), v)
		rows = append(rows, row{md.name, md.bits, worst, pass})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].worst < rows[j].worst })
	t.Logf("best worse-seed means per budget:")
	for _, B := range []int{12, 14, 15, 16} {
		n := 0
		for _, r := range rows {
			if strings.Contains(r.name, fmt.Sprintf("B%d ", B)) && n < 5 {
				t.Logf("  B%d %s %s pass=%v", B, swPad(r.worst), r.name, r.pass)
				n++
			}
		}
	}
}

// TestSwCodes checks the codes' bounds: a cut code's bound never exceeds
// its gap, and grows with the bits read.
func TestSwCodes(t *testing.T) {
	for _, c := range []swCode{{"rice0", false, 0}, {"rice1", false, 1}, {"rice2", false, 2}, {"gamma", true, 0}} {
		for g := uint64(0); g < 300; g++ {
			n := c.len(g)
			prev := uint64(0)
			for a := 0; a < n; a++ {
				lb := c.lb(g, a)
				if lb > g || lb < prev {
					t.Fatalf("%s g=%d avail=%d: lb %d (prev %d)", c.name, g, a, lb, prev)
				}
				prev = lb
			}
		}
	}
}
