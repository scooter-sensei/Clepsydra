package temporalfilter

import (
	"errors"
	"math/bits"
	"sync/atomic"
)

// Merge adds g's keys to f: for a key in both, the two registers are
// re-bucketed to the newer one's epoch and summed bucket by bucket (an
// overflow sets the saturated bit), and the last touch is the newer one; a
// key only g's table holds goes in as a first touch would (which may evict),
// with f's own stashed history of it, if f evicted it, summed in the same
// way (as a first touch restores it: installed without it, f's reads would
// find the key in the table and skip the stashed history). Both
// filters must be quiescent and built with the same Config (and Key). g's
// stash and overflow table (evicted histories) are not merged. ConstantTime,
// Background and hot entries in use are refused with an error, f unchanged.
// A key that can't get a slot (a writer racing the merge) stops it with an
// error, the keys before it merged.
func (f *Filter) Merge(g *Filter) error {
	switch {
	case f == g:
		return errors.New("temporalfilter: Merge with itself")
	case f.lines != g.lines || f.horizon != g.horizon || f.unit != g.unit || f.res != g.res:
		return errors.New("temporalfilter: Merge needs the same Config")
	case f.ct || g.ct || f.bg || g.bg:
		return errors.New("temporalfilter: Merge of ConstantTime or Background filters")
	case f.hot.Load() != nil || g.hot.Load() != nil:
		return errors.New("temporalfilter: Merge with hot entries in use")
	}
	now := uint64(0)
	for _, x := range [2]*Filter{f, g} {
		for i := 0; i < len(x.words); i += 2 {
			now = max(now, atomic.LoadUint64(&x.words[i])&tsMask)
		}
	}
	for i := 0; i < len(g.words); i += 2 {
		w0, w1 := atomic.LoadUint64(&g.words[i]), atomic.LoadUint64(&g.words[i+1])
		last := w0 & tsMask
		if w0 == 0 || w1>>tagShift == tagBusy || age(now, last) > g.horizon {
			continue
		}
		id := g.slotID(uint64(i/wordsPerLine), w0, w1)
		line, fp, tg := id>>28, w0>>48, w1>>tagShift
		lo, _ := bits.Div64(line>>28, line<<36, f.lines) // ceil(line * 2^36 / lines): f.primary(h) == line
		if lo*f.lines < line<<36 {
			lo++
		}
		h := fp<<48 | tg<<36 | lo
		if f.primary(h) != line || fingerprint(h) != fp || tag(h) != tg {
			return errors.New("temporalfilter: Merge: identity does not round-trip")
		}
		vg, _ := g.viewOf(id, w1, last)
		mu := &f.claimMu[h>>(64-claimStripeBits)]
		mu.Lock()
		j, fw0, fw1 := f.lookupStable(h, fp, tg, false)
		if j >= 0 {
			vf, _ := f.viewOf(id, fw1, fw0&tsMask)
			nw1, over := sumRegisters(vf, vg, tg)
			sat := (fw0 | w0) & satBit
			if over {
				sat = satBit
			}
			atomic.StoreUint64(&f.words[j+1], nw1)
			atomic.StoreUint64(&f.words[j], fw0&^(tsMask|satBit)|sat|max(last, fw0&tsMask))
		} else {
			// f may hold the key's evicted history in its stash: take it and
			// sum it in. Background is refused, so recall finds nothing or
			// the stash (srcStash), and with hot entries refused its word1
			// is a register.
			nw1, sat, stamp := w1, w0&satBit, last
			s0, s1, src := f.recall(h, fp, tg, now, true)
			if src == srcStash {
				vf, _ := f.viewOf(id, s1, s0&tsMask)
				var over bool
				nw1, over = sumRegisters(vf, vg, tg)
				sat = (s0 | w0) & satBit
				if over {
					sat = satBit
				}
				stamp = max(last, s0&tsMask)
			}
			placed := false
			for range 8 {
				if f.claim(h, fp, nw1, now, stamp, sat, claimFull) >= 0 {
					placed = true
					break
				}
			}
			if !placed {
				if src == srcStash { // put it back, as miss does for its retry
					f.putBack(f.stashID(h, fp, tg), s0, s1, now)
				}
				mu.Unlock()
				return errors.New("temporalfilter: Merge: a key found no slot in 8 tries (both filters must be quiescent)")
			}
		}
		mu.Unlock()
	}
	return nil
}

// sumRegisters re-buckets views a and b to the newer epoch and adds them
// bucket by bucket: the merged register word for tag tg, and whether a
// bucket overflowed (the caller sets the saturated bit).
func sumRegisters(a, b view, tg uint64) (w1 uint64, over bool) {
	e := max(a.e, b.e)
	ra, rb := register(a.c), register(b.c)
	over = ra.advance(a.e, e)
	over = rb.advance(b.e, e) || over
	for i := range nBuckets {
		over = ra.add(i, rb[i]) || over
	}
	return tg<<tagShift | uint64(e)&epochMask<<48 | ra.pack(), over
}
