package temporalfilter

import (
	"testing"
	"time"
)

// TestSlotExitFreesHotEntry (claims A40): a hot key's history that its slot
// discards past the horizon gives its entry up at once: by a touch's reset
// (realign, with its 16-byte CAS and with its hold), by the sweep, and by an
// insert over the expired occupant. Before, the entry stayed held, and
// HotInUse counted it, until a promotion found no free entry and reclaimed
// it.
func TestSlotExitFreesHotEntry(t *testing.T) {
	const t0 = 60_000
	promoted := func(t *testing.T) (*Filter, uint64) {
		t.Helper()
		f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: 4,
			Clock: func() uint64 { return 1 }})
		if err != nil {
			t.Fatal(err)
		}
		k := mix64(1)
		for range 32 { // past the register's 31 events in a unit: promoted
			f.TouchAt(k, 0, t0)
		}
		if f.Promoted() != 1 || f.HotInUse() != 1 {
			t.Fatalf("K not promoted (promoted %d, in use %d)", f.Promoted(), f.HotInUse())
		}
		return f, k
	}
	t.Run("reset", func(t *testing.T) {
		realignModes(t, func(t *testing.T) {
			f, k := promoted(t)
			now := t0 + f.horizon + f.unit // K's history is past the horizon: its touch starts afresh
			f.TouchAt(k, 0, now)
			if n := f.HotInUse(); n != 0 {
				t.Fatalf("hot entries in use after K's reset: %d, want 0", n)
			}
			if r := f.CountAt(k, f.horizon, now); r.Count != 1 || r.Saturated {
				t.Fatalf("K reads %+v after its reset, want count 1", r)
			}
		})
	})
	t.Run("sweep", func(t *testing.T) {
		f, k := promoted(t)
		i := f.locate(k, fingerprint(k), tag(k))
		f.sweepLine(uint64(i/wordsPerLine), t0+f.horizon+1)
		if f.words[i] != 0 {
			t.Fatal("K's expired slot not swept")
		}
		if n := f.HotInUse(); n != 0 {
			t.Fatalf("hot entries in use after the sweep: %d, want 0", n)
		}
	})
	t.Run("insert", func(t *testing.T) {
		f, k := promoted(t)
		i := f.locate(k, fingerprint(k), tag(k))
		line, tL := uint64(i/wordsPerLine), t0+f.horizon+1
		// Fill K's line's other slots, live at tL, so a key of this line takes K's.
		for s := 0; s < wordsPerLine; s += 2 {
			if j := int(line)*wordsPerLine + s; j != i {
				f.words[j], f.words[j+1] = 0x7777<<48|tL, 0x777<<tagShift|1
			}
		}
		for j := uint64(1 << 40); ; j++ {
			if h := mix64(j); f.primary(h) == line && fingerprint(h) != fingerprint(k) {
				f.TouchAt(h, 0, tL)
				break
			}
		}
		if f.locate(k, fingerprint(k), tag(k)) == i {
			t.Fatal("K's expired slot not taken")
		}
		if n := f.HotInUse(); n != 0 {
			t.Fatalf("hot entries in use after an insert over K's expired slot: %d, want 0", n)
		}
	})
}

// TestExpiredStashWayKeepsSharedEntry (claims A40): an expired stash way of
// K's that points to the hot entry K's live slot uses (the entry was
// reclaimed past the horizon and given to K again) keeps the entry when
// another key's history takes the way: closing it there would close K's
// live entry, and K's next touch would start afresh. Only a slot's own exits
// close its entry.
func TestExpiredStashWayKeepsSharedEntry(t *testing.T) {
	if forceCT || forceBG {
		t.Skip("ConstantTime and Background inserts don't install on the call path")
	}
	f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Hot: 4,
		Clock: func() uint64 { return 1 }})
	if err != nil {
		t.Fatal(err)
	}
	const t0 = 200_000
	k := mix64(1)
	for range 32 {
		f.TouchAt(k, 0, t0)
	}
	fp, tg := fingerprint(k), tag(k)
	i, id := f.locate(k, fp, tg), f.stashID(k, fp, tg)
	if f.Promoted() != 1 || i < 0 || !isHot(f.words[i+1]) {
		t.Fatal("K not promoted")
	}
	// K's old history in the stash, long expired, pointing to K's entry.
	f.stashPut(id, fp<<48|1_000, f.words[i+1], 1_000)
	set, _ := f.stashSet(id)
	tL := uint64(t0 + 10)
	for j := uint64(1 << 40); ; j++ { // another key of the set takes the expired way
		h := mix64(j)
		oid := f.stashID(h, fingerprint(h), tag(h))
		if s, _ := f.stashSet(oid); &s[0] != &set[0] || oid == id {
			continue
		}
		w1, _ := f.nextReg(0, tag(h), tL, tL, true)
		f.stashPut(oid, fingerprint(h)<<48|tL, w1, tL)
		break
	}
	if n := f.HotInUse(); n != 1 {
		t.Fatalf("hot entries in use: %d, want 1 (K's live one)", n)
	}
	f.TouchAt(k, 0, tL)
	if r := f.CountAt(k, f.horizon, tL); r.Upper < 33 {
		t.Fatalf("K reads %+v after another key took its expired stash way, want Upper >= 33", r)
	}
}
