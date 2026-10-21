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
// from its own entries instead of counters shared with every other dropped
// key, so a flood adds no noise: only a fingerprint collision (about live
// entries / 16384 per bucket read) or a bucket's floor can raise a key that
// never spilled.
//
// Each history has two candidate buckets and goes to the one with fewer
// live entries, and a read covers both, so buckets fill evenly: no floor
// rose with the table 93% full, where one bucket per history had floored
// histories at 64% (verify/expected/flood-sizing.txt). Reading two buckets
// doubles fingerprint matches (about 0.3% of keys once the table is full).
// When both are full the lightest history in them (newcomer included)
// leaves, and its bucket's floor keeps the largest count that left. A
// bucket has three floors, 7 bits each in entry 0's place, and a history's
// fingerprint picks its floor: every key reading the bucket adds its own
// floor to Upper, so a raised floor reaches a third of the bucket's keys,
// and Upper never reads below the truth for a key with at most one history
// in a floor. A key floored more than once in the same floor can read low
// by all but its largest floored history; summing floors instead closes
// that, but light floods then push every floor to 31 and deny every new key
// (backlog, 2026-10-09). Floored counts the histories that left to a floor:
// from the first, strict limits deny the keys that read it.
//
// Time is kept per entry as the slice (Horizon/2) of the history's newest
// event, mod 4. epoch is the newest slice an add has seen; before an add
// moves it on, entries that are dead by then are swept, so live entries
// decode unambiguously against the current epoch (and only against it: see
// add). All events of a history count as long as its newest one is inside
// the window: Upper stays a bound, and Count reads a little high for
// windows shorter than the horizon.

const (
	ghFPBits    = 14
	ghCntBits   = 5
	ghSlBits    = 2
	ghEntryBits = ghFPBits + ghCntBits + ghSlBits // 21
	ghPerWord   = 3
	ghWords     = 8 // per bucket: one cache line
	ghEntries   = ghWords * ghPerWord
	ghCntMax    = 1<<ghCntBits - 1
	ghEntryMask = 1<<ghEntryBits - 1
	ghSubBits   = ghCntBits + ghSlBits // a floor: count and slice, no fingerprint
	ghSubMask   = 1<<ghSubBits - 1
	ghSubMax    = ghEntryBits / ghSubBits // floors that fit in entry 0
)

type ghost struct {
	words    []uint64
	nb       uint64 // buckets
	choices  int    // candidate buckets per history: 1, or 2 (the default)
	sub      int    // floors per bucket, picked by fingerprint: 1..3
	sliceLen uint64
	horizon  uint64
	mu       sync.Mutex
	epoch    atomic.Uint64 // newest slice + 1 seen by an add (0: none)
	until    atomic.Uint64 // tick before which an entry may be live
	sat      atomic.Bool   // some entry saturated (count is a floor)
	floored  atomic.Uint64 // histories that left to a bucket's floor
}

// newGhost sizes the table to bytes (rounded down to whole buckets).
func newGhost(bytes, horizon uint64, choices, sub int) *ghost {
	nb := max(1, bytes/(ghWords*8))
	w := alignedWords(int(nb * ghWords))
	prefault(w)
	return &ghost{
		words:    w,
		nb:       nb,
		choices:  max(1, min(choices, 2)),
		sub:      max(1, min(sub, ghSubMax)),
		sliceLen: (horizon + 1) / 2,
		horizon:  horizon,
	}
}

func ghEntry(fp, cnt, sl uint64) uint64 { return fp<<(ghCntBits+ghSlBits) | cnt<<ghSlBits | sl&3 }
func ghFP(e uint64) uint64              { return e >> (ghCntBits + ghSlBits) }
func ghCnt(e uint64) uint64             { return e >> ghSlBits & ghCntMax }

// slot: id's candidate buckets (first word index of each), how many are
// distinct, and its fingerprint (never 0). The first bucket and the
// fingerprint are the one-choice table's.
func (g *ghost) slot(id uint64) (base [2]int, nb int, fp uint64) {
	x := mix64(id ^ 0x5bd1e9955bd1e995)
	b, _ := bits.Mul64(x, g.nb)
	fp = x & (1<<ghFPBits - 1)
	if fp == 0 {
		fp = 1
	}
	base[0], base[1], nb = int(b*ghWords), int(b*ghWords), 1
	if g.choices > 1 {
		if b2, _ := bits.Mul64(mix64(id^0x2545f4914f6cdd1d), g.nb); b2 != b {
			base[1], nb = int(b2*ghWords), 2
		}
	}
	return base, nb, fp
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

// at returns entry k (0..23) of the bucket at base; k 0 holds the floors.
func (g *ghost) at(base, k int) (w *uint64, sh uint) {
	return &g.words[base+k/ghPerWord], uint(k%ghPerWord) * ghEntryBits
}

// floorShift is the bit offset, in a bucket's first word, of the floor that
// covers fingerprint fp.
func (g *ghost) floorShift(fp uint64) uint { return uint(fp%uint64(g.sub)) * ghSubBits }

// floorOf is the live floor covering fp in the bucket at base, at epoch ep.
func (g *ghost) floorOf(base int, fp, ep, now uint64) uint64 {
	e := atomic.LoadUint64(&g.words[base]) >> g.floorShift(fp) & ghSubMask
	if e == 0 || !g.live(g.real(e, ep), g.horizon, now) {
		return 0
	}
	return ghCnt(e)
}

// add records a dropped history: n events, the newest at tick last.
func (g *ghost) add(id, n, last uint64, sat bool, now uint64) {
	if n == 0 || age(now, last) > g.horizon {
		return
	}
	// The epoch must reach the history's own slice, even when the caller's
	// clock is behind its newest event (an evictor with an older clock):
	// an entry from a slice past the epoch would decode four slices old,
	// dead to reads and free to the next add.
	sl := last / g.sliceLen
	if ns := max(now, last) / g.sliceLen; g.epoch.Load() < ns+1 {
		g.advance(ns)
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
	bases, nb, fp := g.slot(id)
outer:
	for {
		// Entries' 2-bit slices are decoded against ep, so a write is
		// committed only if ep is still the epoch: an entry another add
		// wrote after moving the epoch on would read four slices old, dead,
		// and be overwritten.
		ep := g.epoch.Load()
		if sl+3 < ep { // older than the live slices (a caller far behind the others)
			return
		}
		// One pass over both buckets: a matching entry to merge into, else
		// the less loaded bucket's free or dead entry, else the lightest live
		// entry of either.
		var free, live [2]int
		victim, vb := -1, 0
		vc, vsl := uint64(ghCntMax+1), uint64(0)
		for bi, base := range bases[:nb] {
			free[bi] = -1
			for k := 1; k < ghEntries; k++ {
				w, sh := g.at(base, k)
				old := atomic.LoadUint64(w)
				e := old >> sh & ghEntryMask
				if e == 0 || !g.live(g.real(e, ep), g.horizon, now) {
					if free[bi] < 0 {
						free[bi] = k
					}
					continue
				}
				live[bi]++
				if ghFP(e) == fp {
					esl := max(g.real(e, ep), sl)
					ne := ghEntry(fp, min(ghCntMax, ghCnt(e)+n), esl)
					if g.epoch.Load() == ep && atomic.CompareAndSwapUint64(w, old, old&^(ghEntryMask<<sh)|ne<<sh) {
						return
					}
					continue outer
				}
				if c := ghCnt(e); c < vc || c == vc && g.real(e, ep) < vsl {
					victim, vb, vc, vsl = k, bi, c, g.real(e, ep)
				}
			}
		}
		bi := -1
		for i := range nb {
			if free[i] >= 0 && (bi < 0 || live[i] < live[bi]) {
				bi = i
			}
		}
		if bi >= 0 {
			w, sh := g.at(bases[bi], free[bi])
			old := atomic.LoadUint64(w)
			e := old >> sh & ghEntryMask
			if e != 0 && g.live(g.real(e, ep), g.horizon, now) {
				continue // taken meanwhile
			}
			if g.epoch.Load() == ep && atomic.CompareAndSwapUint64(w, old, old&^(ghEntryMask<<sh)|ghEntry(fp, n, sl)<<sh) {
				return
			}
			continue
		}
		if g.epoch.Load() != ep {
			continue
		}
		// Full: the lightest history leaves (the newcomer if it is the
		// lightest) and its bucket's floor rises to cover it. The floor goes
		// up first, so no reader sees the history in neither place.
		if n <= vc {
			// The newcomer may go to either bucket's floor: the one already
			// higher, so a clean bucket stays clean.
			fb := 0
			if nb > 1 && g.floorOf(bases[1], fp, ep, now) > g.floorOf(bases[0], fp, ep, now) {
				fb = 1
			}
			g.raiseFloor(bases[fb], fp, n, sl, now)
			return
		}
		w, sh := g.at(bases[vb], victim)
		old := atomic.LoadUint64(w)
		e := old >> sh & ghEntryMask
		if ghCnt(e) != vc || ghFP(e) == fp {
			continue
		}
		g.raiseFloor(bases[vb], ghFP(e), vc, g.real(e, ep), now)
		if g.epoch.Load() == ep && atomic.CompareAndSwapUint64(w, old, old&^(ghEntryMask<<sh)|ghEntry(fp, n, sl)<<sh) {
			return
		}
	}
}

// raiseFloor lifts the bucket's floor covering fingerprint fp to at least
// n events, live until slice sl's events expire.
func (g *ghost) raiseFloor(base int, fp, n, sl, now uint64) {
	g.floored.Add(1)
	w, sh := &g.words[base], g.floorShift(fp)
	for {
		ep := g.epoch.Load() // as in add: a floor written after the epoch moved must not read as dead
		old := atomic.LoadUint64(w)
		e := old >> sh & ghSubMask
		c, fsl := uint64(0), sl
		if e != 0 && g.live(g.real(e, ep), g.horizon, now) {
			c, fsl = ghCnt(e), max(sl, g.real(e, ep))
		}
		ne := ghEntry(0, max(c, n), fsl)
		if ne == e || g.epoch.Load() == ep && atomic.CompareAndSwapUint64(w, old, old&^(ghSubMask<<sh)|ne<<sh) {
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
					sh, n, mask := uint(k)*ghEntryBits, 1, uint64(ghEntryMask)
					if k == 0 && i%ghWords == 0 { // the floors, each with its own slice
						n, mask = ghSubMax, ghSubMask
					}
					for range n {
						if e := old >> sh & mask; e != 0 && g.real(e, ep)+2 < ns {
							nw &^= mask << sh
						}
						sh += ghSubBits
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
// Count and Upper, the floors in Upper only. It reads both candidate lines
// whatever it finds, so its work doesn't depend on the key (ConstantTime).
func (g *ghost) estimate(id, window, now uint64) (r Result, ok bool) {
	ep := g.epoch.Load()
	if ep == 0 || now > g.until.Load() {
		return Result{}, false
	}
	bases, nb, fp := g.slot(id)
	var line [2][ghWords]uint64
	for bi, base := range bases[:nb] {
		for i := range line[bi] {
			line[bi][i] = atomic.LoadUint64(&g.words[base+i])
		}
	}
	if g.epoch.Load() != ep {
		ep = g.epoch.Load()
	}
	for bi := range nb {
		if e := line[bi][0] >> g.floorShift(fp) & ghSubMask; e != 0 && g.live(g.real(e, ep), window, now) {
			c := int(ghCnt(e))
			r.Upper += c
			r.Saturated = r.Saturated || c == ghCntMax
		}
		for k := 1; k < ghEntries; k++ {
			e := line[bi][k/ghPerWord] >> (uint(k%ghPerWord) * ghEntryBits) & ghEntryMask
			if e == 0 || !g.live(g.real(e, ep), window, now) {
				continue
			}
			c := int(ghCnt(e))
			if ghFP(e) != fp {
				continue
			}
			r.Upper += c
			r.Count += c
			r.Saturated = r.Saturated || c == ghCntMax
		}
	}
	if r.Upper == 0 {
		return Result{}, false
	}
	r.Seen = r.Count > 0
	return r, true
}
