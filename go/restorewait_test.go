package temporalfilter

import (
	"strings"
	"testing"
	"time"
)

// TestEvictedKeyThatWaitsIsRestored: in Background and ConstantTime a first
// touch that finds both lines full waits for a slot. An evicted key's touch
// that does so still brought its history back from the stash: that answer
// is Restored and Restored() counts it, once; the touch that later installs
// the waiting key isn't (it recalls a waiting entry). Before, the waiting
// touch cleared both, so such a key was never reported back.
func TestEvictedKeyThatWaitsIsRestored(t *testing.T) {
	for _, mode := range []string{"Background", "ConstantTime"} {
		t.Run(mode, func(t *testing.T) {
			// A 64-entry stash: one waiting key doesn't force placement.
			f, err := New(Config{Capacity: 4, Horizon: 10 * time.Second, Resolution: time.Millisecond,
				Clock: func() uint64 { return 1 }, Stash: 64})
			if err != nil {
				t.Fatal(err)
			}
			// Evict on the call path first (TF_CT or TF_BG would make inserts wait).
			f.ct, f.bg = false, false
			// Two lines, eight slots: touch keys at 1000 until one is evicted.
			var k uint64
			for ; k < 64 && f.Evictions() == 0; k++ {
				f.TouchAt(mix64(k), 0, 1000)
			}
			var v uint64
			for i := range k {
				if w := f.whereIs(mix64(i), 1000); strings.Contains(w, "stash") && !strings.Contains(w, "slot") {
					v = mix64(i)
				}
			}
			if v == 0 {
				t.Fatalf("no evicted key in the stash after %d touches (evictions %d)", k, f.Evictions())
			}
			if mode == "Background" {
				f.bg, f.bgManual = true, true
			} else {
				f.ct = true
			}
			// Both of v's lines are full and live: v waits, its history back.
			r := f.TouchAt(v, f.horizon, 1000)
			if f.Waited() != 1 {
				t.Fatalf("v didn't wait for a slot (waited %d; %s)", f.Waited(), f.whereIs(v, 1000))
			}
			if !r.Restored || r.Count != 2 || !r.Seen || f.Restored() != 1 {
				t.Fatalf("waiting touch of an evicted key: %+v, Restored() %d; want count 2, seen, restored, Restored() 1", r, f.Restored())
			}
			// Free a slot of v's primary line: v's next touch installs it.
			base := int(f.primary(v) * wordsPerLine)
			f.words[base], f.words[base+1] = 0, 0
			r = f.TouchAt(v, f.horizon, 1000)
			if f.locate(v, fingerprint(v), tag(v)) < 0 {
				t.Fatalf("v not installed: %s", f.whereIs(v, 1000))
			}
			if r.Restored || r.Count != 3 || f.Restored() != 1 {
				t.Fatalf("touch that installs the waiting key: %+v, Restored() %d; want count 3, not restored, Restored() 1", r, f.Restored())
			}
		})
	}
}
