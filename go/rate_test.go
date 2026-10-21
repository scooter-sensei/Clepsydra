package temporalfilter

import (
	"math"
	"testing"
	"time"
)

// TestSustainedRate: one key sent at a steady rate for three horizons of
// 60 s (1 ms ticks; past 1,000/s, several events per tick), queried at
// every touch of the last horizon for 1, 5, 15 and 60 s windows. Per rate and
// window: Count / exact (min, max), the share of answers flagged Saturated,
// and unflagged answers with Upper below the exact count (the fail-closed
// contract: there must be none). With hot entries (the default) and without
// (Hot: -1, the 48-bit register alone). Deterministic: the clock is the
// caller's and nothing is random.
func TestSustainedRate(t *testing.T) {
	const hTicks = 60_000
	rates := []int{10, 12, 13, 20, 50, 100, 200, 500, 1000, 2000, 5000}
	windows := []uint64{1_000, 5_000, 15_000, 60_000}
	low := 0
	for _, hot := range []int{0, -1} {
		for _, rate := range rates {
			every := uint64(max(1, 1000/rate)) // ticks between sends
			per := max(1, rate/1000)           // events per send
			f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: hot})
			if err != nil {
				t.Fatal(err)
			}
			h := mix64(uint64(rate))
			type acc struct {
				lo, hi      float64
				sat, n, bad int
			}
			st := make([]acc, len(windows))
			for i := range st {
				st[i].lo = math.Inf(1)
			}
			for now := uint64(1); now < 3*hTicks; now += every {
				for range per {
					f.TouchAt(h, 0, now)
				}
				if now < 2*hTicks {
					continue
				}
				for wi, w := range windows {
					exact := per * int(w/every+1) // sends in [now-w, now], as the filter counts
					r := f.CountAt(h, w, now)
					a := &st[wi]
					a.n++
					if r.Saturated {
						a.sat++
					} else if r.Upper < exact {
						a.bad++
					}
					q := float64(r.Count) / float64(exact)
					a.lo, a.hi = min(a.lo, q), max(a.hi, q)
				}
			}
			name := "hot entries"
			if hot < 0 {
				name = "register only"
			}
			for wi, w := range windows {
				a := st[wi]
				low += a.bad
				t.Logf("%-13s rate=%d/s window=%ds count/exact min=%.3f max=%.3f saturated=%.1f%% unflagged_upper_low=%d",
					name, rate, w/1000, a.lo, a.hi, 100*float64(a.sat)/float64(a.n), a.bad)
			}
			if hot == 0 {
				t.Logf("%-13s rate=%d/s promoted=%d refused=%d", name, rate, f.Promoted(), f.HotRefused())
			}
		}
	}
	if low > 0 {
		t.Errorf("%d unflagged answers read Upper below the exact count", low)
	}
}
