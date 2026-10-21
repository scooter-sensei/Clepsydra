package temporalfilter

import (
	"testing"
	"time"
)

// Lines of moveTable's 4-line table: X is homed at P and sits displaced in
// A; every line is full.
const mvP, mvA, mvB, mvQ = 0, 1, 2, 3

// moveTable builds a full 4-line table at t1 = 1000: P holds one entry
// from t0 = 1 (expired once t0 + horizon has passed) and three of 5 events;
// X, homed at P, sits displaced in A with 1 event; A, B and Q fill up with
// 5-event entries homed there. key(primary, alt) returns an unused key with
// those lines (alt 4: any) on a claim stripe of its own, so no two keys of
// a test share a lock.
func moveTable(t *testing.T) (f *Filter, x uint64, key func(primary, alt uint64) uint64, touch func(h uint64, n int, now uint64)) {
	t.Helper()
	const t0, t1 = 1, 1000
	f, err := New(Config{Capacity: 13, Horizon: time.Minute, Resolution: time.Millisecond,
		Stash: 64, Overflow: -1, Hot: -1})
	if err != nil {
		t.Fatal(err)
	}
	if f.Slots() != 16 {
		t.Fatalf("want 4 lines, got %d slots", f.Slots())
	}
	stripes := map[uint64]bool{}
	next := uint64(0)
	key = func(primary, alt uint64) uint64 {
		for ; ; next++ {
			h := mix64(next + 1<<52)
			p, s := f.primary(h), h>>(64-claimStripeBits)
			if p == primary && (alt == 4 || f.alt(p, fingerprint(h)) == alt) && !stripes[s] {
				stripes[s] = true
				return h
			}
		}
	}
	touch = func(h uint64, n int, now uint64) {
		for range n {
			f.TouchAt(h, 0, now)
		}
	}
	touch(key(mvP, 4), 1, t0)
	for range 3 {
		touch(key(mvP, 4), 5, t1)
	}
	x = key(mvP, mvA)
	touch(x, 1, t1)
	for _, l := range []uint64{mvA, mvB, mvQ} {
		for countLine(f, l, t1) < 4 {
			touch(key(l, 4), 5, t1)
		}
	}
	for l := range uint64(4) {
		if n := countLine(f, l, t1); n != 4 {
			t.Fatalf("line %d holds %d live entries at t1, want 4", l, n)
		}
	}
	if f.find(mvA, fingerprint(x)<<1|1, tag(x)) < 0 {
		t.Fatal("X is not displaced in A")
	}
	return f, x, key, touch
}

// TestEvictionDuringMoveKeepsHistory drives one interleaving in moveTable.
// A relocation copies X from A to its primary line P; before the move
// hands off, X is touched (the touch finds the copy, which lookups reach
// first), another key's first touch evicts the lightest entry of its two
// lines, and X is touched again. If that evictor took the copy, the second
// touch found the source, the hand-off failed, and X's history was split:
// the copy's events went to the stash, the source's stayed in the table,
// and neither saw the other again. If it took the source, the hand-off
// failed and abandoned the copy, whose events then had no entry to go back
// to. X must end with all its events.
func TestEvictionDuringMoveKeepsHistory(t *testing.T) {
	if forceCT {
		t.Skip("ConstantTime inserts don't search on the call path")
	}
	t.Run("copy", func(t *testing.T) { evictDuringMove(t, false) })
	t.Run("source", func(t *testing.T) { evictDuringMove(t, true) })
}

func evictDuringMove(t *testing.T, source bool) {
	f, x, key, touch := moveTable(t)
	t2 := 1 + f.horizon + 1 // P's oldest entry has expired: room for X's copy
	// k's first touch moves X to P; y's evicts X's copy, the lightest in P and Q.
	k, y := key(mvA, mvB), key(mvP, mvQ)
	if source {
		y = key(mvA, mvQ) // or X's source, the lightest in A and Q
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

// TestEvictionRacingMoveStart: an insert picks X's source as its eviction
// victim before any move of X has begun; a relocation then copies X to its
// primary line and a touch counts in the copy; then the evictor takes the
// source. The hand-off fails and the copy, with the touch, is retired. X
// must end with both its events.
func TestEvictionRacingMoveStart(t *testing.T) {
	if forceCT {
		t.Skip("ConstantTime inserts don't search or evict on the call path")
	}
	f, x, key, touch := moveTable(t)
	tE := 1 + f.horizon     // the evictor still sees P's oldest entry live: its search finds no room
	t2 := 1 + f.horizon + 1 // the relocation sees it expired: room for X's copy
	k, y := key(mvA, mvB), key(mvA, mvQ)
	paused, resume, doneE := make(chan struct{}), make(chan struct{}), make(chan struct{})
	testHookVictim = func() {
		testHookVictim = nil
		close(paused)
		<-resume
	}
	defer func() { testHookVictim, testHookCopied = nil, nil }()
	go func() {
		defer close(doneE)
		touch(y, 1, tE) // chooses X's source, the lightest of A and Q, then waits
	}()
	<-paused
	fired := false
	testHookCopied = func() {
		testHookCopied, fired = nil, true
		touch(x, 1, t2) // counts in the copy
		close(resume)   // the evictor goes on
		<-doneE
	}
	touch(k, 1, t2) // relocates X to P
	if !fired {
		t.Fatal("k's insert did not relocate X")
	}
	if r := f.CountAt(x, f.horizon, t2); r.Count != 2 {
		t.Fatalf("X reads %+v, want 2 events (relocations %d, evictions %d)", r, f.Relocations(), f.Evictions())
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
