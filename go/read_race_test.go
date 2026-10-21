package temporalfilter

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Keys that are never touched again are read continuously while a fill to
// 98% slot load keeps relocating them. A read must find the key wherever it
// is: a relocation that moves it from one of its lines to the other between
// the lookup's two line scans must not make it read as absent (Count 0,
// Upper below the truth), and in ConstantTime mode such a miss must not
// insert it a second time.
func TestReadDuringRelocationFindsKey(t *testing.T) {
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	const capN, keys, k, readers, at = 4000, 256, 3, 6, 60_000
	var bad, reads atomic.Int64
	for round := range rounds {
		f := newTest(t, capN, time.Minute)
		salt := uint64(round) << 40
		hs := make([]uint64, keys)
		for i := range hs {
			hs[i] = mix64(salt + uint64(i))
			for range k {
				f.CheckAndTouchAt(hs[i], 0, at)
			}
		}
		var stop atomic.Bool
		var wg sync.WaitGroup
		for w := range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := w; !stop.Load(); i++ {
					h := hs[i%keys]
					var r Result
					if i&1 == 0 {
						r = f.CountAt(h, 60_000, at)
					} else {
						r.Seen = f.ContainsAt(h, 60_000, at)
						r.Upper = k
					}
					reads.Add(1)
					if !r.Seen || r.Upper < k {
						if bad.Add(1) <= 5 {
							t.Logf("round %d key %d read %+v", round, i%keys, r)
						}
					}
				}
			}()
		}
		fill := int(float64(f.Slots())*0.98) - keys
		for c := range uint64(fill) {
			f.CheckAndTouchAt(mix64(salt+1<<36+c), 0, at)
		}
		stop.Store(true)
		wg.Wait()
		for i, h := range hs {
			if n := f.VelocityCountAt(h, 60_000, at); n != k {
				bad.Add(1)
				t.Logf("round %d key %d ends with count %d", round, i, n)
			}
		}
	}
	t.Logf("%d reads, %d missed a key or read it low", reads.Load(), bad.Load())
	if bad.Load() > 0 {
		t.Fatalf("%d reads missed a key that was there", bad.Load())
	}
}
