package temporalfilter

import (
	"testing"
	"time"
)

// TestReadWhileTouchTakesWaitingKey: with Background, a touch of a key
// waiting in the wait room takes it out under the key's stripe and then
// installs it (or puts it back): meanwhile the key is in neither place. If
// the placer finds the key's set empty then, it clears the set's wait bit,
// and a lock-free read that misses the table takes the clear bit for "not
// waiting". The read must find the key's history, not answer absent (it
// did: Upper 0 for a key with three events).
func TestReadWhileTouchTakesWaitingKey(t *testing.T) {
	if forceCT {
		t.Skip("Background has no effect with ConstantTime")
	}
	f, err := New(Config{Capacity: 4096, Horizon: time.Minute, Resolution: time.Millisecond,
		Clock: func() uint64 { return 1 }, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	f.bgManual = true
	var key uint64
	for i := uint64(1); key == 0; i++ { // a key whose lines are full waits
		w := f.Waited()
		f.TouchAt(mix64(i), 0, 1)
		if f.Waited() != w {
			key = mix64(i)
		}
	}
	f.TouchAt(key, 0, 1)
	f.TouchAt(key, 0, 1) // three events, still waiting
	if f.Waiting() != 1 || f.locate(key, fingerprint(key), tag(key)) >= 0 {
		t.Fatalf("%d waiting; want the key alone, not in the table", f.Waiting())
	}
	got := make(chan Result, 1)
	fired := false
	testHookTaken = func() {
		if fired {
			return
		}
		fired = true
		f.placeNext(f.room.Load(), 0) // the placer finds the set empty and clears its bit
		go func() { got <- f.CountAt(key, 60_000, 1) }()
		select {
		case r := <-got: // the read finished while the key was in neither place
			got <- r
		case <-time.After(20 * time.Millisecond): // it waits for the touch: let it finish
		}
	}
	defer func() { testHookTaken = nil }()
	f.TouchAt(key, 0, 1)
	if !fired {
		t.Fatal("the touch did not take the key from the wait room")
	}
	if r := <-got; r.Upper < 3 && !r.Saturated {
		t.Fatalf("read during the touch: %+v, want Upper >= 3", r)
	}
	if r := f.CountAt(key, 60_000, 1); r.Count != 4 {
		t.Fatalf("after: %+v, want 4 events", r)
	}
}
