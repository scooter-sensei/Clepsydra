package temporalfilter

// lab/sw-2026-11, W11b: the hot restore's legs (W11) on a history whose drip
// log at K's touch tells a fresh start from the history with the touch's one
// push. W11 touched K a horizon less a tick after its history, where next0
// sets the log all ones, so the two were one log (0xfff). Here K's 32 events
// are spread over quanta 2 to 9 of unit 24 and K's touch comes 4 quanta
// after the last, in the same unit; L, the reclaimer, runs more than a
// horizon ahead of K's clock. Where the history goes in and its entry is then
// reclaimed, the look again replaces the dead pointer by unpoint's fresh
// register and keeps the log, and Count and Upper read the log's older events
// beside a register of one. The file has no build tag: the default build
// reads the same legs without a log, Upper's reference.

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	hfT0   = 60_000     // unit 24's start (Horizon 1 minute at 1 ms: units of 2,500 ticks, quanta of 156.25)
	hfTK   = 62_040     // K's touch: quantum 13 of unit 24, 4 quanta after the history's last event
	hfRead = hfTK + 100 // the reads after K's touch
)

// hfHistory is K's history: 29 events at quantum 2 of unit 24, then one each
// at quanta 4, 6 and 9. The 32nd, past the register's 31 in a unit, promotes
// K.
var hfHistory = func() []uint64 {
	h := make([]uint64, 0, 32)
	for range 29 {
		h = append(h, 60_320)
	}
	return append(h, 60_640, 60_960, 61_420)
}()

// hfWindows are the reads' windows at hfRead. Three start inside unit 24:
// 250 ms after the history's last quantum (9), 1 s between its quanta 6 and
// 9, 2 s before its quantum 2. 10 s and 60 s start before unit 24, where a
// register of bucket 0 alone is read whole (estimate's first answer),
// without the log.
var hfWindows = []uint64{250, 1_000, 2_000, 10_000, 60_000}

// hfGiven is the number of K's events given in [hfRead-w, hfRead]: the
// history's and the touch's. It is the probe's count, not the contract's:
// the reclaimer's clock is more than a horizon past all of them.
func hfGiven(w uint64) int {
	n := 0
	for _, e := range append(slices.Clone(hfHistory), hfTK) {
		if e+w >= hfRead && e <= hfRead {
			n++
		}
	}
	return n
}

// hotFreshSetup is hotLegSetup (W11) with hfHistory: K promoted by its 32nd
// event at 61,420, its words moved to the stash at that tick and its slot
// freed; the lines filled as fill says with keys of other claim stripes than
// K's, touched at 61,420, so live at hfTK; L on other lines than K's, on
// another stripe, one whose touches don't run the decay sweep. tL is in unit
// 51, past the reclaim of an entry whose newest unit is 24.
func hotFreshSetup(t testing.TB, bg, placer bool, fill string) (f *Filter, k, l, w0, w1, tL uint64) {
	f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: 1,
		Clock: func() uint64 { return 1 }, Background: bg})
	if err != nil {
		t.Fatal(err)
	}
	f.bgManual = bg && !placer
	if f.unit != 2_500 || f.unitOf(hfT0) != 24 {
		t.Fatalf("unit %d, unit of %d is %d: the history's ticks assume 2,500 and 24", f.unit, hfT0, f.unitOf(hfT0))
	}
	last := hfHistory[len(hfHistory)-1]
	k = mix64(1)
	fp, tg := fingerprint(k), tag(k)
	ks := k >> (64 - claimStripeBits)
	p := f.primary(k)
	kl := [2]uint64{p, f.alt(p, fp)}
	for _, e := range hfHistory {
		f.TouchAt(k, 0, e)
	}
	i := f.locate(k, fp, tg)
	if f.Promoted() != 1 || i < 0 || !isHot(f.words[i+1]) || f.words[i]&tsMask != last {
		t.Fatalf("K not promoted (%d), not in the table, or not last touched at %d", f.Promoted(), last)
	}
	w0, w1 = f.words[i], f.words[i+1] // K's words as stashed
	f.stashPut(f.stashID(k, fp, tg), w0, w1, last)
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
		if countLine(f, line, last) < 4 {
			need[line] = true
		}
	}
	for j := uint64(1 << 40); len(need) > 0; j++ {
		h := mix64(j)
		if line := f.primary(h); need[line] && h>>(64-claimStripeBits) != ks {
			f.TouchAt(h, 0, last)
			if countLine(f, line, last) == 4 {
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
	return f, k, l, w0, w1, hfT0 + f.horizon + 3*f.unit
}

// hfClasses are K's three logs at hfTK from its stashed word0 w0: the
// history with the touch's one push, with two, and the touch afresh.
func hfClasses(f *Filter, w0 uint64) (once, twice, fresh uint64) {
	n := f.next0(w0, hfTK)
	return dripOf(n), dripOf(f.next0(n, hfTK)), dripOf(dripFresh(hfTK))
}

// hfClass names K's drip log d against its stashed word0 w0.
func hfClass(f *Filter, d, w0 uint64) string {
	if !dripOn {
		return "no log"
	}
	once, twice, fresh := hfClasses(f, w0)
	switch d {
	case once:
		return "once"
	case twice:
		return "twice"
	case fresh:
		return "fresh"
	}
	return fmt.Sprintf("other %#x", d)
}

// hfInputs checks the probe's inputs and logs them: the history and K's touch
// in one unit, the touch 1 to 10 quanta after the history's last event; tL's
// unit far enough past it to reclaim K's entry (an entry is reclaimed once its
// newest unit is more than a horizon and two units behind) and tL more than a
// horizon ahead of tK; the reads' window starts where hfWindows says, none
// strictly inside the quantum of a history event; with tfdrip, once, twice and
// fresh three logs.
func hfInputs(t *testing.T, f *Filter, w0, tL uint64) {
	last := hfHistory[len(hfHistory)-1]
	u := f.unitOf(last)
	gap := f.quantum(hfTK) - f.quantum(last)
	var starts []uint64
	var units []int64
	var cut []string
	for _, w := range hfWindows {
		s := hfRead - w
		starts = append(starts, s)
		units = append(units, f.unitOf(s))
		for _, e := range hfHistory {
			q := f.quantum(e)
			lo, hi := float64(q)*float64(f.unit)/16, float64(q+1)*float64(f.unit)/16
			if c := fmt.Sprintf("%d in quantum %d", s, q); float64(s) > lo && float64(s) < hi && !slices.Contains(cut, c) {
				cut = append(cut, c)
			}
		}
	}
	once, twice, fresh := hfClasses(f, w0)
	distinct := once != twice && once != fresh && twice != fresh
	t.Logf("W11b inputs: history in unit %d (first %d, last %d at quantum %d), tK %d (unit %d, quantum %d, gap %d); tL %d (unit %d, %d units past the history's; %d ticks ahead of tK, horizon %d); window starts %v in units %v, inside a history event's quantum %v; logs: history %#x, once %#x, twice %#x, fresh %#x, distinct %v",
		u, hfHistory[0], last, f.quantum(last), hfTK, f.unitOf(hfTK), f.quantum(hfTK), gap, tL, f.unitOf(tL), f.unitOf(tL)-u, tL-hfTK, f.horizon, starts, units, cut, dripOf(w0), once, twice, fresh, distinct)
	switch {
	case f.unitOf(hfHistory[0]) != u || f.unitOf(hfTK) != u || f.unitOf(hfRead) != u:
		t.Fatalf("inputs: the history, K's touch and the reads are not in one unit")
	case gap < 1 || gap > 10:
		t.Fatalf("inputs: K's touch %d quanta after the history's last, want 1 to 10", gap)
	case f.unitOf(tL)-u < unitsPerHorizon+3 || tL-hfTK <= f.horizon:
		t.Fatalf("inputs: tL does not reclaim K's entry, or is not more than a horizon ahead of tK")
	case units[0] != u || units[1] != u || units[2] != u || units[3] >= u || units[4] >= u:
		t.Fatalf("inputs: a window starts on the wrong side of unit %d's start", u)
	case len(cut) > 0:
		t.Fatalf("inputs: a window starts inside a history event's quantum: %v", cut)
	case dripOn && !distinct:
		t.Fatalf("inputs: once, twice and fresh are not three logs")
	}
}

// hfLeg is one leg of TestDripHotRestoreFresh: W11's legs and controls, and
// three where L's promotion reclaims K's entry before K's touch.
type hfLeg struct {
	id, name string
	mode     string // "plain", "ct" (ConstantTime) or "bg" (Background)
	fill     string // "free" (K's slot freed), "lines" (K's two lines full of live keys) or "table" (every line full)
	reclaim  string // "" (a control); the hook L's promotion runs from: "victim", "installing", "restore add"; or "before" K's touch
	placer   bool   // Background: the placer goroutine runs (else the test places, bgManual)
}

var hfLegs = []hfLeg{
	{"c1", "plain, full table, control", "plain", "table", "", false},
	{"1", "plain, full table, reclaim at the eviction", "plain", "table", "victim", false},
	{"c2", "ConstantTime, free slot, control", "ct", "free", "", false},
	{"2", "ConstantTime, free slot, reclaim at install", "ct", "free", "installing", false},
	{"c3", "ConstantTime, full lines, control", "ct", "lines", "", false},
	{"3", "ConstantTime, full lines, reclaim after the wait", "ct", "lines", "restore add", false},
	{"c4", "Background, free slot, control", "bg", "free", "", true},
	{"4", "Background, free slot, reclaim at install", "bg", "free", "installing", true},
	{"c5", "Background, full lines, placer, control", "bg", "lines", "", true},
	{"5", "Background, full lines, placer, placed then reclaimed", "bg", "lines", "restore add", true},
	{"c6", "Background, full lines, no placer, control", "bg", "lines", "", false},
	{"6", "Background, full lines, no placer, reclaim after the wait", "bg", "lines", "restore add", false},
	{"7", "plain, free slot, reclaimed before K's touch", "plain", "free", "before", false},
	{"8", "ConstantTime, free slot, reclaimed before K's touch", "ct", "free", "before", false},
	{"9", "Background, free slot, reclaimed before K's touch", "bg", "free", "before", true},
}

// TestDripHotRestoreFresh: W11's legs on hfHistory, and three legs where K's
// entry is reclaimed before its touch (its touch starts afresh: the
// classifier's sight of a fresh start on a filter's own path). K's log after
// its touch is classed against its stashed word0 (once, twice, fresh), and K
// is read 100 ms after its touch at hfWindows beside the events given. The
// assertions are W4e's library's as the code reads: a control restores K
// with its entry and one push; legs 1, 2, 4 and 5 read once beside unpoint's
// fresh register; legs 3 and 6 (the history waits, its entry goes) and 7 to 9
// read fresh; no reclaim leg reads Upper over the events given.
func TestDripHotRestoreFresh(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("each leg sets its own mode")
	}
	f, _, _, w0, _, tL := hotFreshSetup(t, false, false, "free")
	hfInputs(t, f, w0, tL)
	f.Close()
	for _, leg := range hfLegs {
		t.Run(leg.id+" "+leg.name, func(t *testing.T) { hfRun(t, leg, w0) })
	}
}

func hfRun(t *testing.T, leg hfLeg, w0In uint64) {
	f, k, l, w0, _, tL := hotFreshSetup(t, leg.mode == "bg", leg.placer, leg.fill)
	defer f.Close()
	if w0 != w0In {
		t.Fatalf("path not taken: K's history's word0 %#x, the inputs' %#x", w0, w0In)
	}
	fp, tg := fingerprint(k), tag(k)
	id := f.stashID(k, fp, tg)
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
	promoteL := func() {
		for range 32 {
			f.TouchAt(l, 0, tL)
		}
	}
	fired, wasPlaced, before := "", false, "-"
	take := func(at string) {
		if fired != "" {
			return
		}
		fired = at
		if leg.placer && at == "restore add" {
			wasPlaced = placed()
		}
		promoteL()
	}
	var mu sync.Mutex
	var adding []int // bump's hot add: in K's touch, only a look again after miss returned
	var taken atomic.Int64
	switch leg.reclaim {
	case "before":
		fired = "before"
		promoteL()
		s0, s1, src := f.read(k, hfTK, -1)
		before = fmt.Sprintf("src %d, last touch %d, hot %v, entry gone %v", src, s0&tsMask, isHot(s1), f.hotGone(id, s1))
		if f.Promoted() != 2 || src != srcStash || !isHot(s1) || !f.hotGone(id, s1) {
			t.Fatalf("path not taken: K is not in the stash at tK with its entry gone (promoted %d; %s)", f.Promoted(), before)
		}
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
	f.TouchAt(k, 0, hfTK)
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
	nAdding := len(adding)
	mu.Unlock()
	x0, x1, src := f.read(k, hfTK, -1)
	where := map[int]string{0: "nowhere", srcTable: "the table", srcStash: "the stash or the wait room", srcRoom: "the wait room"}[src]
	if j >= 0 {
		where = "the table"
		x0, x1 = atomic.LoadUint64(&f.words[j]), atomic.LoadUint64(&f.words[j+1])
	}
	_, mine := f.viewOf(id, x1, x0&tsMask)
	class := hfClass(f, dripOf(x0), w0)
	build := "default"
	if dripOn {
		build = "tfdrip"
	}
	var reads []string
	var over []string // a reclaim leg's reads whose Upper is over the events given
	for _, w := range hfWindows {
		r := f.CountAt(k, w, hfRead)
		g := hfGiven(w)
		reads = append(reads, fmt.Sprintf("%d:%d/%d/%d", w, r.Count, r.Upper, g))
		if leg.reclaim != "" && r.Upper > g {
			over = append(over, fmt.Sprintf("Upper %d over %d given at %d ms", r.Upper, g, w))
		}
	}
	t.Logf("W11b leg %s | %s | build %s | hook %s | before %s | promoted %d | placed %v | retried %v | taken %d | restored %d | evictions %d | relocations %d | waited %d | K in %s | last %d | log %#x | class %s | hot %v | mine %v | reads %s",
		leg.id, leg.name, build, fired, before, f.Promoted(), wasPlaced, retried, taken.Load(), res, ev, rel, wt, where, x0&tsMask, dripOf(x0), class, isHot(x1), isHot(x1) && mine, strings.Join(reads, " "))

	// The leg's path, else it says nothing.
	waits := leg.reclaim == "restore add" && (leg.mode == "ct" || !leg.placer) // legs 3 and 6
	switch {
	case leg.reclaim != "" && (fired != leg.reclaim || f.Promoted() != 2):
		t.Fatalf("path not taken: the reclaim did not happen (hook %q, promoted %d)", fired, f.Promoted())
	case leg.reclaim == "before" && (j < 0 || isHot(x1) || res != 0 || nAdding != 0):
		t.Fatalf("path not taken: K's touch did not start K afresh in the table (in the table %v, word1 hot %v, restored %d, bump's hot adds %d)", j >= 0, isHot(x1), res, nAdding)
	case leg.fill == "table" && ev == 0:
		t.Fatalf("path not taken: K's restore did not evict")
	case leg.fill == "lines" && leg.mode != "plain" && wt == 0:
		t.Fatalf("path not taken: K's history did not wait")
	case leg.placer && leg.fill == "lines" && !wasPlaced:
		t.Fatalf("path not taken: the placer did not place K")
	case waits:
		// the look again recalls the waiting history (from the room: testHookTaken)
		if leg.mode == "bg" && taken.Load() == 0 {
			t.Fatalf("path not taken: K's look again did not take K out of the wait room")
		}
	case leg.reclaim != "" && leg.reclaim != "before" && !retried:
		t.Fatalf("path not taken: K's touch did not look again through bump's hot add on its slot")
	}

	// What the leg reads.
	if x0&tsMask != hfTK {
		t.Errorf("K's last touch %d, want %d", x0&tsMask, hfTK)
	}
	for _, o := range over {
		t.Errorf("K reads %s", o)
	}
	if !dripOn {
		return
	}
	switch {
	case leg.reclaim == "":
		if class != "once" || !isHot(x1) || !mine {
			t.Errorf("the control did not restore K with its entry and one push (log %s, hot %v, K's %v)", class, isHot(x1), mine)
		}
	case waits || leg.reclaim == "before":
		if class != "fresh" {
			t.Errorf("K's log %s, want the touch afresh (fresh)", class)
		}
	default:
		if class != "once" {
			t.Errorf("K's log %s, want its history with the touch pushed once (once)", class)
		}
		if isHot(x1) {
			t.Errorf("K's slot still points to a hot entry after the look again")
		}
	}
}

// TestDripHotFreshRace (TF_DRIP_FRESH_ROUNDS=n, tfdrip): TestDripHotRestoreRace
// (W11) on hfHistory. Each round builds the filter with K's slot free (plain
// mode) and touches L 31 times at tL; then K's touch at hfTK and L's 32nd
// touch, its promotion, run at once from two goroutines, each after a random
// spin. K's add at hfTK leaves its entry's newest unit at 24, so L reclaims it
// whichever comes first. A round is classed by K's log (once, twice, fresh),
// whether L was promoted, and whether K's touch looked again through bump's
// hot add (in a round only K's look again adds to a hot entry): L first, K
// starts afresh; K first, its touch is counted in its entry, then L takes the
// entry; L in between, K looks again, which must not push the event again.
func TestDripHotFreshRace(t *testing.T) {
	rounds, _ := strconv.Atoi(os.Getenv("TF_DRIP_FRESH_ROUNDS"))
	if rounds <= 0 {
		t.Skip("set TF_DRIP_FRESH_ROUNDS=<rounds> to run")
	}
	if !dripOn {
		t.Skip("the rounds are classed by the drip log (tfdrip)")
	}
	if forceCT || forceBG {
		t.Skip("plain mode")
	}
	var looked atomic.Int64
	testHookHotAdding = func(int) { looked.Add(1) }
	defer func() { testHookHotAdding = nil }()
	rng := rand.New(rand.NewPCG(11, 12))
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
		f, k, l, w0, w1, tL := hotFreshSetup(t, false, false, "free")
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
			f.TouchAt(k, 0, hfTK)
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
		x0, _, _ := f.read(k, hfTK, -1)
		class := hfClass(f, dripOf(x0), w0)
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
		t.Logf("W11b race: %d of %d rounds: %s", classes[c], rounds, c)
	}
	if twice > 0 {
		t.Errorf("%d of %d rounds pushed K's event twice; first: %s", twice, rounds, first)
	}
}
