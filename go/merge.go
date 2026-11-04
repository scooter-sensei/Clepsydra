package temporalfilter

import (
	"errors"
	"math/bits"
	"sync/atomic"
)

// Merge adds g's keys to f: for a key in both, the two registers are
// re-bucketed to the newer one's epoch and summed bucket by bucket (an
// overflow sets the saturated bit), and the last touch is the newer one; a
// key only g holds goes in as a first touch would (which may evict). Both
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
			e := max(vf.e, vg.e)
			rf, rg := register(vf.c), register(vg.c)
			over := rf.advance(vf.e, e)
			over = rg.advance(vg.e, e) || over
			for b := range nBuckets {
				over = rf.add(b, rg[b]) || over
			}
			sat := (fw0 | w0) & satBit
			if over {
				sat = satBit
			}
			atomic.StoreUint64(&f.words[j+1], tg<<tagShift|uint64(e)&epochMask<<48|rf.pack())
			atomic.StoreUint64(&f.words[j], fw0&^(tsMask|satBit)|sat|max(last, fw0&tsMask))
		} else {
			placed := false
			for range 8 {
				if f.claim(h, fp, w1, now, last, w0&satBit, claimFull) >= 0 {
					placed = true
					break
				}
			}
			if !placed {
				mu.Unlock()
				return errors.New("temporalfilter: Merge: a key found no slot in 8 tries (both filters must be quiescent)")
			}
		}
		mu.Unlock()
	}
	return nil
}
