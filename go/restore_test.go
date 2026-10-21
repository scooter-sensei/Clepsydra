package temporalfilter

import (
	"strings"
	"testing"
	"time"
)

// TestRestoreBehindLastTouch: a key whose history comes back from the stash
// in a call with a clock behind the history's last touch (callers of the *At
// API pass their own clocks, and a concurrent caller's can be a few ticks
// old) keeps that last touch. Before the fix the key's timestamp went back to
// the caller's clock, so a later Count whose window began between the two
// read nothing: Upper 0 for a key touched inside the window, unflagged.
// Found by TestFuzzUpper. Two paths: an evicted key restored into the table,
// and, in ConstantTime, a key waiting in the stash touched again there.
func TestRestoreBehindLastTouch(t *testing.T) {
	for _, ct := range []bool{false, true} {
		name := "evicted"
		if ct {
			name = "ConstantTime waiting"
		}
		t.Run(name, func(t *testing.T) {
			if forceCT && !ct {
				t.Skip("ConstantTime (TF_CT=1) evicts no key here: inserts wait")
			}
			// A 64-entry stash: one waiting key doesn't force placement
			// (more than one per 16 entries does), so it stays waiting.
			f, err := New(Config{Capacity: 4, Horizon: 10 * time.Second, Resolution: time.Millisecond,
				Clock: func() uint64 { return 1 }, Stash: 64})
			if err != nil {
				t.Fatal(err)
			}
			f.ct = ct
			// Two lines, eight slots: touch keys at 1000 until one is in the
			// stash (evicted, or waiting for a slot in ConstantTime).
			var k uint64
			for ; k < 64; k++ {
				f.TouchAt(mix64(k), 0, 1000)
				if f.Evictions()+f.Waited() > 0 {
					break
				}
			}
			var v uint64
			for i := range k + 1 {
				if w := f.whereIs(mix64(i), 1000); strings.Contains(w, "stash") && !strings.Contains(w, "slot") {
					v = mix64(i)
				}
			}
			if v == 0 {
				t.Fatalf("no key in the stash after %d touches (evictions %d, waited %d)", k+1, f.Evictions(), f.Waited())
			}
			f.TouchAt(v, 0, 990) // a caller whose clock is 10 ticks behind v's last touch
			// v's events: 1000 and 990. In [995, 1010] one of them.
			r := f.CountAt(v, 15, 1010)
			if r.Upper < 1 || r.Saturated {
				t.Fatalf("Count after a touch behind the last one: %+v, want Upper >= 1 (events at 990 and 1000; %s)",
					r, f.whereIs(v, 1010))
			}
			if r := f.CountAt(v, 30, 1010); r.Upper < 2 {
				t.Fatalf("Count over both events: %+v, want Upper >= 2", r)
			}
		})
	}
}
