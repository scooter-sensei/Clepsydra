//go:build tfdrip

package temporalfilter

// lab/sw-2026-11, W4e: a hot restore whose entry is reclaimed after hotGone
// pushes its touch's event once (W9 found it pushed twice).

import (
	"testing"
	"time"
)

// TestDripHotRestorePushOnce: K is promoted at t0, its words moved to the
// stash and its slot freed, as claimAt's eviction does; L, whose lines are
// not K's, is promoted at tL, more than a horizon past K's newest event,
// which reclaims K's entry (one hot entry); K's restoring touch at tK is
// behind L's clock and within a horizon of K's last touch (as
// hotReclaimSetup). In the reclaim legs L's touches run from a hook inside
// K's claim, after hotGone and before hotAdd: miss places K's history with
// the touch's push, finds the entry gone and has the touch look again,
// which must not push the event again (lab/sw W9: it did, and a later read
// of K counted it twice). The controls restore K with its entry.
func TestDripHotRestorePushOnce(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("plain mode: the hooks run in the call path's claim")
	}
	for _, leg := range []struct {
		name          string
		full, reclaim bool // full: K's two lines full of live keys (the restore relocates or evicts)
	}{
		{"control, free slot", false, false},
		{"reclaim at install, free slot", false, true},
		{"control, full lines", true, false},
		{"reclaim in the search, full lines", true, true},
	} {
		t.Run(leg.name, func(t *testing.T) { dripHotRestore(t, leg.full, leg.reclaim) })
	}
}

func dripHotRestore(t *testing.T, full, reclaim bool) {
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
	for range 32 { // past the register's 31 events in a unit: promoted
		f.TouchAt(k, 0, t0)
	}
	i := f.locate(k, fp, tg)
	if f.Promoted() != 1 || i < 0 || !isHot(f.words[i+1]) {
		t.Fatalf("K not promoted (%d) or not in the table", f.Promoted())
	}
	w0 := f.words[i] // K's word0 as stashed
	f.stashPut(f.stashID(k, fp, tg), w0, f.words[i+1], t0)
	f.words[i], f.words[i+1] = 0, 0
	if full {
		for _, line := range kl {
			for j := uint64(1 << 40); countLine(f, line, t0) < 4; j++ {
				if h := mix64(j); f.primary(h) == line {
					f.TouchAt(h, 0, t0)
				}
			}
		}
	}
	var l uint64
	for j := uint64(2); ; j++ { // L's lines are not K's: its installs never meet K's
		l = mix64(j)
		lp := f.primary(l)
		if la := f.alt(lp, fingerprint(l)); lp != kl[0] && lp != kl[1] && la != kl[0] && la != kl[1] {
			break
		}
	}
	tL, tK := t0+f.horizon+3*f.unit, t0+f.horizon-1
	fired := ""
	take := func(at string) {
		if fired != "" {
			return
		}
		fired = at
		for range 32 {
			f.TouchAt(l, 0, tL)
		}
	}
	if reclaim {
		if full {
			testHookCopied = func() { take("copied") }
			testHookVictim = func() { take("victim") }
		} else {
			testHookInstalling = func(int) { take("installing") }
		}
	}
	var adding []int // bump's hot add: in K's touch, only a look after miss returned
	testHookHotAdding = func(i int) { adding = append(adding, i) }
	defer func() { testHookCopied, testHookVictim, testHookInstalling, testHookHotAdding = nil, nil, nil, nil }()
	once := dripOf(f.next0(w0, tK))
	res := f.Restored()
	f.TouchAt(k, 0, tK)
	testHookCopied, testHookVictim, testHookInstalling, testHookHotAdding = nil, nil, nil, nil
	res = f.Restored() - res

	j := f.locate(k, fp, tg)
	if j < 0 {
		t.Fatalf("K not in the table after its touch (hook %q)", fired)
	}
	retried := false
	for _, a := range adding {
		retried = retried || a == j
	}
	x0, x1 := f.words[j], f.words[j+1]
	var counts []int
	for _, w := range []uint64{250, 1_000, 10_000} { // read 100 ms after the touch; the truth is 1
		counts = append(counts, f.CountAt(k, w, tK+100).Count)
	}
	t.Logf("hook %q, promoted %d, retried %v, restored %d; log %#x (one push %#x); word1 hot %v; Count at tK+100 ms, 250 ms, 1 s, 10 s: %v (truth 1)",
		fired, f.Promoted(), retried, res, dripOf(x0), once, isHot(x1), counts)

	if reclaim {
		if fired == "" || f.Promoted() != 2 || !retried {
			t.Fatalf("the reclaim did not happen inside K's claim, or K's touch did not look again (hook %q, promoted %d, retried %v)", fired, f.Promoted(), retried)
		}
		if isHot(x1) {
			t.Errorf("K's slot still points to a hot entry after the look again")
		}
		for n, c := range counts {
			if c != 1 {
				t.Errorf("K reads Count %d at window %d of 3 100 ms after its touch, want 1", c, n+1)
			}
		}
	} else if _, ok := f.viewOf(f.stashID(k, fp, tg), x1, x0&tsMask); !isHot(x1) || !ok || res != 1 || retried {
		t.Fatalf("the control did not restore K with its entry (hot %v, K's %v, restored %d, retried %v)", isHot(x1), ok, res, retried)
	}
	if x0&tsMask != tK {
		t.Errorf("K's last touch %d, want %d", x0&tsMask, tK)
	}
	if got := dripOf(x0); got != once {
		t.Errorf("K's log %#x, want one push of the touch's event (%#x)", got, once)
	}
}
