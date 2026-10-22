package temporalfilter

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestBackgroundWaitsAndPlaces: with Background, a first touch whose lines
// are full and that needs a move waits in the wait room instead of
// searching. Its touches and counts find it there, the placer moves it into
// the table with its history, and a filter that never evicts holds no
// stash. The placer runs by hand here (bgManual), so the test is
// deterministic.
func TestBackgroundWaitsAndPlaces(t *testing.T) {
	if forceCT {
		t.Skip("Background has no effect with ConstantTime")
	}
	f, err := New(Config{Capacity: 4096, Horizon: time.Minute, Resolution: time.Millisecond,
		Clock: func() uint64 { return 1 }, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	f.bgManual = true
	var key uint64
	for i := uint64(1); key == 0; i++ {
		w := f.Waited()
		f.TouchAt(mix64(i), 0, 1)
		if f.Waited() != w {
			key = mix64(i)
		}
	}
	fp, tg := fingerprint(key), tag(key)
	if f.locate(key, fp, tg) >= 0 || f.Waiting() != 1 {
		t.Fatalf("waiting key in the table, or %d waiting", f.Waiting())
	}
	for n := 2; n <= 5; n++ {
		if r := f.TouchAt(key, 60_000, 1); r.Count != n || !r.Seen || r.Restored {
			t.Fatalf("touch %d of a waiting key: %+v", n, r)
		}
	}
	if r := f.CountAt(key, 60_000, 1); r.Count != 5 || r.Upper != 5 {
		t.Fatalf("count of a waiting key: %+v", r)
	}
	for f.Waiting() > 0 {
		f.placeNext(f.room.Load(), 0)
	}
	if f.locate(key, fp, tg) < 0 {
		t.Fatal("placed key not in the table")
	}
	if r := f.CountAt(key, 60_000, 1); r.Count != 5 || r.Upper != 5 {
		t.Fatalf("count of the placed key: %+v", r)
	}
	if f.stash.Load() != nil || f.Evictions() != 0 {
		t.Fatalf("stash allocated (%v) or %d evictions without a full table", f.stash.Load() != nil, f.Evictions())
	}
	t.Logf("waited %d, relocations %d, bytes %d (table %d)", f.Waited(), f.Relocations(), f.Bytes(), 16*f.Slots())
}

// TestBackgroundConcurrentCounts: 8 workers touch 32 keys 20 times each in
// a table of ~76 slots while flooding it with heavier keys, as
// TestStashConcurrentEvictions does, with Background on: keys wait in the
// wait room, are evicted to the stash and come back, and the placer moves
// waiting keys into the table without their claim stripe, all concurrently
// with the keys' own touches. With a stash and SpillMin that drop nothing,
// every key must end with its exact count, in at most one slot.
func TestBackgroundConcurrentCounts(t *testing.T) {
	if forceCT {
		t.Skip("Background has no effect with ConstantTime")
	}
	const workers, hot, perKey = 8, 32, 20
	rounds := 200
	if testing.Short() {
		rounds = 40
	}
	var waited uint64
	for round := range rounds {
		var clock atomic.Uint64
		clock.Store(1)
		f, err := New(Config{Capacity: 64, Horizon: time.Minute, Resolution: time.Millisecond,
			Clock: clock.Load, Stash: 1 << 12, SpillMin: 1, Background: true})
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
				t.Fatalf("round %d key %d: %+v, want count %d (waited %d, evictions %d, restored %d, lost %d)",
					round, i, r, perKey, f.Waited(), f.Evictions(), f.Restored(), f.Lost())
			}
		}
		settle(f)
		for i, h := range keys {
			fp, tg := fingerprint(h), tag(h)
			p := f.primary(h)
			n := 0
			for li, l := range []uint64{p, f.alt(p, fp)} {
				if li == 1 && l == p {
					break
				}
				for s := 0; s < wordsPerLine; s += 2 {
					w0, w1 := f.words[int(l)*wordsPerLine+s], f.words[int(l)*wordsPerLine+s+1]
					if w0>>48 == fp && w1>>tagShift == tg && w0&dispBit != 0 == (li == 1) {
						n++
					}
				}
			}
			if n > 1 {
				t.Fatalf("round %d key %d: in %d slots", round, i, n)
			}
		}
		waited += f.Waited()
	}
	t.Logf("%d rounds: %d waits", rounds, waited)
}
