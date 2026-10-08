package temporalfilter

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hot keys touched continuously while inserts at ~98% load keep relocating
// and evicting entries: no hot key may end up in two slots or lose a count.
// (Before registers were published with their key, a touch landing between
// an install's two writes could reset a hot key's register.)
//
// Each hot key starts with initTouches events so eviction, which takes the
// lowest-count slot, never picks it over a one-event cold key: losses can
// then only come from races. With a single initial touch the test failed
// under CPU load when the workers lagged the fill: a hot key still at one
// event could be evicted like any cold key and lose that event.
//
// All touches happen at tick `at`, a multiple of 8 register units (8 ×
// Horizon/24 = 20s), where a one-minute window covers every bucket whole, so
// VelocityCount is exact up to 1,273 events per key; each worker stops after
// 200 passes over the hot keys (1,203 events per key at most). At other
// ticks the window's edge cuts the oldest bucket and VelocityCount
// interpolates it, which the test used to misread as lost counts on fast
// runs.
func TestTouchRacingRelocationKeepsCounts(t *testing.T) {
	const capN = 20_000
	f := newTest(t, capN, time.Minute)
	const hot, initTouches, passes, at = 2000, 3, 200, 60_000
	for k := range uint64(hot) {
		for range initTouches {
			f.CheckAndTouchAt(HashUint64(k), 0, at)
		}
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var touches atomic.Int64
	for w := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := int64(0)
			for i := uint64(w); !stop.Load() && n < passes*hot; i++ {
				f.CheckAndTouchAt(HashUint64(i%hot), 0, at)
				n++
			}
			touches.Add(n)
		}()
	}
	// fill to ~98% slot load with cold keys, forcing cuckoo moves
	fill := int(float64(f.Slots())*0.98) - hot
	for k := range uint64(fill) {
		f.CheckAndTouchAt(HashUint64(1<<40+k), 0, at)
	}
	stop.Store(true)
	wg.Wait()
	dups := 0
	total := 0
	for k := range uint64(hot) {
		h := HashUint64(k)
		fp, tg := fingerprint(h), tag(h)
		p := f.primary(h)
		a, b := f.findAny(p, fp, tg), f.findAny(f.alt(p, fp), fp, tg)
		if a >= 0 && b >= 0 {
			dups++
		}
		total += f.VelocityCountAt(h, f.Ticks(time.Minute), at)
	}
	t.Logf("relocations %d, evictions %d, hot dups %d, counted %d of %d touches", f.Relocations(), f.Evictions(), dups, total, touches.Load()+hot*initTouches)
	if dups > 0 {
		t.Fatalf("%d hot keys in two slots", dups)
	}
	if want := int(touches.Load()) + hot*initTouches; total != want {
		t.Fatalf("counted %d of %d touches", total, want)
	}
}
