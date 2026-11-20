package temporalfilter

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/scooter-sensei/clepsydra/go/internal/cpu"
)

// Hot entries. A key whose register would overflow (more than 31 events in
// its newest Horizon/24 unit, 12.4/s at a 60 s horizon, or a merge past an
// older bucket's width) is promoted instead: its register moves to a hot
// entry, the same 7 buckets with 32-bit counters in a side table of one
// cache line per entry, so its counts stay exact far past any real rate.
// The slot keeps word0 and the key's tag; word1's 48 register bits hold
// the entry's index with bucket 0 empty, which no register has (bucket 0
// counts the unit of the register's last event, so it holds at least 1).
// Lookups, the false-positive rate and the slot protocol are unchanged.
//
// The table is allocated on the first promotion (Config.Hot entries). An
// entry belongs to the identity (stashID) it records and is given up when
// its newest event is past the horizon (then no live slot or stash entry
// can refer to it), or when its key's rate falls (demote). With every entry
// in use a key isn't promoted: it saturates as without hot entries, and
// HotRefused counts it. Writers lock an entry (its version is odd while
// held); readers read it as a seqlock.

const (
	hotIdxShift = bucketBits0 // the index sits above bucket 0's empty bits
	hotIdxMask  = 1<<(48-hotIdxShift) - 1
	satNone     = math.MinInt64
)

type hotEntry struct {
	ver   atomic.Uint64 // odd while a writer holds the entry
	id    atomic.Uint64 // owner's identity; 0: free
	epoch atomic.Int64  // unit of bucket 0
	sat   atomic.Int64  // newest unit whose events may be missing (counts are floors there); satNone
	c0    atomic.Uint64 // bucket 0's count (hotCount) | hotClosed | generation (hotGen and up)
	c     [nBuckets - 1]atomic.Uint32
}

// Bucket 0's word: a touch in the entry's own unit adds to the count with
// one CAS while hotClosed is clear (add). A writer closes it on taking the
// entry and reopens it with the next generation on leaving, so a CAS that
// read the word before a change of owner or unit fails.
//
// The word's top 4 bits hold bucket 0's onset, as a register's onset field
// does (claims D29), so a reader takes it with the count and a snapshot
// carries it; the generation keeps the 27 bits between.
const (
	hotCount   = 1<<32 - 1
	hotClosed  = 1 << 32
	hotGen     = 1 << 33
	hotOnShift = 60
	hotOnMask  = onsetMask << hotOnShift
)

// isHot reports whether word1 points at a hot entry instead of holding a
// register.
func isHot(w1 uint64) bool {
	return w1&(1<<bucketBits0-1) == 0 && w1&regMask != 0
}

func hotIdx(w1 uint64) uint64 { return w1 >> hotIdxShift & hotIdxMask }

type hotTable struct {
	e       []hotEntry // index 0 unused
	mu      sync.Mutex
	free    []uint32
	next    uint64       // entries never handed out: next..len(e)-1
	hand    uint64       // reclaim scan position
	full    atomic.Int64 // unit in which an allocation last found no entry
	inUse   atomic.Int64
	demoted atomic.Uint64
}

func (f *Filter) hotTable() *hotTable {
	if t := f.hot.Load(); t != nil {
		return t
	}
	f.skMu.Lock()
	defer f.skMu.Unlock()
	if t := f.hot.Load(); t != nil {
		return t
	}
	w := alignedWords((f.hotN + 1) * int(unsafe.Sizeof(hotEntry{})/8))
	prefault(w)
	t := &hotTable{e: unsafe.Slice((*hotEntry)(unsafe.Pointer(&w[0])), f.hotN+1), next: 1}
	t.full.Store(satNone)
	f.hot.Store(t)
	return t
}

func (e *hotEntry) lock() uint64 {
	for spins := 0; ; spins++ {
		v := e.ver.Load()
		if v&1 == 0 && e.ver.CompareAndSwap(v, v+1) {
			e.c0.Or(hotClosed)
			return v + 1
		}
		if spins&63 == 63 {
			runtime.Gosched()
		}
	}
}

func (e *hotEntry) unlock(ver uint64) {
	c := e.c0.Load()
	e.c0.Store((c&^hotClosed+hotGen)&^hotOnMask | c&hotOnMask) // the generation wraps inside its bits
	e.ver.Store(ver + 1)
}

// view is a register's state, narrow or hot: bucket counts at unit e,
// bucket 0's onset in sixteenths of its unit (0 says nothing) and, for a hot
// entry, the newest unit whose events may be missing.
type view struct {
	e   int64
	c   [nBuckets]uint64
	on  uint64
	sat int64
	hot bool
}

// load reads the entry's state; the caller holds it or validates the
// version around the call.
func (e *hotEntry) load() (v view) {
	v.e, v.sat, v.hot = e.epoch.Load(), e.sat.Load(), true
	c0 := e.c0.Load()
	v.c[0], v.on = c0&hotCount, c0>>hotOnShift
	for j := 1; j < nBuckets; j++ {
		v.c[j] = uint64(e.c[j-1].Load())
	}
	return v
}

// store writes v into a held entry. A count past 32 bits (~1.7e9 events
// per second for a unit) is kept as a floor.
func (e *hotEntry) store(v *view) {
	for j, c := range v.c {
		if c > math.MaxUint32 {
			v.c[j], v.sat = math.MaxUint32, max(v.sat, v.e)
		}
		if j == 0 {
			e.c0.Store(e.c0.Load()&^(hotCount|hotOnMask) | v.c[0] | v.on<<hotOnShift)
		} else {
			e.c[j-1].Store(uint32(v.c[j]))
		}
	}
	e.epoch.Store(v.e)
	e.sat.Store(v.sat)
}

// read returns entry idx's state if it belongs to id.
func (t *hotTable) read(idx, id uint64) (view, bool) {
	e := &t.e[idx]
	for spins := 0; ; spins++ {
		if v1 := e.ver.Load(); v1&1 == 0 {
			owner := e.id.Load()
			v := e.load()
			if e.ver.Load() == v1 {
				return v, owner == id
			}
		}
		if spins&63 == 63 {
			runtime.Gosched()
		}
	}
}

// add counts n events at now (unit u) into entry idx of f's table t if it
// belongs to id. Events at a unit older than the entry's go to the bucket
// that covers it. If slot is the word1 that points here (old), a touch that
// starts a new unit with every bucket at most half the register's width
// hands the counts back to the slot as a register and frees the entry
// (demote).
func (t *hotTable) add(f *Filter, idx, id, n uint64, u int64, now uint64, slot *uint64, old uint64) bool {
	e := &t.e[idx]
	for { // the usual case, a touch in the entry's own unit: one CAS
		c := e.c0.Load()
		if c&hotClosed != 0 || e.id.Load() != id || e.epoch.Load() != u || c&hotCount+n > hotCount {
			break
		}
		if e.c0.CompareAndSwap(c, c+n) {
			return true
		}
	}
	ver := e.lock()
	if e.id.Load() != id {
		e.unlock(ver)
		return false
	}
	if u == e.epoch.Load() {
		if c := e.c0.Load()&hotCount + n; c <= hotCount {
			e.c0.Store(e.c0.Load()&^hotCount | c)
			e.unlock(ver)
			return true
		}
	}
	v := e.load()
	switch {
	case u > v.e:
		advanceWide(&v.c, v.e, u)
		v.e, v.on = u, f.onsetOf(now, u) // these events start bucket 0
		v.c[0] += n
	case u == v.e:
		v.c[0] += n
		v.on = min(v.on, f.onsetOf(now, u)) // a caller behind the onset
	default:
		var x [nBuckets]uint64
		x[0] = n
		advanceWide(&x, u, v.e)
		for j := range x {
			v.c[j] += x[j]
		}
	}
	if slot != nil && v.e == u && demotable(&v, u) {
		var r register
		for j := range r {
			r[j] = v.c[j]
		}
		nw := old&^(1<<tagShift-1) | v.on<<onsetShift | r.pack()
		if f.demoteSlot(slot, old, nw, u) {
			e.id.Store(0)
			e.unlock(ver)
			t.putFree(idx)
			t.demoted.Add(1)
			return true
		}
	}
	e.store(&v)
	e.unlock(ver)
	return true
}

// demoteSlot is a demotion's swap of word1 (slot) from the hot pointer p to
// the register nw, at unit u, taken only while word0's last touch is in u: by
// one 16-byte CAS of both words, word0 compared as loaded, or else under a
// hold of word1, word0 read under it. A register must never sit beside a
// last touch of another unit, and a touch of the hot key into a later unit
// moves word0 while word1 points here (touch's stamp, which compares word1
// or holds it in turn). A demotion that loses the race is left for a later
// unit.
func (f *Filter) demoteSlot(slot *uint64, p, nw uint64, u int64) bool {
	p0 := (*uint64)(unsafe.Add(unsafe.Pointer(slot), -8))
	if cpu.HasCAS2 {
		w0 := atomic.LoadUint64(p0)
		return f.unitOf(w0&tsMask) == u && cpu.CAS2(p0, w0, p, w0, nw)
	}
	m := f.moveOf(p >> tagShift)
	m.begun.Add(1)
	defer m.done.Add(1)
	if !atomic.CompareAndSwapUint64(slot, p, busy) {
		return false
	}
	if f.unitOf(atomic.LoadUint64(p0)&tsMask) != u {
		atomic.StoreUint64(slot, p)
		return false
	}
	atomic.StoreUint64(slot, nw)
	return true
}

// demotable: v fits a register at half of every bucket's width (so the key
// isn't promoted again at once), and no event of the register's span is
// missing.
func demotable(v *view, u int64) bool {
	if v.sat != satNone && v.sat+2*unitsPerHorizon >= u {
		return false
	}
	for j, c := range v.c {
		if c > (1<<bucketBits[j]-1)/2 {
			return false
		}
	}
	return true
}

// putFree puts a closed entry (its owner 0) on the free list.
func (t *hotTable) putFree(idx uint64) {
	t.inUse.Add(-1)
	t.mu.Lock()
	t.free = append(t.free, uint32(idx))
	t.full.Store(satNone)
	t.mu.Unlock()
}

// hotAlloc takes a free entry for id, holding v, or returns 0 if none is
// free.
func (f *Filter) hotAlloc(id uint64, v *view, now uint64) uint64 {
	t := f.hotTable()
	u := f.unitOf(now)
	if t.full.Load() == u { // found none in this unit already: don't search again
		f.hotRefused.Add(1)
		return 0
	}
	t.mu.Lock()
	idx := uint64(0)
	switch {
	case len(t.free) > 0:
		idx = uint64(t.free[len(t.free)-1])
		t.free = t.free[:len(t.free)-1]
	case t.next < uint64(len(t.e)):
		idx = t.next
		t.next++
	default:
		idx = t.reclaim(u)
	}
	if idx == 0 {
		t.full.Store(u)
	}
	t.mu.Unlock()
	if idx == 0 {
		f.hotRefused.Add(1)
		return 0
	}
	e := &t.e[idx]
	ver := e.lock()
	e.id.Store(id)
	e.store(v)
	e.unlock(ver)
	t.inUse.Add(1)
	return idx
}

// reclaim takes an entry whose newest event is past the horizon at unit u,
// so no live slot or stash entry can refer to it (caller holds t.mu).
func (t *hotTable) reclaim(u int64) uint64 {
	dead := u - unitsPerHorizon - 2
	n := uint64(len(t.e) - 1)
	for range min(n, 256) {
		t.hand = t.hand%n + 1
		e := &t.e[t.hand]
		if e.id.Load() == 0 || e.epoch.Load() >= dead {
			continue // free ones are on the free list
		}
		ver := e.lock()
		ok := e.id.Load() != 0 && e.epoch.Load() < dead
		if ok {
			e.id.Store(0)
		}
		e.unlock(ver)
		if ok {
			t.inUse.Add(-1)
			return t.hand
		}
	}
	return 0
}

// hotClose gives up entry idx if it belongs to id and returns its last
// state. A writer that loses the race finds the id changed and looks again.
func (f *Filter) hotClose(idx, id uint64) (v view, ok bool) {
	t := f.hot.Load()
	if t == nil {
		return view{}, false
	}
	e := &t.e[idx]
	ver := e.lock()
	if e.id.Load() == id {
		v, ok = e.load(), true
		e.id.Store(0)
	}
	e.unlock(ver)
	if ok {
		t.putFree(idx)
	}
	return v, ok
}

// promote moves the register of the key in slot i (word index; w0 and old:
// its words, last: its lastTouch before this touch) to a hot entry with
// this touch counted, instead of letting the touch spill. It returns the
// slot's new word1, or ok false if no entry is free, word1 changed
// meanwhile, or the slot is another key's (claims A29).
func (f *Filter) promote(i int, w0, old, tg, last, now uint64) (w1 uint64, ok bool) {
	w1, idx, id := f.hotEntryFor(i, w0, old, tg, last, now)
	if w1 == 0 {
		return 0, false
	}
	// Published under a hold, counted as a move, not by a CAS from old: a
	// slot handed to a key with old's tag can hold old bit for bit, and
	// would point to this key's entry. Under the hold, word0 says whose the
	// slot is.
	m := f.moveOf(tg)
	m.begun.Add(1)
	defer m.done.Add(1)
	if !atomic.CompareAndSwapUint64(&f.words[i+1], old, busy) {
		f.hotClose(idx, id) // never published
		return 0, false
	}
	if atomic.LoadUint64(&f.words[i])>>47 != w0>>47 {
		atomic.StoreUint64(&f.words[i+1], old)
		f.hotClose(idx, id)
		return 0, false
	}
	atomic.StoreUint64(&f.words[i+1], w1)
	f.promoted.Add(1)
	return w1, true
}

// promoteHeld is promote for a touch that holds slot i's word1 busy
// (realign), which publishes w1 itself.
func (f *Filter) promoteHeld(i int, w0, old, tg, last, now uint64) (w1 uint64, ok bool) {
	if w1, _, _ = f.hotEntryFor(i, w0, old, tg, last, now); w1 == 0 {
		return 0, false
	}
	f.promoted.Add(1)
	return w1, true
}

// hotEntryFor takes a free hot entry for the key in slot i (word0 w0, its
// register old, last touched at last) holding old's counts and one event at
// now: word1 pointing to it (0 if none is free), its index and identity.
func (f *Filter) hotEntryFor(i int, w0, old, tg, last, now uint64) (w1, idx, id uint64) {
	if f.hotN == 0 {
		return 0, 0, 0
	}
	if t := f.hot.Load(); t != nil && t.full.Load() == f.unitOf(now) {
		f.hotRefused.Add(1) // every entry was in use a moment ago: don't search again this unit
		return 0, 0, 0
	}
	id = f.slotID(uint64(i/wordsPerLine), w0, old)
	v := view{e: f.unitOf(last), on: old >> onsetShift & onsetMask, sat: satNone, hot: true}
	r := unpack(old & regMask)
	for j := range r {
		v.c[j] = r[j]
	}
	if w0&satBit != 0 { // it overflowed before: its counts up to now are floors
		v.sat = v.e
	}
	switch u := f.unitOf(now); {
	case u > v.e:
		advanceWide(&v.c, v.e, u)
		v.e, v.on = u, f.onsetOf(now, u) // this touch starts bucket 0
	case u == v.e:
		v.on = min(v.on, f.onsetOf(now, u))
	default:
		v.on = 0 // an event from an earlier unit, counted at v.e
	}
	v.c[0]++
	if idx = f.hotAlloc(id, &v, now); idx == 0 {
		return 0, 0, 0
	}
	return tg<<tagShift | idx<<hotIdxShift, idx, id
}

// viewOf returns the register state that word1 holds or points to, for the
// key with identity id and lastTouch last. ok is false if w1 points to an
// entry that is no longer the key's.
func (f *Filter) viewOf(id, w1, last uint64) (v view, ok bool) {
	if isHot(w1) {
		if t := f.hot.Load(); t != nil {
			return t.read(hotIdx(w1), id)
		}
		return view{}, false
	}
	v.e, v.on = f.unitOf(last), w1>>onsetShift&onsetMask
	v.c[0] = w1 & 0x1F
	v.c[1] = (w1 >> 5) & 0x1F
	v.c[2] = (w1 >> 10) & 0x3F
	v.c[3] = (w1 >> 16) & 0x7F
	v.c[4] = (w1 >> 23) & 0xFF
	v.c[5] = (w1 >> 31) & 0xFF
	v.c[6] = (w1 >> 39) & 0x1FF
	v.sat = satNone
	return v, true
}

// hotGone: w1 points to a hot entry that is no longer the key id's. An entry
// is reclaimed once its newest event is past the horizon at the reclaiming
// call's clock, which can be ahead of another caller's by more: a history
// out of the table (stash, wait room) whose entry went that way is gone,
// though its last touch is within a horizon of a lagging caller's clock.
// Restored as is, the touch restoring it went nowhere, and the slot pointed
// at another key's entry (claims A25).
func (f *Filter) hotGone(id, w1 uint64) bool {
	if !isHot(w1) {
		return false
	}
	_, ok := f.viewOf(id, w1, 0)
	return !ok
}

// slotDrop gives up the hot entry, if any, of a slot's history that the
// slot itself discards past the horizon (a touch's reset, the sweep, an
// insert over the expired occupant): id is the history's identity, w1 the
// slot's word1 as taken. A slot is its key's only live reference to the
// entry, so nothing else counts in it any more; left held, it stayed in
// HotInUse until a reclaim, and a lagging touch's add that came after
// landed in it, where nothing pointed, instead of failing and looking
// again. The stash's and the wait room's expired ways don't close theirs:
// such a way can share its entry with the key's live slot, once the entry
// was reclaimed and given to the same key again.
func (f *Filter) slotDrop(id, w1 uint64) {
	if isHot(w1) {
		f.hotClose(hotIdx(w1), id)
	}
}

// testHookRestoreAdd, if set, runs in hotAdd when it counts a hot restore's
// touch (slot nil: miss's add, after the history went in or waits with the
// touch's push), before the add (lab/sw W11).
var testHookRestoreAdd func()

// hotAdd counts one event at now into the entry word1 points to. slot, if
// not nil, is the word1 (holding w1) the touch found the key by: the entry
// may then be handed back to it as a register (demote).
func (f *Filter) hotAdd(w1, id, now uint64, slot *uint64) bool {
	if slot == nil && testHookRestoreAdd != nil {
		testHookRestoreAdd()
	}
	t := f.hot.Load()
	return t != nil && t.add(f, hotIdx(w1), id, 1, f.unitOf(now), now, slot, w1)
}

// advanceWide is register.advance for a hot entry's uncapped counts.
func advanceWide(r *[nBuckets]uint64, u0, u1 int64) {
	if u1 <= u0 {
		return
	}
	if u1-u0 > 2*unitsPerHorizon {
		*r = [nBuckets]uint64{}
		return
	}
	var starts [nBuckets]int64
	for j := range starts {
		starts[j] = bucketStart(u1, j)
	}
	var out [nBuckets]uint64
	j := 0
	for k := range r {
		if r[k] == 0 {
			continue
		}
		start := bucketStart(u0, k)
		for j < nBuckets && start < starts[j] {
			j++
		}
		if j == nBuckets {
			break // older buckets only start earlier: all dropped
		}
		out[j] += r[k]
	}
	*r = out
}

// Promoted counts keys whose register moved to a hot entry.
func (f *Filter) Promoted() uint64 { return f.promoted.Load() }

// HotRefused counts promotions refused because every hot entry was in use:
// those keys saturate (Saturated) as they would without hot entries.
// Alert on it.
func (f *Filter) HotRefused() uint64 { return f.hotRefused.Load() }

// Demoted counts hot entries handed back to their key's slot once its rate
// fell (every bucket at most half the register's width).
func (f *Filter) Demoted() uint64 {
	if t := f.hot.Load(); t != nil {
		return t.demoted.Load()
	}
	return 0
}

// HotInUse is the number of hot entries held now.
func (f *Filter) HotInUse() int {
	if t := f.hot.Load(); t != nil {
		return int(t.inUse.Load())
	}
	return 0
}
