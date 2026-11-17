package temporalfilter

// lab/sw-2026-11, arm W6: W4d's drip log on seeds no arm of the lab had
// read (SW-RESULTS.md).

import (
	"fmt"
	"math"
	"os"
	"slices"
	"testing"
)

// swFreshSeeds are W6's seeds, named in SW-RESULTS.md before its reading.
func swFreshSeeds() [][2]uint64 {
	var s [][2]uint64
	for i := uint64(1); i <= 40; i++ {
		s = append(s, [2]uint64{2026, i})
	}
	return s
}

// TestShortWindowFreshSeeds (TF_EXP=1) is lab/sw's W6. It replays
// tfbench's velocity workload once per fresh seed and scores the register
// with its onset (the default build's filter), the 11-bit unary log at
// unit/8, unit/16 and unit/32 with the onset, and the filter of the build
// it runs in. It judges nothing by failing: each clause's verdict is logged.
func TestShortWindowFreshSeeds(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	names := []string{"register +on4", "W2b B11 rice0 r=u/8", "W2b B11 rice0 r=u/16", "W2b B11 rice0 r=u/32"}
	all := swW2Models()
	var models []swModel
	for _, n := range names {
		for _, m := range all {
			if m.name == n {
				models = append(models, m)
			}
		}
	}
	if len(models) != len(names) {
		t.Fatalf("found %d of the %d models", len(models), len(names))
	}
	const reg, u8, u16, u32 = 0, 1, 2, 3
	build := "default"
	if dripOn {
		build = "tfdrip"
	}
	seeds := swFreshSeeds()
	var fils, regs []swScore
	c1, c2, c3, c4, best16 := 0, 0, 0, 0, 0
	t.Logf("%s build; per seed: the filter's 1 5 15 30 60 s and mean | the register +on4's mean | the 11-bit unary log's mean at u/8 u/16 u/32", build)
	for _, sd := range seeds {
		sc := swScoreAll(swRecord(t, sd[0], sd[1]), models)
		fil := sc[len(models)]
		fils, regs = append(fils, fil), append(regs, sc[reg])
		ok1, ok2, ok4 := true, true, true
		for wi := range 5 {
			if math.Abs(sc[reg].err[wi]-fil.err[wi]) > 1e-9 {
				ok1 = false
			}
			if math.Abs(fil.win(wi)-sc[u16].win(wi)) > 0.002 {
				ok2 = false
			}
			if fil.win(wi) > 1.01*sc[reg].win(wi) {
				ok4 = false
			}
		}
		ok3 := fil.mean() <= 0.050
		if !ok1 {
			c1++
		}
		if !ok2 {
			c2++
		}
		if !ok3 {
			c3++
		}
		if !ok4 {
			c4++
		}
		if sc[u16].mean() <= sc[u8].mean() && sc[u16].mean() <= sc[u32].mean() {
			best16++
		}
		s := ""
		for wi := range 5 {
			s += " " + swPad(fil.win(wi))
		}
		t.Logf("(%d,%2d) %s %s | %s | %s %s %s", sd[0], sd[1], s, swPad(fil.mean()), swPad(sc[reg].mean()),
			swPad(sc[u8].mean()), swPad(sc[u16].mean()), swPad(sc[u32].mean()))
	}
	spread := func(name string, ss []swScore) {
		row := func(get func(swScore) float64) string {
			v := make([]float64, len(ss))
			for i, s := range ss {
				v[i] = get(s)
			}
			slices.Sort(v)
			return fmt.Sprintf("%s %s %s", swPad(v[0]), swPad(v[len(v)/2]), swPad(v[len(v)-1]))
		}
		for wi, w := range []string{"1 s", "5 s", "15 s", "30 s", "60 s"} {
			t.Logf("%s %-5s min, median, max: %s", name, w, row(func(s swScore) float64 { return s.win(wi) }))
		}
		t.Logf("%s mean  min, median, max: %s", name, row(swScore.mean))
	}
	spread("filter ("+build+")", fils)
	spread("register +on4", regs)
	n := len(seeds)
	if dripOn {
		t.Logf("clause 2 (filter within 0.002 of the 11-bit log at u/16, every window): fails on %d of %d seeds", c2, n)
		t.Logf("clause 3 (target 7's metric at most 0.050): fails on %d of %d seeds", c3, n)
		t.Logf("clause 4 (no window over 1.01x the register +on4's, the default build's): fails on %d of %d seeds", c4, n)
	} else {
		t.Logf("clause 1 (the register +on4 equals the filter, every window): fails on %d of %d seeds", c1, n)
	}
	t.Logf("reported: u/16 the best of the three quanta on %d of %d seeds", best16, n)
}
