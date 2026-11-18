//go:build tfdrip

package temporalfilter

// lab/sw-2026-11, W10's sight (SW-RESULTS.md): do countcheck_test.go's
// checks see a double push? lab/w10.sh copies this file and the tree's
// countcheck_test.go into a detached worktree of the tree under test; it is
// never part of the tree.

import (
	"testing"
	"time"
)

// TestW10Sight: W9's restore of K (promoted at t0, its words in the stash,
// its slot freed; L promoted at tL reclaims K's one hot entry), K's touch at
// tK, then both checks 100 ms later against K's true events (32 at t0, one
// at tK). Legs: the reclaim inside K's install (W9's leg 2: on W4d's
// library the touch pushed twice); a control (K restored with its entry);
// the control with the touch's event pushed a second time by hand.
func TestW10Sight(t *testing.T) {
	for _, leg := range []string{"reclaim at install", "control", "control, pushed again by hand"} {
		t.Run(leg, func(t *testing.T) {
			f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: 1,
				Clock: func() uint64 { return 1 }})
			if err != nil {
				t.Fatal(err)
			}
			const t0 = 60_000
			k := mix64(1)
			fp, tg := fingerprint(k), tag(k)
			p := f.primary(k)
			kl := [2]uint64{p, f.alt(p, fp)}
			for range 32 {
				f.TouchAt(k, 0, t0)
			}
			i := f.locate(k, fp, tg)
			if f.Promoted() != 1 || i < 0 || !isHot(f.words[i+1]) {
				t.Fatalf("K not promoted (%d) or not in the table", f.Promoted())
			}
			f.stashPut(f.stashID(k, fp, tg), f.words[i], f.words[i+1], t0)
			f.words[i], f.words[i+1] = 0, 0
			var l uint64
			for j := uint64(2); ; j++ {
				l = mix64(j)
				lp := f.primary(l)
				if la := f.alt(lp, fingerprint(l)); lp != kl[0] && lp != kl[1] && la != kl[0] && la != kl[1] {
					break
				}
			}
			tL, tK := t0+f.horizon+3*f.unit, t0+f.horizon-1
			fired := false
			if leg == "reclaim at install" {
				testHookInstalling = func(int) {
					if !fired {
						fired = true
						for range 32 {
							f.TouchAt(l, 0, tL)
						}
					}
				}
			}
			defer func() { testHookInstalling = nil }()
			f.TouchAt(k, 0, tK)
			testHookInstalling = nil
			j := f.locate(k, fp, tg)
			if j < 0 {
				t.Fatal("K not in the table after its touch")
			}
			if leg == "control, pushed again by hand" {
				f.words[j] = f.next0(f.words[j], tK)
			}
			evs := make([]uint64, 0, 33)
			for range 32 {
				evs = append(evs, t0)
			}
			evs = append(evs, tK)
			now := tK + 100
			logMsg, checked := f.logHigh(k, now, 0, evs)
			var high []string
			for _, w := range []uint64{250, 1_000} {
				r := f.CountAt(k, w, now)
				high = append(high, f.countHigh(r, f.ghostUpper(k, w, now), evs, w, now, 0))
			}
			t.Logf("W10 sight, %s: reclaim %v, promoted %d; log %#x: log check %v %q; high at 250 ms, 1 s: %q",
				leg, fired, f.Promoted(), dripOf(f.words[j]), checked, logMsg, high)
			flagged := logMsg != "" && high[0] != "" && high[1] != ""
			switch {
			case leg == "reclaim at install" && (!fired || f.Promoted() != 2):
				t.Errorf("the reclaim did not happen inside K's install")
			case leg == "control" && (logMsg != "" || high[0] != "" || high[1] != "" || !checked):
				t.Errorf("the checks flag a control (or skip its log)")
			case leg == "control, pushed again by hand" && !flagged:
				t.Errorf("the checks miss a second push made by hand")
			}
		})
	}
}
