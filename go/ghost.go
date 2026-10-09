package temporalfilter

import (
	"math/bits"
	"sync"
	"sync/atomic"
)

// Ghost table: an overflow store that keeps each dropped history as a
// 21-bit entry (14-bit fingerprint, 5-bit event count, 2-bit time slice),
// three to a word, in buckets of one cache line (8 words: a floor and 23
// entries). It takes the count-min sketch's bytes, but a key's answer comes
// from its own entry instead of counters shared with every other dropped
// key, so a flood adds no noise: only a fingerprint collision (about live
// entries / 16384 per read) or a bucket's floor can raise a key that never
// spilled. When a bucket has no room the lightest history in it (newcomer
// included) leaves, and the bucket's floor keeps the largest count that
// left: every key of the bucket adds the floor to Upper, so Upper never
// reads below the truth. Reads take one cache line (the sketch takes four).
//
// Time is kept per entry as the slice (Horizon/2) of the history's newest
// event, mod 4. epoch is the newest slice an add has seen; before an add
// moves it on, entries that are dead by then are swept, so live entries
// decode unambiguously. All events of a history count as long as its
// newest one is inside the window: Upper stays a bound, and Count reads
// a little high for windows shorter than the horizon.

const (
	ghFPBits    = 14
	ghCntBits   = 5
	ghSlBits    = 2
	ghEntryBits = ghFPBits + ghCntBits + ghSlBits // 21
	ghPerWord   = 3
	ghWords     = 8 // per bucket: one cache line
	ghCntMax    = 1<<ghCntBits - 1
	ghEntryMask = 1<<ghEntryBits - 1
)

type ghost struct {
	words    []uint64
	nb       uint64 // buckets
	sliceLen uint64
	horizon  uint64
	mu       sync.Mutex
	epoch    atomic.Uint64 // newest slice + 1 seen by an add (0: none)
	until    atomic.Uint64 // tick before which an entry may be live
	sat      atomic.Bool   // some entry saturated (count is a floor)
}

// newGhost sizes the table to bytes (rounded down to whole buckets).
func newGhost(bytes, horizon uint64) *ghost {
	nb := max(1, bytes/(ghWords*8))
	return &ghost{
		words:    alignedWords(int(nb * ghWords)),
		nb:       nb,
		sliceLen: (horizon + 1) / 2,
		horizon:  horizon,
	}
}

func ghEntry(fp, cnt, sl uint64) uint64 { return fp<<(ghCntBits+ghSlBits) | cnt<<ghSlBits | sl&3 }
func ghFP(e uint64) uint64              { return e >> (ghCntBits + ghSlBits) }
func ghCnt(e uint64) uint64             { return e >> ghSlBits & ghCntMax }

// slot: id's bucket (first word index) and fingerprint (never 0).
func (g *ghost) slot(id uint64) (base int, fp uint64) {
	x := mix64(id ^ 0x5bd1e9955bd1e995)
	b, _ := bits.Mul64(x, g.nb)
	fp = x & (1<<ghFPBits - 1)
	if fp == 0 {
		fp = 1
	}
	return int(b * ghWords), fp
}

// real decodes an entry's slice against epoch ep (newest slice + 1).
func (g *ghost) real(e, ep uint64) uint64 {
	cur := ep - 1
	return cur - (cur-e)&3
}

// live: slice sl's events can be within window of now.
func (g *ghost) live(sl, window, now uint64) bool {
	end := sl*g.sliceLen + g.sliceLen - 1
	return age(now, end) <= min(window, g.horizon)
}

// at returns entry k (0..23) of the bucket at base; k 0 is the floor.
func (g *ghost) at(base, k int) (w *uint64, sh uint) {
	return &g.words[base+k/ghPerWord], uint(k%ghPerWord) * ghEntryBits
}

// add records a dropped history: n events, the newest at tick last.
func (g *ghost) add(id, n, last uint64, sat bool, now uint64) {
	if n == 0 || age(now, last) > g.horizon {
		return
	}
	sl := last / g.sliceLen
	if ns := now / g.sliceLen; g.epoch.Load() < ns+1 {
		g.advance(ns)
	}
	ep := g.epoch.Load()
	if sl+3 < ep { // older than the live slices (can't happen for a live history)
		return
	}
	if sat || n >= ghCntMax {
		n, sat = ghCntMax, true
		g.sat.Store(true)
	}
	if u := sl*g.sliceLen + g.sliceLen - 1 + g.horizon; g.until.Load() < u {
		for {
			cur := g.until.Load()
			if cur >= u || g.until.CompareAndSwap(cur, u) {
				break
			}
		}
	}
	base, fp := g.slot(id)
outer:
	for {
		// One pass: a matching entry to merge into, else a free or dead
		// one, else the lightest live one.
		free, victim := -1, -1
		vc, vsl := uint64(ghCntMax+1), uint64(0)
		for k := 1; k < ghWords*ghPerWord; k++ {
			w, sh := g.at(base, k)
			old := atomic.LoadUint64(w)
			e := old >> sh & ghEntryMask
			if e == 0 || !g.live(g.real(e, ep), g.horizon, now) {
				if free < 0 {
					free = k
				}
				continue
			}
			if ghFP(e) == fp {
				esl := max(g.real(e, ep), sl)
				ne := ghEntry(fp, min(ghCntMax, ghCnt(e)+n), esl)
				if atomic.CompareAndSwapUint64(w, old, old&^(ghEntryMask<<sh)|ne<<sh) {
					return
				}
				continue outer
			}
			if c := ghCnt(e); c < vc || c == vc && g.real(e, ep) < vsl {
				victim, vc, vsl = k, c, g.real(e, ep)
			}
		}
		if free >= 0 {
			w, sh := g.at(base, free)
			old := atomic.LoadUint64(w)
			e := old >> sh & ghEntryMask
			if e != 0 && g.live(g.real(e, ep), g.horizon, now) {
				continue // taken meanwhile
			}
			if atomic.CompareAndSwapUint64(w, old, old&^(ghEntryMask<<sh)|ghEntry(fp, n, sl)<<sh) {
				return
			}
			continue
		}
		// Full: the lightest history leaves (the newcomer if it is the
		// lightest) and the floor rises to cover it. The floor goes up
		// first, so no reader sees the history in neither place.
		if n <= vc {
			g.raiseFloor(base, n, sl, ep, now)
			return
		}
		w, sh := g.at(base, victim)
		old := atomic.LoadUint64(w)
		e := old >> sh & ghEntryMask
		if ghCnt(e) != vc || ghFP(e) == fp {
			continue
		}
		g.raiseFloor(base, vc, g.real(e, ep), ep, now)
		if atomic.CompareAndSwapUint64(w, old, old&^(ghEntryMask<<sh)|ghEntry(fp, n, sl)<<sh) {
			return
		}
	}
}

// raiseFloor lifts the bucket's floor to at least n events, live until
// slice sl's events expire.
func (g *ghost) raiseFloor(base int, n, sl, ep, now uint64) {
	w := &g.words[base]
	for {
		old := atomic.LoadUint64(w)
		e := old & ghEntryMask
		c, fsl := uint64(0), sl
		if e != 0 && g.live(g.real(e, ep), g.horizon, now) {
			c, fsl = ghCnt(e), max(sl, g.real(e, ep))
		}
		ne := ghEntry(0, max(c, n), fsl)
		if ne == e || atomic.CompareAndSwapUint64(w, old, old&^ghEntryMask|ne) {
			return
		}
	}
}

// advance moves the epoch to slice ns, first clearing every entry that is
// dead by then (so the 2-bit slices of the rest decode unambiguously).
func (g *ghost) advance(ns uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ep := g.epoch.Load()
	if ep >= ns+1 {
		return
	}
	if ep != 0 {
		for i := range g.words {
			for {
				old := atomic.LoadUint64(&g.words[i])
				nw := old
				for k := range ghPerWord {
					sh := uint(k) * ghEntryBits
					e := old >> sh & ghEntryMask
					if e != 0 && g.real(e, ep)+2 < ns {
						nw &^= ghEntryMask << sh
					}
				}
				if nw == old || atomic.CompareAndSwapUint64(&g.words[i], old, nw) {
					break
				}
			}
		}
	}
	g.epoch.Store(ns + 1)
}

// estimate is id's dropped events within window: matching entries count in
// Count and Upper, the floor in Upper only. full reads the whole line
// whatever it finds (ConstantTime); it always does, as it is one line.
func (g *ghost) estimate(id, window, now uint64) (r Result, ok bool) {
	ep := g.epoch.Load()
	if ep == 0 || now > g.until.Load() {
		return Result{}, false
	}
	base, fp := g.slot(id)
	var line [ghWords]uint64
	for i := range line {
		line[i] = atomic.LoadUint64(&g.words[base+i])
	}
	if g.epoch.Load() != ep {
		ep = g.epoch.Load()
	}
	for k := range ghWords * ghPerWord {
		e := line[k/ghPerWord] >> (uint(k%ghPerWord) * ghEntryBits) & ghEntryMask
		if e == 0 || !g.live(g.real(e, ep), window, now) {
			continue
		}
		c := int(ghCnt(e))
		if k == 0 {
			r.Upper += c
			r.Saturated = r.Saturated || c == ghCntMax
			continue
		}
		if ghFP(e) != fp {
			continue
		}
		r.Upper += c
		r.Count += c
		r.Saturated = r.Saturated || c == ghCntMax
	}
	if r.Upper == 0 {
		return Result{}, false
	}
	r.Seen = r.Count > 0
	return r, true
}
