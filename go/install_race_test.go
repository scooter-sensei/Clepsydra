package temporalfilter

import (
	"sync/atomic"
	"testing"
	"time"
)

// keySlots counts the slots of k's two lines holding k (its fingerprint
// and tag).
func keySlots(f *Filter, k uint64) int {
	fp, tg := fingerprint(k), tag(k)
	p := f.primary(k)
	lines := []uint64{p}
	if a := f.alt(p, fp); a != p {
		lines = append(lines, a)
	}
	n := 0
	for _, l := range lines {
		for s := 0; s < wordsPerLine; s += 2 {
			j := int(l)*wordsPerLine + s
			if atomic.LoadUint64(&f.words[j])>>48 == fp && atomic.LoadUint64(&f.words[j+1])>>tagShift == tg {
				n++
			}
		}
	}
	return n
}

// laggingTouchDuringHold returns a hook for testHookInstalling: when an
// install holds slot i (word1 busy), a touch of k at clock tL, which found
// the slot before, moves its word0 on (the install then loses the slot and
// puts k's register back); having found word1 busy when it counted, the
// touch looks k up again, in a goroutine that closes done. moved reports
// whether the touch moved word0.
func laggingTouchDuringHold(f *Filter, k uint64, i int, tL uint64, done chan struct{}) (hook func(int), moved *bool) {
	w0 := f.words[i]
	moved = new(bool)
	return func(j int) {
		if *moved || j != i {
			return
		}
		if *moved = atomic.CompareAndSwapUint64(&f.words[i], w0, w0&^tsMask|tL); !*moved {
			return
		}
		go func() {
			f.TouchAt(k, 0, tL)
			close(done)
		}()
		select { // the touch runs whole, or waits for the hold
		case <-done:
		case <-time.After(20 * time.Millisecond):
		}
	}, moved
}

// TestInsertOverExpiredHoldLaggingTouch: a first touch of L, at a clock a
// horizon past K's last touch, takes K's expired slot (install: word1 busy,
// then word0). Meanwhile a touch of K at a clock 5 units after K's last,
// live at L's, which found the slot before, moves word0 on: L's install
// loses the slot and puts K's register back. That touch's next look at K,
// during the hold, must not put K in a second slot (claims A27).
func TestInsertOverExpiredHoldLaggingTouch(t *testing.T) {
	f, k, i := sweepSetup(t)
	tS := sweepT0 + f.horizon + f.unit // L's clock: K expired
	tL := sweepT0 + 5*f.unit           // K's touch's: live at tS
	line := uint64(i / wordsPerLine)
	var l uint64 // L: K's line its primary, its own claim stripe, no inline sweep
	for j := uint64(1 << 40); l == 0; j++ {
		if h := mix64(j); f.primary(h) == line && h&decayEvery != 0 && h>>(64-claimStripeBits) != k>>(64-claimStripeBits) && fingerprint(h) != fingerprint(k) {
			l = h
		}
	}
	done := make(chan struct{})
	hook, moved := laggingTouchDuringHold(f, k, i, tL, done)
	testHookInstalling = hook
	defer func() { testHookInstalling = nil }()
	f.TouchAt(l, 0, tS)
	if !*moved {
		t.Fatal("L's insert did not take K's slot")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("K's touch did not return")
	}
	if n := keySlots(f, k); n != 1 {
		t.Fatalf("K holds %d slots, want 1", n)
	}
	f.TouchAt(k, 0, tS)
	if r := f.CountAt(k, f.horizon, tS); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K reads %+v after touches at %d and %d, want Upper >= 2", r, tL, tS)
	}
}

// TestMoveOverExpiredHoldLaggingTouch: as TestInsertOverExpiredHoldLaggingTouch,
// with the hold a relocation's. A 4-line table: k's lines A and B are full
// and live, and their entries' other lines are full, except X's: X, homed
// at P, sits displaced in A, and P holds an entry E expired at t2. k's
// first touch at t2 moves X to P, into E's slot, while a lagging touch of
// E moves E's word0 on. E's other line, Q, has room: the touch's next look
// at E, during the hold, must not put E there.
func TestMoveOverExpiredHoldLaggingTouch(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("ConstantTime and Background inserts don't search or evict on the call path")
	}
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
	key := func(primary, alt uint64) uint64 { // an unused key with those lines, on a stripe of its own
		for ; ; next++ {
			h := mix64(next + 1<<52)
			p, s := f.primary(h), h>>(64-claimStripeBits)
			if p == primary && f.alt(p, fingerprint(h)) == alt && h&decayEvery != 0 && !stripes[s] {
				stripes[s] = true
				return h
			}
		}
	}
	const t0, t1 = 1, 1000
	t2 := t0 + f.horizon + 1 // E expired, the rest (from t1) live
	tL := t0 + 5*f.unit      // E's touch's clock: live at t2
	e := key(mvP, mvQ)
	f.TouchAt(e, 0, t0)
	for range 3 {
		f.TouchAt(key(mvP, mvB), 0, t1)
	}
	x := key(mvP, mvA)
	f.TouchAt(x, 0, t1) // P is full: X goes to A, displaced
	for _, la := range [][2]uint64{{mvA, mvB}, {mvB, mvA}} {
		for countLine(f, la[0], t1) < 4 {
			f.TouchAt(key(la[0], la[1]), 0, t1)
		}
	}
	if f.find(mvA, fingerprint(x)<<1|1, tag(x)) < 0 || countLine(f, mvP, t1) != 4 || countLine(f, mvQ, tL) != 0 {
		t.Fatal("the table is not as the test needs")
	}
	done := make(chan struct{})
	hook, moved := laggingTouchDuringHold(f, e, f.locate(e, fingerprint(e), tag(e)), tL, done)
	testHookInstalling = hook
	defer func() { testHookInstalling = nil }()
	f.TouchAt(key(mvA, mvB), 0, t2) // moves X to P, into E's slot
	if !*moved {
		t.Fatal("no relocation into E's slot")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("E's touch did not return")
	}
	if n := keySlots(f, e); n != 1 {
		t.Fatalf("E holds %d slots, want 1", n)
	}
}
