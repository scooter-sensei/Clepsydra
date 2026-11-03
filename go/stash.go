package temporalfilter

import (
	"math/bits"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"
)

// Where a key's history was found.
const (
	srcTable = iota + 1
	srcStash
	srcRetry // a waiting key was being committed to the table: look there again
	srcRoom  // a waiting key taken out of the wait room (recall with take): its move is under way
)

// recall finds h's evicted history in the stash, or (Background) h waiting
// in the wait room, removed if take. The caller holds h's claim stripe. A
// key taken out of the wait room (srcRoom) counts as a move of its stripe
// from before it leaves until miss has placed it: lock-free lookups gate
// on the room's wait bits, which the placer clears once a set holds no
// waiting key, so a key in neither the table nor the room meanwhile would
// read absent.
func (f *Filter) recall(h, fp, tg, now uint64, take bool) (w0, w1 uint64, src int) {
	id := f.stashID(h, fp, tg)
	if !f.ct {
		if p := f.room.Load(); p != nil && p.waiting(id) {
			var mv *moveCount
			if take {
				mv = f.moveOf(tg)
			}
			w0, w1, ok, retry := f.roomGet(p, id, now, mv)
			if retry {
				return 0, 0, srcRetry
			}
			if ok && take {
				return w0, w1, srcRoom
			}
			if ok {
				return w0, w1, srcStash
			}
		}
		if now > f.stashUntil.Load() {
			return 0, 0, 0
		}
	}
	if w0, w1, ok := f.stashGet(id, now, take); ok {
		return w0, w1, srcStash
	}
	return 0, 0, 0
}

// stashMay: h's history may be outside the table at now: an evicted entry
// live in the stash, or (Background) h waiting in the wait room, whose set's
// bit is set while it waits.
func (f *Filter) stashMay(h, fp, tg, now uint64) bool {
	if now <= f.stashUntil.Load() {
		return true
	}
	p := f.room.Load()
	return p != nil && p.waiting(f.stashID(h, fp, tg))
}

// settled: no move of the stripe whose counts are m overlapped a lookup that
// read them as b and d before its table scan and its recall. A waiting key
// the placer commits to the table between the two (placeRoomKey, without
// the key's stripe) counts as such a move, so a key found in neither is
// absent only if settled. Without a wait room no key moves that way.
func (f *Filter) settled(m *moveCount, b, d uint64) bool {
	return f.room.Load() == nil || b == d && m.begun.Load() == b
}

// roomGet is tableGet in the wait room (Background), where an entry being
// committed to the table (w1 busy, placeRoomKey) is waited out; retry then
// tells the caller to look in the table again if the entry went there. With
// mv (the key's move counts) it takes the entry out, its move begun first.
func (f *Filter) roomGet(p *stashTable, id, now uint64, mv *moveCount) (w0, w1 uint64, ok, retry bool) {
	take := mv != nil
	set, mu := p.set(id)
	for {
		busyNow := false
		mu.Lock()
		for i := range set {
			e := &set[i]
			if e.id != id {
				continue
			}
			if e.w1>>tagShift == tagBusy {
				busyNow = true
				break
			}
			if age(now, e.w0&tsMask) > f.horizon {
				if take { // a fresh insert follows: the placer mustn't install this one too
					f.stashWrite(p, e, stashEntry{})
				}
				break
			}
			w0, w1 = e.w0, e.w1
			if take {
				mv.begun.Add(1)
				f.stashWrite(p, e, stashEntry{})
			}
			mu.Unlock()
			if take && testHookTaken != nil {
				testHookTaken()
			}
			return w0, w1, true, false
		}
		mu.Unlock()
		if !busyNow {
			return 0, 0, false, retry
		}
		retry = true
		runtime.Gosched()
	}
}

// lostRecently: without an overflow table, some history was lost within a horizon.
func (f *Filter) lostRecently(now uint64) bool {
	la := f.lostAt.Load()
	return la != 0 && age(now, la) <= f.horizon
}

// placeRoomKey places waiting key e (as its set held it when read) at the
// clock of its last touch, without its claim stripe: the search blocks no
// first touch, and the key stays in the room meanwhile, where its touches
// and counts find it. put commits it: it marks the room entry busy (w1
// busy; a recall of the key waits that out, then looks in the table
// again), installs the entry's words as they are now, and removes the entry,
// or restores it if the install lost its race. The commit counts as a move
// of the key's stripe, so a lookup that misses the table meanwhile looks
// again. If a touch of the key took it out of the room first, put fails and
// the key is that touch's to place.
func (f *Filter) placeRoomKey(p *stashTable, e stashEntry) {
	id := e.id
	fp := id >> tagBits & (1<<16 - 1)
	m := f.moveOf(id & (1<<tagBits - 1))
	pl := id >> 28
	put := func(i int, w0, w1, mine0, _ uint64) bool {
		set, mu := p.set(id)
		mu.Lock()
		var c *stashEntry
		for k := range set {
			if set[k].id == id && set[k].w0&deferMark != 0 && set[k].w1>>tagShift != tagBusy {
				c = &set[k]
			}
		}
		if c == nil {
			mu.Unlock()
			return false
		}
		c0, c1 := c.w0, c.w1
		c.w1 = busy
		mu.Unlock()
		if testHookPlacing != nil {
			testHookPlacing()
		}
		m.begun.Add(1)
		ok := f.install(i, w0, w1, c0&^deferMark|mine0&dispBit, c1)
		mu.Lock()
		if ok {
			f.stashWrite(p, c, stashEntry{})
		} else {
			c.w1 = c1
		}
		mu.Unlock()
		m.done.Add(1)
		return ok
	}
	f.claimAt([2]uint64{pl, f.alt(pl, fp)}, [2]uint64{e.w0 &^ deferMark, e.w0 | dispBit}, e.w1, e.w0&tsMask, noStripe, claimFull, put)
}

// bgKick (Background) starts the placer goroutine unless it is running.
func (f *Filter) bgKick() {
	if !f.bgManual && !f.placing.Load() && f.placing.CompareAndSwap(false, true) {
		go f.bgPlace()
	}
}

const (
	bgNap    = 50 * time.Microsecond
	bgLinger = 20 // naps with nothing waiting before the placer exits
)

// bgPlace is the Background placer: it places waiting keys while any wait
// (it never reads the filter's clock: see placeRoomKey), and exits after
// bgLinger naps with nothing to place. A key that starts waiting as it
// exits sees placing false and starts another, or is seen here (both flags
// are sequentially consistent).
func (f *Filter) bgPlace() {
	for {
		for idle := 0; idle < bgLinger; {
			if f.deferred.Load() > 0 && f.placeNext(f.room.Load(), 0) {
				idle = 0
				continue
			}
			idle++
			time.Sleep(bgNap)
		}
		f.placing.Store(false)
		if f.deferred.Load() == 0 || !f.placing.CompareAndSwap(false, true) {
			return
		}
	}
}

// Eviction stash: a 4-way set-associative array of evicted entries, keyed
// by the entry's full identity (primary line, fp, tag: the same bits the
// table compares, so the stash adds no false positives beyond the table's
// own). Evictions are rare and slow already, so it uses plain striped locks.

const (
	stashStripes = 64
	stashWays    = 4
)

type stashEntry struct {
	id, w0, w1 uint64 // id 0 = empty; w0's displaced bit is clear
}

// stashSet is id's set in the stash and its lock (stashTable.set): none
// before the stash is allocated.
func (f *Filter) stashSet(id uint64) ([]stashEntry, *paddedMutex) {
	if p := f.stash.Load(); p != nil {
		return p.set(id)
	}
	return nil, nil
}

// stashTable: sets of stashWays entries, and a bit per set that holds a key
// waiting for a slot, set and cleared under the set's lock. The eviction
// stash is one (its waiting keys are ConstantTime's); the wait room is
// another (Background's, outside ConstantTime).
type stashTable struct {
	e    []stashEntry
	wait []atomic.Uint64
	n    uint64                     // sets
	mu   *[stashStripes]paddedMutex // the filter's, which exist before the table
	at   atomic.Uint64              // the next set to place waiting keys from
}

func newStashTable(sets uint64, mu *[stashStripes]paddedMutex) *stashTable {
	e := make([]stashEntry, sets*stashWays)
	prefault(unsafe.Slice((*uint64)(unsafe.Pointer(&e[0])), len(e)*3))
	return &stashTable{e: e, wait: make([]atomic.Uint64, (sets+63)/64), n: sets, mu: mu}
}

// set returns id's set and its lock.
func (p *stashTable) set(id uint64) ([]stashEntry, *paddedMutex) {
	i, _ := bits.Mul64(mix64(id), p.n)
	return p.e[i*stashWays : (i+1)*stashWays : (i+1)*stashWays], &p.mu[i%stashStripes]
}

// waiting: id's set may hold a key waiting for a slot (its bit is set from
// the first such key's write until the placer finds the set without one).
func (p *stashTable) waiting(id uint64) bool {
	i, _ := bits.Mul64(mix64(id), p.n)
	return p.wait[i/64].Load()&(1<<(i%64)) != 0
}

// allocStash allocates the stash on the first eviction, so a filter that
// never evicts doesn't pay for it.
func (f *Filter) allocStash() {
	if f.stash.Load() != nil {
		return
	}
	f.skMu.Lock()
	defer f.skMu.Unlock()
	if f.stash.Load() == nil {
		f.stash.Store(newStashTable(f.stashN, &f.stashMu))
	}
}

// waitTable is where keys wait for a slot, allocated on first use (nil:
// nowhere). ConstantTime: the stash, whose set every call reads anyway.
// Background: the wait room, a small table of its own (one entry per 128
// slots), so a filter that never evicts holds no stash.
func (f *Filter) waitTable() *stashTable {
	if f.ct {
		if f.stashN == 0 {
			return nil
		}
		f.allocStash()
		return f.stash.Load()
	}
	if p := f.room.Load(); p != nil {
		return p
	}
	f.skMu.Lock()
	defer f.skMu.Unlock()
	if f.room.Load() == nil {
		f.room.Store(newStashTable(f.roomN, &f.roomMu))
	}
	return f.room.Load()
}

// stashPut keeps an evicted entry in its set: in its own old place, a free
// or expired way, or else in place of the way with the fewest events in the
// horizon if that has fewer than it. Whichever history is dropped, Lost
// counts it. Reports whether the entry was kept.
func (f *Filter) stashPut(id, w0, w1, now uint64) { f.stashPutAs(id, w0, w1, now, false) }

// deferMark, the displaced bit of a stash entry's word0 (otherwise clear),
// marks a key that waits for a slot (ConstantTime) rather than one evicted.
const deferMark = dispBit

// stashPutAs is stashPut; wait marks the entry as waiting for a slot.
func (f *Filter) stashPutAs(id, w0, w1, now uint64, wait bool) {
	if f.stashN == 0 {
		f.spill(id, w0, w1, now)
		return
	}
	w0 &^= dispBit
	if wait {
		w0 |= deferMark
	}
	f.allocStash()
	set, mu := f.stashSet(id)
	mu.Lock()
	way, least, full := -1, uint64(1<<64-1), true // full: every way holds another live history
	for i := range set {
		e := &set[i]
		if e.id == id {
			// An evictor holding an older snapshot of the same key (it was
			// evicted, restored and evicted again meanwhile) must not
			// overwrite the newer one.
			if e.w0&tsMask > w0&tsMask || e.w0&tsMask == w0&tsMask && f.mass(e.id, e.w1, e.w0&tsMask, now) >= f.mass(id, w1, w0&tsMask, now) {
				mu.Unlock()
				return
			}
			way, least, full = i, 0, false
			break
		}
		if e.id == 0 || age(now, e.w0&tsMask) > f.horizon {
			way, least, full = i, 0, false
			break
		}
		if m := f.mass(e.id, e.w1, e.w0&tsMask, now); m < least {
			way, least = i, m
		}
	}
	var out stashEntry // the history that leaves the stash, if any
	if full {
		if least > f.mass(id, w1, w0&tsMask, now) {
			f.spill(id, w0, w1, now)
			mu.Unlock()
			return
		}
		// The lightest leaves through spill even with no events in the
		// horizon: a touch of a hot key that has moved word0 adds to its
		// entry by the key's identity, and spill closes the entry, so the
		// add fails and the touch counts afresh. Overwritten in place, the
		// history left its entry the key's, and the add counted where
		// nothing pointed (claims A32).
		out = set[way]
	}
	if out.id != 0 {
		f.spill(out.id, out.w0&^deferMark, out.w1, now)
	}
	f.stashWrite(f.stash.Load(), &set[way], stashEntry{id, w0, w1})
	if !wait { // a waiting key moves no gate: see stashWait
		f.raiseStashUntil((w0 & tsMask) + f.horizon)
	}
	mu.Unlock()
}

// raiseStashUntil moves stashUntil up to until, if it is below.
func (f *Filter) raiseStashUntil(until uint64) {
	for {
		u := f.stashUntil.Load()
		if u >= until || f.stashUntil.CompareAndSwap(u, until) {
			return
		}
	}
}

// stashWait puts a key that waits for a slot in a free or expired way of its
// set in the wait table (waitTable), or in its own old place, and reports
// whether it did: a full set drops nothing for it. No stashUntil:
// ConstantTime reads the stash anyway, and Background lookups read the
// set's wait bit (stashMay). A way being committed to the table (word1
// busy, placeRoomKey) is never taken, expired or not: the placer, which
// released the set's lock to install it, removes it or puts its word1 back
// afterwards, and would remove or overwrite the newcomer instead (claims
// A24).
func (f *Filter) stashWait(id, w0, w1, now uint64) bool {
	p := f.waitTable()
	if p == nil {
		return false
	}
	set, mu := p.set(id)
	mu.Lock()
	defer mu.Unlock()
	for i := range set {
		if e := &set[i]; e.w1>>tagShift != tagBusy && (e.id == 0 || e.id == id || age(now, e.w0&tsMask) > f.horizon) {
			f.stashWrite(p, e, stashEntry{id, w0&^dispBit | deferMark, w1})
			return true
		}
	}
	return false
}

// putBack returns a history recalled for an insert that lost a race for
// its slot, for the retry to find: a key that waited back to wait, an
// evicted one to the stash.
func (f *Filter) putBack(id, w0, w1, now uint64) {
	if w0&deferMark != 0 && f.stashWait(id, w0, w1, now) {
		return
	}
	f.stashPut(id, w0, w1, now)
}

// stashWrite replaces entry e of table p (its set's lock held), keeping the
// count of entries that wait for a slot.
func (f *Filter) stashWrite(p *stashTable, e *stashEntry, ne stashEntry) {
	if e.id != 0 && e.w0&deferMark != 0 {
		f.deferred.Add(-1)
	}
	if ne.id != 0 && ne.w0&deferMark != 0 {
		f.deferred.Add(1)
		set := uint64((uintptr(unsafe.Pointer(e)) - uintptr(unsafe.Pointer(&p.e[0]))) / unsafe.Sizeof(stashEntry{}) / stashWays)
		if w := &p.wait[set/64]; w.Load()&(1<<(set%64)) == 0 {
			w.Or(1 << (set % 64))
		}
	}
	*e = ne
}

// stashGet returns the live stash entry for id; take also removes it.
func (f *Filter) stashGet(id, now uint64, take bool) (w0, w1 uint64, ok bool) {
	return f.tableGet(f.stash.Load(), id, now, take)
}

// tableGet is stashGet in table p (nil: not allocated, so empty).
func (f *Filter) tableGet(p *stashTable, id, now uint64, take bool) (w0, w1 uint64, ok bool) {
	if p == nil {
		return 0, 0, false
	}
	set, mu := p.set(id)
	mu.Lock()
	defer mu.Unlock()
	for i := range set {
		e := &set[i]
		if e.id != id {
			continue
		}
		if age(now, e.w0&tsMask) > f.horizon {
			return 0, 0, false
		}
		w0, w1 = e.w0, e.w1
		if take {
			f.stashWrite(p, e, stashEntry{})
		}
		return w0, w1, true
	}
	return 0, 0, false
}

// dropped records that a live key's history was lost at now.
func (f *Filter) dropped(now uint64) {
	f.lost.Add(1)
	for {
		la := f.lostAt.Load()
		if la >= now || f.lostAt.CompareAndSwap(la, now) {
			return
		}
	}
}
