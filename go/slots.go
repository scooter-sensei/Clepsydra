package temporalfilter

import (
	"math/bits"
	"sync"
	"sync/atomic"
)

const (
	slotsPerLine = 4
	wordsPerLine = 2 * slotsPerLine
	// dispBit marks an entry living in its alternate line ("displaced").
	// Lookups match it too, so a key is compared only with entries that
	// share its primary line, wherever they live: half the false matches.
	dispBit = 1 << 47
	// satBit marks a slot whose register overflowed (an event spilled to an
	// older bucket or was dropped) within its span; see estimate.
	satBit    = 1 << 46
	tsMask    = satBit - 1
	tagBits   = 12
	tagShift  = 64 - tagBits
	epochMask = 0xf
	// busy is word1 while a slot is being handed over (install), while a
	// touch moves its register and last touch together (realign), and in a
	// relocation's copy until the hand-off (relocate).
	// No key has its tag, so no lookup or touch can match the slot until
	// the new occupant's word1 replaces it.
	tagBusy = 1<<tagBits - 1
	busy    = tagBusy << tagShift
	// claimStripeBits sets 256 insert-path lock stripes, picked by the
	// hash's top bits.
	claimStripeBits = 8
)

type paddedMutex struct {
	sync.Mutex
	_ [56]byte
}

// moveCount: moves of a stripe's keys begun and done, and evictions of
// them under way. A relocation counts its move, then checks for an
// eviction; an eviction counts itself, then checks for a move. With both
// counts sequentially consistent at least one of two that overlap sees the
// other and backs off, so no eviction takes the source of an entry being
// moved, which would fail the move's hand-off: the move's work would be
// lost, not the key's history (relocate).
type moveCount struct {
	begun, done atomic.Uint64
	evicting    atomic.Int64
}

// moveOf is the move counts of entries whose word1 tag is tg. A hold of a
// slot takes word1 busy by a CAS from the value it read, so whoever the
// slot holds when the CAS succeeds has that value's tag: counted on the
// tag's stripe, the hold tells that key's lookups to look again, even when
// it is not the occupant whose word0 the holder read (a key installed
// between the holder's reads of word0 and word1, or one whose word1 is
// bit-identical to the old occupant's: the same tag, epoch bits and counts).
func (f *Filter) moveOf(tg uint64) *moveCount { return &f.moves[tg&(1<<claimStripeBits-1)] }

// slotWords loads slot i's words if it still holds fpd/tg: the fingerprint
// and displaced bit (word0>>47) and the tag that the lookup matched there.
// The displaced bit is part of the key's identity, as in find: a key with
// the same fingerprint and tag whose primary line is this key's alternate
// lives in this key's primary line displaced, and a relocation can move it
// into the slot this key was found in between the lookup and the load.
// word0 is read again after word1: a realign between the two loads would
// pair the old last touch with the new register, whose epoch is read
// against it.
func (f *Filter) slotWords(i int, fpd, tg uint64) (w0, w1 uint64, ok bool) {
	if i < 0 {
		return 0, 0, false
	}
	w0 = atomic.LoadUint64(&f.words[i])
	w1 = atomic.LoadUint64(&f.words[i+1])
	if w0>>47 != fpd || w1>>tagShift != tg {
		return w0, w1, false
	}
	if a := atomic.LoadUint64(&f.words[i]); a != w0 && f.unitOf(a&tsMask) != f.unitOf(w0&tsMask) {
		return w0, w1, false
	}
	return w0, w1, true
}

// markSat sets (on) or clears the saturated bit of the slot at word index i
// if it still holds fpd (fingerprint and displaced bit: word0>>47, as bump
// checks), and returns the bit as left. It clears only while no bucket is
// hot, reading word1 after word0: an overflowing touch writes its register
// before it sets the bit, so a clear that read a register from before that
// touch fails its CAS on the newer word0.
func (f *Filter) markSat(i int, fpd uint64, on bool) bool {
	for {
		w0 := atomic.LoadUint64(&f.words[i])
		if w0>>47 != fpd {
			return on
		}
		if w0&satBit != 0 == on {
			return on
		}
		if w1 := atomic.LoadUint64(&f.words[i+1]); !on && (w1>>tagShift == tagBusy || hot(w1)) {
			return true // a busy word1 (realign) may come back with a hot bucket
		}
		if atomic.CompareAndSwapUint64(&f.words[i], w0, w0^satBit) {
			return on
		}
	}
}

// locate returns the word index of h's slot, or -1: an undisplaced entry in
// the primary line or a displaced one in the alternate line.
func (f *Filter) locate(h, fp, tg uint64) int {
	i, _ := f.locateAt(h, fp, tg)
	return i
}

// locateAt is locate, with the fingerprint and displaced bit it matched
// (fpd, word0>>47): what a later load of the slot must still hold.
func (f *Filter) locateAt(h, fp, tg uint64) (int, uint64) {
	p := f.primary(h)
	if i := f.find(p, fp<<1, tg); i >= 0 {
		return i, fp << 1
	}
	return f.find(f.alt(p, fp), fp<<1|1, tg), fp<<1 | 1
}

// locateBoth is locateAt reading both lines every time (ConstantTime).
func (f *Filter) locateBoth(h, fp, tg uint64) (int, uint64) {
	p := f.primary(h)
	i := f.find(p, fp<<1, tg)
	j := f.find(f.alt(p, fp), fp<<1|1, tg)
	if i >= 0 {
		return i, fp << 1
	}
	return j, fp<<1 | 1
}

// find returns the word index of the slot in line whose fingerprint and
// displaced bit equal fpd and whose tag is tg, or -1. The tag lives in word1
// of the same cache line, so identity is effectively 28 bits at no extra
// memory traffic.
func (f *Filter) find(line, fpd, tg uint64) int {
	base := int(line * wordsPerLine)
	w := f.words[base : base+wordsPerLine : base+wordsPerLine]
	for s := 0; s < wordsPerLine; s += 2 {
		if atomic.LoadUint64(&w[s])>>47 == fpd && atomic.LoadUint64(&w[s+1])>>tagShift == tg {
			return base + s
		}
	}
	return -1
}

// primary is h's home line. It uses hash bits 0..35, disjoint from the
// fingerprint (48..63) and tag (36..47), so keys sharing a line share no
// identity bits. Multiply-shift lets the table be any size.
func (f *Filter) primary(h uint64) uint64 {
	hi, _ := bits.Mul64(h<<28, f.lines) // bits 0..35 moved to the top
	return hi
}

// alt is the other candidate line for fp stored in line. It depends only on
// (line, fp), as in cuckoo filters, so a stored entry can be relocated
// without knowing its key: alt(l) = (x - l) mod lines is its own inverse.
// For one or two lines per fp, alt(l) == l; those keys get one line.
func (f *Filter) alt(line, fp uint64) uint64 {
	x, _ := bits.Mul64(mix64(fp*0x9e3779b97f4a7c15), f.lines)
	if x >= line {
		return x - line
	}
	return x + f.lines - line
}

func fingerprint(h uint64) uint64 {
	if fp := h >> 48; fp != 0 {
		return fp
	}
	return 1
}

// tag is 12 more fingerprint bits, kept in word1: with the 16-bit fp the
// slot identity is 28 bits. tagBusy is reserved; keys that would have it
// share the tag below.
func tag(h uint64) uint64 {
	t := h >> 36 & (1<<tagBits - 1)
	return t ^ (t+1)>>tagBits
}

// age is now-ts, or 0 if ts is ahead (a caller with a slightly stale clock).
func age(now, ts uint64) uint64 {
	if now < ts {
		return 0
	}
	return now - ts
}

func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}

// stashID packs a key's identity: primary line (< 2^36) | fp | tag.
func (f *Filter) stashID(h, fp, tg uint64) uint64 {
	return f.primary(h)<<28 | fp<<tagBits | tg
}

// slotID is stashID for the entry in a slot of line, without its key.
func (f *Filter) slotID(line, w0, w1 uint64) uint64 {
	fp := w0 >> 48
	if w0&dispBit != 0 {
		line = f.alt(line, fp)
	}
	return line<<28 | fp<<tagBits | w1>>tagShift
}
