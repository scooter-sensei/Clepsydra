package temporalfilter

// lab/sw-2026-11, W9 (SW-RESULTS.md): does a touch whose hot restore loses
// its entry after hotGone push its event twice into the drip log? lab/w9.sh
// copies this file into a detached worktree of the tree under test; it is
// never part of the tree. Only the tree's own hooks are used.

import (
	"fmt"
	"testing"
	"time"
)

type w9Leg struct {
	name    string
	full    bool // K's two lines full of live keys: the restore relocates or evicts (else: K's freed slot)
	reclaim bool // L takes K's hot entry from a hook inside K's claim
}

// TestW9HotRestorePush: K is promoted at t0, its words moved to the stash
// and its slot freed, as claimAt's eviction does; L, whose lines are not K's,
// is promoted at tL, more than a horizon past K's newest event, which
// reclaims K's entry (one hot entry); K's restoring touch at tK is behind
// L's clock and within a horizon of K's last touch (hot_test.go's
// hotReclaimSetup). The probe reads K's slot after that touch.
func TestW9HotRestorePush(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("plain mode only")
	}
	for _, leg := range []w9Leg{
		{"control, free slot", false, false},
		{"reclaim at install, free slot", false, true},
		{"control, full lines", true, false},
		{"reclaim in the search, full lines", true, true},
	} {
		t.Run(leg.name, func(t *testing.T) { w9Run(t, leg) })
	}
}

func w9Run(t *testing.T, leg w9Leg) {
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
		t.Fatalf("probe: K not promoted (%d) or not in the table", f.Promoted())
	}
	w0 := f.words[i] // K's word0 as stashed
	f.stashPut(f.stashID(k, fp, tg), w0, f.words[i+1], t0)
	f.words[i], f.words[i+1] = 0, 0
	if leg.full {
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
	reclaim := func(at string) {
		if fired != "" {
			return
		}
		fired = at
		for range 32 {
			f.TouchAt(l, 0, tL)
		}
	}
	if leg.reclaim {
		if leg.full {
			testHookCopied = func() { reclaim("copied") }
			testHookVictim = func() { reclaim("victim") }
		} else {
			testHookInstalling = func(int) { reclaim("installing") }
		}
	}
	// bump's hot add, on any slot: in K's touch, it runs only on a look after
	// miss (miss adds to the entry itself), and finds the pointer miss placed
	var adding []int
	testHookHotAdding = func(i int) { adding = append(adding, i) }
	defer func() { testHookCopied, testHookVictim, testHookInstalling, testHookHotAdding = nil, nil, nil, nil }()
	once, twice := dripOf(f.next0(w0, tK)), dripOf(f.next0(f.next0(w0, tK), tK))
	rel, ev, res := f.Relocations(), f.Evictions(), f.Restored()
	f.TouchAt(k, 0, tK)
	testHookCopied, testHookVictim, testHookInstalling, testHookHotAdding = nil, nil, nil, nil
	rel, ev, res = f.Relocations()-rel, f.Evictions()-ev, f.Restored()-res

	j := f.locate(k, fp, tg)
	if j < 0 {
		t.Fatalf("probe: K not in the table after its touch (hook %q)", fired)
	}
	retried := false // the path: bump's hot add on K's slot, after miss returned
	for _, a := range adding {
		retried = retried || a == j
	}
	x0, x1 := f.words[j], f.words[j+1]
	got := dripOf(x0)
	pushes := "n/a (no log in this build)"
	if dripOn {
		switch got {
		case once:
			pushes = "once"
		case twice:
			pushes = "twice"
		default:
			pushes = "neither"
		}
	}
	slot, kept := "", false
	if v, ok := f.viewOf(f.stashID(k, fp, tg), x1, x0&tsMask); isHot(x1) {
		slot, kept = fmt.Sprintf("hot pointer, the entry K's: %v", ok), ok
	} else {
		n := uint64(0)
		for _, c := range v.c {
			n += c
		}
		slot = fmt.Sprintf("register of %d events", n)
	}
	var counts []string
	for _, w := range []uint64{250, 1_000, 10_000, 30_000} {
		counts = append(counts, fmt.Sprintf("%v", f.CountAt(k, w, tK).Count))
	}
	t.Logf("W9 %s: hook %q, promoted %d; retried through bump's hot add on K's slot: %v; relocations %d, evictions %d, restored %d; last touch at tK %v; log %#x (one push %#x, two %#x): pushed %s; slot: %s; Count at 250 ms, 1 s, 10 s, 30 s: %v (truth 1)",
		leg.name, fired, f.Promoted(), retried, rel, ev, res, x0&tsMask == tK, got, once, twice, pushes, slot, counts)

	// the probe's own checks (rule clause 1); a reclaim leg whose touch did
	// not retry through bump is reported, not failed: the path was not taken
	if leg.reclaim && (fired == "" || f.Promoted() != 2) {
		t.Errorf("probe: no reclaim inside K's claim (hook %q, promoted %d)", fired, f.Promoted())
	}
	if !leg.reclaim && (f.Promoted() != 1 || !kept || res != 1 || retried) {
		t.Errorf("probe: the control did not restore K with its entry (promoted %d, slot %s, restored %d, retried %v)", f.Promoted(), slot, res, retried)
	}
	if dripOn && once == twice {
		t.Errorf("probe: one push and two leave the same log %#x: the leg cannot tell", once)
	}
	if dripOn && !leg.reclaim && pushes != "once" {
		t.Errorf("probe: the control reads %s", pushes)
	}
}
