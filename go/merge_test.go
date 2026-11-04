package temporalfilter

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// TestMerge splits tfbench's velocity workload at random between filters A
// and B, against S fed everything; every 5 s a fresh M = Merge(A) + Merge(B)
// must answer as S does: the same Seen everywhere (no filter evicts here), a
// count error per window at most 1.5x S's, and no Upper below the truth
// unless flagged Saturated (hot entries are off, so some buckets overflow,
// in M and S alike).
func TestMerge(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("Merge refuses ConstantTime and Background filters (TestMergeRefuses)")
	}
	cfg := Config{Capacity: 4096, Horizon: time.Minute, Resolution: time.Millisecond, Hot: -1}
	mk := func() *Filter {
		f, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	a, b, s := mk(), mk(), mk()
	key := func(k int) uint64 { return mix64(uint64(k) + 1) }
	rng := rand.New(rand.NewPCG(13, 13))
	times := make([][]uint64, 2000)
	wins := []uint64{1_000, 5_000, 15_000, 30_000, 60_000}
	var errM, errS, sum [5]float64
	var seenDiff, cmp, lowUnflagged, lowFlagged, checkpoints int
	var m *Filter
	velWorkload(func(k int, at uint64) {
		times[k] = append(times[k], at)
		s.CheckAndTouchAt(key(k), 0, at)
		if rng.IntN(2) == 0 {
			a.CheckAndTouchAt(key(k), 0, at)
		} else {
			b.CheckAndTouchAt(key(k), 0, at)
		}
	}, func(_ int, _ []uint64, _ int, now uint64) {
		if now%5000 != 0 || now < 65_000 {
			return
		}
		checkpoints++
		m = mk()
		if err := m.Merge(a); err != nil {
			t.Fatal(err)
		}
		if err := m.Merge(b); err != nil {
			t.Fatal(err)
		}
		for k := range times {
			for wi, w := range wins {
				exact := 0
				for i := len(times[k]) - 1; i >= 0 && times[k][i] > now-w; i-- {
					exact++
				}
				rm, rs := m.CountAt(key(k), w, now), s.CountAt(key(k), w, now)
				cmp++
				if rm.Seen != rs.Seen {
					seenDiff++
				}
				errM[wi] += float64(max(rm.Count-exact, exact-rm.Count))
				errS[wi] += float64(max(rs.Count-exact, exact-rs.Count))
				sum[wi] += float64(exact)
				if rm.Upper < exact {
					if rm.Saturated {
						lowFlagged++
					} else {
						lowUnflagged++
					}
				}
			}
		}
	})
	if checkpoints == 0 {
		t.Fatal("no checkpoint")
	}
	t.Logf("checkpoints %d, comparisons %d; Upper < exact flagged Saturated %d", checkpoints, cmp, lowFlagged)
	if ev := a.Evictions() + b.Evictions() + s.Evictions() + m.Evictions(); ev != 0 {
		t.Fatalf("%d evictions: the comparison needs none", ev)
	}
	if seenDiff != 0 {
		t.Errorf("Seen differs from the single filter's in %d of %d answers", seenDiff, cmp)
	}
	if lowUnflagged != 0 {
		t.Errorf("Upper below the truth, unflagged, in %d answers", lowUnflagged)
	}
	for wi, w := range wins {
		t.Logf("window %2ds  MAE merged %.4f  single %.4f", w/1000, errM[wi]/sum[wi], errS[wi]/sum[wi])
		if errM[wi] > 1.5*errS[wi] {
			t.Errorf("window %ds: merged count error %.0f over 1.5x the single filter's %.0f", w/1000, errM[wi], errS[wi])
		}
	}
	const end = 5 * 60_000
	fm, fs := 0, 0
	for i := range uint64(1_000_000) {
		h := mix64(i + 1<<40)
		if m.ContainsAt(h, 60_000, end) {
			fm++
		}
		if s.ContainsAt(h, 60_000, end) {
			fs++
		}
	}
	t.Logf("absent probes 1e6: false Seen merged %d, single %d", fm, fs)
}

// TestMergeRefuses: Merge returns an error and leaves f as it was for f
// itself, another Config, ConstantTime, and hot entries in use.
func TestMergeRefuses(t *testing.T) {
	mk := func(c Config) *Filter {
		f, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	base := Config{Capacity: 4096, Horizon: time.Minute}
	f := mk(base)
	f.CheckAndTouchAt(mix64(1), 0, 1000)
	hot := mk(base)
	for i := range 100 {
		hot.CheckAndTouchAt(mix64(2), 0, 1000+uint64(i))
	}
	if hot.Promoted() == 0 {
		t.Fatal("no key was promoted to a hot entry")
	}
	before := slices.Clone(f.words)
	for name, g := range map[string]*Filter{
		"itself":         f,
		"other Capacity": mk(Config{Capacity: 8192, Horizon: time.Minute}),
		"other Horizon":  mk(Config{Capacity: 4096, Horizon: 2 * time.Minute}),
		"ConstantTime":   mk(Config{Capacity: 4096, Horizon: time.Minute, ConstantTime: time.Microsecond}),
		"hot entries":    hot,
	} {
		if err := f.Merge(g); err == nil {
			t.Errorf("Merge with %s: no error", name)
		}
	}
	if !slices.Equal(before, f.words) {
		t.Error("a refused Merge changed f")
	}
}
