package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The report scores every arm's answers against the trace's truths with one
// piece of code, and summarizes the passes' JSON lines (results.jsonl),
// under the rules of docs/artifact/head-to-head.md.

var arms = []string{"clepsydra", "pingora", "dashmap"}

// spillMin is Clepsydra's default Config.SpillMin: lighter histories may be
// shed under floods (docs/paper/06-security.md, Shed light histories).
const spillMin = 4

type tally struct {
	n, deny        int // probes, denied
	absErr, sgnErr float64
}

type score struct {
	byClass     [4]tally
	allow       [2]int // over the limit: allowed, total
	deny        [2]int // under the limit: denied, total
	absErr      float64
	probes      int
	upperLow    int // probes with at least spillMin events whose upper bound reads below the truth
	upperLowLt  int // the same, fewer than spillMin events (Clepsydra sheds these under floods)
	estMismatch int // probes whose estimate differs from the truth
	rounds      int
}

func (s *score) add(t *trace, as []answer) {
	s.rounds++
	for i, p := range t.probes {
		a := as[i]
		truth := int(p.truth)
		c := &s.byClass[p.class]
		c.n++
		e := float64(int(a.est) - truth)
		c.absErr += math.Abs(e)
		c.sgnErr += e
		s.absErr += math.Abs(e)
		s.probes++
		if a.deny {
			c.deny++
		}
		if truth >= int(t.Limit) {
			s.allow[1]++
			if !a.deny {
				s.allow[0]++
			}
		} else {
			s.deny[1]++
			if a.deny {
				s.deny[0]++
			}
		}
		if int(a.upper) < truth {
			if truth >= spillMin {
				s.upperLow++
			} else {
				s.upperLowLt++
			}
		}
		if int(a.est) != truth {
			s.estMismatch++
		}
	}
}

// wilson is the Wilson score 95% interval for k of n.
func wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return math.NaN(), math.NaN()
	}
	const z = 1.959963984540054
	p, nf := float64(k)/float64(n), float64(n)
	c := (p + z*z/(2*nf)) / (1 + z*z/nf)
	h := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / (1 + z*z/nf)
	return max(0, c-h), min(1, c+h)
}

func pct(k, n int) string {
	lo, hi := wilson(k, n)
	return fmt.Sprintf("%.2f%% [%.2f, %.2f]", 100*float64(k)/float64(n), 100*lo, 100*hi)
}

// med returns the median and range of xs as "m (lo-hi)".
func med(xs []float64, prec int) string {
	if len(xs) == 0 {
		return "–"
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return fmt.Sprintf("%.*f (%.*f–%.*f)", prec, m, prec, s[0], prec, s[len(s)-1])
}

func cmdReport(dir string) error {
	traces := map[string]*trace{}
	var names []string
	paths, _ := filepath.Glob(filepath.Join(dir, "*.trace"))
	for _, p := range paths {
		t, err := readTrace(p)
		if err != nil {
			return err
		}
		n := floodName(t.Mult, t.Weight)
		traces[n] = t
		names = append(names, n)
	}
	order := func(n string) int {
		for i, fl := range floods {
			if floodName(uint64(fl.mult), uint64(fl.weight)) == n {
				return i
			}
		}
		return 99
	}
	slices.SortFunc(names, func(a, b string) int { return order(a) - order(b) })

	type key struct{ arm, flood, pass string }
	runs := map[key][]result{}
	fh, err := os.Open(filepath.Join(dir, "results.jsonl"))
	if err != nil {
		return err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var r result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return fmt.Errorf("results.jsonl: %v: %s", err, line)
		}
		if t := traces[r.Flood]; t != nil && r.Trace != t.sha {
			return fmt.Errorf("%s %s round %d replayed trace %s, not %s", r.Arm, r.Flood, r.Round, r.Trace, t.sha)
		}
		runs[key{r.Arm, r.Flood, r.Pass}] = append(runs[key{r.Arm, r.Flood, r.Pass}], r)
	}

	// The arms: the three of the protocol first, then any variant with
	// answers (the sizing runs' labels).
	extra := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(dir, "ans-*.bin"))
	for _, f := range files {
		a := strings.TrimPrefix(filepath.Base(f), "ans-")
		for _, n := range names {
			if i := strings.Index(a, "-"+n+"-r"); i > 0 {
				if a := a[:i]; !slices.Contains(arms, a) {
					extra[a] = true
				}
			}
		}
	}
	var more []string
	for a := range extra {
		more = append(more, a)
	}
	slices.Sort(more)
	arms := append(slices.Clone(arms), more...)

	scores := map[[2]string]*score{}
	for _, n := range names {
		t := traces[n]
		for _, a := range arms {
			s := &score{}
			files, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("ans-%s-%s-r*.bin", a, n)))
			for _, f := range files {
				as, err := readAnswers(f)
				if err != nil {
					return err
				}
				if len(as) != len(t.probes) {
					return fmt.Errorf("%s: %d answers for %d probes", f, len(as), len(t.probes))
				}
				s.add(t, as)
			}
			scores[[2]string{a, n}] = s
		}
	}

	t0 := traces[names[0]]
	fmt.Printf("Key count N = %d (Clepsydra: %d slots); limit %d per %d ms; probes per round: %d offenders, %d flood, %d base, %d new.\n\n",
		t0.Capacity, t0.Slots, t0.Limit, t0.WindowMs, offenders, probesPer, probesPer, probesPer)

	fmt.Printf("Validity (rule 1), all rounds: probes whose upper bound reads below the truth, keys with >= %d events | fewer | probes whose estimate differs from the truth\n", spillMin)
	for _, a := range arms {
		low, lt, mis, n := 0, 0, 0, 0
		for _, f := range names {
			s := scores[[2]string{a, f}]
			low, lt, mis, n = low+s.upperLow, lt+s.upperLowLt, mis+s.estMismatch, n+s.probes
		}
		fmt.Printf("  %-10s %d | %d | %d of %d\n", a, low, lt, mis, n)
	}
	fmt.Println()

	fmt.Println("| flood | arm | bytes/key | Mops/s, 1 thread | Mops/s, 12 threads | p99.9 ns, 1 thread | p99.9 ns, 12 threads | count error | false-allow | false-deny |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|")
	for _, n := range names {
		t := traces[n]
		for _, a := range arms {
			s := scores[[2]string{a, n}]
			get := func(pass string, f func(result) float64) []float64 {
				var xs []float64
				for _, r := range runs[key{a, n, pass}] {
					xs = append(xs, f(r))
				}
				return xs
			}
			mops := func(r result) float64 { return r.Mops }
			p999 := func(r result) float64 { return r.P999 }
			bpk := get("acc", func(r result) float64 { return float64(r.Bytes) / float64(t.Capacity) })
			fa, fd := "–", "–"
			if s.allow[1] > 0 {
				fa = pct(s.allow[0], s.allow[1])
			}
			if s.deny[1] > 0 {
				fd = pct(s.deny[0], s.deny[1])
			}
			ce := "–"
			if s.probes > 0 {
				ce = fmt.Sprintf("%.3f", s.absErr/float64(s.probes))
			}
			fmt.Printf("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", n, a, med(bpk, 1),
				med(get("t1", mops), 2), med(get("t12", mops), 2), med(get("lat1", p999), 0), med(get("lat12", p999), 0), ce, fa, fd)
		}
	}
	fmt.Println()

	fmt.Println("Per probe class, all rounds pooled: mean |estimate - truth| (mean signed error) and the share denied.")
	fmt.Println()
	fmt.Println("| flood | arm | offenders (20) | flood keys | base keys (1) | never sent (0) |")
	fmt.Println("|---|---|---|---|---|---|")
	for _, n := range names {
		for _, a := range arms {
			s := scores[[2]string{a, n}]
			var cells []string
			for c := range s.byClass {
				t := s.byClass[c]
				if t.n == 0 {
					cells = append(cells, "–")
					continue
				}
				cells = append(cells, fmt.Sprintf("%.2f (%+.2f), denied %.2f%%", t.absErr/float64(t.n), t.sgnErr/float64(t.n), 100*float64(t.deny)/float64(t.n)))
			}
			fmt.Printf("| %s | %s | %s |\n", n, a, strings.Join(cells, " | "))
		}
	}
	fmt.Println()

	// Rule 3: one arm is faster only when it is faster in every round.
	fmt.Println("Rule 3, round by round: rounds in which Clepsydra is faster (more ops/s, or a lower p99.9) than each competitor, of the rounds both ran.")
	fmt.Println()
	fmt.Println("| flood | Mops/s, 1 thread | Mops/s, 12 threads | p99.9, 1 thread | p99.9, 12 threads |")
	fmt.Println("|---|---|---|---|---|")
	for _, n := range names {
		var cells []string
		for _, m := range []struct {
			pass   string
			better func(a, b result) bool
		}{
			{"t1", func(a, b result) bool { return a.Mops > b.Mops }},
			{"t12", func(a, b result) bool { return a.Mops > b.Mops }},
			{"lat1", func(a, b result) bool { return a.P999 < b.P999 }},
			{"lat12", func(a, b result) bool { return a.P999 < b.P999 }},
		} {
			var vs []string
			for _, c := range []string{"pingora", "dashmap"} {
				byRound := map[int]result{}
				for _, r := range runs[key{c, n, m.pass}] {
					byRound[r.Round] = r
				}
				wins, both := 0, 0
				for _, r := range runs[key{"clepsydra", n, m.pass}] {
					if o, ok := byRound[r.Round]; ok {
						both++
						if m.better(r, o) {
							wins++
						}
					}
				}
				vs = append(vs, fmt.Sprintf("%s %d/%d", c, wins, both))
			}
			cells = append(cells, strings.Join(vs, ", "))
		}
		fmt.Printf("| %s | %s |\n", n, strings.Join(cells, " | "))
	}
	fmt.Println()

	fmt.Println("Latency detail, medians over rounds (ns): p50 / p99 / p99.9 / max; the timer's own p99.9 beside.")
	fmt.Println()
	fmt.Println("| flood | arm | 1 thread | 12 threads | timer p99.9 |")
	fmt.Println("|---|---|---|---|---|")
	for _, n := range names {
		for _, a := range arms {
			cell := func(pass string) string {
				rs := runs[key{a, n, pass}]
				if len(rs) == 0 {
					return "–"
				}
				q := func(f func(result) float64) float64 {
					var xs []float64
					for _, r := range rs {
						xs = append(xs, f(r))
					}
					slices.Sort(xs)
					return xs[len(xs)/2]
				}
				return fmt.Sprintf("%.0f / %.0f / %.0f / %.0f", q(func(r result) float64 { return r.P50 }), q(func(r result) float64 { return r.P99 }),
					q(func(r result) float64 { return r.P999 }), q(func(r result) float64 { return r.Max }))
			}
			var tp []float64
			for _, r := range runs[key{a, n, "lat1"}] {
				tp = append(tp, r.TimerP)
			}
			fmt.Printf("| %s | %s | %s | %s | %s |\n", n, a, cell("lat1"), cell("lat12"), med(tp, 0))
		}
	}
	return nil
}
