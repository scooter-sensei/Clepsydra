package temporalfilter

import (
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
)

// TestFloodSizing (TF_EXP=1) sizes the ghost overflow table and the stash
// against heavy floods of 1x-16x the table's slots (and a 16x flood of
// one-event keys): for each ghost size (units = slots / div), placement
// (one or two candidate buckets), floors per bucket and stash size, the
// share of new keys a strict limit of k turns away, split by cause, the
// offenders' answers, and the bytes per key the filter holds once the flood
// is in. Same workload as TestAttackFloodCollateral.
func TestFloodSizing(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	const k, offenders, probes = 20, 200, 20_000
	window := uint64(60_000)
	type cfg struct {
		name    string
		div     float64 // units = slots / div
		choices int
		sub     int // floors per bucket
		stash   int // stash entries: slots / stash
	}
	cfgs := []cfg{
		{"one /4   f1 s/8 (17c55c8)", 4, 1, 1, 8},
		{"one /4   f3 s/8", 4, 1, 3, 8},
		{"one /1.8 f1 s/8", 1.8, 1, 1, 8},
		{"two /4   f1 s/8", 4, 2, 1, 8},
		{"two /2   f1 s/8", 2, 2, 1, 8},
		{"two /2   f3 s/8", 2, 2, 3, 8},
		{"two /1.8 f1 s/8", 1.8, 2, 1, 8},
		{"two /1.8 f3 s/8", 1.8, 2, 3, 8},
		{"two /1.8 f3 s/16 (default)", 1.8, 2, 3, 16},
		{"two /1.9 f3 s/16", 1.9, 2, 3, 16},
		{"two /1.75 f3 s/16", 1.75, 2, 3, 16},
		{"two /1.8 f3 s/32", 1.8, 2, 3, 32},
	}
	if s := os.Getenv("TF_FLOOD_CFG"); s != "" {
		var c cfg
		if _, err := fmt.Sscanf(s, "%f %d %d %d", &c.div, &c.choices, &c.sub, &c.stash); err != nil {
			t.Fatal(err)
		}
		c.name = "env " + s
		cfgs = []cfg{c}
	}
	floods := []struct{ mult, weight int }{{1, k + 1}, {2, k + 1}, {3, k + 1}, {4, k + 1}, {6, k + 1}, {8, k + 1}, {16, k + 1}, {16, 1}}
	for _, c := range cfgs {
		for _, fl := range floods {
			f, now := attackFilter(t, 4096, 4820/c.stash) // 4,820 slots at 4,096 keys
			f.skW = uint64(max(64, int(float64(f.Slots())/c.div)))
			f.ghChoices = c.choices
			f.ghSub = c.sub
			rng := rand.New(rand.NewPCG(11, uint64(fl.mult*100+fl.weight)))
			for range f.Slots() * 85 / 100 {
				*now++
				f.TouchAt(rng.Uint64(), 0, *now)
			}
			offs := make([]uint64, offenders)
			for i := range offs {
				offs[i] = rng.Uint64()
				for range k {
					f.TouchAt(offs[i], 0, *now)
				}
				*now++
			}
			per := max(1, f.Slots()*fl.mult/30_000)
			for i := range f.Slots() * fl.mult {
				h := rng.Uint64()
				for range fl.weight {
					f.TouchAt(h, 0, *now)
				}
				if i%per == 0 {
					*now++ // the flood spans at most 30 s of the 60 s horizon
				}
			}
			*now++
			below, rec := 0, 0
			for _, h := range offs {
				r := f.CountAt(h, window, *now)
				if r.Upper < k {
					below++
				}
				if r.Count >= k*9/10 {
					rec++
				}
			}
			// Turned away: as the attack tests count it (Degraded or Upper
			// over k), and as the fail-closed recipe in docs/paper/06-security.md does (Upper reaching k).
			// Causes: a fingerprint match (Count from the table) or floors
			// only (Upper, not Count).
			deg, test, recipe, byMatch, byFloor := 0, 0, 0, 0, 0
			for range probes {
				r := f.CountAt(rng.Uint64(), window, *now)
				if r.Degraded {
					deg++
				}
				if r.Degraded || r.Upper > k {
					test++
				}
				if r.Upper >= k || r.Saturated {
					recipe++
					if r.Count > 0 {
						byMatch++
					} else {
						byFloor++
					}
				}
			}
			bytes := f.Bytes()
			pct := func(n int) float64 { return 100 * float64(n) / probes }
			t.Logf("%-27s flood %2dx x %2d: offenders Upper<true %d, Count>=90%% %3d/%d | new keys Degraded %5.2f%%, turned away (test) %6.2f%% (recipe) %6.2f%% = match %5.2f%% + floor %5.2f%% | floored %6d folded %6d | %5.1f B/key",
				c.name, fl.mult, fl.weight, below, rec, offenders, pct(deg), pct(test), pct(recipe), pct(byMatch), pct(byFloor), f.Floored(), f.Folded(), float64(bytes)/4096)
			if below > 0 {
				t.Errorf("%s flood %dx: %d offenders read Upper below the truth", c.name, fl.mult, below)
			}
		}
	}
}
