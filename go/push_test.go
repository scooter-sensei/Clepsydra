package temporalfilter

// lab/sw-2026-11, arm W13: each touch's stores of its push into the drip
// log, counted (push.go; tfpush with tfdrip), and read by the concurrent
// fuzzer, W7's scenario and W11b's legs and race (SW-RESULTS.md).

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func (c *pushCount) add(o pushCount) {
	for i := range c.N {
		c.N[i] += o.N[i]
	}
	if c.First == "" {
		c.First = o.First
	}
}

// String lists the counts by name.
func (c pushCount) String() string {
	s := make([]string, len(c.N))
	for i, n := range c.N {
		s[i] = fmt.Sprintf("%s %d", pushNames[i], n)
	}
	return strings.Join(s, ", ")
}

func TestPushNames(t *testing.T) {
	for i, n := range pushNames {
		if n == "" {
			t.Errorf("push count %d has no name", i)
		}
	}
}

// TestPushPaths (tfpush) makes the notes W11b's legs don't, one touch each
// on one goroutine, no Background: realign (a key into its next unit), stamp
// (a promoted key into its next unit), and a silent push (a 13th touch at
// one tick: 11 zero codes have filled the log's field, and next0 leaves the
// word as it is).
func TestPushPaths(t *testing.T) {
	if !pushOn {
		t.Skip("the push count (tfpush with tfdrip)")
	}
	if forceCT || forceBG {
		t.Skip("plain mode")
	}
	f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: 1,
		Clock: func() uint64 { return 1 }})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []string
	testHookPushDone = func(h, now uint64, class, tally string) { got = append(got, class+": "+tally) }
	defer func() { testHookPushDone = nil }()
	const t0 = 10_000 // unit 4 at 2,500 ticks a unit
	touch := func(h, now uint64) string {
		got = got[:0]
		f.TouchAt(h, 0, now)
		if len(got) != 1 {
			t.Fatalf("%d push notes for one touch of %#x at %d", len(got), h, now)
		}
		return got[0]
	}
	if f.unit != 2_500 {
		t.Fatalf("unit %d, want 2,500", f.unit)
	}
	a, b, c := mix64(101), mix64(102), mix64(103)
	type path struct{ name, want, got string }
	var paths []path
	touch(a, t0)
	paths = append(paths, path{"realign, a key into its next unit", "once: stores 1 [realign into last 10000, flag false]", touch(a, t0+f.unit)})
	for range 32 {
		touch(b, t0)
	}
	if f.Promoted() != 1 {
		t.Fatalf("path not taken: B not promoted (%d)", f.Promoted())
	}
	paths = append(paths, path{"stamp, a promoted key into its next unit", "once: stores 1 [stamp into last 10000, flag false]", touch(b, t0+f.unit)})
	for range 12 {
		touch(c, t0)
	}
	paths = append(paths, path{"silent, a 13th touch at one tick", "none: stores 0 [], skips table 0 miss 0, silent 1,", touch(c, t0)})
	for _, p := range paths {
		t.Logf("W13 path %s | %s", p.name, p.got)
		if !strings.HasPrefix(p.got, p.want) {
			t.Errorf("%s: %s, want %s...", p.name, p.got, p.want)
		}
	}
	// The filter's counts of these 48 touches, the counters' sight: A's two
	// (a claim afresh, the realign); B's 32 at t0 (a claim afresh, 11 table
	// stores, 20 silent) and its stamp; C's 13 (a claim afresh, 11 table
	// stores, one silent).
	want := pushCount{}
	for k, v := range map[int]int64{pcCalls: 48, pcOnce: 27, pcNone: 21, pcSilent: 21, pcFirstFresh: 3,
		pcStores + pushClaim: 3, pcStores + pushTable: 22, pcStores + pushRealign: 1, pcStores + pushStamp: 1} {
		want.N[k] = v
	}
	pc := f.pushCounts()
	t.Logf("W13 path counts | %v", pc)
	if pc.N != want.N {
		t.Errorf("the filter's push counts: %v, want %v", pc, want)
	}
}
