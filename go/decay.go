package temporalfilter

import (
	"math/bits"
	"sync/atomic"
)

const (
	// decayEvery gates the inline decay step to 1/64 of operations (by key
	// hash) so the shared cursor's cache line is not touched on every call.
	decayEvery = 63
)

// ContinuousDecay reclaims expired slots at a rate set by the clock: one full
// pass of the table per Horizon of elapsed ticks, spread across callers in
// steps of at most a few cache lines. It never blocks and never pauses
// readers. Expiry correctness does not depend on it (ages are evaluated
// arithmetically on every access); it only frees slots and keeps occupancy
// honest. touch calls it inline on 1/64 of operations; calling it from a
// ticker as well is optional. Returns lines swept.
func (f *Filter) ContinuousDecay() int { return f.decayStep(f.clock(), 4) }

func (f *Filter) decayStep(now uint64, budget uint64) int {
	hi, lo := bits.Mul64(now, f.lines)
	if hi >= f.horizon {
		return 0
	}
	target, _ := bits.Div64(hi, lo, f.horizon)
	c := f.cursor.Load()
	if c >= target {
		return 0
	}
	checkTick(now) // past it, every slot would read expired (only a step that sweeps gets here)
	start := c
	if target-start > f.lines { // fell behind by over a lap: skip ahead, lazy expiry covered it
		start = target - f.lines
	}
	n := min(target-start, budget)
	if !f.cursor.CompareAndSwap(c, start+n) {
		return 0 // another caller took this step
	}
	for l := start; l < start+n; l++ {
		f.sweepLine(l%f.lines, now)
	}
	return int(n)
}

// sweepLine clears line's slots expired at now. It takes a slot as install
// does, for no key: word1 busy first, then word0 to 0 (or word1 back if
// word0 changed), then word1 to 0. A first touch takes a free slot only
// with word1 not busy, so none lands in the slot while it is cleared.
// Before, the sweep cleared word0, then CASed word1 from the register it
// had read to 0: a first touch could take the slot between the two and
// install a register bit-identical to the old one (its key's, one event,
// the same 4 epoch bits), which the CAS then cleared, leaving the key's
// fingerprint with an empty register, a slot no lookup matches (claims
// A26).
//
// The hold counts as a move of the stripe of the word1 it takes busy, as
// every hold does (moveOf): a touch at a clock that finds the occupant
// live may move word0 on during it (the sweep then puts word1 back), and
// its count, finding word1 busy, looks the key up again. That lookup must
// not miss the slot and put the key in a second one.
func (f *Filter) sweepLine(line, now uint64) {
	base := int(line * wordsPerLine)
	for s := 0; s < wordsPerLine; s += 2 {
		w0 := atomic.LoadUint64(&f.words[base+s])
		if w0 == 0 || age(now, w0&tsMask) <= f.horizon {
			continue
		}
		if testHookSweepSeen != nil {
			testHookSweepSeen(base + s)
		}
		w1 := atomic.LoadUint64(&f.words[base+s+1])
		if w1>>tagShift == tagBusy {
			continue // being handed over
		}
		if testHookSweepRead != nil {
			testHookSweepRead(base + s)
		}
		m := f.moveOf(w1 >> tagShift) // the stripe of whoever holds w1 when the CAS takes it
		m.begun.Add(1)
		if !atomic.CompareAndSwapUint64(&f.words[base+s+1], w1, busy) {
			m.done.Add(1)
			continue // touched since
		}
		if testHookSweeping != nil {
			testHookSweeping()
		}
		if atomic.CompareAndSwapUint64(&f.words[base+s], w0, 0) {
			atomic.StoreUint64(&f.words[base+s+1], 0)
			f.slotDrop(f.slotID(line, w0, w1), w1)
		} else {
			atomic.StoreUint64(&f.words[base+s+1], w1) // touched since: put word1 back
		}
		m.done.Add(1)
	}
}
