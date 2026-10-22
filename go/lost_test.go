package temporalfilter

import (
	"testing"
	"time"
)

// TestLostAtNewestEvent: without an overflow table a history the stash
// drops is lost, and unseen keys read Degraded while its events may still be
// in a window: until a horizon after its newest event. A dropper whose clock
// is behind that event (a lagging caller, or Background's placer, which
// places a key at the clock of its last touch) must not end that early.
func TestLostAtNewestEvent(t *testing.T) {
	f, err := New(Config{Capacity: 64, Horizon: time.Second, Resolution: time.Millisecond, Overflow: -1})
	if err != nil {
		t.Fatal(err)
	}
	h := mix64(1)
	fp, tg := fingerprint(h), tag(h)
	w1, _ := f.nextReg(0, tg, 1000, 1000, true)
	f.spill(f.stashID(h, fp, tg), fp<<48|1000, w1, 990) // dropped by a clock 10 ticks behind its last touch
	if f.Lost() != 1 {
		t.Fatalf("Lost() %d, want 1", f.Lost())
	}
	if r := f.CountAt(h, 1000, 1995); !r.Degraded { // its event at 1000 is in this window
		t.Fatalf("at 1995, a horizon after the dropper's clock but not the event: %+v, want Degraded", r)
	}
	if r := f.CountAt(h, 1000, 2001); r.Degraded {
		t.Fatalf("at 2001, past a horizon after the event: %+v, want not Degraded", r)
	}
}
