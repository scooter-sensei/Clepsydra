package temporalfilter

import (
	"math/bits"
	"sync/atomic"
	"unsafe"

	"github.com/scooter-sensei/clepsydra/go/internal/cpu"
)

// Buzz entries: an exact log for a conditional touch whose refusal was in
// doubt (claims A46; the lab arm B12b, the feeding buzz: a bat calls faster
// in the last moments of a strike, buying resolution where a decision turns
// on it).
//
// TouchIfUnder refuses while Upper is at the limit, and Upper counts the
// bucket the window's edge cuts as whole, so on windows shorter than a few
// units a strict limiter admits 30-65% of its limit (paper §4.2). A refusal
// is uncertain when the events certainly inside the window (the buckets
// wholly inside it, and the last touch) are under the limit: the register
// can't say whether the key is over, and every false denial is such a
// refusal. On an uncertain refusal of a limit of buzzCap or less, the key
// gets a buzz entry: the slot's two words as they are, and from then on the
// tick of every event TouchIfUnder admits for it, the newest buzzCap of
// them, in 16-bit steps of Horizon/32768.
//
// Trust. An entry answers only while the key's slot holds, bit for bit, what
// the entry's last admission left (the displaced bit aside, which a
// relocation flips), with a register that is not full in every bucket and
// not saturated. Every event the filter counts for such a key changes a
// word: some bucket's count, or the last touch. So while they match, the
// events since the entry's start are exactly the logged ones. Anything else
// that counts for the key (a plain touch, a promotion, an eviction whose
// history is shed) changes a word; the entry is dropped, and the key is
// decided by its register again, as without entries. An admission records
// as the entry's words what one event at now makes of the words read before
// it (nextReg), and the key's next conditional touch checks them: a plain
// touch between the read and the admission leaves another count, and the
// entry is dropped there, before it decides.
//
// Decision. With a trusted entry and a window that starts after the tick
// before which the log may be missing events (its start, then the newest
// event it dropped), the count is the log's: an event is inside unless its
// step ends before the window starts; the overflow table's part of Upper is
// added as without entries. A limit of buzzCap or less never needs an event
// the log dropped: if fewer than the limit of its newest buzzCap are inside
// the window, the next older one is outside it. So the log's count is at
// least the truth, and a request is admitted only under the limit, as
// Upper guarantees. (The one way an event counted for the key leaves no
// trace in its words is the documented shedding of a history under
// SpillMin, which Upper doesn't see either.)
//
// Release. An event a horizon old leaves the ring at the next admission, so
// no step is read two horizons on (a step repeats every 65,536). A way is
// free once its entry's newest event is older than the window it was given
// for and eight units more; a key that finds none of its block's ways free
// is decided by its register.
//
// The table is allocated on the first uncertain refusal: one entry per 128
// slots, in 512 B blocks of buzzWays ways (the ways a key may use): the
// ways' owners in the block's first line, so a key with no entry reads one
// line, then the entries, 56 B each. With 64 blocks or more each block
// belongs to one conditional stripe, whose lock (condMu) guards it; with
// fewer, blocks are shared, each under a lock of its own stripe. Nothing but
// TouchIfUnder reads it, and only for a limit of buzzCap or less;
// ConstantTime filters keep TouchIfUnder as without entries. A snapshot
// carries it.

const (
	buzzCap     = 16 // events a log holds: a limit up to it is decided by the log alone
	buzzWays    = 8  // ways a key may use: one block
	buzzStripes = 64 // = len(condMu)
	buzzNShift  = 46 // meta: before (46 bits) | n (5) | head (4) | units to release (5)
	buzzHShift  = 51
	buzzUShift  = 55
)

type buzzEntry struct {
	w0, w1 uint64    // the key's slot as the entry's last admission, or its start, left it
	meta   uint64    // the tick at or before which the log may miss events, the ring's fill and head, the release span
	ev     [4]uint64 // a ring of buzzCap steps (tick / q, mod 2^16)
}

// buzzBlock is the ways a key may use: their owners in one line, then the
// entries.
type buzzBlock struct {
	ids [buzzWays]uint64 // each way's owner (stashID); 0: free
	e   [buzzWays]buzzEntry
}

func (e *buzzEntry) before() uint64 { return e.meta & tsMask }
func (e *buzzEntry) n() int         { return int(e.meta >> buzzNShift & 31) }
func (e *buzzEntry) head() int      { return int(e.meta >> buzzHShift & 15) }
func (e *buzzEntry) units() uint64  { return e.meta >> buzzUShift & 31 }

func (e *buzzEntry) step(k int) uint64 { return e.ev[k/4] >> (16 * (k % 4)) & 0xFFFF }

// stepAt is the step index (tick / q) of the step at ring position k,
// decoded against rq, the step of a tick no event of the log is after (steps
// cover two horizons, and no step in the ring is older than a horizon
// before the log's newest event); 0 if it would be before tick 0.
func (e *buzzEntry) stepAt(k int, rq uint64) uint64 {
	d := (rq - e.step(k)) & 0xFFFF
	if d > rq {
		return 0
	}
	return rq - d
}

// count is the log's Upper for [now-window, now]: an event counts unless its
// step ends before the window starts. A step s ends at (s+1)q - 1, so it
// reaches the window's start from exactly when s >= from/q.
func (e *buzzEntry) count(now, window, q uint64) int {
	rq := max(now, e.w0&tsMask) / q
	fq := (now - min(now, window)) / q
	n, head := 0, e.head()
	for k := range e.n() {
		if e.stepAt((head-1-k)&15, rq) >= fq {
			n++
		}
	}
	return n
}

// covers: every event the log may miss is outside [now-window, now].
func (e *buzzEntry) covers(now, window uint64) bool {
	return now >= window && e.before() < now-window
}

// log records an admitted event at tick t. Events a horizon old (their step
// ends a horizon before t) leave the ring first; a full ring drops its
// oldest step, and the log may from then on miss events up to that step's
// end.
func (e *buzzEntry) log(t, q, horizon uint64) {
	before, n, head := e.before(), e.n(), e.head()
	rq := max(t, e.w0&tsMask) / q
	if t >= horizon+q { // else no step can be a horizon old
		for fq := (t - horizon) / q; n > 0; n-- {
			if e.stepAt((head-n)&15, rq) >= fq {
				break
			}
		}
	}
	if n == buzzCap {
		before = max(before, (e.stepAt(head, rq)+1)*q-1) & tsMask
	} else {
		n++
	}
	sh := 16 * uint(head%4)
	e.ev[head/4] = e.ev[head/4]&^(0xFFFF<<sh) | (t/q&0xFFFF)<<sh
	e.meta = before | uint64(n)<<buzzNShift | uint64((head+1)&15)<<buzzHShift | e.units()<<buzzUShift
}

type buzzTable struct {
	b []buzzBlock
	// used: a bit per block, set before an entry goes in and cleared when
	// the block's last entry leaves, by whoever guards the block. A key
	// whose block's bit is clear holds no entry, and its conditional touch
	// reads nothing of the table: most keys, when few are contested.
	used []atomic.Uint64
	q    uint64 // ticks per step
	mu   [buzzStripes]paddedMutex
	// given: entries handed out; dropped: entries whose slot no longer
	// matched; decided: conditional touches a log decided.
	given, dropped, decided atomic.Uint64
}

// block returns the index of the block of the key with hash h and identity
// id, and the lock that guards it, or nil. With 64 blocks or more, the
// blocks are split in 64 runs, one per conditional stripe (h's top 6 bits,
// condMu's index), so only keys whose conditional touches condMu already
// serializes share a block. With fewer, blocks are shared across stripes
// and locked.
func (t *buzzTable) block(h, id uint64) (uint64, *paddedMutex) {
	n := uint64(len(t.b))
	x := mix64(id ^ 0x9e3779b97f4a7c15)
	if n < buzzStripes {
		k, _ := bits.Mul64(x, n)
		return k, &t.mu[k]
	}
	s := h >> (64 - 6)
	lo := s * n / buzzStripes
	k, _ := bits.Mul64(x, (s+1)*n/buzzStripes-lo)
	return lo + k, nil
}

// mayHold: block k may hold an entry.
func (t *buzzTable) mayHold(k uint64) bool { return t.used[k>>6].Load()>>(k&63)&1 != 0 }

// leave frees way i of block k, clearing the block's bit if it was the
// last (the caller guards the block).
func (t *buzzTable) leave(k uint64, i int) {
	b := &t.b[k]
	b.ids[i] = 0
	for _, x := range &b.ids {
		if x != 0 {
			return
		}
	}
	t.used[k>>6].And(^(1 << (k & 63)))
}

// mark sets block k's bit from its owners (a restore).
func (t *buzzTable) mark(k uint64) {
	for _, x := range &t.b[k].ids {
		if x != 0 {
			t.used[k>>6].Or(1 << (k & 63))
			return
		}
	}
}

// buzzTable returns the table, allocated on first use: one entry per 128
// slots.
func (f *Filter) buzzTable() *buzzTable {
	if t := f.buzz.Load(); t != nil {
		return t
	}
	f.skMu.Lock()
	defer f.skMu.Unlock()
	if t := f.buzz.Load(); t != nil {
		return t
	}
	blocks := max(2, f.lines*slotsPerLine/(128*buzzWays))
	w := alignedWords(int(blocks * uint64(unsafe.Sizeof(buzzBlock{})/8)))
	prefault(w)
	t := &buzzTable{b: unsafe.Slice((*buzzBlock)(unsafe.Pointer(&w[0])), blocks), used: make([]atomic.Uint64, (blocks+63)/64),
		q: max(1, (f.horizon+1<<15-1)>>15)}
	f.buzz.Store(t)
	return t
}

// certain is the number of events of the register w1 (last touched at last)
// that lie inside [now-window, now] wherever they fall in their buckets: the
// buckets wholly inside it, and the last touch.
func (f *Filter) certain(w1, last, now, window uint64) int {
	if age(now, last) > window {
		return 0
	}
	v, _ := f.viewOf(0, w1, last)
	unit := float64(f.unit)
	from := float64(now) - float64(window)
	n := 0
	for j := range nBuckets {
		if from > float64(bucketStart(v.e, j))*unit {
			break
		}
		n += int(v.c[j])
	}
	return max(1, n)
}

// touchIfBuzz is TouchIfUnderAt for a limit of buzzCap or less, outside
// ConstantTime (the caller holds h's conditional stripe). Its first half is
// CountAt and the strict recipe, as without entries, keeping the slot's
// words and the overflow table's part of Upper.
func (f *Filter) touchIfBuzz(h, window, limit, now uint64) (Result, bool) {
	fp, tg := fingerprint(h), tag(h)
	id := f.stashID(h, fp, tg)
	t := f.buzz.Load()
	var k uint64
	var mu *paddedMutex
	held := false // h's block may hold an entry (h's own, if any, put there under h's stripe)
	if t != nil {
		k, mu = t.block(h, id)
		if held = t.mayHold(k); held && mu == nil { // its owners, loading while the slot is looked up (condMu guards them)
			var line uint64
			cpu.PrefetchLines(unsafe.Pointer(&t.b[k].ids), &line, 1)
		}
	}
	w0, w1, src := f.read(h, now, -1)
	last := w0 & tsMask
	live := src != 0 && age(now, last) <= f.horizon
	var r Result
	if live {
		r = f.estimateAt(h, w1, last, now, window, w0&satBit != 0)
		r.Seen = r.Count > 0
	} else {
		r.Degraded = f.lostRecently(now)
	}
	over := r.Upper
	if f.sketchLive(now) {
		f.addSketch(&r, h, window, now)
	}
	over = r.Upper - over
	refuse := r.Saturated || f.skW == 0 && r.Degraded && !r.Seen || uint64(max(r.Upper, 0)) >= limit

	// A register the two words answer for: no hot entry, not saturated, not
	// full in every bucket.
	plain := live && !isHot(w1) && w0&satBit == 0 && ^w1&regMask != 0
	win := min(window, f.horizon)
	if !held { // no entry: as without entries, unless the refusal is in doubt
		if !refuse {
			return f.TouchAt(h, window, now), true
		}
		if !f.uncertain(plain, r, w1, last, now, win, over, limit) {
			return r, false
		}
		if t == nil {
			t = f.buzzTable()
			k, mu = t.block(h, id)
		}
	}
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	b := &t.b[k]
	way := -1
	if held {
		for i, x := range &b.ids {
			if x == id {
				way = i
				break
			}
		}
	}
	var e *buzzEntry
	if way >= 0 {
		if e = &b.e[way]; !(plain && (w0^e.w0)&^dispBit == 0 && w1 == e.w1) {
			t.leave(k, way) // something else counted for the key, or its slot is gone
			e = nil
			t.dropped.Add(1)
		}
	}
	if e != nil && e.covers(now, win) {
		r.Upper = e.count(now, win, t.q) + over
		r.Count = min(r.Count, r.Upper)
		refuse = r.Saturated || uint64(r.Upper) >= limit
		t.decided.Add(1)
	}
	if refuse {
		if e == nil && f.uncertain(plain, r, w1, last, now, win, over, limit) {
			for i := range b.ids {
				if b.ids[i] == 0 || age(now, b.e[i].w0&tsMask) > b.e[i].units()*f.unit {
					units := min(31, (win+f.unit-1)/f.unit+8)
					t.used[k>>6].Or(1 << (k & 63))
					b.ids[i], b.e[i] = id, buzzEntry{w0: w0, w1: w1, meta: max(now, last) | units<<buzzUShift}
					t.given.Add(1)
					break
				}
			}
		}
		return r, false
	}
	var want uint64
	var ovf bool
	if e != nil {
		want, ovf = f.nextReg(w1, tg, last, now, false) // what the touch below makes of the words read
	}
	res := f.TouchAt(h, window, now)
	if e != nil {
		if ovf { // the touch saturates or promotes the key: its words no longer answer for it
			t.leave(k, way)
			t.dropped.Add(1)
		} else {
			stamp := max(now, last)
			e.log(stamp, t.q, f.horizon)
			e.w0, e.w1 = w0&^tsMask|stamp, want // checked at the key's next conditional touch
		}
	}
	return res, true
}

// uncertain: a refusal (r) of a key whose words answer for it (plain) at a
// limit of buzzCap or less, though the events certainly inside the window,
// with the overflow table's part (over), are under the limit.
func (f *Filter) uncertain(plain bool, r Result, w1, last, now, win uint64, over int, limit uint64) bool {
	return plain && !r.Saturated && uint64(f.certain(w1, last, now, win)+over) < limit
}
