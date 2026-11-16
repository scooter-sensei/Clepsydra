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
		r = f.estimateAt(h, w1, w0&tsMask, dripOf(w0), now, window, w0&satBit != 0)
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
func (f *Filter) estimateAt(h, w1, last, drip, now, window uint64, sat bool) Result {
	if !isHot(w1) {
		// A register's estimate never fails and reads no id (viewOf
		// needs it only for a hot entry): the loop's first answer.
		r, _ := f.estimate(0, w1, last, drip, now, window, sat)
		return r
	}
	fp, tg := fingerprint(h), tag(h)
	id := f.stashID(h, fp, tg)
	for range 8 {
		if r, ok := f.estimate(id, w1, last, drip, now, window, sat); ok {
			return r
		}
		i, w0, nw1 := f.lookupStable(h, fp, tg, false)
		if i < 0 {
			break
		}
		if w0&tsMask >= last {
			drip = dripOf(w0)
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
// A register is read at the unit of the slot's last touch (word0), so the
// two must change unit together where a touch or a read can see the pair
// (claims D29). A touch inside the last touch's unit moves word0 lock-free
// and counts with bump, which reads the register against word0 as it is
// then. A touch that starts a unit moves word0 and the register together
// (realign): by one 16-byte CAS of both words, or else under word1 busy.
// Before, the register kept 4 bits of its unit and only a jump of 8 units or
// more went through realign (a touch that moved word0 first, the register
// after, then read a register at a lag by its stored unit; before realign,
// a second touch of the key between those two writes read the old register
// as the new unit's, and the first then aged that touch's event by the gap,
// or, past the horizon, reset it away: claims A21). A hot entry keeps its
// own full unit: its key's touches count lock-free, and one that starts a
// unit moves word0 only while word1 still points to the entry (stamp), so
// that a demotion, which puts a register at its unit there, and the touch
// can't cross; one that finds the slot past the horizon resets through
// realign. Before, it moved word0, then reset word1 by a CAS from the hot
// pointer; a touch after its word0 found the key live and added to the
// entry, which leaves word1 as it was, and the reset dropped that event
// (claims A30).
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
	pushed := false       // this touch's event is in its slot's drip log (W4c)
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
		if first && !ct && i0 >= 0 { // (slotWords of no slot is no match: skip the call)
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
		var old uint64
		var far, wait bool
		if nu > lu { // else farAhead's answer is (0, false, false): skip the call
			old, far, wait = f.farAhead(i, nu, lu, stale)
		}
		if wait {
			runtime.Gosched()
			continue // another touch is realigning the slot
		} else if far {
			if cpu.HasCAS2 && old>>tagShift == tg && !isHot(old) { // realign's usual case, without its call
				checkTick(now)
				w1, over, ok = f.realign2U(i, w0, old, tg, now, nu, lu)
			} else {
				w1, over, ok = f.realign(h, i, w0, old, tg, now, nu, lu)
			}
			ref = now
		} else {
			if nu > lu { // a hot key into a new unit: word0 moves only while word1 still points to the entry
				if testHookStamping != nil {
					testHookStamping()
				}
				if !f.stamp(i, w0, old, tg, now) {
					continue
				}
			} else if n0 := f.next0(w0, now); !(pushed && now <= last) && n0 != w0 && !atomic.CompareAndSwapUint64(&f.words[i], w0, n0) {
				continue // (a touch that pushed its event and starts over pushes it again only into a history from before it: W4c)
			}
			pushed = true
			if nu > lu && testHookStamped != nil {
				testHookStamped()
			}
			w1, ref, over, ok = f.bump(i, fpd, tg, now, nu, last, lu, stale)
		}
		if !ok {
			continue // the slot was handed to another key, or moved on, meanwhile; look again
		}
		if dripOn && ref > now && f.unitOf(ref) > nu {
			f.dripForget(i, fpd) // counted at a later unit than its own (W4b)
		}
		sat := w0&satBit != 0
		if over != sat {
			sat = f.markSat(i, fpd, over)
		}
		var r Result
		if wantCount {
			var drip uint64
			if dripOn { // the drip log the slot holds at max(now, ref)
				if x := atomic.LoadUint64(&f.words[i]); x>>47 == fpd && x&tsMask == max(now, ref) {
					drip = dripOf(x)
				}
			}
			r = f.estimateAt(h, w1, max(now, ref), drip, now, window, sat)
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
// to a later unit, and word1 (old) must move with it: a register always
// (realign), a hot entry's pointer only when the key is past the horizon (a
// reset; else the entry keeps its own full unit and the touch moves word0
// alone, by stamp). wait: word1 is busy, held by a writer that may publish a
// register at the old unit.
func (f *Filter) farAhead(i int, nu, lu int64, stale bool) (old uint64, far, wait bool) {
	if nu <= lu {
		return 0, false, false
	}
	old = atomic.LoadUint64(&f.words[i+1])
	if old>>tagShift == tagBusy {
		return old, false, true
	}
	if isHot(old) {
		return old, stale, false
	}
	return old, true, false
}

// stamp moves slot i's last touch (w0) to now, in a later unit, while word1
// still holds p, a hot entry's pointer of tag tg: by one 16-byte CAS of both
// words, or else under a hold of word1. A demotion meanwhile has put a
// register there, at word0's unit, which the last touch must not leave
// without it (demoteSlot pins that unit the same way). False: a word
// changed; look again.
func (f *Filter) stamp(i int, w0, p, tg, now uint64) bool {
	n0 := f.next0(w0, now)
	if cpu.HasCAS2 {
		return cpu.CAS2(&f.words[i], w0, p, n0, p)
	}
	m := f.moveOf(tg)
	m.begun.Add(1)
	defer m.done.Add(1)
	if !atomic.CompareAndSwapUint64(&f.words[i+1], p, busy) {
		return false
	}
	ok := atomic.CompareAndSwapUint64(&f.words[i], w0, n0)
	atomic.StoreUint64(&f.words[i+1], p)
	return ok
}

// realign is touch's count of a touch at now in a later unit than slot i's
// last touch (word0 w0) and register (old), or past the horizon of
// the slot of a hot key (old points to its entry, which it gives up and
// starts afresh): word0 moves to now and word1 to the register, advanced to
// now's unit with the touch counted. With CMPXCHG16B (amd64) both move by
// one CAS (realign2). Elsewhere word1 goes busy, word0 moves, then word1 is
// published; while word1 is busy, a lookup that misses the slot looks
// again: the hold counts as a move of the key's stripe. ok is false if the
// slot changed meanwhile.
func (f *Filter) realign(h uint64, i int, w0, old, tg, now uint64, nu, lu int64) (w1 uint64, over, ok bool) {
	checkTick(now)
	if old>>tagShift != tg || isHot(old) && age(now, w0&tsMask) <= f.horizon {
		return 0, false, false
	}
	if cpu.HasCAS2 {
		return f.realign2U(i, w0, old, tg, now, nu, lu)
	}
	m := f.moveOf(tg)
	m.begun.Add(1)
	defer m.done.Add(1)
	if !atomic.CompareAndSwapUint64(&f.words[i+1], old, busy) {
		return 0, false, false
	}
	if !atomic.CompareAndSwapUint64(&f.words[i], w0, f.next0(w0, now)) {
		atomic.StoreUint64(&f.words[i+1], old)
		return 0, false, false
	}
	if testHookStamped != nil {
		testHookStamped()
	}
	last := w0 & tsMask
	w1, over = f.nextRegU(old, tg, last, now, lu, nu, age(now, last) > f.horizon)
	if over {
		if pw, ok := f.promoteHeld(i, w0, old, tg, last, now); ok {
			w1, over = pw, false
		}
	}
	atomic.StoreUint64(&f.words[i+1], w1)
	if isHot(old) {
		f.slotDrop(f.slotID(uint64(i/wordsPerLine), w0, old), old) // a stale hot key's reset
	}
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
	return f.realign2U(i, w0, old, tg, now, f.unitOf(now), f.unitOf(w0&tsMask))
}

// realign2U is realign2 with now's unit nu and the last touch's, lu.
func (f *Filter) realign2U(i int, w0, old, tg, now uint64, nu, lu int64) (w1 uint64, over, ok bool) {
	last := w0 & tsMask
	n0 := f.next0(w0, now)
	w1, over = f.nextRegU(old, tg, last, now, lu, nu, age(now, last) > f.horizon)
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
	if isHot(old) {
		f.slotDrop(f.slotID(uint64(i/wordsPerLine), w0, old), old) // a stale hot key's reset
	}
	if testHookStamped != nil {
		testHookStamped()
	}
	return w1, over, true
}
