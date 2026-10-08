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
func TestTouchRacingRelocationKeepsCounts(t *testing.T) {
	const capN = 20_000
	f := newTest(t, capN, time.Minute)
	const hot = 2000
	for k := range uint64(hot) {
		f.CheckAndTouchAt(HashUint64(k), 0, 1000)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var touches atomic.Int64
	for w := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := int64(0)
			for i := uint64(w); !stop.Load(); i++ {
				f.CheckAndTouchAt(HashUint64(i%hot), 0, 1000)
				n++
			}
			touches.Add(n)
		}()
	}
	// fill to ~98% slot load with cold keys, forcing cuckoo moves
	fill := int(float64(f.Slots())*0.98) - hot
	for k := range uint64(fill) {
		f.CheckAndTouchAt(HashUint64(1<<40+k), 0, 1000)
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
		total += f.VelocityCountAt(h, f.Ticks(time.Minute), 1000)
	}
	t.Logf("relocations %d, evictions %d, hot dups %d, counted %d of %d touches", f.Relocations(), f.Evictions(), dups, total, touches.Load()+hot)
	if dups > 0 {
		t.Fatalf("%d hot keys in two slots", dups)
	}
	if want := int(touches.Load()) + hot; total != want {
		t.Fatalf("counted %d of %d touches", total, want)
	}
}
