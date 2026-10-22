package temporalfilter

import (
	"math/bits"
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

// TestWaitBesidePlacementKeepsKey: with Background, the placer commits a
// waiting key L by marking its wait-room entry busy, installing it, and then
// removing the entry, or putting its word1 back if the install failed. A
// first touch of K meanwhile, whose lines are full and whose room set is
// L's, waits in that set, where L's entry, last touched a horizon before
// K's clock, looks expired. If K's wait takes L's way, busy as it is, the
// placer's removal erases K's entry: K reads Upper 0 for its event, and its
// next touch starts it afresh (the concurrent fuzzer's case 1068). K must
// keep its event.
func TestWaitBesidePlacementKeepsKey(t *testing.T) {
	if forceCT {
		t.Skip("Background has no effect with ConstantTime")
	}
	f, err := New(Config{Capacity: 4096, Horizon: time.Minute, Resolution: time.Millisecond,
		Clock: func() uint64 { return 1 }, Background: true, Hot: -1})
	if err != nil {
		t.Fatal(err)
	}
	f.bgManual = true
	const t0 = 1
	t1 := t0 + f.horizon + 1 // entries last touched at t0 have expired
	var l uint64
	for i := uint64(1); l == 0; i++ { // a key whose lines are full waits
		w := f.Waited()
		f.TouchAt(mix64(i), 0, t0)
		if f.Waited() != w {
			l = mix64(i)
		}
	}
	p := f.room.Load()
	set := func(h uint64) uint64 {
		s, _ := bits.Mul64(mix64(f.stashID(h, fingerprint(h), tag(h))), p.n)
		return s
	}
	lines := func(h uint64) [2]uint64 {
		pl := f.primary(h)
		return [2]uint64{pl, f.alt(pl, fingerprint(h))}
	}
	ll := lines(l)
	var k uint64 // L's room set; two lines, neither of them L's; a claim stripe of its own
	for i := uint64(1 << 40); k == 0; i++ {
		h, kl := mix64(i), lines(mix64(i))
		if set(h) == set(l) && h>>56 != l>>56 && kl[0] != kl[1] &&
			kl[0] != ll[0] && kl[0] != ll[1] && kl[1] != ll[0] && kl[1] != ll[1] {
			k = h
		}
	}
	kl := lines(k)
	for _, line := range kl { // K's lines full of entries live at t1, so its first touch waits
		for i := uint64(1 << 44); countLine(f, line, t1) < 4; i++ {
			if h := mix64(i); f.primary(h) == line {
				f.TouchAt(h, 0, t1)
			}
		}
	}
	for _, line := range ll { // room for L: its lines' entries from t0 are swept
		f.sweepLine(line, t1)
	}
	if f.Waiting() != 1 {
		t.Fatalf("%d keys waiting, want L alone", f.Waiting())
	}
	fired := false
	testHookPlacing = func() {
		if fired {
			return
		}
		fired = true
		w := f.Waited()
		f.TouchAt(k, 0, t1) // K waits in L's set while L's entry is busy
		if f.Waited() == w {
			t.Error("K's first touch did not wait")
		}
	}
	defer func() { testHookPlacing = nil }()
	f.placeNext(p, 0) // places L
	if !fired {
		t.Fatal("the placer did not commit L")
	}
	if f.locate(l, fingerprint(l), tag(l)) < 0 {
		t.Fatal("L is not in the table")
	}
	if r := f.CountAt(k, f.horizon, t1); r.Upper < 1 && !r.Saturated {
		t.Fatalf("K reads %+v after L's placement, want Upper >= 1", r)
	}
	if r := f.TouchAt(k, f.horizon, t1); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K's second touch reads %+v, want Upper >= 2", r)
	}
}
