package temporalfilter

import (
	"runtime"
	"sync/atomic"
)

const (
	// relocDepth bounds the cuckoo path an insert into two full lines may
	// take before evicting.
	relocDepth = 3
	// relocBudget caps the alternate lines one insert may scan. The usable
	// part above the first 8 shrinks with pressure (see claim).
	relocBudget   = 500
	pressureShift = 16
	pressureOne   = 1 << pressureShift
)

// noStripe, as claimAt's stripe: the caller holds no claim stripe.
const noStripe = 1 << claimStripeBits

// miss installs h, absent from the table, with this touch counted: a fresh
// register, or its evicted history from the stash. Count < 0 means it lost a
// race for a slot and the caller should look again. The caller holds h's
// claim stripe, so no other first touch of h runs meanwhile.
func (f *Filter) miss(h, fp, tg, window, now uint64, wantCount bool, s0, s1 uint64, src int) Result {
	var r Result
	// Taken out of the wait room (srcRoom): its move ends once it is placed.
	room := src == srcRoom
	w1, _ := f.nextReg(0, tg, now, now, true) // first event, published with the key
	last, sat := now, uint64(0)
	hotRestore := false
	if src != 0 && age(now, s0&tsMask) <= f.horizon && !f.hotGone(f.stashID(h, fp, tg), s1) {
		last = s0 & tsMask
		if isHot(s1) { // it keeps its hot entry; the touch is counted once it is installed
			w1, hotRestore = s1, true
		} else {
			var over bool
			w1, over = f.nextReg(s1, tg, last, now, false)
			if over || s0&satBit != 0 && hot(w1) {
				sat = satBit
			}
		}
		r.Seen = age(now, last) <= window
		r.Restored = s0&deferMark == 0 // a key that waited for a slot was never evicted
	} else if f.lostRecently(now) {
		r.Degraded = true
	}
	// A recalled history keeps its last touch even when this caller's clock is
	// behind it (the register counted this touch in that unit too): stamped
	// with the older clock, a later window starting between the two would
	// miss the history's newest event.
	stamp := max(now, last)
	mode := claimFull
	if f.ct || f.bg {
		mode = claimDefer
	}
	ci := f.claim(h, fp, w1, now, stamp, sat, mode)
	if ci == claimWait {
		// No free slot: wait for one, in the stash (ConstantTime; pads place
		// it) or the wait room (Background; the placer goroutine places it).
		// If more than one key per 16 wait-room entries waits, the placer
		// isn't keeping up: search now.
		if (f.ct || f.deferred.Load() <= int64(f.roomN*stashWays/16)) && f.stashWait(f.stashID(h, fp, tg), fp<<48|sat|stamp, w1, now) {
			// An evicted history that waits came back all the same: this
			// touch is Restored (and counted once; the touch that installs
			// it later recalls a waiting entry, which isn't).
			f.waited.Add(1)
			if f.bg {
				f.bgKick()
			}
		} else { // its set is full: search (and evict) now rather than drop a history
			ci = f.claim(h, fp, w1, now, stamp, sat, claimFull)
		}
	}
	if ci == -1 && (src == srcStash || room) { // put it back for the retry
		f.putBack(f.stashID(h, fp, tg), s0, s1, now)
	}
	if room { // in the table, or back in the room or the stash
		f.moveOf(tg).done.Add(1)
	}
	if ci == -1 {
		return Result{Count: -1}
	}
	if hotRestore && !f.hotAdd(w1, f.stashID(h, fp, tg), now, nil) {
		// The entry was reclaimed since hotGone: look again. The key's slot
		// (or wait) now points to another key's entry; the next look finds
		// that and counts this touch afresh (bump's reset, or hotGone).
		return Result{Count: -1}
	}
	if src == srcStash && r.Restored {
		f.restored.Add(1)
	}
	if !wantCount {
		return r
	}
	e := f.estimateAt(h, w1, stamp, now, window, sat != 0)
	e.Seen, e.Restored, e.Degraded = r.Seen, r.Restored, r.Degraded
	return e
}

// claim installs fp in an empty or expired slot of the primary line, then the
// alternate line. If all eight are live it tries to relocate one of them to
// its own alternate line (one cuckoo step). Only then does it evict, taking
// the slot with the fewest events in the horizon (oldest on ties), so a spray
// of fresh keys cannot flush a repeat offender. w1 is the new key's tagged
// register, published together with its word0. Returns word0 index, or -1
// if it lost a race for a slot and the caller should look the key up again
// (a racing writer may have completed it). The caller holds the key's claim
// stripe. With mode claimDefer (ConstantTime and Background) it searches
// and evicts nothing: with no free slot it returns claimWait, and the key
// waits for a slot (miss). The entry's last touch is stamp; now is the
// caller's clock (expiry, eviction scores).
func (f *Filter) claim(h, fp, w1, now, stamp, sat uint64, mode int) int {
	p := f.primary(h)
	return f.claimAt([2]uint64{p, f.alt(p, fp)},
		[2]uint64{fp<<48 | sat | stamp, fp<<48 | dispBit | sat | stamp}, w1, now, h>>(64-claimStripeBits), mode, nil)
}

// claimAt modes: free slot, path or eviction (claimFull); free slot only,
// else claimWait (claimDefer).
const (
	claimFull = iota
	claimDefer

	claimWait = -2
)

// claimAt is claim for an entry with lines and words mine (primary,
// alternate), on claim stripe stripe, which the caller holds (noStripe: none).
// put installs the entry (nil: install; see putNew).
func (f *Filter) claimAt(lines, mine [2]uint64, w1, now, stripe uint64, mode int, put putFunc) int {
	for {
		for li, line := range lines {
			base := int(line * wordsPerLine)
			for s := 0; s < wordsPerLine; s += 2 {
				w0 := atomic.LoadUint64(&f.words[base+s])
				if w0 == 0 || age(now, w0&tsMask) > f.horizon {
					if testHookOpenSeen != nil {
						testHookOpenSeen(base + s)
					}
					if o1 := atomic.LoadUint64(&f.words[base+s+1]); f.putOver(put, base+s, w0, o1, mine[li], w1) {
						if w0 != 0 {
							f.slotDrop(f.slotID(line, w0, o1), o1) // the expired occupant's
						}
						return base + s
					}
					return -1
				}
			}
		}
		if mode == claimDefer {
			return claimWait
		}
		// Iterative deepening: all short paths before any long one. The deep
		// budget shrinks as recent searches fail, so an overfull table falls
		// back to cheap eviction instead of searching in vain.
		p := f.pressure.Load()
		budget := 8 + int(uint64(relocBudget-8)*(pressureOne-uint64(p))>>pressureShift)
		f.warmAlts(lines)
		for depth := 0; depth <= relocDepth; depth++ {
			for li, line := range lines {
				base := int(line * wordsPerLine)
				for s := 0; s < wordsPerLine; s += 2 {
					if f.relocate(base+s, line, mine[li], w1, now, depth, &budget, nil, put) {
						f.pressure.Store(p - p>>3) // EWMA toward "searches succeed"
						return base + s
					}
				}
			}
		}
		f.pressure.Store(p - p>>3 + pressureOne>>3) // EWMA toward "searches fail"
		victim, vli, victimScore, victimW0, victimW1 := -1, 0, uint64(1<<64-1), uint64(0), uint64(0)
		for li, line := range lines {
			base := int(line * wordsPerLine)
			for s := 0; s < wordsPerLine; s += 2 {
				w0 := atomic.LoadUint64(&f.words[base+s])
				ts := w0 & tsMask
				vw1 := atomic.LoadUint64(&f.words[base+s+1])
				// Read after the slot: a copy goes in only once its move is
				// counted, so a key whose move is under way is left alone. A
				// busy word1 (a hold, or a move's hidden copy) can't be taken.
				if mv := f.moveOf(vw1 >> tagShift); vw1>>tagShift == tagBusy || mv.begun.Load() != mv.done.Load() {
					continue
				}
				score := min(f.mass(f.slotID(line, w0, vw1), vw1, ts, now), 1<<16-1)<<48 | ts
				if score < victimScore {
					victim, vli, victimScore, victimW0, victimW1 = base+s, li, score, w0, vw1
				}
			}
		}
		if victim < 0 {
			return -1 // every candidate is mid-move: look again
		}
		if testHookVictim != nil {
			testHookVictim()
		}
		// The victim's history goes to the stash under the victim's own claim
		// stripe (its hash's top bits are its fp's), so a first touch of the
		// victim, which takes that stripe, finds it either still here or
		// already stashed. TryLock: the caller holds its own stripe.
		vs := victimW0 >> (64 - claimStripeBits)
		if vs != stripe {
			mu := &f.claimMu[vs]
			if !mu.TryLock() {
				return -1
			}
			defer mu.Unlock()
		}
		// A move of the victim's stripe that began after the scan may have
		// copied the victim, and its hand-off would then fail: back off (see
		// moveCount).
		m := f.moveOf(victimW1 >> tagShift)
		m.evicting.Add(1)
		defer m.evicting.Add(-1)
		if m.begun.Load() != m.done.Load() {
			return -1
		}
		// Between the install and the stash a lock-free lookup finds the
		// victim in neither; the move count tells it to look again.
		m.begun.Add(1)
		defer m.done.Add(1)
		if !f.putNew(put, victim, victimW0, victimW1, mine[vli], w1) {
			return -1
		}
		f.evictions.Add(1)
		f.stashPut(f.slotID(lines[vli], victimW0, victimW1), victimW0, victimW1, now)
		return victim
	}
}

// putFunc installs a new key's entry in slot i (word index) if the slot still
// holds w0/w1, as install does; a Background placement's put also takes the
// key out of the wait room as it goes in.
type putFunc func(i int, w0, w1, mine0, mine1 uint64) bool

// putNew installs a new key's entry with put, or install if put is nil.
func (f *Filter) putNew(put putFunc, i int, w0, w1, mine0, mine1 uint64) bool {
	if put != nil {
		return put(i, w0, w1, mine0, mine1)
	}
	return f.install(i, w0, w1, mine0, mine1)
}

// putOver is putNew over slot i's expired occupant w0 (0: none), whose
// word1 read w1. The hold (word1 busy until word0 is taken) counts as a
// move of the stripe of w1's tag, whoever has w1 by then: a touch at a
// clock that finds the occupant live may move word0 on during it (the
// install then puts word1 back), and its count, finding word1 busy, looks
// the key up again; that lookup, or another touch's, must not miss the
// slot and put the key in a second one (claims A27). w0 and w1 are two
// reads: a key installed between them has w1, and the hold, counted on
// w0's stripe (or on none, over a word0 of 0), hid it from its own touches
// and reads (claims A28).
func (f *Filter) putOver(put putFunc, i int, w0, w1, mine0, mine1 uint64) bool {
	if w1 == 0 || w1>>tagShift == tagBusy { // free (word1 is 0 only once word0 is), or held: no hold to count
		return f.putNew(put, i, w0, w1, mine0, mine1)
	}
	m := f.moveOf(w1 >> tagShift) // whoever holds w1 when the install takes it
	m.begun.Add(1)
	ok := f.putNew(put, i, w0, w1, mine0, mine1)
	m.done.Add(1)
	return ok
}

// warmAlts loads one word of the alternate line of every entry in lines
// without using it: the misses are in flight together, and the search that
// follows finds the lines on their way or cached.
func (f *Filter) warmAlts(lines [2]uint64) {
	f.warmLine(lines[0])
	f.warmLine(lines[1])
}

// warmLine loads one word of the alternate line of each entry in line.
// Four misses at once is about what this host keeps in flight (claims S42);
// a whole level of the search at once (32) is not.
func (f *Filter) warmLine(line uint64) {
	var sink uint64
	base := int(line * wordsPerLine)
	for s := 0; s < wordsPerLine; s += 2 {
		if w0 := atomic.LoadUint64(&f.words[base+s]); w0 != 0 {
			sink += atomic.LoadUint64(&f.words[f.alt(line, w0>>48)*wordsPerLine])
		}
	}
	runtime.KeepAlive(sink)
}

// install hands slot i (word index), if it still holds w0/w1, to the entry
// mine0/mine1. word1 goes busy first: a reader of the old occupant then
// misses rather than matching a half-written slot, and a late writer to the
// old register (bump) backs off. Nobody else writes a busy word1, so it is
// then set with plain stores.
func (f *Filter) install(i int, w0, w1, mine0, mine1 uint64) bool {
	if w1>>tagShift == tagBusy || !atomic.CompareAndSwapUint64(&f.words[i+1], w1, busy) {
		return false
	}
	if testHookInstalling != nil {
		testHookInstalling(i)
	}
	if !atomic.CompareAndSwapUint64(&f.words[i], w0, mine0) {
		atomic.StoreUint64(&f.words[i+1], w1) // lost the slot; put word1 back
		return false
	}
	if mine0&dispBit != 0 { // displaced: the line's disp bit before the entry is published (mayDisp)
		f.setDisp(uint64(i) / wordsPerLine)
	}
	atomic.StoreUint64(&f.words[i+1], mine1)
	return true
}

// relocate moves the entry at word index i (in line) to its alternate line
// and installs mine0/mine1 in its place. If that line is full it first
// relocates one of its entries, up to depth more steps (a bounded cuckoo
// path).
//
// The copy is installed hidden (word1 busy): no lookup finds it and no
// touch counts in it. The hand-off takes the source only if its words are
// still those copied (install's CASes), and only then is the copy's word1
// published, so a key is never live in two slots. Before, the copy was
// visible from its install: a touch that had found the source a moment
// before counted there while reads found the copy (they scan its line
// first), and a copy of words read before the key was evicted came back as
// a second history whose touches the failed hand-off then dropped (claims
// A23). If the source changed (a touch reached it, or the key was evicted)
// the move is abandoned and the copy, which counted nothing, cleared.
//
// The move counts in its key's stripe (f.moves) from before the copy is
// installed to its publication or clearing: a lookup that misses meanwhile
// (between the hand-off and the publication the key is in no slot a lookup
// matches) looks again, and an evictor leaves the source alone. parent is
// the move whose copy (mine0; mine1 is busy, the parent publishes it) this
// one installs, nil for a new key, which put installs.
func (f *Filter) relocate(i int, line, mine0, mine1, now uint64, depth int, budget *int, parent *moveCount, put putFunc) bool {
	w0 := atomic.LoadUint64(&f.words[i])
	if w0 == 0 {
		return false
	}
	if *budget <= 0 {
		return false
	}
	*budget--
	w1 := atomic.LoadUint64(&f.words[i+1])
	if w1>>tagShift == tagBusy || atomic.LoadUint64(&f.words[i]) != w0 {
		return false // being installed or realigned, or touched since: a copy would pair stale words
	}
	if testHookPicked != nil {
		testHookPicked(i)
	}
	a := f.alt(line, w0>>48)
	base := int(a * wordsPerLine)
	moved := w0 ^ dispBit         // displaced iff it wasn't
	m := f.moveOf(w1 >> tagShift) // the entry's key's stripe
	replace := func(d int) bool { // the hidden copy is at d; now take the source
		defer m.done.Add(1)
		if parent != nil {
			parent.begun.Add(1) // the parent's copy goes in (hidden) with this install
			if parent.evicting.Load() != 0 {
				parent.done.Add(1)
				f.abandon(d)
				return false
			}
		}
		if f.putNew(put, i, w0, w1, mine0, mine1) { // deeper levels pass no put: install
			atomic.StoreUint64(&f.words[d+1], w1) // the source held w0/w1 until the hand-off: publish
			f.relocations.Add(1)
			return true
		}
		if parent != nil {
			parent.done.Add(1) // ...or doesn't
		}
		f.abandon(d) // a touch reached the source first, or the key was evicted
		return false
	}
	for s := 0; s < wordsPerLine; s += 2 {
		d0 := atomic.LoadUint64(&f.words[base+s])
		if d0 != 0 && age(now, d0&tsMask) <= f.horizon {
			continue
		}
		m.begun.Add(1)
		if m.evicting.Load() != 0 { // an eviction of this stripe is under way: it may take the source
			m.done.Add(1)
			return false
		}
		if d1 := atomic.LoadUint64(&f.words[base+s+1]); f.putOver(nil, base+s, d0, d1, moved, busy) {
			if d0 != 0 {
				f.slotDrop(f.slotID(a, d0, d1), d1) // the expired occupant's
			}
			if testHookCopied != nil {
				testHookCopied()
			}
			return replace(base + s)
		}
		m.done.Add(1)
	}
	if depth > 0 {
		f.warmLine(a) // the next lines of a's entries together, not one miss at a time
		for s := 0; s < wordsPerLine; s += 2 {
			if f.relocate(base+s, a, moved, busy, now, depth-1, budget, m, nil) {
				return replace(base + s)
			}
		}
	}
	return false
}

// abandon clears the hidden copy at word index d of a move whose source
// changed before the hand-off, word0 first (a slot is never a fingerprint
// with an empty register). Nobody else writes a busy word1.
func (f *Filter) abandon(d int) {
	atomic.StoreUint64(&f.words[d], 0)
	atomic.StoreUint64(&f.words[d+1], 0)
}
