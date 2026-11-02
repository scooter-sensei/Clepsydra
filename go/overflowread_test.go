package temporalfilter

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// TestCheckAndTouchMatchesTouchSeen: CheckAndTouch's answer is Touch's Seen.
// Two filters with one Config are fed one seeded sequence, a flood of
// one-off keys (8x the slots per horizon) against 6,000 repeat keys coming
// back at random gaps up to the horizon, with SpillMin 1 so every dropped
// history goes to the overflow table. Filter A calls CheckAndTouchAt, B
// TouchAt; they differ only in reads, so their states stay equal and every
// answer must too. Run with the ghost table and the count-min overflow, each
// in the default mode and in Background, the placer run by hand at seeded
// points.
func TestCheckAndTouchMatchesTouchSeen(t *testing.T) {
	const horizon, span = 60_000, 3 * 60_000 // ticks: the sequence runs three horizons
	key := [16]byte{9, 8, 7, 6, 5, 4, 3, 2, 1}
	wins := []uint64{1_000, 10_000, 60_000}
	for _, cms := range []bool{false, true} {
		for _, bg := range []bool{false, true} {
			now := uint64(1)
			cfg := Config{Capacity: 2048, Horizon: time.Minute, Resolution: time.Millisecond, SpillMin: 1,
				Key: &key, Clock: func() uint64 { return now }, Background: bg}
			var fs [2]*Filter
			for j := range fs {
				f, err := New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				f.bgManual = true // also under TF_BG=1: no placer goroutine, so the twins stay in step
				f.cms = cms
				fs[j] = f
			}
			a, b := fs[0], fs[1]
			rng := rand.New(rand.NewPCG(5, 1))
			type event struct{ at, h uint64 }
			var evs []event
			for i := range 6_000 {
				h := a.HashUint64(uint64(i) + 1<<40)
				for at := rng.Uint64N(horizon); at < span; at += 1 + rng.Uint64N(horizon) {
					evs = append(evs, event{1 + at, h})
				}
			}
			for i := range 8 * a.Slots() * span / horizon {
				evs = append(evs, event{1 + rng.Uint64N(span), a.HashUint64(uint64(i))})
			}
			slices.SortStableFunc(evs, func(x, y event) int { return cmp.Compare(x.at, y.at) })
			seenDegraded := 0
			for i, e := range evs {
				now = e.at
				w := wins[rng.IntN(len(wins))]
				ra := a.CheckAndTouchAt(e.h, w, now)
				rb := b.TouchAt(e.h, w, now)
				if ra != rb.Seen {
					t.Errorf("cms %v, bg %v: call %d (key %x, window %d, at %d): CheckAndTouch %v, Touch %+v",
						cms, bg, i, e.h, w, now, ra, rb)
					break
				}
				if rb.Seen && rb.Degraded {
					seenDegraded++
				}
				if a.bg && rng.IntN(50) == 0 {
					for _, f := range fs {
						for f.room.Load() != nil && f.placeNext(f.room.Load(), now) {
						}
					}
				}
			}
			if a.Evictions() != b.Evictions() || a.Folded() != b.Folded() || a.Lost() != b.Lost() || a.Occupancy(now) != b.Occupancy(now) {
				t.Errorf("cms %v, bg %v: the twins' states differ: evictions %d/%d, folded %d/%d, lost %d/%d, occupancy %d/%d",
					cms, bg, a.Evictions(), b.Evictions(), a.Folded(), b.Folded(), a.Lost(), b.Lost(), a.Occupancy(now), b.Occupancy(now))
			}
			t.Logf("cms %v, bg %v: %d calls, %d Seen with a dropped history behind it, folded %d, evictions %d",
				cms, bg, len(evs), seenDegraded, b.Folded(), b.Evictions())
			if seenDegraded == 0 {
				t.Errorf("cms %v, bg %v: Touch never answered Seen with a dropped history behind it (folded %d): the overflow table never answered",
					cms, bg, b.Folded())
			}
			a.Close()
			b.Close()
		}
	}
}

// TestConstantTimeReadsOverflowOnEveryHit: a ConstantTime filter reads the
// overflow table on every CheckAndTouch, Seen or not, so its timing does not
// depend on the answer. With the table flooded and the overflow table live,
// 1,000 present keys that answer Seen each read it.
func TestConstantTimeReadsOverflowOnEveryHit(t *testing.T) {
	now := uint64(1)
	f, err := New(Config{Capacity: 4096, Horizon: time.Minute, Resolution: time.Millisecond,
		ConstantTime: 200 * time.Nanosecond, Clock: func() uint64 { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 8 * f.Slots()
	for i := range n {
		h := f.HashUint64(uint64(i))
		for range 4 {
			f.TouchAt(h, 60_000, now)
		}
		if i%256 == 0 {
			now++
		}
	}
	if g := f.gh.Load(); !f.sketchLive(now) || g == nil || now > g.until.Load() || f.Folded() == 0 {
		t.Fatalf("overflow table not live at %d (folded %d)", now, f.Folded())
	}
	var present []uint64
	for i := n - 1; i >= 0 && len(present) < 1_000; i-- {
		if h := f.HashUint64(uint64(i)); f.locate(h, fingerprint(h), tag(h)) >= 0 {
			present = append(present, h)
		}
	}
	if len(present) < 1_000 {
		t.Fatalf("only %d flood keys left in the table", len(present))
	}
	reads := 0
	testHookGhostRead = func() { reads++ }
	t.Cleanup(func() { testHookGhostRead = nil })
	for _, h := range present {
		if !f.CheckAndTouchAt(h, 60_000, now) {
			t.Fatalf("present key %x answered not Seen", h)
		}
	}
	testHookGhostRead = nil
	if reads < len(present) {
		t.Fatalf("%d overflow reads for %d Seen hits: ConstantTime skipped the overflow table", reads, len(present))
	}
}
