package temporalfilter

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Many workers send the same keys past the register's capacity at once, so
// promotions race each other, while a fill to 98% slot load relocates and
// evicts them: every key must end with its exact count, in one slot.
func TestHotPromotionUnderRaces(t *testing.T) {
	rounds := 40
	if testing.Short() {
		rounds = 6
	}
	const keys, workers, per, at = 128, 8, 200, 60_000 // at: a multiple of 8 units, where a 60 s window covers every bucket whole
	for round := range rounds {
		hotN := 2 * keys
		if os.Getenv("TF_HOTOFF") != "" {
			hotN = -1
		}
		f, err := New(Config{Capacity: 4000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: hotN})
		if err != nil {
			t.Fatal(err)
		}
		salt := uint64(round) << 40
		hs := make([]uint64, keys)
		for i := range hs {
			hs[i] = mix64(salt + uint64(i))
			for range 3 { // heavier than the fill, so eviction never picks them over a one-event key
				f.CheckAndTouchAt(hs[i], 0, at)
			}
		}
		var wg sync.WaitGroup
		var sent atomic.Int64
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := range per * keys / 2 {
					f.CheckAndTouchAt(hs[(n+w*7)%keys], 0, at)
					sent.Add(1)
				}
			}()
		}
		fill := int(float64(f.Slots())*0.98) - keys
		for c := range uint64(fill) {
			f.CheckAndTouchAt(mix64(salt+1<<36+c), 0, at)
		}
		wg.Wait()
		total := 0
		for i, h := range hs {
			fp, tg := fingerprint(h), tag(h)
			p := f.primary(h)
			// As lookups match: undisplaced in the primary line, displaced in
			// the alternate (findAny would also count another key that has
			// this identity but is homed at the alternate line).
			if a, b := f.find(p, fp<<1, tg), f.find(f.alt(p, fp), fp<<1|1, tg); a >= 0 && b >= 0 {
				t.Fatalf("round %d key %d in two slots", round, i)
			}
			r := f.CountAt(h, 60_000, at)
			if r.Saturated && hotN > 0 {
				t.Fatalf("round %d key %d Saturated with %d hot entries free", round, i, 2*keys-f.HotInUse())
			}
			total += r.Count
		}
		if want := int(sent.Load()) + 3*keys; total != want {
			t.Fatalf("round %d: counted %d of %d events (promoted %d, refused %d, relocations %d, evictions %d)",
				round, total, want, f.Promoted(), f.HotRefused(), f.Relocations(), f.Evictions())
		}
	}
}

// A key that bursts past the register is promoted; once it slows to a
// trickle its entry is handed back as a register, and its counts stay exact
// (against an exact log) before, across and after both moves.
func TestHotDemotion(t *testing.T) {
	now := uint64(1)
	f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond,
		Clock: func() uint64 { return now }})
	if err != nil {
		t.Fatal(err)
	}
	h := mix64(7)
	var log []uint64
	check := func(when string) {
		for _, w := range []uint64{1_000, 5_000, 15_000, 60_000} {
			exact := 0
			for _, ts := range log {
				if now-ts <= w {
					exact++
				}
			}
			r := f.CountAt(h, w, now)
			if !r.Saturated && r.Upper < exact {
				t.Fatalf("%s, window %ds: exact %d, got %+v", when, w/1000, exact, r)
			}
		}
	}
	for range 200 { // a burst in one tick: promoted
		f.TouchAt(h, 0, now)
		log = append(log, now)
	}
	if f.Promoted() != 1 || f.HotInUse() != 1 {
		t.Fatalf("burst: promoted %d, in use %d", f.Promoted(), f.HotInUse())
	}
	check("after the burst")
	for ; now < 3*60_000; now += 1000 { // then one event a second
		f.TouchAt(h, 0, now)
		log = append(log, now)
		check("trickle")
	}
	if f.Demoted() != 1 || f.HotInUse() != 0 {
		t.Fatalf("trickle: demoted %d, in use %d", f.Demoted(), f.HotInUse())
	}
	if r := f.CountAt(h, 60_000, now); r.Count != 60 || r.Saturated {
		t.Fatalf("after demotion, 60 s window: %+v, want Count 60", r)
	}
}

// With fewer hot entries than heavy keys, the rest saturate as without hot
// entries (flagged, so the fail-closed contract holds), HotRefused counts
// them, and entries whose history has aged out are taken by new keys.
func TestHotTableExhaustion(t *testing.T) {
	now := uint64(1)
	f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: 4,
		Clock: func() uint64 { return now }})
	if err != nil {
		t.Fatal(err)
	}
	const heavy = 10
	sent := make([]int, heavy)
	for ; now < 60_000; now += 10 { // 100 events per second per key
		for k := range heavy {
			f.TouchAt(mix64(uint64(k)), 0, now)
			sent[k]++
		}
	}
	exact, sat := 0, 0
	for k := range heavy {
		r := f.CountAt(mix64(uint64(k)), 60_000, now)
		switch {
		case r.Saturated:
			sat++
		case r.Upper < sent[k] || r.Count < sent[k]*99/100 || r.Count > sent[k]*101/100:
			t.Errorf("key %d unflagged: sent %d, got %+v", k, sent[k], r)
		default:
			exact++
		}
	}
	if exact != 4 || sat != heavy-4 || f.HotRefused() == 0 || f.HotInUse() != 4 {
		t.Fatalf("exact %d saturated %d (want 4, %d); refused %d, in use %d", exact, sat, heavy-4, f.HotRefused(), f.HotInUse())
	}
	// A horizon and two units of silence later the entries are dead: new
	// heavy keys take them.
	now += 60_000 + 2*2500 + 1
	before := f.Promoted()
	for range 40 {
		for k := range 4 {
			f.TouchAt(mix64(uint64(100+k)), 0, now)
		}
	}
	if got := f.Promoted() - before; got != 4 {
		t.Fatalf("after the horizon %d of 4 new heavy keys were promoted (in use %d, refused %d)", got, f.HotInUse(), f.HotRefused())
	}
	for k := range 4 {
		if r := f.CountAt(mix64(uint64(100+k)), 60_000, now); r.Count != 40 || r.Saturated {
			t.Errorf("new heavy key %d: %+v, want Count 40", k, r)
		}
	}
}

// TestRestoreAfterHotEntryReclaimed: a hot key K is evicted to the stash,
// its stash entry pointing at its hot entry. A promotion at a clock more
// than a horizon past K's newest event reclaims the entry for another key.
// K's next touch, at a clock behind that one but within a horizon of K's
// last touch, restores K's history: the entry is no longer K's. That touch
// must still be counted (it went nowhere, its slot left pointing at the
// other key's entry, and K's next touch started afresh: Upper 1 for 2).
func TestRestoreAfterHotEntryReclaimed(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("ConstantTime and Background inserts don't install on the call path")
	}
	f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: 1})
	if err != nil {
		t.Fatal(err)
	}
	const t0 = 60_000
	k, l := mix64(1), mix64(2)
	for range 32 { // past the register's 31 events in a unit: promoted
		f.TouchAt(k, 0, t0)
	}
	if f.Promoted() != 1 {
		t.Fatalf("K not promoted (%d)", f.Promoted())
	}
	// Evict K as claimAt does: its words to the stash, its slot to another key (here: freed).
	fp, tg := fingerprint(k), tag(k)
	i := f.locate(k, fp, tg)
	w0, w1 := f.words[i], f.words[i+1]
	if !isHot(w1) {
		t.Fatal("K's slot holds no hot entry")
	}
	f.stashPut(f.stashID(k, fp, tg), w0, w1, t0)
	f.words[i], f.words[i+1] = 0, 0
	tL := t0 + f.horizon + 3*f.unit // K's entry is past the horizon here
	for range 32 {
		f.TouchAt(l, 0, tL)
	}
	if f.Promoted() != 2 {
		t.Fatalf("L not promoted (%d): K's entry not reclaimed", f.Promoted())
	}
	tK := t0 + f.horizon - 1 // K's clock: behind L's, within a horizon of K's last touch
	f.TouchAt(k, 0, tK)
	f.TouchAt(k, 0, tK)
	if r := f.CountAt(k, f.horizon, tK); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K reads %+v after two touches, want Upper >= 2", r)
	}
}

// hotReclaimSetup: a filter with one hot entry; K promoted at t0 and
// evicted as claimAt does (words to the stash, slot freed); both of K's
// lines then full of entries live until t0 + horizon. reclaim promotes
// another key at a clock more than a horizon past K's newest event, which
// takes K's entry. tK is a clock behind that one, within a horizon of K's
// last touch.
func hotReclaimSetup(t *testing.T, bg bool) (f *Filter, k, tK uint64, reclaim func()) {
	t.Helper()
	f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: 1,
		Clock: func() uint64 { return 1 }, Background: bg})
	if err != nil {
		t.Fatal(err)
	}
	f.bgManual = bg
	const t0 = 60_000
	k = mix64(1)
	for range 32 { // past the register's 31 events in a unit: promoted
		f.TouchAt(k, 0, t0)
	}
	fp, tg := fingerprint(k), tag(k)
	i := f.locate(k, fp, tg)
	if f.Promoted() != 1 || i < 0 || !isHot(f.words[i+1]) {
		t.Fatalf("K not promoted (%d) or not in the table", f.Promoted())
	}
	f.stashPut(f.stashID(k, fp, tg), f.words[i], f.words[i+1], t0)
	f.words[i], f.words[i+1] = 0, 0
	p := f.primary(k)
	for _, line := range [2]uint64{p, f.alt(p, fp)} {
		for j := uint64(1 << 40); countLine(f, line, t0) < 4; j++ {
			if h := mix64(j); f.primary(h) == line {
				f.TouchAt(h, 0, t0)
			}
		}
	}
	l, tL := mix64(2), t0+f.horizon+3*f.unit
	reclaim = func() {
		for range 32 {
			f.TouchAt(l, 0, tL)
		}
		if f.Promoted() != 2 {
			t.Errorf("L not promoted (%d): K's entry not reclaimed", f.Promoted())
		}
	}
	return f, k, t0 + f.horizon - 1, reclaim
}

// TestHotEntryReclaimedDuringRestore: as TestRestoreAfterHotEntryReclaimed,
// but the entry is reclaimed while K's restoring touch is in its claim
// (relocating an entry of K's full lines to make room), after the check
// that its entry is still K's. K's touch must still be counted.
func TestHotEntryReclaimedDuringRestore(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("ConstantTime and Background inserts don't search on the call path")
	}
	f, k, tK, reclaim := hotReclaimSetup(t, false)
	fired := false
	hook := func() {
		if !fired {
			fired = true
			reclaim()
		}
	}
	testHookCopied, testHookVictim = hook, hook
	defer func() { testHookCopied, testHookVictim = nil, nil }()
	f.TouchAt(k, 0, tK)
	if !fired {
		t.Fatal("K's restore neither relocated nor evicted")
	}
	f.TouchAt(k, 0, tK)
	if r := f.CountAt(k, f.horizon, tK); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K reads %+v after two touches, want Upper >= 2", r)
	}
}

// TestHotEntryReclaimedBeforeWait: with Background, K's restoring touch
// finds its lines full and waits in the wait room, after its entry was
// reclaimed. It must wait with this touch counted, not the dead entry
// (with which every retry would take K out of the room and wait again).
func TestHotEntryReclaimedBeforeWait(t *testing.T) {
	if forceCT {
		t.Skip("Background has no effect with ConstantTime")
	}
	f, k, tK, reclaim := hotReclaimSetup(t, true)
	reclaim()
	done := make(chan struct{})
	go func() {
		f.TouchAt(k, 0, tK)
		f.TouchAt(k, 0, tK)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("K's touches did not return")
	}
	if f.Waiting() != 1 {
		t.Errorf("%d keys waiting, want K", f.Waiting())
	}
	if r := f.CountAt(k, f.horizon, tK); r.Upper < 2 && !r.Saturated {
		t.Fatalf("K reads %+v after two touches, want Upper >= 2", r)
	}
}
