package temporalfilter

import (
	"testing"
	"time"
)

// TestSmoke calls every public operation once on a default filter and
// checks the obvious answers. It is the smoke tier (go/test.sh smoke): a
// build that fails it is not worth testing further.
func TestSmoke(t *testing.T) {
	for _, cfg := range []Config{
		{Capacity: 1000, Horizon: time.Minute},
		{Capacity: 1000, Horizon: time.Minute, CoarseClock: time.Millisecond},
		{Capacity: 1000, Horizon: time.Minute, Key: vecKey, ConstantTime: time.Microsecond},
	} {
		f, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		w := time.Minute
		if f.CheckAndTouch("a", w) {
			t.Error("first CheckAndTouch reported seen")
		}
		if !f.CheckAndTouch("a", w) {
			t.Error("second CheckAndTouch reported unseen")
		}
		if seen, n := f.TouchAndCount("a", w); !seen || n != 3 {
			t.Errorf("TouchAndCount = %v, %d; want true, 3", seen, n)
		}
		if n := f.VelocityCount("a", w); n != 3 {
			t.Errorf("VelocityCount = %d; want 3", n)
		}
		if r := f.Touch("a", w); !r.Seen || r.Count != 4 || r.Upper < 4 || r.Saturated || r.Degraded {
			t.Errorf("Touch = %+v", r)
		}
		if r := f.Count("b", w); r.Seen || r.Count != 0 {
			t.Errorf("Count of an unseen key = %+v", r)
		}
		now := f.Now()
		hs := []uint64{f.HashUint64(1), f.HashUint64(2), f.HashUint64(1)}
		seen := make([]bool, len(hs))
		counts := make([]int, len(hs))
		f.CheckAndTouchBatch(hs, f.Ticks(w), now, seen)
		f.TouchAndCountBatch(hs, f.Ticks(w), now, seen, counts)
		if !seen[0] || counts[0] != 3 || counts[1] != 2 {
			t.Errorf("batches: seen %v counts %v", seen, counts)
		}
		f.VelocityCountBatch(hs, f.Ticks(w), now, counts)
		if counts[0] != 4 || counts[1] != 2 { // key 1 four times, key 2 twice
			t.Errorf("VelocityCountBatch = %v", counts)
		}
		if f.ContainsAt(f.HashUint64(3), f.Ticks(w), now) {
			t.Error("ContainsAt of an unseen key")
		}
		f.ContinuousDecay()
		if occ := f.Occupancy(f.Now()); occ < 3 || occ > f.Slots() {
			t.Errorf("Occupancy %d of %d slots", occ, f.Slots())
		}
		f.Close()
	}
	if _, err := New(Config{}); err == nil {
		t.Error("New accepted a zero Config")
	}
}
