package temporalfilter

import (
	"sync/atomic"
	"testing"
	"time"
)

// sweepSetup: a filter holding one key K, touched once at t0, whose touches
// don't sweep inline; i is K's slot (word index).
func sweepSetup(t *testing.T) (f *Filter, k uint64, i int) {
	t.Helper()
	f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond,
		Clock: func() uint64 { return 1 }})
	if err != nil {
		t.Fatal(err)
	}
	for j := uint64(1); k == 0; j++ {
		if h := mix64(j); h&decayEvery != 0 {
			k = h
		}
	}
	f.TouchAt(k, 0, sweepT0)
	if i = f.locate(k, fingerprint(k), tag(k)); i < 0 {
		t.Fatal("K is not in the table")
	}
	return f, k, i
}

const sweepT0 = 60_000 // a unit boundary (units of 2,500 ticks)

// TestSweepRacingReinsertKeepsKey: the decay sweep clears K's expired slot.
// Between its first and last write to the slot, K's next touch, 32 units
// on, puts K back in the slot with a fresh register bit-identical to the old
// one (one event, K's tag, a unit with the same 4 epoch bits). The sweep
// must leave the new register alone. Before (claims A26), the sweep cleared
// word0, then CASed word1 from the old register to 0, which took the new
// one: K's fingerprint was left with an empty register, a slot no lookup
// matches, and K's next touch went into a second slot (Upper 1 for 2
// events).
func TestSweepRacingReinsertKeepsKey(t *testing.T) {
	f, k, i := sweepSetup(t)
	t1 := sweepT0 + 32*f.unit // past the horizon (24 units); 32 units: the same epoch bits
	fired := false
	done := make(chan struct{})
	testHookSweeping = func() {
		if fired {
			return
		}
		fired = true
		go func() {
			f.TouchAt(k, 0, t1)
			close(done)
		}()
		select { // the touch runs whole, or waits for the sweep
		case <-done:
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer func() { testHookSweeping = nil }()
	f.sweepLine(uint64(i/wordsPerLine), t1)
	if !fired {
		t.Fatal("the sweep did not take K's slot")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("K's touch did not return")
	}
	f.TouchAt(k, 0, t1)
	if r := f.CountAt(k, f.horizon, t1); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K reads %+v after two touches at t1, want Upper >= 2", r)
	}
}

// TestSweepRacingTouchPutsSlotBack: the decay sweep, at a clock past K's
// horizon, takes K's slot; a touch of K at a clock a horizon behind, which
// found the slot before, moves its last touch (word0) on first, as a touch
// in the same unit does. The sweep's clear of word0 then fails, and it must
// put K's register back: K's touches at that clock find their history.
func TestSweepRacingTouchPutsSlotBack(t *testing.T) {
	f, k, i := sweepSetup(t)
	tS := sweepT0 + f.horizon + 2*f.unit // the sweep's clock: K expired
	tL := uint64(sweepT0 + 1)            // the touch's: K's unit
	w0, w1 := f.words[i], f.words[i+1]
	moved := false
	testHookSweeping = func() {
		if !moved {
			moved = atomic.CompareAndSwapUint64(&f.words[i], w0, w0&^tsMask|tL)
		}
	}
	defer func() { testHookSweeping = nil }()
	f.sweepLine(uint64(i/wordsPerLine), tS)
	if !moved {
		t.Fatal("the touch did not move K's last touch while the sweep held the slot")
	}
	if got := atomic.LoadUint64(&f.words[i+1]); got != w1 {
		t.Errorf("K's word1 is %#x after the sweep, want its register %#x back", got, w1)
	}
	f.TouchAt(k, 0, tL) // the touch's count, after the sweep
	if r := f.CountAt(k, f.horizon, tL); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K reads %+v after two touches, want Upper >= 2", r)
	}
}

// TestSweepHoldLaggingTouchFindsSlot: as TestSweepRacingTouchPutsSlotBack,
// but the lagging touch, 5 units after K's last, is live at the sweep's
// clock, and it counts while the sweep holds the slot: having moved word0,
// its count of the register finds word1 busy, and it looks K up again. That
// lookup must wait out the hold (it counts as a move of K's stripe), then
// find K's slot put back. Without, it missed the held slot and put K, with
// the touch's event, in a second slot, which K's touches and reads, finding
// the first, then passed over.
func TestSweepHoldLaggingTouchFindsSlot(t *testing.T) {
	f, k, i := sweepSetup(t)
	tS := sweepT0 + f.horizon + f.unit // the sweep's clock: K expired
	tL := sweepT0 + 5*f.unit           // the touch's: live at tS
	w0 := f.words[i]
	moved := false
	done := make(chan struct{})
	testHookSweeping = func() {
		if moved {
			return
		}
		if moved = atomic.CompareAndSwapUint64(&f.words[i], w0, w0&^tsMask|tL); !moved {
			return
		}
		go func() { // the touch's next look, its first count having failed
			f.TouchAt(k, 0, tL)
			close(done)
		}()
		select { // the touch runs whole, or waits for the sweep
		case <-done:
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer func() { testHookSweeping = nil }()
	f.sweepLine(uint64(i/wordsPerLine), tS)
	if !moved {
		t.Fatal("the touch did not move K's last touch while the sweep held the slot")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("K's touch did not return")
	}
	if n := f.Occupancy(tL); n != 1 {
		t.Fatalf("K holds %d slots, want 1", n)
	}
	f.TouchAt(k, 0, tS)
	if r := f.CountAt(k, f.horizon, tS); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K reads %+v after touches at %d and %d, want Upper >= 2", r, tL, tS)
	}
}
