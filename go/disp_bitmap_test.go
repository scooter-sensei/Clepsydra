package temporalfilter

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dispBitsCover reports a line holding a published displaced entry whose
// disp bit is clear (quiescent table), or -1.
func dispBitsCover(f *Filter) int {
	for l := uint64(0); l < f.lines; l++ {
		base := int(l * wordsPerLine)
		for s := 0; s < wordsPerLine; s += 2 {
			w0, w1 := f.words[base+s], f.words[base+s+1]
			if w0&dispBit != 0 && w1>>tagShift != tagBusy && f.disp[l>>6]&(1<<(l&63)) == 0 {
				return int(l)
			}
		}
	}
	return -1
}

// TestDispBitsCoverDisplaced: after fills, relocations and evictions, every
// line holding a displaced entry has its disp bit set, and sweeps keep it so.
func TestDispBitsCoverDisplaced(t *testing.T) {
	for _, load := range []float64{0.5, 0.9, 0.98, 1.2} {
		f := newTest(t, 4000, time.Minute)
		n := int(float64(f.Slots()) * load)
		for i := range uint64(n) {
			f.CheckAndTouchAt(mix64(i+1<<40), 0, 60_000)
		}
		if l := dispBitsCover(f); l >= 0 {
			t.Fatalf("load %.2f: line %d holds a displaced entry with its bit clear", load, l)
		}
		for l := uint64(0); l < f.lines; l++ {
			f.sweepLine(l, 60_000)
		}
		if l := dispBitsCover(f); l >= 0 {
			t.Fatalf("load %.2f, after a sweep: line %d holds a displaced entry with its bit clear", load, l)
		}
	}
}

// TestDispClearRacingPlacement: a sweep finds line A's disp bit set and no
// displaced entry in A (one held once is gone) and clears the bit. Before it
// looks at A again, K's entry is in A, displaced and published, by a writer
// whose setDisp found the bit still set (it ran before the clear), so the
// bit is clear while K is there. A lookup of K in that window must read A
// (the clear is under way: the sequence check), and the sweep must leave
// the bit set.
func TestDispClearRacingPlacement(t *testing.T) {
	f := newTest(t, 1000, time.Minute)
	const t0 = 60_000
	var k, p, a uint64
	for j := uint64(1); ; j++ {
		h := mix64(j)
		p = f.primary(h)
		if a = f.alt(p, fingerprint(h)); a != p {
			k = h
			break
		}
	}
	fp, tg := fingerprint(k), tag(k)
	base := int(a * wordsPerLine)
	// A held a displaced entry (some other key's) that has since gone.
	if !f.install(base, 0, 0, 0x4321<<48|dispBit|t0, 0x321<<tagShift|1) {
		t.Fatal("install failed")
	}
	f.abandon(base)
	if !f.mayDisp(a) {
		t.Fatal("A's bit not set by a displaced install")
	}
	kw1, _ := f.nextReg(0, tg, t0, t0, true)
	fired, found := false, false
	testHookDispCleared = func(line uint64) {
		if line != a || fired {
			return
		}
		fired = true
		atomic.StoreUint64(&f.words[base+2], fp<<48|dispBit|t0) // install's word0, setDisp done before the clear,
		atomic.StoreUint64(&f.words[base+3], kw1)               // then the publication
		found = f.ContainsAt(k, f.horizon, t0)
	}
	defer func() { testHookDispCleared = nil }()
	f.sweepDisp(a)
	if !fired {
		t.Fatal("the sweep did not clear A's bit")
	}
	if !found {
		t.Error("a lookup during the clear missed K in A")
	}
	if !f.mayDisp(a) || !f.ContainsAt(k, f.horizon, t0) {
		t.Error("the sweep left A's bit clear with K displaced in it")
	}
}

// TestDispBitmapSweepRace: keys never touched again are read continuously
// while inserts keep relocating them and a sweeper keeps clearing the disp
// bits of lines with no displaced entry. The table is filled to 80% with
// live keys, then churned (20 batches of 15% of the slots, keys at a clock
// a horizon behind, which the sweeps' clock expires and clears, displaced
// entries and all), then filled to 98%. No read may miss a key that is
// there, and no line may be left with a displaced entry and a clear bit.
func TestDispBitmapSweepRace(t *testing.T) {
	rounds := 20
	if testing.Short() {
		rounds = 4
	}
	const capN, keys, readers = 4000, 256, 4
	var bad, reads, clears, evs atomic.Int64
	for round := range rounds {
		f := newTest(t, capN, time.Minute)
		at := 4 * f.horizon
		salt := uint64(round) << 40
		hs := make([]uint64, keys)
		for i := range hs {
			hs[i] = mix64(salt + uint64(i))
			f.CheckAndTouchAt(hs[i], 0, at)
		}
		var stop atomic.Bool
		var wg sync.WaitGroup
		for w := range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := w; !stop.Load(); i++ {
					h := hs[i%keys]
					reads.Add(1)
					if !f.ContainsAt(h, 60_000, at) {
						if bad.Add(1) <= 5 {
							t.Logf("round %d key %d read absent", round, i%keys)
						}
					}
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				for l := uint64(0); l < f.lines && !stop.Load(); l++ {
					f.sweepLine(l, at)
				}
			}
		}()
		c := uint64(0)
		fill := func(n int, now uint64) {
			for range n {
				c++
				f.CheckAndTouchAt(mix64(salt+1<<36+c), 0, now)
			}
		}
		fill(int(float64(f.Slots())*0.80)-keys, at)
		for range 20 {
			fill(f.Slots()*15/100, at-f.horizon-1)
			for l := uint64(0); l < f.lines; l++ {
				f.sweepLine(l, at)
			}
		}
		fill(int(float64(f.Slots())*0.18), at)
		evs.Add(int64(f.evictions.Load()))
		stop.Store(true)
		wg.Wait()
		for i := range f.dispClr {
			clears.Add(int64(f.dispClr[i].begun.Load()))
		}
		if l := dispBitsCover(f); l >= 0 {
			bad.Add(1)
			t.Logf("round %d: line %d holds a displaced entry with its bit clear", round, l)
		}
	}
	t.Logf("%d reads, %d clears, %d evictions, %d misses", reads.Load(), clears.Load(), evs.Load(), bad.Load())
	if clears.Load() == 0 {
		t.Fatal("no disp bit was cleared: the race was not run")
	}
	if bad.Load() > 0 {
		t.Fatalf("%d reads missed a key that was there", bad.Load())
	}
}

// TestDispBitmapSkipRate (TF_EXP=1): the share of absent keys whose
// alternate line's disp bit is clear, at four slot loads, no sweeps.
func TestDispBitmapSkipRate(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	const absent = 1 << 20
	for _, load := range []float64{0.50, 0.85, 0.90, 0.94} {
		f := newTest(t, 1<<20, time.Minute)
		n := int(float64(f.Slots()) * load)
		for i := range uint64(n) {
			f.CheckAndTouchAt(mix64(i), 0, 1000)
		}
		skip, setLines := 0, 0
		for l := uint64(0); l < f.lines; l++ {
			if f.disp[l>>6]&(1<<(l&63)) != 0 {
				setLines++
			}
		}
		for i := range uint64(absent) {
			h := mix64(1<<40 + i)
			p := f.primary(h)
			if f.find(p, fingerprint(h)<<1, tag(h)) >= 0 {
				continue
			}
			if !f.mayDisp(f.alt(p, fingerprint(h))) {
				skip++
			}
		}
		t.Logf("skiprate load %.2f slots %d keys %d lines-with-bit %.4f absent-skip %d/%d = %.4f",
			load, f.Slots(), n, float64(setLines)/float64(f.lines), skip, absent, float64(skip)/absent)
	}
}
