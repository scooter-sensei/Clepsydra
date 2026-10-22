package temporalfilter

import (
	"testing"
	"time"
)

// TestTouchesRacingAfterGap: K's last touch is g units back when touch X
// moves its last touch to the current unit; before X's register follows, a
// second touch Y of K in the same unit runs. A register's 4-bit epoch is
// resolved against the slot's last touch, so for g >= 8 Y read the old
// register as the new unit's and X then aged Y's event with it by g units
// (past the horizon, X's reset dropped it): the last two units read
// Upper 1 for two events. Y must count in the current unit.
func TestTouchesRacingAfterGap(t *testing.T) {
	if forceCT {
		t.Skip("ConstantTime touches of a key run one at a time under its stripe")
	}
	for g := uint64(1); g <= 30; g++ {
		f, err := New(Config{Capacity: 1000, Horizon: 48 * time.Millisecond, Resolution: time.Millisecond,
			Clock: func() uint64 { return 1 }, Hot: -1})
		if err != nil {
			t.Fatal(err)
		}
		k, u, t0 := mix64(99), f.unit, uint64(10_000)
		f.TouchAt(k, 0, t0)
		nowX := t0 + g*u
		done := make(chan struct{})
		fired := false
		testHookStamped = func() {
			if fired {
				return
			}
			fired = true
			go func() {
				f.TouchAt(k, 0, nowX+1) // Y
				close(done)
			}()
			select { // Y runs whole, or waits for X
			case <-done:
			case <-time.After(10 * time.Millisecond):
			}
		}
		f.TouchAt(k, 0, nowX) // X
		testHookStamped = nil
		<-done
		if !fired {
			t.Fatalf("gap %d: X's touch never moved the last touch on", g)
		}
		if r := f.CountAt(k, 2*u, nowX+1); r.Upper < 2 && !r.Saturated {
			t.Fatalf("gap %d units: the last two units read %+v, want Upper >= 2 (X and Y)", g, r)
		}
		want := 2
		if nowX+1-t0 <= f.horizon {
			want = 3
		}
		if r := f.CountAt(k, f.horizon, nowX+1); r.Upper < want && !r.Saturated {
			t.Fatalf("gap %d units: the horizon reads %+v, want Upper >= %d", g, r, want)
		}
	}
}
