package temporalfilter

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// Races where a CAS from a value read earlier takes a slot that changed
// hands: a slot's word1 isn't unique to its occupant, since two keys with
// the same 12-bit tag have bit-identical first registers in one unit, and a
// holder reads word0 and word1 in two loads (claims A28-A30).

// keyAt: a hash with primary line line, fingerprint fp and tag tg, whose
// touches don't run the inline decay step.
func keyAt(f *Filter, line, fp, tg uint64) uint64 {
	for lo := (line<<36 + f.lines - 1) / f.lines; ; lo++ {
		h := fp<<48 | tg<<36 | lo&(1<<36-1)
		if f.primary(h) == line && h&decayEvery != 0 {
			return h
		}
	}
}

// twoLineFPs: n fingerprints from start on whose alternate line from line
// is another line.
func twoLineFPs(f *Filter, line, start uint64, n int) []uint64 {
	var out []uint64
	for fp := start; len(out) < n; fp++ {
		if f.alt(line, fp) != line {
			out = append(out, fp)
		}
	}
	return out
}

// slotsOf: the word indexes of the slots where h is visible.
func slotsOf(f *Filter, h uint64) []int {
	fp, tg := fingerprint(h), tag(h)
	p := f.primary(h)
	var out []int
	for li, l := range []uint64{p, f.alt(p, fp)} {
		if li == 1 && l == p {
			break
		}
		base := int(l * wordsPerLine)
		for s := 0; s < wordsPerLine; s += 2 {
			w0, w1 := atomic.LoadUint64(&f.words[base+s]), atomic.LoadUint64(&f.words[base+s+1])
			if w0>>48 == fp && (w0&dispBit != 0) == (li == 1) && w1>>tagShift == tg {
				out = append(out, base+s)
			}
		}
	}
	return out
}

func newMinuteFilter(t *testing.T, capacity int) *Filter {
	t.Helper()
	f, err := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond,
		Clock: func() uint64 { return 1 }})
	if err != nil {
		t.Fatal(err)
	}
	f.ct, f.bg = false, false // these races are in the plain paths' order of steps (TF_CT, TF_BG)
	return f
}

// during runs fn in a goroutine and waits for it up to 20 ms: it may wait
// for a hold its caller has; the channel is closed when it returns.
func during(fn func()) chan struct{} {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Millisecond):
	}
	return done
}

func waitFor(t *testing.T, done chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
	}
}

// TestCountAcrossSameTagHandOver (claims A29): K's touch at the tick of its
// last checks K's slot, then, before its count's CAS, an insert of K2,
// whose tag is K's, evicts K (one event, the lightest of the 8 slots) and
// takes its slot with a fresh register bit-identical to K's. The CAS from
// K's register then succeeds, into K2's: before, K's history went to the
// stash without the touch (case 12598's kind).
func TestCountAcrossSameTagHandOver(t *testing.T) {
	f := newMinuteFilter(t, 4)
	if f.lines != 2 {
		t.Fatalf("lines %d, want 2", f.lines)
	}
	const t0, T = 60_000, 0x5a5
	fps := twoLineFPs(f, 0, 0x1100, 9)
	for j := range 7 { // two events each
		h := keyAt(f, uint64(j%2), fps[j], 0x100+uint64(j))
		f.TouchAt(h, 0, t0)
		f.TouchAt(h, 0, t0)
	}
	k, k2 := keyAt(f, 1, fps[7], T), keyAt(f, 0, fps[8], T)
	f.TouchAt(k, 0, t0)
	ks := slotsOf(f, k)
	if len(ks) != 1 || f.Occupancy(t0) != 8 {
		t.Fatalf("setup: K in %v, occupancy %d", ks, f.Occupancy(t0))
	}
	i, kw1 := ks[0], atomic.LoadUint64(&f.words[ks[0]+1])
	fired, same := false, false
	testHookBumping = func(j int) {
		if fired || j != i {
			return
		}
		fired = true
		f.TouchAt(k2, 0, t0)
		sl := slotsOf(f, k2)
		same = len(sl) == 1 && sl[0] == i && atomic.LoadUint64(&f.words[i+1]) == kw1
	}
	defer func() { testHookBumping = nil }()
	f.TouchAt(k, 0, t0)
	testHookBumping = nil
	if !fired || !same {
		t.Fatalf("K2 did not take K's slot with K's word1 (%#x) during K's count", kw1)
	}
	if r := f.CountAt(k, f.horizon, t0); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K reads %+v after 2 touches", r)
	}
	if r := f.CountAt(k2, f.horizon, t0); r.Upper < 1 {
		t.Fatalf("K2 reads %+v after 1 touch", r)
	}
}

// TestCountThenEvictionCountsOnce (claims A29): right after K's count's
// CAS, an insert of K2, of another tag, evicts K (two events now, the
// lightest of the 8 slots) and takes its slot. word0, read again, is K2's,
// but the count went with K's history to the stash: K must not count it
// again (the slot holds another tag). The first fix B looked again on any
// change of word0, and K read one high (the racy stress's kind).
func TestCountThenEvictionCountsOnce(t *testing.T) {
	f := newMinuteFilter(t, 4)
	if f.lines != 2 {
		t.Fatalf("lines %d, want 2", f.lines)
	}
	const t0 = 60_000
	fps := twoLineFPs(f, 0, 0x1100, 9)
	for j := range 7 { // three events each
		h := keyAt(f, uint64(j%2), fps[j], 0x100+uint64(j))
		for range 3 {
			f.TouchAt(h, 0, t0)
		}
	}
	k, k2 := keyAt(f, 1, fps[7], 0x5a5), keyAt(f, 0, fps[8], 0x5a6)
	f.TouchAt(k, 0, t0)
	ks := slotsOf(f, k)
	if len(ks) != 1 || f.Occupancy(t0) != 8 {
		t.Fatalf("setup: K in %v, occupancy %d", ks, f.Occupancy(t0))
	}
	i := ks[0]
	fired, taken := false, false
	testHookBumped = func(j int) {
		if fired || j != i {
			return
		}
		fired = true
		f.TouchAt(k2, 0, t0)
		sl := slotsOf(f, k2)
		taken = len(sl) == 1 && sl[0] == i
	}
	defer func() { testHookBumped = nil }()
	f.TouchAt(k, 0, t0)
	testHookBumped = nil
	if !fired || !taken {
		t.Fatal("K2 did not take K's slot after K's count")
	}
	if r := f.CountAt(k, f.horizon, t0); r.Count != 2 || r.Upper != 2 {
		t.Fatalf("K reads %+v after 2 touches", r)
	}
}

// TestCountThenMoveCountsOnce (claims A29): right after K's count's CAS,
// an insert of N, whose two lines are full, moves K (the first entry of
// N's first line) to K's other line and takes its slot. The count went
// with the copy: K must not count it again.
func TestCountThenMoveCountsOnce(t *testing.T) {
	f := newMinuteFilter(t, 1000)
	const p, t0 = 7, 60_000
	fk := twoLineFPs(f, p, 0x7100, 1)[0]
	a := f.alt(p, fk)
	var fn uint64
	for _, c := range twoLineFPs(f, p, 0x7200, 64) {
		if b := f.alt(p, c); b != a {
			fn = c
			break
		}
	}
	b := f.alt(p, fn)
	k := keyAt(f, p, fk, 0x5a5)
	f.TouchAt(k, 0, t0)
	for j, l := range []uint64{p, p, p, b, b, b, b} { // N's lines full, K's other line empty
		h := keyAt(f, l, 0x7300+uint64(j), 0x100+uint64(j))
		f.TouchAt(h, 0, t0)
		f.TouchAt(h, 0, t0)
	}
	n := keyAt(f, p, fn, 0x5a6)
	ks := slotsOf(f, k)
	if len(ks) != 1 || ks[0] != int(p*wordsPerLine) || f.Occupancy(t0) != 8 {
		t.Fatalf("setup: K in %v, occupancy %d", ks, f.Occupancy(t0))
	}
	i := ks[0]
	fired, moved := false, false
	testHookBumped = func(j int) {
		if fired || j != i {
			return
		}
		fired = true
		f.TouchAt(n, 0, t0)
		sn, sk := slotsOf(f, n), slotsOf(f, k)
		moved = len(sn) == 1 && sn[0] == i && len(sk) == 1 && uint64(sk[0]/wordsPerLine) == a
	}
	defer func() { testHookBumped = nil }()
	f.TouchAt(k, 0, t0)
	testHookBumped = nil
	if !fired || !moved {
		t.Fatal("N did not move K out of its slot after K's count")
	}
	if r := f.CountAt(k, f.horizon, t0); r.Count != 2 || r.Upper != 2 {
		t.Fatalf("K reads %+v after 2 touches", r)
	}
}

// TestCountIntoHeldSameTagSlotLooksAgain (claims A29): K's count lands in
// K2's register, as in TestCountAcrossSameTagHandOver; then, before K's
// touch reads word0 again, a touch of K2 10 units on holds the slot
// (realign) for 20 ms. word0 is K2's and word1 busy: the touch must wait
// out the hold, which puts back a register of K's tag, and count afresh.
// Reading the busy word1 as another tag, K would read one low.
func TestCountIntoHeldSameTagSlotLooksAgain(t *testing.T) {
	f := newMinuteFilter(t, 4)
	if f.lines != 2 {
		t.Fatalf("lines %d, want 2", f.lines)
	}
	const t0, T = 60_000, 0x5a5
	fps := twoLineFPs(f, 0, 0x1100, 9)
	for j := range 7 { // two events each
		h := keyAt(f, uint64(j%2), fps[j], 0x100+uint64(j))
		f.TouchAt(h, 0, t0)
		f.TouchAt(h, 0, t0)
	}
	k, k2 := keyAt(f, 1, fps[7], T), keyAt(f, 0, fps[8], T)
	f.TouchAt(k, 0, t0)
	ks := slotsOf(f, k)
	if len(ks) != 1 || f.Occupancy(t0) != 8 {
		t.Fatalf("setup: K in %v, occupancy %d", ks, f.Occupancy(t0))
	}
	i, kw1 := ks[0], atomic.LoadUint64(&f.words[ks[0]+1])
	t1 := t0 + 10*f.unit
	stage := 0
	var inHold atomic.Bool
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	testHookStamped = func() { // K2's realign, word1 busy
		if inHold.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	testHookBumping = func(j int) {
		if stage == 0 && j == i {
			stage = 1
			f.TouchAt(k2, 0, t0)
			if sl := slotsOf(f, k2); len(sl) == 1 && sl[0] == i && atomic.LoadUint64(&f.words[i+1]) == kw1 {
				stage = 2
			}
		}
	}
	testHookBumped = func(j int) {
		if stage == 2 && j == i {
			stage = 3
			go func() {
				f.TouchAt(k2, 0, t1)
				close(done)
			}()
			<-held
			time.AfterFunc(20*time.Millisecond, func() { close(release) })
		}
	}
	defer func() { testHookStamped, testHookBumping, testHookBumped = nil, nil, nil }()
	f.TouchAt(k, 0, t0)
	testHookBumping, testHookBumped = nil, nil
	if stage != 3 {
		t.Fatalf("stage %d: K's count did not land in K2's slot", stage)
	}
	waitFor(t, done, "K2's touch")
	if r := f.CountAt(k, f.horizon, t0); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K reads %+v after 2 touches", r)
	}
}

// keyPair: K and L with primary line 7 (empty), different fingerprints,
// claim stripes and tags (the hooks run K's insert inside L's).
func keyPair(t *testing.T) (f *Filter, k, l, p uint64) {
	f, p = newMinuteFilter(t, 1000), 7
	return f, keyAt(f, p, twoLineFPs(f, p, 0x2200, 1)[0], 0x123), keyAt(f, p, twoLineFPs(f, p, 0x6600, 1)[0], 0x456), p
}

// tornOpenHold runs L's insert, which reads line p's first slot open (word0
// 0); K's insert then takes the slot; L reads its word1 (K's) and its
// install takes word1 busy from K's register, then fails on word0 and puts
// it back. during runs while L holds K's slot.
func tornOpenHold(t *testing.T, f *Filter, k, l, p uint64, t0 uint64, during func()) {
	t.Helper()
	s := int(p * wordsPerLine)
	stage := 0
	testHookOpenSeen = func(i int) {
		if stage == 0 && i == s {
			stage = 1
			f.TouchAt(k, 0, t0)
			stage = 2
		}
	}
	testHookInstalling = func(i int) {
		if stage == 2 && i == s {
			stage = 3
			during()
		}
	}
	defer func() { testHookOpenSeen, testHookInstalling = nil, nil }()
	f.TouchAt(l, 0, t0)
	if stage != 3 {
		t.Fatalf("stage %d: L's install did not take K's slot", stage)
	}
}

// TestTornOpenScanHoldHidesKey (claims A28): K's second touch runs while
// L holds K's slot. The hold must count on K's stripe (the tag of the word1
// it took): before, putOver counted none over a word0 of 0, and K's touch,
// missing K, put it in a second slot (case 3974's kind).
func TestTornOpenScanHoldHidesKey(t *testing.T) {
	f, k, l, p := keyPair(t)
	const t0 = 60_000
	var done chan struct{}
	tornOpenHold(t, f, k, l, p, t0, func() { done = during(func() { f.TouchAt(k, 0, t0) }) })
	waitFor(t, done, "K's touch")
	if sl := slotsOf(f, k); len(sl) != 1 {
		t.Fatalf("K in slots %v", sl)
	}
	if r := f.CountAt(k, f.horizon, t0); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K reads %+v after 2 touches", r)
	}
}

// TestTornOpenScanHoldRead (claims A28): a Count of K during L's hold must
// wait it out: before, it found K in no slot and answered absent (case
// 1220's kind).
func TestTornOpenScanHoldRead(t *testing.T) {
	f, k, l, p := keyPair(t)
	const t0 = 60_000
	var done chan struct{}
	var r Result
	tornOpenHold(t, f, k, l, p, t0, func() { done = during(func() { r = f.CountAt(k, f.horizon, t0) }) })
	waitFor(t, done, "K's count")
	if r.Upper < 1 && !r.Saturated {
		t.Fatalf("K read %+v during another key's hold, after 1 touch", r)
	}
}

// sweepOverJ: a filter whose line 7's first slot (word index s) holds J,
// tagged tgJ, touched once at sweepT0.
func sweepOverJ(t *testing.T, tgJ uint64) (f *Filter, p uint64, s int) {
	f, p = newMinuteFilter(t, 1000), 7
	j := keyAt(f, p, twoLineFPs(f, p, 0x3300, 1)[0], tgJ)
	f.TouchAt(j, 0, sweepT0)
	s = int(p * wordsPerLine)
	if sl := slotsOf(f, j); len(sl) != 1 || sl[0] != s {
		t.Fatalf("setup: J in %v", sl)
	}
	return f, p, s
}

// sweepHolds runs the decay sweep of line p at tS; when the hook named by
// seen fires on slot s, K's touch at tS takes the slot (J expired there),
// and ok checks the result; during runs while the sweep holds the slot.
func sweepHolds(t *testing.T, f *Filter, p uint64, s int, k, tS uint64, seen *func(int), ok func() bool, during func()) {
	t.Helper()
	stage := 0
	*seen = func(i int) {
		if stage == 0 && i == s {
			stage = 1
			f.TouchAt(k, 0, tS)
			if ok() {
				stage = 2
			}
		}
	}
	testHookSweeping = func() {
		if stage == 2 {
			stage = 3
			during()
		}
	}
	defer func() { *seen, testHookSweeping = nil, nil }()
	f.sweepLine(p, tS)
	if stage != 3 {
		t.Fatalf("stage %d: the sweep did not hold K's slot", stage)
	}
}

// TestSweepTornHoldHidesKey (claims A28): the sweep reads the slot's word0
// (J's, expired), K's insert takes the slot, the sweep reads word1 (K's)
// and holds K's slot. Counted on J's stripe, the hold let K's next touch
// put K in a second slot.
func TestSweepTornHoldHidesKey(t *testing.T) {
	f, p, s := sweepOverJ(t, 0x321)
	tS := sweepT0 + f.horizon + 2*f.unit
	k := keyAt(f, p, twoLineFPs(f, p, 0x4400, 1)[0], 0x654)
	var done chan struct{}
	sweepHolds(t, f, p, s, k, tS, &testHookSweepSeen,
		func() bool { sl := slotsOf(f, k); return len(sl) == 1 && sl[0] == s },
		func() { done = during(func() { f.TouchAt(k, 0, tS) }) })
	waitFor(t, done, "K's touch")
	if sl, r := slotsOf(f, k), f.CountAt(k, f.horizon, tS); len(sl) != 1 || r.Upper < 2 && !r.Saturated {
		t.Fatalf("K in slots %v, reads %+v after 2 touches", sl, r)
	}
}

// TestSweepSameTagHoldHidesKey (claims A28): the sweep reads both of J's
// words (expired, one event); K, whose tag is J's, takes the slot 32 units
// on with a fresh register bit-identical to J's, and the sweep's CAS from
// J's register holds K's slot. A consistent read of the two words (word0
// again after word1) would not catch this; counting on the tag's stripe
// does.
func TestSweepSameTagHoldHidesKey(t *testing.T) {
	const T = 0x2a7
	f, p, s := sweepOverJ(t, T)
	jw1 := atomic.LoadUint64(&f.words[s+1])
	tS := sweepT0 + 32*f.unit
	k := keyAt(f, p, twoLineFPs(f, p, 0x5500, 1)[0], T)
	var done chan struct{}
	sweepHolds(t, f, p, s, k, tS, &testHookSweepRead,
		func() bool {
			sl := slotsOf(f, k)
			return len(sl) == 1 && sl[0] == s && atomic.LoadUint64(&f.words[s+1]) == jw1
		},
		func() { done = during(func() { f.TouchAt(k, 0, tS) }) })
	waitFor(t, done, "K's touch")
	if sl, r := slotsOf(f, k), f.CountAt(k, f.horizon, tS); len(sl) != 1 || r.Upper < 2 && !r.Saturated {
		t.Fatalf("K in slots %v, reads %+v after 2 touches", sl, r)
	}
}

// TestStaleHotResetKeepsTouches (claims A30): K is hot. At t1 touch A
// finds K's slot past the horizon and resets it; touch B, after A's word0,
// finds K live. Before, A moved word0, then reset whatever word1 held by a
// CAS: B's event was gone whether B had added to the entry (word1
// unchanged: 300 events at sweepT0 stay in the entry's oldest bucket 25
// units on, so B doesn't demote it; case 4291's kind) or demoted it (50
// units on, the entry's events are past it, and B hands the register, with
// its event, back to the slot; case 3490's kind).
func TestStaleHotResetKeepsTouches(t *testing.T) {
	for _, gap := range []uint64{25, 50} {
		t.Run(fmt.Sprintf("gap%d", gap), func(t *testing.T) { staleHotReset(t, gap) })
	}
}

func staleHotReset(t *testing.T, gap uint64) {
	f := newMinuteFilter(t, 1000)
	k := keyAt(f, 7, twoLineFPs(f, 7, 0x6600, 1)[0], 0x321)
	for range 300 {
		f.TouchAt(k, 0, sweepT0)
	}
	if sl := slotsOf(f, k); len(sl) != 1 || !isHot(atomic.LoadUint64(&f.words[sl[0]+1])) {
		t.Fatalf("setup: K in %v, not hot", sl)
	}
	t1 := sweepT0 + gap*f.unit
	fired := false
	var done chan struct{}
	testHookStamped = func() {
		if !fired {
			fired = true
			done = during(func() { f.TouchAt(k, 0, t1) })
		}
	}
	defer func() { testHookStamped = nil }()
	f.TouchAt(k, 0, t1)
	testHookStamped = nil
	if !fired {
		t.Fatal("A did not move K's last touch")
	}
	waitFor(t, done, "B's touch")
	if r := f.CountAt(k, f.horizon, t1); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K reads %+v after 2 touches at t1", r)
	}
}
