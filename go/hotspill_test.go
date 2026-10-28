package temporalfilter

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestHotAddAfterSpillCounts (claims A32): K is hot, its 40 events at t0.
// K's touch T at t1, a horizon less a second later, moves word0 and reads
// K's slot (the hot pointer). Before T adds to the entry, an insert of K2
// at t1 + 6 s finds both lines full and evicts K, whose entry's events are
// past the horizon at K2's clock (the lightest slot), and with no stash
// spills K's history to the overflow table. T's add counts by K's identity
// in K's entry, which no slot or stash points to any more: before, it
// succeeded there and the event was gone (case 9787 of the concurrent
// fuzzer, 2026-10-25).
func TestHotAddAfterSpillCounts(t *testing.T) {
	f, err := New(Config{Capacity: 4, Horizon: time.Minute, Resolution: time.Millisecond,
		Clock: func() uint64 { return 1 }, Stash: -1})
	if err != nil {
		t.Fatal(err)
	}
	f.ct, f.bg = false, false // the race is in the plain path's order of steps (TF_CT, TF_BG)
	if f.lines != 2 {
		t.Fatalf("lines %d, want 2", f.lines)
	}
	const t0 = 60_000
	t1 := uint64(t0 + 59_000) // within a horizon of K's last touch
	t2 := t1 + 6_000          // K's events past the horizon (their unit too)
	fps := twoLineFPs(f, 0, 0x1100, 9)
	k := keyAt(f, 1, fps[7], 0x5a5)
	for range 40 {
		f.TouchAt(k, 0, t0)
	}
	ks := slotsOf(f, k)
	if len(ks) != 1 || !isHot(atomic.LoadUint64(&f.words[ks[0]+1])) {
		t.Fatalf("setup: K in %v, not hot", ks)
	}
	for j := range 7 { // two events each at t1: heavier than K at t2
		h := keyAt(f, uint64(j%2), fps[j], 0x100+uint64(j))
		f.TouchAt(h, 0, t1)
		f.TouchAt(h, 0, t1)
	}
	if f.Occupancy(t1) != 8 {
		t.Fatalf("setup: occupancy %d, want 8", f.Occupancy(t1))
	}
	k2 := keyAt(f, 0, fps[8], 0x5a6)
	i := ks[0]
	fired, gone := false, false
	testHookHotAdding = func(j int) {
		if fired || j != i {
			return
		}
		fired = true
		f.TouchAt(k2, 0, t2)
		gone = len(slotsOf(f, k)) == 0
	}
	defer func() { testHookHotAdding = nil }()
	f.TouchAt(k, 0, t1) // T
	testHookHotAdding = nil
	if !fired || !gone {
		t.Fatal("K2 did not evict K during K's hot add")
	}
	if r := f.CountAt(k, f.horizon, t2); r.Upper < 1 && !r.Saturated {
		t.Fatalf("K reads %+v after a touch at t1", r)
	}
}

// TestHotAddAfterStashDropCounts (claims A32): as TestHotAddAfterSpillCounts,
// but K's history goes to the stash (one set of 4 ways), and is dropped from
// it before T adds: K's entry holds only events past the horizon, so the
// history weighs nothing (T's event isn't in it yet), and the fourth put
// after it, finding no free or expired way, takes K's as the lightest, with
// nothing to spill. T's add counted in K's entry, which no slot or stash
// pointed to any more, and the event was gone (case 9787, 2026-10-28).
func TestHotAddAfterStashDropCounts(t *testing.T) {
	f, err := New(Config{Capacity: 4, Horizon: time.Minute, Resolution: time.Millisecond,
		Clock: func() uint64 { return 1 }, Stash: 4})
	if err != nil {
		t.Fatal(err)
	}
	f.ct, f.bg = false, false // the race is in the plain path's order of steps (TF_CT, TF_BG)
	if f.lines != 2 || f.stashN != 1 {
		t.Fatalf("lines %d, stash sets %d; want 2, 1", f.lines, f.stashN)
	}
	const t0 = 60_000
	t1 := uint64(t0 + 59_000) // within a horizon of K's last touch
	t2 := t1 + 6_000          // K's events past the horizon (their unit too)
	fps := twoLineFPs(f, 0, 0x1100, 13)
	k := keyAt(f, 1, fps[7], 0x5a5)
	for range 40 {
		f.TouchAt(k, 0, t0)
	}
	ks := slotsOf(f, k)
	if len(ks) != 1 || !isHot(atomic.LoadUint64(&f.words[ks[0]+1])) {
		t.Fatalf("setup: K in %v, not hot", ks)
	}
	for j := range 7 { // two events each at t1: heavier than K, and than the newcomers, at t2
		h := keyAt(f, uint64(j%2), fps[j], 0x100+uint64(j))
		f.TouchAt(h, 0, t1)
		f.TouchAt(h, 0, t1)
	}
	if f.Occupancy(t1) != 8 || f.Evictions() != 0 {
		t.Fatalf("setup: occupancy %d, evictions %d; want 8, 0", f.Occupancy(t1), f.Evictions())
	}
	i := ks[0]
	id := f.stashID(k, fingerprint(k), tag(k))
	fired, gone, stashed, dropped, ev := false, false, false, false, uint64(0)
	testHookHotAdding = func(j int) {
		if fired || j != i {
			return
		}
		fired = true
		// K2 evicts K (into the stash's first way); K3-K5 each evict the
		// newcomer before them (one event: lighter than the rest) into the
		// next ways; K6's eviction finds all four ways live and K's the
		// lightest.
		for n := range 5 {
			f.TouchAt(keyAt(f, uint64(n%2), fps[8+n], 0x5a6+uint64(n)), 0, t2)
			if n == 0 {
				gone = len(slotsOf(f, k)) == 0
				_, _, stashed = f.stashGet(id, t2, false)
			}
		}
		_, _, inStash := f.stashGet(id, t2, false)
		dropped, ev = !inStash, f.Evictions()
	}
	defer func() { testHookHotAdding = nil }()
	f.TouchAt(k, 0, t1) // T
	testHookHotAdding = nil
	if !fired || !gone || !stashed || !dropped || ev != 5 {
		t.Fatalf("during K's hot add: K evicted %v, stashed %v, then dropped %v; evictions %d, want 5", gone, stashed, dropped, ev)
	}
	if r := f.CountAt(k, f.horizon, t2); r.Upper < 1 && !r.Saturated {
		t.Fatalf("K reads %+v after a touch at t1", r)
	}
}
