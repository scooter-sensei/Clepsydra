package temporalfilter

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// Concurrent adds to a small ghost table across many slice boundaries. An
// add decodes the entries' 2-bit slices against the epoch it read; if
// another add moved the epoch on and wrote an entry for the new slice
// meanwhile, that entry decodes as four slices old, dead, and must not be
// taken for a free one and overwritten. Each worker reads back its last
// three histories after every add: each must keep its count in Upper,
// through its own entry or a floor, unless the shared clock has moved far
// enough during the read for the history to have rightly aged out.
func TestGhostEpochRace(t *testing.T) {
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	// More goroutines than processors, so an add can be preempted between
	// reading the epoch and scanning its bucket; 4 buckets and a clock that
	// moves 200 ticks per add keep about 75 histories live (of 92 entries)
	// and cross a slice boundary every 25 adds.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	const horizon, n, workers, per = 10_000, 5, 8, 4000
	var lost atomic.Int64
	for round := range rounds {
		g := newGhost(4*64, horizon)
		var clock atomic.Uint64
		clock.Store(horizon)
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				type rec struct{ id, last uint64 }
				var ring [3]rec
				for i := range uint64(per) {
					now := clock.Add(200)
					id := uint64(round)<<48 | uint64(w)<<32 | i + 1
					g.add(id, n, now, false, now)
					ring[i%3] = rec{id, now}
					at := clock.Load()
					for _, r := range ring {
						if r.id == 0 || at-r.last > horizon-g.sliceLen {
							continue
						}
						e, _ := g.estimate(r.id, horizon, at)
						// A slice's events are dead once an add has seen the slice
						// three later: the clock may have got there during the read.
						if after := clock.Load(); after/g.sliceLen >= r.last/g.sliceLen+3 {
							continue
						}
						if e.Upper < n && lost.Add(1) <= 5 {
							t.Logf("round %d: history %x added at %d reads %+v at %d", round, r.id, r.last, e, at)
						}
					}
				}
			}()
		}
		wg.Wait()
	}
	if lost.Load() > 0 {
		t.Fatalf("%d reads found a live history below its count", lost.Load())
	}
}
