package temporalfilter

import (
	"testing"
	"time"
)

// TestMergeKeepsOwnStash: a key evicted to f's stash and live in g's table;
// after f.Merge(g), f must still count the stashed history (Upper at least
// the truth), and still after the key's next eviction. D24's Merge failed it
// (it installed the key from g without recalling f's stash; found by the
// StashSet model, verify/tla/StashSet.tla; claims D26).
func TestMergeKeepsOwnStash(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("Merge refuses ConstantTime and Background filters (TestMergeRefuses)")
	}
	cfg := Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond}
	f, _ := New(cfg)
	g, _ := New(cfg)
	k := mix64(42)
	for i := range 5 {
		f.TouchAt(k, 0, uint64(100+i))
	}
	for i := range 3 {
		g.TouchAt(k, 0, uint64(110+i))
	}
	evict := func(now uint64) { // as claimAt's eviction does
		i, w0, w1 := f.lookupStable(k, fingerprint(k), tag(k), false)
		if i < 0 {
			t.Fatal("not found")
		}
		f.words[i+1], f.words[i] = 0, 0
		f.stashPut(f.slotID(uint64(i/wordsPerLine), w0, w1), w0, w1, now)
	}
	evict(120)
	if err := f.Merge(g); err != nil {
		t.Fatal(err)
	}
	if r := f.CountAt(k, 60_000, 130); r.Upper < 8 && !r.Saturated {
		t.Errorf("after Merge: %+v, truth 8 (5 in f's stash, 3 in g)", r)
	}
	f.TouchAt(k, 0, 131)
	evict(132)
	if r := f.CountAt(k, 60_000, 133); r.Upper < 9 && !r.Saturated {
		t.Errorf("after the next eviction: %+v, truth 9", r)
	}
}
