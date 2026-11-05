package temporalfilter

import (
	"os"
	"slices"
	"testing"
	"time"
)

// TestSweepLatency (TF_EXP=1; claims S79, E36) floods a filter of 256K keys'
// capacity with 4-event keys across 7 slices of the overflow table and times
// every TouchAt after the first slice (the tables allocated and full), and
// logs the largest calls with their slice and offset in it. A sweep of the
// whole overflow table when its epoch moves shows as the largest calls, on a
// slice's first tick.
func TestSweepLatency(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	f, err := New(Config{Capacity: 1 << 18, Horizon: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	h := f.Ticks(time.Minute)
	const keys = 2 << 20
	span := h * 7 / 2
	lat := make([]uint32, 0, 4*keys)
	at := make([]uint64, 0, 4*keys)
	sl := (h + 1) / 2
	for k := range uint64(keys) {
		x := mix64(k + 1<<50)
		now := 1 + span*k/keys
		for range 4 {
			t0 := time.Now()
			f.TouchAt(x, h, now)
			d := uint32(min(time.Since(t0), 1<<31))
			if now >= sl {
				lat, at = append(lat, d), append(at, now)
			}
		}
	}
	idx := make([]int, len(lat))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int { return int(lat[b]) - int(lat[a]) })
	for _, i := range idx[:10] {
		t.Logf("top %d ns at slice %d +%.4f of a slice", lat[i], at[i]/sl, float64(at[i]%sl)/float64(sl))
	}
	slices.Sort(lat)
	q := func(p float64) uint32 { return lat[int(p*float64(len(lat)-1))] }
	t.Logf("calls %d max %d p99.99 %d p99.9 %d p50 %d ns; folded %d floored %d bytes %d",
		len(lat), lat[len(lat)-1], q(0.9999), q(0.999), q(0.5), f.folded.Load(), f.Floored(), f.Bytes())
}
