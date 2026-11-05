package temporalfilter

import (
	"math"
	"sort"
	"testing"
	"time"
)

// TestResize (claims D28): a Resizable resized at 120 s of tfbench's
// velocity workload (2,000 keys, hot entries off) against a filter built at
// the new size and fed every event, every 5 s from the resize: every key at
// windows of 1-60 s. Grown (2,560 -> 4,096) and shrunk (8,192 -> 4,096) with
// no filter evicting, it must give the same Seen, a count error per window
// within 1.05x the reference's and no unflagged Upper below the truth.
// Shrunk into a size that evicts (4,096 -> 1,280), the error within 1.05x
// and no more unflagged lows than the reference's (which sheds histories
// below SpillMin at that load). Grown from a filter that had evicted
// (1,280 -> 1,792), the old filter's losses stay until the transition ends
// and none after it (reported, the rest not judged).
func TestResize(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("Resizable refuses ConstantTime and Background filters")
	}
	const H = 60_000
	for _, c := range []struct {
		name     string
		from, to int
		evicts   bool // a filter evicts: Seen not compared
		lossy    bool // grown from a filter that evicted: reported
	}{
		{"grow", 2560, 4096, false, false},
		{"shrink", 8192, 4096, false, false},
		{"shrink, evicting", 4096, 1280, true, false},
		{"grow from an evicting filter", 1280, 1792, true, true},
	} {
		cfg := Config{Capacity: c.from, Horizon: time.Minute, Resolution: time.Millisecond, Hot: -1}
		z, err := NewResizable(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Capacity = c.to
		s, _ := New(cfg)
		hs := make([]uint64, 2000)
		for k := range hs {
			hs[k] = mix64(uint64(k) + 1<<50)
		}
		times := make([][]uint64, len(hs))
		resizeAt := uint64(0)
		var errM, errS, ex [5]float64
		var seenDiff, lowM, lowMflag, lowS, lowIn, lowAfter, checkpoints, inTransition, falseM, falseS int
		probed := false
		velWorkload(func(k int, tk uint64) {
			if resizeAt == 0 && tk >= 2*H {
				if err := z.Resize(c.to, tk); err != nil {
					t.Fatal(err)
				}
				resizeAt = tk
			}
			z.TouchAt(hs[k], 0, tk)
			s.TouchAt(hs[k], 0, tk)
			times[k] = append(times[k], tk)
		}, func(_ int, _ []uint64, _ int, now uint64) {
			if resizeAt == 0 || now%5000 != 0 {
				return
			}
			checkpoints++
			z.mu.Lock()
			in := z.old != nil
			z.mu.Unlock()
			if in {
				inTransition++
			}
			if in && now+5000 > resizeAt+H && !probed { // the last checkpoint in transition
				probed = true
				for i := range uint64(100_000) {
					a := mix64(i + 1<<52)
					if z.ContainsAt(a, H, now) {
						falseM++
					}
					if s.ContainsAt(a, H, now) {
						falseS++
					}
				}
			}
			for k, h := range hs {
				ts := times[k]
				for wi, w := range velWins {
					exact := len(ts) - sort.Search(len(ts), func(j int) bool { return ts[j]+w >= now })
					rm, rs := z.CountAt(h, w, now), s.CountAt(h, w, now)
					if !c.evicts && rm.Seen != rs.Seen {
						seenDiff++
					}
					errM[wi] += math.Abs(float64(rm.Count - exact))
					errS[wi] += math.Abs(float64(rs.Count - exact))
					ex[wi] += float64(exact)
					if rm.Upper < exact {
						switch {
						case rm.Saturated:
							lowMflag++
						case in:
							lowM++
							lowIn++
						default:
							lowM++
							lowAfter++
						}
					}
					if rs.Upper < exact && !rs.Saturated {
						lowS++
					}
				}
			}
		})
		ratio := [5]float64{}
		for wi := range ratio {
			ratio[wi] = errM[wi] / errS[wi]
		}
		t.Logf("%s %d -> %d: checkpoints %d (%d in transition); evictions resized %d, reference %d; moved %d",
			c.name, c.from, c.to, checkpoints, inTransition, z.Evictions(), s.Evictions(), z.moved)
		t.Logf("%s: count error ratio resized/reference at 1s 5s 15s 30s 60s: %.4f %.4f %.4f %.4f %.4f",
			c.name, ratio[0], ratio[1], ratio[2], ratio[3], ratio[4])
		t.Logf("%s: Seen differs %d; Upper < exact: resized unflagged %d (in transition %d, after %d; flagged %d), reference unflagged %d; false Seen of 100,000 absent keys in transition %d, reference %d",
			c.name, seenDiff, lowM, lowIn, lowAfter, lowMflag, lowS, falseM, falseS)
		if c.lossy {
			if lowAfter != 0 {
				t.Errorf("%s: %d unflagged lows after the transition", c.name, lowAfter)
			}
			continue
		}
		if seenDiff != 0 {
			t.Errorf("%s: Seen differs %d times", c.name, seenDiff)
		}
		for wi, r := range ratio {
			if r > 1.05 {
				t.Errorf("%s: count error at %d ms %.4fx the reference's", c.name, velWins[wi], r)
			}
		}
		if lowM > lowS {
			t.Errorf("%s: %d unflagged lows against the reference's %d", c.name, lowM, lowS)
		}
		if !c.evicts && (z.Evictions() != 0 || s.Evictions() != 0 || lowM != 0) {
			t.Errorf("%s: evictions or unflagged lows where none were expected", c.name)
		}
	}
}

// TestResizeRefuses: hot entries, ConstantTime and Background are refused,
// and so is a resize during a transition.
func TestResizeRefuses(t *testing.T) {
	if _, err := NewResizable(Config{Capacity: 1000, Horizon: time.Minute}); err == nil {
		t.Error("hot entries on: no error")
	}
	if _, err := NewResizable(Config{Capacity: 1000, Horizon: time.Minute, Hot: -1, ConstantTime: time.Microsecond}); err == nil {
		t.Error("ConstantTime: no error")
	}
	if _, err := NewResizable(Config{Capacity: 1000, Horizon: time.Minute, Hot: -1, Background: true}); err == nil {
		t.Error("Background: no error")
	}
	if forceCT || forceBG {
		return
	}
	z, err := NewResizable(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := z.Resize(2000, 10); err != nil {
		t.Fatal(err)
	}
	if err := z.Resize(4000, 20); err == nil {
		t.Error("a resize during a transition: no error")
	}
	if err := z.Resize(4000, 10+z.cur.horizon+1); err != nil {
		t.Errorf("a resize after the transition: %v", err)
	}
}

// BenchmarkResizeAbsent: ContainsAt of absent keys on a Resizable in mid
// transition (old table full, half its keys moved) and on a plain filter of
// the new size holding the same keys.
func BenchmarkResizeAbsent(b *testing.B) {
	const n = 1 << 18
	cfg := Config{Capacity: n, Horizon: time.Minute, Hot: -1}
	z, err := NewResizable(cfg)
	if err != nil {
		b.Skip(err)
	}
	cfg.Capacity = 2 * n
	p, _ := New(cfg)
	for i := range uint64(n) {
		z.TouchAt(mix64(i), 0, 1000)
		p.TouchAt(mix64(i), 0, 1000)
	}
	z.Resize(2*n, 2000)
	for i := range uint64(n / 2) {
		z.TouchAt(mix64(i), 0, 3000)
	}
	b.Run("transition", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			z.ContainsAt(mix64(uint64(i)+1<<40), 60_000_000, 4000)
		}
	})
	b.Run("plain", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			p.ContainsAt(mix64(uint64(i)+1<<40), 60_000_000, 4000)
		}
	})
}
