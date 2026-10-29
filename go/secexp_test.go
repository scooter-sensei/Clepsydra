package temporalfilter

import (
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
)

// TestSketchShapes compares overflow-sketch geometries, and the ghost table
// in the same bytes, under floods (set TF_EXP=1). Each sketch config is a
// counter width, row count, slices per horizon and columns per slot ("ghost
// /4": a ghost table of the bytes of a sketch of one column per 4 slots); floods are heavy (keys sent k+1 times, so they evict
// the offenders) or light (one event per key, the cheapest flood).
func TestSketchShapes(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	const k, offenders, probes = 20, 200, 20_000
	window := uint64(60_000)
	type cfg struct {
		name string
		sh   sketchShape
		div  int // columns = slots / div
		gh   bool
	}
	cfgs := []cfg{
		{"16b r4 s4 /8 (dd30cbc)", sketchShape{4, 16, 4}, 8, false},
		{"16b r4 s4 /16", sketchShape{4, 16, 4}, 16, false},
		{"16b r4 s4 /4", sketchShape{4, 16, 4}, 4, false},
		{"8b  r4 s4 /8", sketchShape{4, 8, 4}, 8, false},
		{"8b  r4 s4 /4", sketchShape{4, 8, 4}, 4, false},
		{"8b  r4 s4 /2", sketchShape{4, 8, 4}, 2, false},
		{"16b r3 s4 /8", sketchShape{3, 16, 4}, 8, false},
		{"8b  r3 s4 /4", sketchShape{3, 8, 4}, 4, false},
		{"16b r4 s2 /8", sketchShape{4, 16, 2}, 8, false},
		{"8b  r4 s2 /4", sketchShape{4, 8, 2}, 4, false},
		{"8b  r4 s2 /2", sketchShape{4, 8, 2}, 2, false},
		{"ghost /8", sketchShape{4, 8, 2}, 8, true},
		{"ghost /4", sketchShape{4, 8, 2}, 4, true},
		{"ghost /2", sketchShape{4, 8, 2}, 2, true},
	}
	floods := []struct {
		name   string
		mult   int
		weight int
	}{
		{"heavy 1x", 1, k + 1}, {"heavy 2x", 2, k + 1}, {"heavy 3x", 3, k + 1}, {"heavy 4x", 4, k + 1},
		{"light 16x", 16, 1}, {"light 64x", 64, 1},
	}
	for _, c := range cfgs {
		line := ""
		var bpk float64
		for _, fl := range floods {
			f, now := attackFilter(t, 4096, histStash)
			f.skW = uint64(max(64, f.Slots()/c.div))
			f.skShape, f.ghChoices, f.ghSub = c.sh, 1, 1
			f.spillMin = 1 // shapes alone; TestSpillThreshold varies this
			f.cms = !c.gh
			bpk = float64(c.sh.bytes(f.skW)) / 4096
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
					*now++ // the whole flood spans at most 30 s of the 60 s horizon
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
			deg, strict := 0, 0
			for range probes {
				r := f.CountAt(rng.Uint64(), window, *now)
				if r.Degraded {
					deg++
				}
				if r.Degraded || r.Upper > k {
					strict++
				}
			}
			if below > 0 {
				t.Errorf("%s %s: %d offenders Upper below the truth", c.name, fl.name, below)
			}
			line += fmt.Sprintf(" | %3d %5.1f%% %5.1f%%", rec, 100*float64(deg)/probes, 100*float64(strict)/probes)
		}
		t.Logf("%-24s %5.1f B/key%s", c.name, bpk, line)
	}
	t.Logf("columns per flood (heavy 1x, 2x, 3x, 4x, light 16x, 64x): offenders with Count >= 90%% of truth (of %d) | new keys Degraded | new keys turned away by a strict limit of %d", offenders, k)
}

// histStash is the stash these experiments ran with (slots/8 at 4,096 keys,
// the default until a6aa29e); with the ghost table's one bucket per
// history and one floor, it keeps their recorded results reproducible.
const histStash = 4820 / 8

// TestSpillThreshold: histories lighter than spillMin events are shed
// instead of spilled, so cheap one-event floods add no sketch noise
// (TF_EXP=1).
func TestSpillThreshold(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	const k, offenders, probes = 20, 200, 20_000
	window := uint64(60_000)
	floods := []struct {
		name         string
		mult, weight int
	}{
		{"heavy 1x", 1, k + 1}, {"heavy 2x", 2, k + 1},
		{"w1 16x", 16, 1}, {"w2 16x", 16, 2}, {"w4 16x", 16, 4}, {"w8 8x", 8, 8},
	}
	for _, sh := range []struct {
		name string
		sh   sketchShape
		div  int
		gh   bool
	}{{"16b r4 s4 /8", sketchShape{4, 16, 4}, 8, false}, {"8b r4 s2 /4", sketchShape{4, 8, 2}, 4, false}, {"ghost /4", sketchShape{4, 8, 2}, 4, true}} {
		for _, min := range []uint64{1, 2, 4, 8} {
			line := ""
			for _, fl := range floods {
				f, now := attackFilter(t, 4096, histStash)
				f.skW = uint64(max(64, f.Slots()/sh.div))
				f.skShape, f.ghChoices, f.ghSub = sh.sh, 1, 1
				f.spillMin = min
				f.cms = !sh.gh
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
						*now++ // the whole flood spans at most 30 s of the 60 s horizon
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
				strict := 0
				for range probes {
					r := f.CountAt(rng.Uint64(), window, *now)
					if r.Degraded || r.Upper > k {
						strict++
					}
				}
				if below > 0 {
					t.Errorf("%s min %d %s: %d offenders Upper below the truth", sh.name, min, fl.name, below)
				}
				line += fmt.Sprintf(" | %3d %5.1f%%", rec, 100*float64(strict)/probes)
			}
			t.Logf("%-14s spillMin %d%s", sh.name, min, line)
		}
	}
	t.Logf("floods: heavy 1x, heavy 2x, 16x slots of 1-, 2-, 4-event keys, 8x of 8-event keys; per flood: offenders with Count >= 90%% (of %d) | new keys turned away by a strict limit of %d", offenders, k)
}
