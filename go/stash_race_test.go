package temporalfilter

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStashConcurrentEvictions: 8 workers touch 32 hot keys in a table of
// ~76 slots while flooding it with heavier keys, so hot keys are evicted to
// the stash and restored over and over, concurrently with their own touches.
// With a stash big enough to drop nothing, every hot key must end with its
// exact count.
func TestStashConcurrentEvictions(t *testing.T) {
	const workers, hot, perKey = 8, 32, 20
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	for round := range rounds {
		var clock atomic.Uint64
		clock.Store(1)
		f, err := New(Config{Capacity: 64, Horizon: time.Minute, Resolution: time.Millisecond,
			Clock: clock.Load, Stash: 1 << 12})
		if err != nil {
			t.Fatal(err)
		}
		keys := make([]uint64, hot)
		for i := range keys {
			keys[i] = mix64(uint64(round)<<32 | uint64(i))
		}
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(uint64(round), uint64(w)))
				for i := range hot * perKey / workers {
					now := clock.Add(25)
					f.TouchAt(keys[(i*workers+w)%hot], 0, now)
					if i%2 == 0 {
						h := rng.Uint64()
						for range 25 {
							f.TouchAt(h, 0, now)
						}
					}
				}
			}()
		}
		wg.Wait()
		now := clock.Load()
		for i, h := range keys {
			r := f.CountAt(h, f.Ticks(time.Minute), now)
			if r.Count != perKey || r.Saturated {
				t.Fatalf("round %d key %d: %+v, want count %d (evictions %d, restored %d, lost %d)",
					round, i, r, perKey, f.Evictions(), f.Restored(), f.Lost())
			}
		}
		if round == 0 {
			t.Logf("round 0: evictions %d, restored %d, lost %d", f.Evictions(), f.Restored(), f.Lost())
		}
	}
}
