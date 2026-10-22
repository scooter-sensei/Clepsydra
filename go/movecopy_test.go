package temporalfilter

import "testing"

// TestReadDuringMoveSeesEveryEvent drives one interleaving in moveTable. A
// relocation copies X from A to its primary line P, which lookups reach
// first; before the move hands off, a touch that found X at the source just
// before the copy went in (a batch's warm, or any lookup a moment earlier)
// counts there. X's next count, and its next touch, must see that event.
// While copies were visible from their install (until claims A23) they
// found the copy, without it: Upper 1 for 2 events, then 2 for 3.
func TestReadDuringMoveSeesEveryEvent(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("ConstantTime and Background inserts don't search on the call path")
	}
	f, x, key, _ := moveTable(t)
	t2 := 1 + f.horizon + 1 // P's oldest entry has expired: room for X's copy
	src := f.find(mvA, fingerprint(x)<<1|1, tag(x))
	k := key(mvA, mvB) // its first touch moves X to P
	fired := false
	var count, touch Result
	testHookCopied = func() {
		if fired {
			return
		}
		fired = true
		f.touch(x, 0, t2, false, src) // starts from the slot it found X in: the source
		count = f.CountAt(x, f.horizon, t2)
		touch = f.TouchAt(x, f.horizon, t2)
	}
	defer func() { testHookCopied = nil }()
	f.TouchAt(k, 0, t2)
	if !fired {
		t.Fatal("k's insert did not relocate X")
	}
	if count.Upper < 2 && !count.Saturated {
		t.Errorf("during the move, X's count read %+v, want Upper >= 2", count)
	}
	if touch.Upper < 3 && !touch.Saturated {
		t.Errorf("during the move, X's touch read %+v, want Upper >= 3", touch)
	}
	if r := f.CountAt(x, f.horizon, t2); r.Count != 3 {
		t.Errorf("after the move, X reads %+v, want 3 events (relocations %d)", r, f.Relocations())
	}
}
