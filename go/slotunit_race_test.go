package temporalfilter

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestSlotUnitRaces (claims D29): a register stores no unit and is read at
// word0's, so the three writers that put a register beside a word0 they did
// not move, or move word0 beside a register they did not write, are pinned
// to word0's unit (SlotUnit.tla). Each subtest drives the interleaving the
// model's plain variant breaks, by 16-byte CAS and by hold, and requires
// the slot (both words) and every answer to be what a sequential order of
// the same touches leaves:
//
//   - demote: B's touch into unit u1 demotes K's hot entry while A's touch
//     into u2 > u1 has already stamped word0 into u2 (B's demotion must
//     fail, and A's demote at u2). A demotion by a CAS of word1 alone
//     leaves a register at u1 beside a last touch at u2.
//   - stamp: A reads word0 (at u1) and the pointer, then E, a touch at
//     word0's own tick, demotes the entry at u1 without moving word0; A's
//     stamp into u2 must fail and A realign. A stamp by a CAS of word0
//     alone moves word0 past the register.
//   - unpoint: K's entry is reclaimed; S's touch into u1 finds it gone and
//     resets the pointer to a register, after T's touch into u2 stamped
//     word0 (S's reset must be at u2). A reset by a CAS of word1 alone
//     puts S's register, with its onset, at u1.
//
// The undone guards were each checked to fail their subtest.
func TestSlotUnitRaces(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(t *testing.T)
	}{{"demote", slotUnitDemote}, {"stamp", slotUnitStamp}, {"unpoint", slotUnitUnpoint}} {
		t.Run(c.name, func(t *testing.T) { realignModes(t, c.run) })
	}
}

// slotUnitTicks: K's burst at t0 (unit 24), touches at t1 (unit 34, 700
// ticks in, onset 4) and t2 (unit 47, within a horizon of t0).
const (
	slotUnitT1 = sweepT0 + 10*2_500 + 700
	slotUnitT2 = sweepT0 + 23*2_500 + 1_900
)

// slotUnitKey: a filter and K, promoted by a burst of 40 touches at t0
// (its entry at unit 24); with reclaim, the entry is then taken from K as
// the hot table's reclaim does.
func slotUnitKey(t *testing.T, reclaim bool) (*Filter, uint64) {
	t.Helper()
	f := newMinuteFilter(t, 1000)
	k := keyAt(f, 7, twoLineFPs(f, 7, 0x6600, 1)[0], 0x321)
	for range 40 {
		f.TouchAt(k, 0, sweepT0)
	}
	sl := slotsOf(f, k)
	if len(sl) != 1 || f.Promoted() != 1 || !isHot(atomic.LoadUint64(&f.words[sl[0]+1])) {
		t.Fatalf("setup: K in %v, promoted %d, not hot", sl, f.Promoted())
	}
	if reclaim {
		e := &f.hot.Load().e[hotIdx(atomic.LoadUint64(&f.words[sl[0]+1]))]
		ver := e.lock()
		e.id.Store(0)
		e.unlock(ver)
	}
	return f, k
}

// slotUnitState: K's slot words and its answers at every window, at t2 and
// later, with the hot table's counters.
func slotUnitState(f *Filter, k uint64) string {
	var b strings.Builder
	sl := slotsOf(f, k)
	fmt.Fprintf(&b, "slots %v", len(sl))
	for _, i := range sl {
		fmt.Fprintf(&b, " words %#x %#x", atomic.LoadUint64(&f.words[i]), atomic.LoadUint64(&f.words[i+1]))
	}
	for _, at := range []uint64{slotUnitT2, slotUnitT2 + f.unit, slotUnitT2 + 10*f.unit} {
		for _, w := range []uint64{500, 1_000, 2_500, 5_000, 15_000, 30_000, 60_000} {
			fmt.Fprintf(&b, "\n  at %d window %5d: %+v", at, w, f.CountAt(k, w, at))
		}
	}
	fmt.Fprintf(&b, "\n  promoted %d demoted %d", f.Promoted(), f.Demoted())
	return b.String()
}

func slotUnitCompare(t *testing.T, raced, want string) {
	t.Helper()
	if raced != want {
		t.Fatalf("after the race:\n%s\nsequential:\n%s", raced, want)
	}
}

func slotUnitDemote(t *testing.T) {
	ref, k := slotUnitKey(t, false)
	ref.TouchAt(k, 0, slotUnitT1) // B: demotes at u1
	if ref.Demoted() != 1 {
		t.Fatalf("setup: B's touch did not demote K (demoted %d)", ref.Demoted())
	}
	ref.TouchAt(k, 0, slotUnitT2) // A: realigns to u2
	want := slotUnitState(ref, k)

	f, _ := slotUnitKey(t, false)
	var stamped, adding atomic.Int32
	aStamped, release, aDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	testHookStamped = func() {
		if stamped.Add(1) == 2 { // A's stamp, into u2: hold A there
			close(aStamped)
			<-release
		}
	}
	testHookHotAdding = func(int) {
		if adding.Add(1) == 1 { // B, before it adds at u1: A stamps first
			go func() {
				f.TouchAt(k, 0, slotUnitT2)
				close(aDone)
			}()
			awaitHook(aStamped)
		}
	}
	defer func() { testHookStamped, testHookHotAdding = nil, nil }()
	f.TouchAt(k, 0, slotUnitT1) // B
	close(release)
	waitFor(t, aDone, "A's touch")
	testHookStamped, testHookHotAdding = nil, nil
	if stamped.Load() < 2 {
		t.Fatal("A did not stamp during B's touch")
	}
	slotUnitCompare(t, slotUnitState(f, k), want)
}

func slotUnitStamp(t *testing.T) {
	ref, k := slotUnitKey(t, false)
	ref.TouchAt(k, 0, slotUnitT1) // E (as D's stamp and E's count): demotes at u1
	if ref.Demoted() != 1 {
		t.Fatalf("setup: the touch at t1 did not demote K (demoted %d)", ref.Demoted())
	}
	ref.TouchAt(k, 0, slotUnitT2) // A: realigns to u2
	ref.TouchAt(k, 0, slotUnitT1) // D's count: behind the last touch, counted at it
	want := slotUnitState(ref, k)

	f, _ := slotUnitKey(t, false)
	var stamping, stamped atomic.Int32
	testHookStamped = func() {
		if stamped.Add(1) == 1 { // D has stamped word0 into u1: A's touch
			f.TouchAt(k, 0, slotUnitT2)
		}
	}
	testHookStamping = func() {
		if stamping.Add(1) == 2 { // A has read word0 and the pointer: E demotes, word0 unmoved
			f.TouchAt(k, 0, slotUnitT1)
		}
	}
	defer func() { testHookStamped, testHookStamping = nil, nil }()
	f.TouchAt(k, 0, slotUnitT1) // D
	testHookStamped, testHookStamping = nil, nil
	if stamping.Load() < 2 || f.Demoted() != 1 {
		t.Fatalf("the interleaving did not happen (stamps %d, demoted %d)", stamping.Load(), f.Demoted())
	}
	slotUnitCompare(t, slotUnitState(f, k), want)
}

func slotUnitUnpoint(t *testing.T) {
	ref, k := slotUnitKey(t, true)
	ref.TouchAt(k, 0, slotUnitT2) // T: resets the pointer at u2
	ref.TouchAt(k, 0, slotUnitT1) // S: behind the last touch, counted at it
	want := slotUnitState(ref, k)

	f, _ := slotUnitKey(t, true)
	var stamped, adding atomic.Int32
	tStamped, release, tDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	testHookStamped = func() {
		if stamped.Add(1) == 2 { // T's stamp, into u2: hold T there
			close(tStamped)
			<-release
		}
	}
	testHookHotAdding = func(int) {
		if adding.Add(1) == 1 { // S has stamped word0 into u1 and read it: T stamps
			go func() {
				f.TouchAt(k, 0, slotUnitT2)
				close(tDone)
			}()
			awaitHook(tStamped)
		}
	}
	defer func() { testHookStamped, testHookHotAdding = nil, nil }()
	f.TouchAt(k, 0, slotUnitT1) // S
	close(release)
	waitFor(t, tDone, "T's touch")
	testHookStamped, testHookHotAdding = nil, nil
	if stamped.Load() < 2 {
		t.Fatal("T did not stamp during S's touch")
	}
	slotUnitCompare(t, slotUnitState(f, k), want)
}

// awaitHook waits for another touch to reach its hook, up to 5 s (the
// subtest then fails on what did not happen).
func awaitHook(c chan struct{}) {
	select {
	case <-c:
	case <-time.After(5 * time.Second):
	}
}
