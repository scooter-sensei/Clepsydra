package temporalfilter

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Few hot keys, eight workers and a fill to 99% slot load, repeated: two
// touches of one key often overlap a move of it, so moves get abandoned (a
// touch reached the source first). While copies were visible from their
// install (until claims A23) the copy had often counted touches of its
// own, which had to be added back, not dropped with it; now a copy is
// hidden until the hand-off, and no touch may be lost either way.
//
// It also catches a rarer race: a touch that checked its slot's fingerprint
// counting into the next occupant's register, when the slot is handed over
// before its CAS and the newcomer has the same 12-bit tag. TF_DISTINCT_TAGS=1
// keeps cold keys off the hot keys' tags, which rules that one out.
func TestAbandonedMoveKeepsCounts(t *testing.T) {
	iters := 300
	if testing.Short() {
		iters = 30
	}
	distinct := os.Getenv("TF_DISTINCT_TAGS") != ""
	const hot, workers, at = 64, 8, 60_000
	failed, lost := 0, 0
	for it := range iters {
		f := newTest(t, 4000, time.Minute)
		salt := uint64(it) << 32
		hotTags := map[uint64]bool{}
		for k := range uint64(hot) {
			hotTags[tag(mix64(salt+k))] = true
			for range 3 {
				f.CheckAndTouchAt(mix64(salt+k), 0, at)
			}
		}
		var stop atomic.Bool
		var wg sync.WaitGroup
		var touches atomic.Int64
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n := int64(0)
				for i := uint64(w / 2); !stop.Load() && n < 150*hot; i++ {
					f.CheckAndTouchAt(mix64(salt+i%hot), 0, at)
					n++
				}
				touches.Add(n)
			}()
		}
		fill := int(float64(f.Slots())*0.99) - hot
		for k, n := uint64(0), 0; n < fill; k++ {
			h := mix64(salt + 1<<40 + k)
			if distinct && hotTags[tag(h)] {
				continue
			}
			f.CheckAndTouchAt(h, 0, at)
			n++
		}
		stop.Store(true)
		wg.Wait()
		total := 0
		for k := range uint64(hot) {
			total += f.VelocityCountAt(mix64(salt+k), f.Ticks(time.Minute), at)
		}
		if want := int(touches.Load()) + hot*3; total != want {
			failed++
			lost += want - total
		}
	}
	t.Logf("%d of %d rounds lost counts (%d in all)", failed, iters, lost)
	if failed > 0 {
		t.Fatalf("%d of %d rounds lost counts", failed, iters)
	}
}
