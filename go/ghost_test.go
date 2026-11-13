package temporalfilter

import (
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrent adds to a small ghost table across many slice boundaries. An
// add decodes the entries' 2-bit slices against the epoch it read; if
// another add moved the epoch on and wrote an entry for the new slice
// meanwhile, that entry decodes as four slices old, dead, and must not be
// taken for a free one and overwritten. Each worker reads back its last
// three histories after every add: each must keep its count in Upper,
// through its own entry or a floor, unless the shared clock has moved far
// enough during the read for the history to have rightly aged out.
//
// The interleaving is made to happen rather than waited for: on one add pass
// in 16 a worker stops in the window (testHookGhostAdding: the pass has
// loaded the epoch and not yet its buckets; testHookGhostFloor, one raise in
// 4: a floor raise likewise) and yields until other adds have moved the
// epoch on one to three slices, then a few times more so the mover can write
// its entry for the new slice. The scan that follows decodes against the
// stale epoch (an entry one slice newer reads as four slices old, two or
// three newer as live but old) thousands of times a run, where preemption
// alone landed there a few dozen times. The workers run on every processor:
// the stops reach the guards there as often as on 2 (GOMAXPROCS(2), which
// waited for preemption), and a dropped guard shows in more reads. The test
// fails if no stop ever saw the epoch move, since it would no longer test
// the race (docs/artifact/claims.md, A47, has the measurements).
func TestGhostEpochRace(t *testing.T) {
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	// 4 buckets and a clock that moves 200 ticks per add keep about 75
	// histories live (of 92 entries) and cross a slice boundary every 25 adds.
	const horizon, n, workers, per = 10_000, 5, 8, 4000
	var lost, stops, stale atomic.Int64
	t.Cleanup(func() { testHookGhostAdding, testHookGhostFloor = nil, nil })
	for round := range rounds {
		g := newGhost(4*64, horizon, 1+round%2, 1+round%3) // one and two candidate buckets, one to three floors
		// stop holds the calling worker in its window on one pass in every,
		// until the epoch has moved on one to three slices (2 ms at most).
		// math/rand/v2's functions draw from the runtime's per-thread
		// generator: no state shared between the workers.
		stop := func(every uint64) {
			x := rand.Uint64()
			if x%every != 0 {
				return
			}
			stops.Add(1)
			ep, moves := g.epoch.Load(), 1+(x>>32)%3
			deadline := time.Now().Add(2 * time.Millisecond)
			for g.epoch.Load() < ep+moves {
				if time.Now().After(deadline) {
					return
				}
				runtime.Gosched()
			}
			for range 3 { // the mover writes its entry for the new slice
				runtime.Gosched()
			}
			stale.Add(1)
		}
		testHookGhostAdding = func() { stop(16) }
		testHookGhostFloor = func() { stop(4) }
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
		testHookGhostAdding, testHookGhostFloor = nil, nil
	}
	t.Logf("%d stops in the window, %d across an epoch move", stops.Load(), stale.Load())
	if lost.Load() > 0 {
		t.Fatalf("%d reads found a live history below its count", lost.Load())
	}
	if stale.Load() == 0 {
		t.Fatal("no worker stopped in its window saw the epoch move: the race went untested")
	}
}

// TestGhostAddAcrossEpoch: TestGhostEpochRace's interleaving, made to happen
// once and checked exactly. An add loads the epoch; before it scans its
// bucket another add moves the epoch on a slice and writes an entry for the
// new slice there. Decoded against the epoch the first add loaded, that entry
// is four slices old, dead: the first add must not take its place (the
// defect of changes 28, an add that read the epoch once).
func TestGhostAddAcrossEpoch(t *testing.T) {
	const horizon = 10_000             // slices of 5,000 ticks
	g := newGhost(64, horizon, 2, 3)   // one bucket: the histories meet in it
	g.add(1, 5, 52_000, false, 52_000) // slice 10: the epoch is 11
	fired := false
	testHookGhostAdding = func() {
		testHookGhostAdding = nil
		fired = true
		g.add(2, 7, 55_000, false, 55_000) // slice 11: the epoch moves on; history 2 takes the first free entry
	}
	t.Cleanup(func() { testHookGhostAdding = nil })
	g.add(3, 9, 54_999, false, 54_999) // slice 10, the epoch loaded before the move
	testHookGhostAdding = nil
	if !fired {
		t.Fatal("the hook didn't run: the add's window is gone")
	}
	for _, h := range []struct{ id, want uint64 }{{1, 5}, {2, 7}, {3, 9}} {
		if r, ok := g.estimate(h.id, horizon, 55_000); !ok || r.Count != int(h.want) {
			t.Errorf("history %d reads %+v, want %d events", h.id, r, h.want)
		}
	}
}

// TestGhostFloorAcrossEpoch: the same for a floor. A raise loads the epoch;
// before it loads the floor another raise moves the epoch on a slice and
// lifts the floor in the new slice. Decoded against the stale epoch the
// higher floor is dead, and writing the first raise's count over it would
// lower it (42cc598: "raiseFloor could lower a floor the same way"): every
// key reading the bucket must still see the higher floor in Upper.
func TestGhostFloorAcrossEpoch(t *testing.T) {
	const horizon = 10_000             // slices of 5,000 ticks
	g := newGhost(64, horizon, 1, 1)   // one bucket, one floor that every fingerprint reads
	g.add(1, 5, 52_000, false, 52_000) // slice 10: the epoch is 11
	fired := false
	testHookGhostFloor = func() {
		testHookGhostFloor = nil
		fired = true
		g.advance(11)                      // slice 11: the epoch moves on
		g.raiseFloor(0, 2, 20, 11, 55_000) // a history of 20 events leaves to the floor
	}
	t.Cleanup(func() { testHookGhostFloor = nil })
	g.raiseFloor(0, 3, 4, 10, 54_999) // a history of 4 events, slice 10, the epoch loaded before the move
	testHookGhostFloor = nil
	if !fired {
		t.Fatal("the hook didn't run: the raise's window is gone")
	}
	for _, id := range []uint64{1, 99} {
		if r, ok := g.estimate(id, horizon, 55_000); !ok || r.Upper < 20 {
			t.Errorf("key %d reads %+v: the floor of 20 was lowered", id, r)
		}
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

// TestGhostScanLayout: estimate's scan decodes three entries per word
// (entries 1 and 2 beside the floors in word 0, three in each of words
// 1-7), so the layout must keep three entries of ghEntryBits to a word.
func TestGhostScanLayout(t *testing.T) {
	if ghPerWord != 3 || 3*ghEntryBits > 64 {
		t.Fatalf("ghPerWord %d, ghEntryBits %d: estimate's scan decodes three entries per word", ghPerWord, ghEntryBits)
	}
}
