package temporalfilter

import (
	"sync"
	"testing"
	"time"
)

// ctFilter is a ConstantTime filter with a pad long enough to leave room
// for placing waiting keys, and a stash big enough never to drop a history
// (so a short count is a bug, not the stash's policy).
func ctFilter(t *testing.T, capacity int) *Filter {
	t.Helper()
	f, err := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond,
		ConstantTime: 4 * time.Microsecond, Stash: capacity})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// whereKey counts h's entries: in the table (as lookups match) and in the
// stash.
func (f *Filter) whereKey(h uint64) (table, stash int) {
	fp, tg := fingerprint(h), tag(h)
	p := f.primary(h)
	if f.find(p, fp<<1, tg) >= 0 {
		table++
	}
	if f.find(f.alt(p, fp), fp<<1|1, tg) >= 0 {
		table++
	}
	set, _ := f.stashSet(f.stashID(h, fp, tg))
	for _, e := range set {
		if e.id == f.stashID(h, fp, tg) {
			stash++
		}
	}
	return table, stash
}

// TestCTWaitingKeysKeepCounts: in ConstantTime an insert that finds its two
// lines full waits in the stash instead of searching, and the pads of later
// calls place it (by path or eviction, as a plain insert would). Filled to
// 97% of the slots, every key must keep its exact count, in one place,
// before and after placement has drained the stash.
func TestCTWaitingKeysKeepCounts(t *testing.T) {
	const capacity, per = 4096, 3
	f := ctFilter(t, capacity)
	n := f.Slots() * 97 / 100
	for round := range per {
		for i := range n {
			f.TouchAt(mix64(uint64(i)), 0, 1000+uint64(round))
		}
	}
	check := func(when string) {
		t.Helper()
		for i := range n {
			h := mix64(uint64(i))
			if r := f.CountAt(h, 60_000, 2000); r.Count != per || r.Saturated {
				t.Fatalf("%s: key %d reads %+v, want %d events", when, i, r, per)
			}
			if tb, st := f.whereKey(h); tb+st != 1 {
				t.Fatalf("%s: key %d has %d table and %d stash entries", when, i, tb, st)
			}
		}
	}
	check("after the inserts")
	waiting := f.Waiting()
	for range 1000 { // pads of any length (the race detector slows a step past 4 µs)
		if f.Waiting() == 0 {
			break
		}
		f.place(time.Now(), time.Millisecond, 2000)
	}
	t.Logf("%d keys at 97%% of %d slots: %d inserts waited, %d waiting after the inserts and Counts, %d after placing; evictions %d, folded %d",
		n, f.Slots(), f.Waited(), waiting, f.Waiting(), f.Evictions(), f.Folded())
	if f.Waited() == 0 {
		t.Fatal("no insert waited: the test doesn't exercise placement")
	}
	if f.Waiting() != 0 {
		t.Fatalf("%d keys still wait", f.Waiting())
	}
	check("after placing")
}

// TestCTWaitingKeysUnderRaces: workers touch shared keys at 98% load in
// ConstantTime, so inserts wait in the stash and pads place them while
// other workers touch the same keys. Every key must end in one place with
// its exact count.
func TestCTWaitingKeysUnderRaces(t *testing.T) {
	rounds := 10
	if testing.Short() {
		rounds = 3
	}
	const capacity, workers, per = 4096, 8, 4
	for round := range rounds {
		f := ctFilter(t, capacity)
		n := f.Slots() * 98 / 100
		keys := make([]uint64, n)
		for i := range keys {
			keys[i] = mix64(uint64(round)<<40 | uint64(i))
		}
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for p := range per * n / workers {
					f.TouchAt(keys[(p*workers+w)%n], 0, 1000)
				}
			}()
		}
		wg.Wait()
		total := 0
		for i, h := range keys {
			r := f.CountAt(h, 60_000, 1000)
			total += r.Count
			if tb, st := f.whereKey(h); tb+st != 1 {
				t.Fatalf("round %d key %d has %d table and %d stash entries", round, i, tb, st)
			}
		}
		if want := per * n / workers * workers; total != want {
			t.Fatalf("round %d: counted %d of %d touches (waiting %d, evictions %d, folded %d)", round, total, want, f.deferred.Load(), f.Evictions(), f.Folded())
		}
	}
}
