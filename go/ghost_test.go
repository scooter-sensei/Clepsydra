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
		g := newGhost(4*64, horizon, 1+round%2, 1+round%3) // one and two candidate buckets, one to three floors
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

// TestGhostAddAheadOfClock: an evictor whose clock is behind the evicted
// key's last touch spills a history whose newest event lies in a slice past
// the one its own clock is in. The entry's 2-bit slice must still decode
// to that slice: before, it decoded four slices old, so reads skipped it,
// adds took its place as free and the next epoch swept it.
func TestGhostAddAheadOfClock(t *testing.T) {
	const horizon = 10_000 // slices of 5,000 ticks
	g := newGhost(4*64, horizon, 2, 3)
	g.add(1, 5, 80_008, false, 79_989) // last touch 80,008; the evictor's clock reads 79,989
	for _, now := range []uint64{79_989, 80_008, 80_037} {
		if r, _ := g.estimate(1, horizon, now); r.Count != 5 {
			t.Fatalf("at %d the history reads %+v, want 5 events", now, r)
		}
	}
	g.add(2, 5, 85_000, false, 85_000) // a later slice: the epoch moves on, dead entries are swept
	if r, _ := g.estimate(1, horizon, 85_000); r.Count != 5 {
		t.Fatalf("after the next epoch the history reads %+v, want 5 events", r)
	}
}

// TestGhostReadAcrossEpoch: a read loads its buckets, then an add moves the
// epoch on two slices and sweeps the entries dead by then, before the read
// decodes what it loaded. Decoded against the new epoch, an entry four
// slices older than it (the sweep's, loaded before it was cleared) reads as
// current: a history whose newest event left the horizon reads as live,
// Seen with its Count. Against the epoch the lines were loaded under it is
// dead.
func TestGhostReadAcrossEpoch(t *testing.T) {
	const horizon = 10_000 // slices of 5,000 ticks
	for _, choices := range []int{1, 2} {
		g := newGhost(4*64, horizon, choices, 3)
		g.add(1, 5, 1_000, false, 1_000)   // slice 0
		g.add(2, 5, 10_500, false, 10_500) // slice 2: the epoch moves on, nothing is dead yet
		// At 20,100 (slice 4) history 1's newest event is 19,100 old: dead.
		// History 2's (slice 2) is live, so the read gets past g.until.
		const now = 20_100
		if r, ok := g.estimate(1, horizon, now); ok || r != (Result{}) {
			t.Fatalf("choices %d: history 1 reads %+v before the add, want nothing", choices, r)
		}
		testHookGhostRead = func() {
			testHookGhostRead = nil
			g.add(3, 5, now-50, false, now-50) // slice 4: the epoch moves on two, slice 0 is swept
		}
		r, ok := g.estimate(1, horizon, now)
		testHookGhostRead = nil
		if ok || r != (Result{}) {
			t.Errorf("choices %d: history 1, dead, reads %+v across the epoch's move", choices, r)
		}
		if r, ok := g.estimate(2, horizon, now); !ok || r.Count != 5 {
			t.Errorf("choices %d: history 2, live, reads %+v", choices, r)
		}
	}
}
