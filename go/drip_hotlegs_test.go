//go:build tfdrip

package temporalfilter

// lab/sw-2026-11, W11: the hot restore's legs W4e left open. A restore whose
// claim evicts; ConstantTime's and Background's installs; their waits (the
// stash, the wait room, with and without the placer); and a reclaim by a
// concurrent caller rather than a hook. In each, a touch that placed its
// event with the history (or let it wait with it) and then found its hot
// entry reclaimed must not push the event again.

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hotLeg is one leg of TestDripHotRestoreLegs.
type hotLeg struct {
	name    string
	mode    string // "plain", "ct" (ConstantTime) or "bg" (Background)
	fill    string // "free" (K's slot freed), "lines" (K's two lines full of live keys) or "table" (every line full)
	reclaim string // "" (a control), or the hook L's promotion runs from: "victim", "installing", "restore add"
	placer  bool   // Background: the placer goroutine runs (else the test places, bgManual)
}

// TestDripHotRestoreLegs: as TestDripHotRestorePushOnce (W4e), K is promoted
// at t0, its words moved to the stash and its slot freed; L, on other lines
// and another claim stripe, is promoted at tL, more than a horizon past K's
// newest event, which reclaims K's one hot entry; K's restoring touch is at
// tK, behind L's clock and within a horizon of K's last touch. The legs:
//
//   - plain, every line full of keys live at tK: K's claim can't relocate and
//     evicts; L's promotion runs from testHookVictim, inside the claim;
//   - ConstantTime and Background, K's slot free: the claim installs on the
//     call path; L's promotion runs from testHookInstalling;
//   - ConstantTime, K's lines full: K's history waits in the stash with the
//     touch's push; L's promotion runs from testHookRestoreAdd, after the
//     wait and before hotAdd; the look again recalls the waiting history,
//     finds its entry gone and starts K afresh (claims A25);
//   - Background, K's lines full, the placer running: K waits in the wait
//     room; from testHookRestoreAdd the test waits for the placer to place
//     K, then runs L's promotion; the look again finds K in the table;
//   - Background, K's lines full, no placer: as above with nothing placing
//     K; the look again takes K out of the wait room and starts it afresh.
//
// Each reclaim leg has a control with no reclaim. K's log after the touch is
// classed against W4e's history with the touch's one push ("once"), with
// two ("twice") and a fresh start ("fresh"), and K is read 100 ms after its
// touch at 250 ms, 1 s and 10 s, where the truth is 1, and at tK at 60 s,
// where it is 33 (reported).
func TestDripHotRestoreLegs(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("each leg sets its own mode")
	}
	for _, leg := range []hotLeg{
		{"plain, full table, control", "plain", "table", "", false},
		{"plain, full table, reclaim at the eviction", "plain", "table", "victim", false},
		{"ConstantTime, free slot, control", "ct", "free", "", false},
		{"ConstantTime, free slot, reclaim at install", "ct", "free", "installing", false},
		{"ConstantTime, full lines, control", "ct", "lines", "", false},
		{"ConstantTime, full lines, reclaim after the wait", "ct", "lines", "restore add", false},
		{"Background, free slot, control", "bg", "free", "", true},
		{"Background, free slot, reclaim at install", "bg", "free", "installing", true},
		{"Background, full lines, placer, control", "bg", "lines", "", true},
		{"Background, full lines, placer, placed then reclaimed", "bg", "lines", "restore add", true},
		{"Background, full lines, no placer, control", "bg", "lines", "", false},
		{"Background, full lines, no placer, reclaim after the wait", "bg", "lines", "restore add", false},
	} {
		t.Run(leg.name, func(t *testing.T) { dripHotLeg(t, leg) })
	}
}

// hotLegSetup builds a leg's filter: K promoted at t0, stashed, its slot
// freed, the lines filled as fill says with keys of other claim stripes
// than K's; L chosen on other lines than K's, on another stripe, one whose
// touches don't run the decay sweep.
func hotLegSetup(t testing.TB, bg, placer bool, fill string) (f *Filter, k, l, w0, w1, tK, tL uint64) {
	f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: 1,
		Clock: func() uint64 { return 1 }, Background: bg})
	if err != nil {
		t.Fatal(err)
	}
	f.bgManual = bg && !placer
	const t0 = 60_000
	k = mix64(1)
	fp, tg := fingerprint(k), tag(k)
	ks := k >> (64 - claimStripeBits)
	p := f.primary(k)
	kl := [2]uint64{p, f.alt(p, fp)}
	for range 32 { // past the register's 31 events in a unit: promoted
		f.TouchAt(k, 0, t0)
	}
	i := f.locate(k, fp, tg)
	if f.Promoted() != 1 || i < 0 || !isHot(f.words[i+1]) {
		t.Fatalf("K not promoted (%d) or not in the table", f.Promoted())
	}
	w0, w1 = f.words[i], f.words[i+1] // K's words as stashed
	f.stashPut(f.stashID(k, fp, tg), w0, w1, t0)
	f.words[i], f.words[i+1] = 0, 0
	lines := []uint64{}
	switch fill {
	case "lines":
		lines = kl[:]
	case "table":
		for line := range f.lines {
			lines = append(lines, line)
		}
	}
	need := map[uint64]bool{}
	for _, line := range lines {
		if countLine(f, line, t0) < 4 {
			need[line] = true
		}
	}
	for j := uint64(1 << 40); len(need) > 0; j++ {
		h := mix64(j)
		if line := f.primary(h); need[line] && h>>(64-claimStripeBits) != ks {
			f.TouchAt(h, 0, t0)
			if countLine(f, line, t0) == 4 {
				delete(need, line)
			}
		}
	}
	if f.Evictions() != 0 {
		t.Fatalf("the fill evicted %d keys", f.Evictions())
	}
	for j := uint64(2); ; j++ {
		l = mix64(j)
		lp := f.primary(l)
		la := f.alt(lp, fingerprint(l))
		if lp != kl[0] && lp != kl[1] && la != kl[0] && la != kl[1] && l>>(64-claimStripeBits) != ks && l&decayEvery != 0 {
			break
		}
	}
	return f, k, l, w0, w1, t0 + f.horizon - 1, t0 + f.horizon + 3*f.unit
}

// dripClass names K's drip log d against W4e's history w0 at tK.
func dripClass(f *Filter, d, w0, tK uint64) string {
	switch d {
	case dripOf(f.next0(w0, tK)):
		return "once"
	case dripOf(f.next0(f.next0(w0, tK), tK)):
		return "twice"
	case dripOf(dripFresh(tK)):
		return "fresh"
	}
	return fmt.Sprintf("other %#x", d)
}

func dripHotLeg(t *testing.T, leg hotLeg) {
	f, k, l, w0, _, tK, tL := hotLegSetup(t, leg.mode == "bg", leg.placer, leg.fill)
	defer f.Close()
	fp, tg := fingerprint(k), tag(k)
	if leg.mode == "ct" {
		f.ct = true // functional, as the concurrent fuzzer's ConstantTime cases
	}
	placed := func() bool { // K in the table (the placer's)
		for range 40_000 {
			if f.locate(k, fp, tg) >= 0 {
				return true
			}
			time.Sleep(50 * time.Microsecond)
		}
		return false
	}
	fired, wasPlaced := "", false
	take := func(at string) {
		if fired != "" {
			return
		}
		fired = at
		if leg.placer && at == "restore add" {
			wasPlaced = placed()
		}
		for range 32 {
			f.TouchAt(l, 0, tL)
		}
	}
	var mu sync.Mutex
	var adding []int // bump's hot add: in K's touch, only a look again after miss returned
	var taken atomic.Int64
	switch leg.reclaim {
	case "victim":
		testHookVictim = func() { take("victim") }
	case "installing":
		testHookInstalling = func(int) { take("installing") }
	case "restore add":
		testHookRestoreAdd = func() { take("restore add") }
	}
	testHookHotAdding = func(i int) { mu.Lock(); adding = append(adding, i); mu.Unlock() }
	testHookTaken = func() { taken.Add(1) }
	// The hooks are cleared once, at the end, after the placer has nothing
	// left to place: it reads them as it places, and Close doesn't wait for it.
	defer func() {
		for i := 0; leg.placer && i < 2000 && f.deferred.Load() != 0; i++ {
			time.Sleep(time.Millisecond)
		}
		testHookVictim, testHookInstalling, testHookRestoreAdd, testHookHotAdding, testHookTaken = nil, nil, nil, nil, nil
	}()
	res, ev, rel, wt := f.Restored(), f.Evictions(), f.Relocations(), f.Waited()
	f.TouchAt(k, 0, tK)
	res, ev, rel, wt = f.Restored()-res, f.Evictions()-ev, f.Relocations()-rel, f.Waited()-wt
	if leg.placer && leg.fill == "lines" && leg.reclaim == "" {
		wasPlaced = placed()
	}

	j := f.locate(k, fp, tg)
	retried := false
	mu.Lock()
	for _, a := range adding {
		retried = retried || a == j && j >= 0
	}
	mu.Unlock()
	x0, x1, src := f.read(k, tK, -1)
	where := map[int]string{0: "nowhere", srcTable: "the table", srcStash: "the stash or the wait room", srcRoom: "the wait room"}[src]
	if j >= 0 {
		where = "the table"
		x0, x1 = atomic.LoadUint64(&f.words[j]), atomic.LoadUint64(&f.words[j+1])
	}
	_, mine := f.viewOf(f.stashID(k, fp, tg), x1, x0&tsMask)
	class := dripClass(f, dripOf(x0), w0, tK)
	var counts []int
	for _, w := range []uint64{250, 1_000, 10_000} { // read 100 ms after the touch; the truth is 1
		counts = append(counts, f.CountAt(k, w, tK+100).Count)
	}
	r60 := f.CountAt(k, 60_000, tK) // reported: K's events in the window are its 32 at t0 and the touch
	t.Logf("W11 %s: hook %q, promoted %d, placed %v, retried %v, taken from the room %d; restored %d, evictions %d, relocations %d, waited %d; K in %s, last touch %d, log %#x: %s; word1 hot %v, K's entry %v; Count at tK+100 ms, 250 ms, 1 s, 10 s: %v (truth 1); at tK, 60 s: %+v (K's events in it: 32 at t0 and the touch)",
		leg.name, fired, f.Promoted(), wasPlaced, retried, taken.Load(), res, ev, rel, wt, where, x0&tsMask, dripOf(x0), class, isHot(x1), isHot(x1) && mine, counts, r60)

	// The leg's path, else it says nothing.
	switch {
	case leg.reclaim != "" && (fired != leg.reclaim || f.Promoted() != 2):
		t.Fatalf("the reclaim did not happen in K's touch (hook %q, promoted %d)", fired, f.Promoted())
	case leg.fill == "table" && ev == 0:
		t.Fatalf("K's restore did not evict")
	case leg.fill == "lines" && leg.mode != "plain" && wt == 0:
		t.Fatalf("K's history did not wait")
	case leg.placer && leg.fill == "lines" && !wasPlaced:
		t.Fatalf("the placer did not place K")
	case leg.reclaim != "" && (leg.mode == "ct" && leg.fill == "lines" || leg.mode == "bg" && leg.fill == "lines" && !leg.placer):
		// the look again recalls the waiting history (from the room: testHookTaken)
		if leg.mode == "bg" && taken.Load() == 0 {
			t.Fatalf("K's look again did not take K out of the wait room")
		}
	case leg.reclaim != "" && !retried:
		t.Fatalf("K's touch did not look again through bump's hot add on its slot")
	}

	// What the leg reads.
	if x0&tsMask != tK {
		t.Errorf("K's last touch %d, want %d", x0&tsMask, tK)
	}
	switch {
	case leg.reclaim == "":
		if class != "once" || !isHot(x1) || !mine {
			t.Errorf("the control did not restore K with its entry and one push (log %s, hot %v, K's %v)", class, isHot(x1), mine)
		}
	case leg.mode == "ct" && leg.fill == "lines" || leg.mode == "bg" && leg.fill == "lines" && !leg.placer:
		if class != "fresh" {
			t.Errorf("K's log %s after its waiting history's entry went, want the touch afresh (fresh)", class)
		}
	default:
		if class != "once" {
			t.Errorf("K's log %s, want its history with the touch pushed once (once)", class)
		}
		if isHot(x1) {
			t.Errorf("K's slot still points to a hot entry after the look again")
		}
	}
	if leg.reclaim != "" {
		for n, c := range counts {
			if c != 1 {
				t.Errorf("K reads Count %d at window %d of 3 100 ms after its touch, want 1", c, n+1)
			}
		}
	}
}

// TestDripHotRestoreRace (TF_DRIP_RACE_ROUNDS=n): a reclaim by a concurrent
// caller, not a hook. Each round builds TestDripHotRestoreLegs's filter with
// K's slot free (plain mode) and touches L 31 times at tL; then K's
// restoring touch at tK and L's 32nd touch, its promotion, which reclaims
// K's entry while the entry holds no event newer than K's history's, run at
// once from two goroutines, each after a random spin. A round is classed by
// K's log (once, twice, fresh), whether L was promoted, and whether K's touch
// looked again through bump's hot add (testHookHotAdding: in a round only
// K's look again adds to a hot entry): L first, K starts afresh; K first,
// its touch is counted in its entry and L is refused; L in between, K looks
// again, which must not push the event again.
func TestDripHotRestoreRace(t *testing.T) {
	rounds, _ := strconv.Atoi(os.Getenv("TF_DRIP_RACE_ROUNDS"))
	if rounds <= 0 {
		t.Skip("set TF_DRIP_RACE_ROUNDS=<rounds> to run")
	}
	var looked atomic.Int64
	testHookHotAdding = func(int) { looked.Add(1) }
	defer func() { testHookHotAdding = nil }()
	rng := rand.New(rand.NewPCG(11, 11))
	var sink atomic.Int64
	spin := func(n int) {
		x := 0
		for i := range n {
			x += i
		}
		sink.Add(int64(x))
	}
	classes := map[string]int{}
	var twice int
	var first string
	for round := range rounds {
		f, k, l, w0, w1, tK, tL := hotLegSetup(t, false, false, "free")
		for range 31 {
			f.TouchAt(l, 0, tL)
		}
		if f.Promoted() != 1 || f.hotGone(f.stashID(k, fingerprint(k), tag(k)), w1) {
			t.Fatalf("round %d: L promoted before the race (%d), or K's entry no longer K's", round, f.Promoted())
		}
		looked.Store(0)
		sk, sl := rng.IntN(4000), rng.IntN(4000)
		var start atomic.Bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for !start.Load() {
			}
			spin(sk)
			f.TouchAt(k, 0, tK)
		}()
		go func() {
			defer wg.Done()
			for !start.Load() {
			}
			spin(sl)
			f.TouchAt(l, 0, tL)
		}()
		start.Store(true)
		wg.Wait()
		x0, _, _ := f.read(k, tK, -1)
		class := dripClass(f, dripOf(x0), w0, tK)
		key := fmt.Sprintf("log %s, L promoted %v, K looked again %v", class, f.Promoted() == 2, looked.Load() > 0)
		classes[key]++
		if class == "twice" {
			if twice++; first == "" {
				first = fmt.Sprintf("round %d: %s (spins %d, %d)", round, key, sk, sl)
			}
		}
		f.Close()
	}
	keys := make([]string, 0, len(classes))
	for c := range classes {
		keys = append(keys, c)
	}
	slices.Sort(keys)
	for _, c := range keys {
		t.Logf("W11 race: %d of %d rounds: %s", classes[c], rounds, c)
	}
	if twice > 0 {
		t.Errorf("%d of %d rounds pushed K's event twice; first: %s", twice, rounds, first)
	}
}
