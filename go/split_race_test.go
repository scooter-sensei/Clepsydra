package temporalfilter

import (
	"testing"
	"time"
)

// TestEvictionDuringMoveKeepsHistory drives one interleaving in a full
// 4-line table. A relocation copies key X from its alternate line A to its
// primary line P; before the move hands off, X is touched (the touch finds
// the copy, which lookups reach first), another key's first touch evicts
// the lightest entry of its two lines, and X is touched again. If that
// evictor took the copy, the second touch found the source, the hand-off
// failed, and X's history was split: the copy's events went to the stash,
// the source's stayed in the table, and neither saw the other again. If it
// took the source, the hand-off failed and abandoned the copy, whose events
// then had no entry to go back to. X must end with all its events.
func TestEvictionDuringMoveKeepsHistory(t *testing.T) {
	t.Run("copy", func(t *testing.T) { evictDuringMove(t, false) })
	t.Run("source", func(t *testing.T) { evictDuringMove(t, true) })
}

func evictDuringMove(t *testing.T, source bool) {
	const t0, t1 = 1, 1000
	f, err := New(Config{Capacity: 13, Horizon: time.Minute, Resolution: time.Millisecond,
		Stash: 64, Overflow: -1, Hot: -1})
	if err != nil {
		t.Fatal(err)
	}
	if f.Slots() != 16 {
		t.Fatalf("want 4 lines, got %d slots", f.Slots())
	}
	t2 := t0 + f.horizon + 1 // t0's events have expired, t1's are live
	const P, A, B, Q = 0, 1, 2, 3
	stripes := map[uint64]bool{} // one key per claim stripe: no lock is shared
	next := uint64(0)
	key := func(primary, alt uint64) uint64 { // alt 4: any
		for ; ; next++ {
			h := mix64(next + 1<<52)
			p, s := f.primary(h), h>>(64-claimStripeBits)
			if p == primary && (alt == 4 || f.alt(p, fingerprint(h)) == alt) && !stripes[s] {
				stripes[s] = true
				return h
			}
		}
	}
	touch := func(h uint64, n int, now uint64) {
		for range n {
			f.TouchAt(h, 0, now)
		}
	}
	// P: one entry that expires by t2 and three heavy ones. X, homed at P,
	// then goes to A. A, B and Q fill up with heavy entries homed there.
	touch(key(P, 4), 1, t0)
	for range 3 {
		touch(key(P, 4), 5, t1)
	}
	x := key(P, A)
	touch(x, 1, t1)
	for _, l := range []uint64{A, B, Q} {
		for countLine(f, l, t1) < 4 {
			touch(key(l, 4), 5, t1)
		}
	}
	for l := range uint64(4) {
		if n := countLine(f, l, t1); n != 4 {
			t.Fatalf("line %d holds %d live entries at t1, want 4", l, n)
		}
	}
	if f.find(A, fingerprint(x)<<1|1, tag(x)) < 0 {
		t.Fatal("X is not displaced in A")
	}
	k, y := key(A, B), key(P, Q) // k's first touch moves X to P; y's evicts X's copy, the lightest in P and Q
	if source {
		y = key(A, Q) // or X's source, the lightest in A and Q
	}
	fired := false
	testHookCopied = func() {
		if fired {
			return
		}
		fired = true
		touch(x, 1, t2) // counts in the copy
		touch(y, 1, t2) // evicts X's copy or source, unless its move is left alone
		touch(x, 1, t2) // the copy if it is still there, else the source
	}
	defer func() { testHookCopied = nil }()
	touch(k, 1, t2)
	if !fired {
		t.Fatal("k's insert did not relocate X")
	}
	if r := f.CountAt(x, f.horizon, t2); r.Count != 3 {
		t.Fatalf("X reads %+v, want 3 events (relocations %d, evictions %d)", r, f.Relocations(), f.Evictions())
	}
}

// countLine is the number of entries in line l live at now.
func countLine(f *Filter, l, now uint64) int {
	n := 0
	for s := 0; s < wordsPerLine; s += 2 {
		if w0 := f.words[int(l*wordsPerLine)+s]; w0 != 0 && age(now, w0&tsMask) <= f.horizon {
			n++
		}
	}
	return n
}
