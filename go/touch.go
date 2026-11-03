package temporalfilter

import (
	"runtime"
	"sync/atomic"
	"time"

	"github.com/scooter-sensei/clepsydra/go/internal/cpu"
)

// count is CountAt; i0, if not -1, is a slot where a lookup just found h
// (warm), as for touch. In ConstantTime its work is padded (ctBegin,
// ctEnd), and sketchLive reads the overflow table whatever it holds.
func (f *Filter) count(h, window, now uint64, i0 int) Result {
	var t0, s time.Time
	var pad time.Duration
	if f.ct {
		t0, pad = f.ctBegin(opCount, now)
		s = time.Now()
	}
	w0, w1, src := f.read(h, now, i0)
	var r Result
	if src != 0 && age(now, w0&tsMask) <= f.horizon {
		r = f.estimateAt(h, w1, w0&tsMask, now, window, w0&satBit != 0)
		r.Seen = r.Count > 0
	} else {
		r.Degraded = f.lostRecently(now)
	}
	if f.sketchLive(now) {
		f.addSketch(&r, h, window, now)
	}
	if f.ct {
		f.ctEnd(opCount, h, t0, s, pad)
	}
	return r
}

// estimateAt is estimate for key h, whose register (or hot entry) is in w1
// and whose lastTouch is last. If w1 points to an entry the key gave up
// meanwhile (a touch demoted it), the key's slot is looked up again,
// without its stripe (the caller may hold it). If it can't be found, the
// answer fails closed.
func (f *Filter) estimateAt(h, w1, last, now, window uint64, sat bool) Result {
	fp, tg := fingerprint(h), tag(h)
	id := f.stashID(h, fp, tg)
	for range 8 {
		if r, ok := f.estimate(id, w1, last, now, window, sat); ok {
			return r
		}
		i, w0, nw1 := f.lookupStable(h, fp, tg, false)
		if i < 0 {
			break
		}
		w1, last, sat = nw1, max(last, w0&tsMask), w0&satBit != 0
	}
	return Result{Count: 1, Upper: 1, Saturated: true}
}

// read returns h's slot words from the table or, if it was evicted, the
// stash, and where they came from (0: neither). i0, if not -1, is a slot
// where a lookup just found h (warm): the first look reads it instead of
// scanning, and a miss is concluded only from a scan.
func (f *Filter) read(h, now uint64, i0 int) (w0, w1 uint64, src int) {
	fp, tg := fingerprint(h), tag(h)
	if !f.ct {
		// lookupStable, inline: one scan whether the key is found or not.
		// The stash gate is read between the scan and the stability check:
		// a waiting key that leaves the wait room for the table counts as a
		// move of its stripe (placeRoomKey), so if no move began by the
		// check, the key was still waiting when the gate was read.
		m := f.moveOf(tg)
		for try := 0; ; try++ {
			b, d := m.begun.Load(), m.done.Load()
			i, fpd := i0, fp<<1 // warm finds a key undisplaced in its primary line
			if try > 0 || i < 0 {
				i, fpd = f.locateAt(h, fp, tg)
			}
			if w0, w1, ok := f.slotWords(i, fpd, tg); ok {
				return w0, w1, srcTable
			}
			may := f.stashMay(h, fp, tg, now)
			if i < 0 && b == d && m.begun.Load() == b {
				if !may {
					return 0, 0, 0
				}
				break
			}
			if try > 0 {
				runtime.Gosched()
			}
		}
	}
	// Under the key's stripe: an eviction moves the key to the stash while
	// holding it, so the key is found in one of the two.
	mu := &f.claimMu[h>>(64-claimStripeBits)]
	mu.Lock()
	defer mu.Unlock()
	m := f.moveOf(tg)
	for {
		b, d := m.begun.Load(), m.done.Load()
		i, w0, w1 := f.lookupStable(h, fp, tg, true)
		s0, s1, src := f.recall(h, fp, tg, now, false)
		if src == srcRetry {
			continue
		}
		if i >= 0 {
			return w0, w1, srcTable
		}
		if src == 0 && !f.settled(m, b, d) {
			continue
		}
		return s0, s1, src
	}
}

// lookupStable returns the index and words of h's slot, or -1 if h has
// none, reading both lines if both (ConstantTime). A lookup scans one line
// and then the other, so an entry that a relocation moves into the line
// already scanned and out of the other in between is missed by both scans;
// an eviction likewise takes a key out of the table before its history
// reaches the stash. A miss is trusted only if no such move of a key on h's
// tag stripe (moveOf) began or ended around the scan. Moves never wait on a
// lookup, so the retry loop ends.
func (f *Filter) lookupStable(h, fp, tg uint64, both bool) (i int, w0, w1 uint64) {
	m := f.moveOf(tg)
	for {
		b, d := m.begun.Load(), m.done.Load()
		var fpd uint64
		if both {
			i, fpd = f.locateBoth(h, fp, tg)
		} else {
			i, fpd = f.locateAt(h, fp, tg)
		}
		if i >= 0 {
			if w0, w1, ok := f.slotWords(i, fpd, tg); ok {
				return i, w0, w1
			}
		} else if b == d && m.begun.Load() == b {
			return -1, 0, 0
		}
		runtime.Gosched()
	}
}

// sketchLive: the overflow table may hold a count at now. ConstantTime
// reads it whatever it holds.
func (f *Filter) sketchLive(now uint64) bool {
	if !f.cms {
		g := f.gh.Load()
		return g != nil && (f.ct || now <= g.until.Load())
	}
	sk := f.sk.Load()
	return sk != nil && (f.ct || now <= sk.until.Load())
}

// addSketch adds h's events in the overflow table to r, the answer from
// the table or stash. The overflow holds only histories the stash dropped,
// and a key that comes back starts a fresh slot, so the two never count the
// same event. Upper takes the overflow's bound, Count the key's own entry
// (the count-min sketch: the part that clears the noise), and Degraded is
// set when there is one: the key likely has a dropped history.
func (f *Filter) addSketch(r *Result, h, window, now uint64) {
	var s Result
	var ok bool
	if !f.cms {
		s, ok = f.gh.Load().estimate(f.stashID(h, fingerprint(h), tag(h)), min(window, f.horizon), now)
	} else {
		s, ok = f.sk.Load().estimate(f.stashID(h, fingerprint(h), tag(h)), min(window, f.horizon), now, f.ct)
	}
	if !ok {
		return
	}
	r.Count += s.Count
	r.Upper += s.Upper
	r.Saturated = r.Saturated || s.Saturated
	r.Seen = r.Seen || s.Seen
	r.Degraded = r.Degraded || s.Count > 0
}

// touch is TouchAt. i0, if not -1, is a slot where a lookup just found h
// (warm): its first look starts there if the slot still holds h. i0
// ctInside is touchCT's call: every attempt takes h's stripe and makes the
// same lookups whatever h's state (both lines, the stash), then counts in
// h's slot or inserts it, the stripe held throughout.
//
// The count is the slot protocol's one copy, inline here: called as a
// function it cost the cache-resident hit 1.05x and the touch batch 1.08x
// (placement-randomized, 6 layouts, cycles/op).
//
// A register keeps 4 bits of its unit and is read against the slot's last
// touch (word0), so word0 must never run 8 units or more ahead of it where
// a touch or a read can see the pair. A touch that moves word0 first and
// the register after keeps that only for a jump of under 8 units from the
// register's own unit: it then moves word0 lock-free and counts with bump,
// which reads the register against word0 as it is then. A touch 8 or more
// units past the register's unit (a key idle a third of the horizon, or
// past it) moves word0 and the register together under word1 busy
// (realign). Before realign, such a touch moved word0 first: a second touch
// of the key between its two writes read the old register as the new
// unit's, and the first then aged that touch's event by the gap, or, past
// the horizon, reset it away (claims A21). A hot entry keeps its own full
// unit: its key's touches count lock-free, except one that finds the slot
// past the horizon, whose reset goes through realign too. Before, it moved
// word0, then reset word1 by a CAS from the hot pointer; a touch after its
// word0 found the key live and added to the entry, which leaves word1 as it
// was, and the reset dropped that event (claims A30).
func (f *Filter) touch(h, window, now uint64, wantCount bool, i0 int) Result {
	ct := i0 == ctInside
	if !ct {
		if h&decayEvery == 0 {
			f.decayStep(now, 1)
		}
		if f.ct {
			return f.touchCT(h, window, now, wantCount)
		}
	}
	fp, tg := fingerprint(h), tag(h)
	var held *paddedMutex // ConstantTime: h's stripe, while held
	for first := true; ; first = false {
		if held != nil { // released between attempts: a claim that fails on another stripe's TryLock must not hold its own while it retries
			held.Unlock()
			held = nil
		}
		i, fpd := -1, uint64(0) // fpd: what the lookup matched in the slot (word0>>47)
		if ct {
			held = &f.claimMu[h>>(64-claimStripeBits)]
			held.Lock()
			var w0 uint64
			i, w0, _ = f.lookupStable(h, fp, tg, true)
			s0, s1, src := f.recall(h, fp, tg, now, true)
			if i < 0 {
				r := f.miss(h, fp, tg, window, now, wantCount, s0, s1, src)
				if r.Count < 0 {
					continue
				}
				held.Unlock()
				if f.sketchLive(now) {
					f.addSketch(&r, h, window, now)
				}
				return r
			}
			if src == srcStash { // a stale copy: the table's entry is newer
				f.stashPut(f.stashID(h, fp, tg), s0, s1, now)
			}
			fpd = w0 >> 47
		}
		if first && !ct {
			if _, _, ok := f.slotWords(i0, fp<<1, tg); ok { // warm finds a key undisplaced in its primary line
				i, fpd = i0, fp<<1
			}
		}
		if i < 0 {
			i, fpd = f.locateAt(h, fp, tg)
		}
		if i < 0 {
			// Miss: serialize first touches of the same key so two racers
			// can't each claim a slot (which would split its count). The
			// lock covers only this path; hits never take it. A tick past
			// MaxTick panics before it: a caller that recovers keeps a
			// usable filter.
			checkTick(now)
			mu := &f.claimMu[h>>(64-claimStripeBits)]
			mu.Lock()
			m := f.moveOf(tg)
			b, d := m.begun.Load(), m.done.Load()
			var w0 uint64
			if i, w0, _ = f.lookupStable(h, fp, tg, false); i < 0 {
				s0, s1, src := f.recall(h, fp, tg, now, true)
				if src == srcRetry || src == 0 && !f.settled(m, b, d) {
					mu.Unlock()
					continue
				}
				r := f.miss(h, fp, tg, window, now, wantCount, s0, s1, src)
				mu.Unlock()
				if r.Count < 0 {
					continue
				}
				if f.sketchLive(now) {
					f.addSketch(&r, h, window, now)
				}
				return r
			}
			mu.Unlock() // another first touch got there first: count as a hit
			fpd = w0 >> 47
		}
		// The count: slot i held h when it was found.
		w0 := atomic.LoadUint64(&f.words[i])
		if w0>>47 != fpd {
			continue // slot was reclaimed, or handed to another key, between find and load; retry
		}
		last := w0 & tsMask
		stale := age(now, last) > f.horizon
		var w1, ref uint64
		var over, ok bool
		nu, lu := f.unitOf(now), f.unitOf(last)
		if old, far, wait := f.farAhead(i, nu, lu, stale); wait {
			runtime.Gosched()
			continue // another touch is realigning the slot
		} else if far {
			w1, over, ok = f.realign(h, i, w0, old, tg, now)
			ref = now
		} else {
			if now > last && !atomic.CompareAndSwapUint64(&f.words[i], w0, w0&^tsMask|now) {
				continue
			}
			if nu > lu && testHookStamped != nil {
				testHookStamped()
			}
			w1, ref, over, ok = f.bump(i, fpd, tg, now, nu, last, lu, stale)
		}
		if !ok {
			continue // the slot was handed to another key, or moved on, meanwhile; look again
		}
		sat := w0&satBit != 0
		if over != sat {
			sat = f.markSat(i, fpd, over)
		}
		var r Result
		if wantCount {
			r = f.estimateAt(h, w1, max(now, ref), now, window, sat)
		}
		r.Seen = !stale && age(now, last) <= window
		if held != nil {
			held.Unlock()
		}
		// CheckAndTouch asks only for Seen, which addSketch can set but never
		// clear: a hit already Seen skips the overflow read. ConstantTime
		// reads the overflow table whatever it holds.
		if f.sketchLive(now) && (wantCount || !r.Seen || f.ct) {
			f.addSketch(&r, h, window, now)
		}
		return r
	}
}

// ctInside, as touch's i0: the call is touchCT's.
const ctInside = -2

// farAhead: a touch in unit nu would move slot i's last touch (in unit lu)
// 8 or more units past its register's unit (old: the register read, or a
// hot entry's word1, which counts lock-free whatever the gap). wait: word1
// is busy, so its unit can't be read; a touch that moved word0 on now could
// leave it 8 units past the register the holder publishes.
func (f *Filter) farAhead(i int, nu, lu int64, stale bool) (old uint64, far, wait bool) {
	if nu <= lu {
		return 0, false, false
	}
	old = atomic.LoadUint64(&f.words[i+1])
	if old>>tagShift == tagBusy {
		return old, false, true
	}
	if isHot(old) { // its own full unit; but a reset must hold the slot (realign)
		return old, stale, false
	}
	return old, nu-regEpoch(old, lu) > epochMask/2, false
}

// realign is touch's count of a touch at now 8 or more units past the unit of
// slot i's register (old, read against word0 w0), or past the horizon of
// the slot of a hot key (old points to its entry, which it gives up and
// starts afresh): word0 moves to now and word1 to the register, advanced to
// now's unit with the touch counted. With CMPXCHG16B (amd64) both move by
// one CAS (realign2). Elsewhere word1 goes busy, word0 moves, then word1 is
// published; while word1 is busy, a lookup that misses the slot looks
// again: the hold counts as a move of the key's stripe. ok is false if the
// slot changed meanwhile.
func (f *Filter) realign(h uint64, i int, w0, old, tg, now uint64) (w1 uint64, over, ok bool) {
	checkTick(now)
	if old>>tagShift != tg || isHot(old) && age(now, w0&tsMask) <= f.horizon {
		return 0, false, false
	}
	if cpu.HasCAS2 {
		return f.realign2(i, w0, old, tg, now)
	}
	m := f.moveOf(tg)
	m.begun.Add(1)
	defer m.done.Add(1)
	if !atomic.CompareAndSwapUint64(&f.words[i+1], old, busy) {
		return 0, false, false
	}
	if !atomic.CompareAndSwapUint64(&f.words[i], w0, w0&^tsMask|now) {
		atomic.StoreUint64(&f.words[i+1], old)
		return 0, false, false
	}
	if testHookStamped != nil {
		testHookStamped()
	}
	last := w0 & tsMask
	w1, over = f.nextReg(old, tg, last, now, age(now, last) > f.horizon)
	if over {
		if pw, ok := f.promoteHeld(i, w0, old, tg, last, now); ok {
			w1, over = pw, false
		}
	}
	atomic.StoreUint64(&f.words[i+1], w1)
	f.slotDrop(f.slotID(uint64(i/wordsPerLine), w0, old), old) // a stale hot key's reset
	return w1, over, true
}

// realign2 is realign by one 16-byte CAS of both words from w0, old (claims
// A33): word1 never goes busy, so no lookup looks again and no move is
// counted, and a touch whose two loads straddle the CAS fails its own CAS
// from what it read. The CAS compares word0 too: a slot handed meanwhile to
// a key of old's tag can hold old bit for bit (promote). A promotion takes
// its entry first and publishes it by the same CAS, and closes it again if
// the CAS fails.
func (f *Filter) realign2(i int, w0, old, tg, now uint64) (w1 uint64, over, ok bool) {
	last := w0 & tsMask
	n0 := w0&^tsMask | now
	w1, over = f.nextReg(old, tg, last, now, age(now, last) > f.horizon)
	var idx, id uint64
	if over {
		if pw, x, y := f.hotEntryFor(i, w0, old, tg, last, now); pw != 0 {
			w1, over, idx, id = pw, false, x, y
		}
	}
	if !cpu.CAS2(&f.words[i], w0, old, n0, w1) {
		if idx != 0 {
			f.hotClose(idx, id) // never published
		}
		return 0, false, false
	}
	if idx != 0 {
		f.promoted.Add(1)
	}
	f.slotDrop(f.slotID(uint64(i/wordsPerLine), w0, old), old) // a stale hot key's reset
	if testHookStamped != nil {
		testHookStamped()
	}
	return w1, over, true
}
