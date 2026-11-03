package temporalfilter

import (
	"math/bits"
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// AutoPad as Config.ConstantTime pads each operation to a running estimate
// of its own work's 99.5th-percentile latency instead of a fixed duration.
const AutoPad time.Duration = -1

const (
	opTouch = iota
	opCount
	// The estimate (in ps) moves up autoUp when a sampled call's own work
	// ran past it and down autoDown otherwise, so it settles where 0.5% of calls run
	// past it (autoDown/(autoUp+autoDown)). One slow call (a GC pause)
	// moves it 100 ns.
	autoUp    = 99_500
	autoDown  = 500
	autoStart = 4 * time.Microsecond
	autoMax   = 50 * time.Microsecond
)

type quantilePad struct {
	q atomic.Int64 // ps
	_ [56]byte
}

// ctBegin starts a ConstantTime call of kind op and returns its start and
// pad. Touch's pad is ConstantTime; Count's, whose paths differ far less
// (no insert), is a running estimate of its own work's 99.5th percentile,
// at most ConstantTime; with AutoPad both are. Before the call's own work,
// so that how much it does can't depend on the caller's key, it spends what
// the pad leaves beyond that estimate placing keys that wait in the stash.
func (f *Filter) ctBegin(op int, now uint64) (t0 time.Time, pad time.Duration) {
	t0 = time.Now()
	own := time.Duration(f.padQ[op].q.Load() / 1000)
	switch {
	case op == opTouch && !f.auto:
		pad = f.pad
	case f.auto:
		pad = own
	default:
		pad = min(own, f.pad)
	}
	f.place(t0, pad-own, now)
	return t0, pad
}

// ctEnd moves op's own-work estimate with the work done since s (a sample
// of calls, 1 in 4 by key, moves it) and spins until the pad, plus a random
// dither, has passed since t0. The spin reads the clock every ~37 ns (the Ryzen
// host), so a pad ending at a fixed time ends at a phase of those reads
// that follows the call's own work, and keys whose work time is narrow show
// it (Count at a fixed 1 µs pad: D 0.019-0.041; with the dither 0.007-0.014,
// claims S41). The dither spans several reads and doesn't depend on the key.
func (f *Filter) ctEnd(op int, h uint64, t0, s time.Time, pad time.Duration) {
	if h>>8&3 == 0 {
		qp := &f.padQ[op].q
		if est := time.Duration(qp.Load() / 1000); time.Since(s) > est {
			if est < autoMax {
				qp.Add(autoUp)
			}
		} else if est > 0 {
			qp.Add(-autoDown)
		}
	}
	pad += time.Duration(rand.Uint64N(uint64(padDither)))
	for time.Since(t0) < pad {
	}
}

// padDither is the span over which a ConstantTime pad's end is drawn: a mean
// cost of 64 ns per call.
const padDither = 128 * time.Nanosecond

// place spends up to budget since t0 on keys that wait in the stash for a
// slot (an insert that found its two lines full defers the cuckoo search to
// here, so no call's own work includes one). A step takes the next stash
// set with a waiting key and inserts its waiting keys as a first touch
// outside ConstantTime would: free slot, cuckoo path, or eviction of the
// slot with the fewest events into the stash. A step starts only if the
// rest of the budget covers a step's running mean cost. If more than one
// key per 16 stash entries waits (pads too short for the insert rate),
// every call places one set regardless: calls then run longer for every key
// alike, which says the table is under insert pressure, not which keys were
// seen, and the stash's sets stay far from full (a waiting key whose set is
// full is inserted at once, which takes time).
func (f *Filter) place(t0 time.Time, budget time.Duration, now uint64) {
	p := f.stash.Load()
	if p == nil {
		return
	}
	forced := f.deferred.Load() > int64(len(p.e)/16)
	for f.deferred.Load() > 0 {
		est := time.Duration(f.placeNs.Load())
		if est == 0 {
			est = time.Microsecond
		}
		if !forced && budget-time.Since(t0) < est {
			f.placeNs.Store(int64(est - est/64)) // a step that ran long once mustn't stop placing for good
			return
		}
		forced = false
		s := time.Now()
		if !f.placeNext(p, now) {
			return
		}
		f.placeNs.Store(int64(est + (time.Since(s)-est)/8))
	}
}

// placeNext places the waiting keys of table p's next set (after the last
// one visited) whose wait bit is set; false if no bit is set.
func (f *Filter) placeNext(p *stashTable, now uint64) bool {
	if p == nil {
		return false
	}
	nw := uint64(len(p.wait))
	start := p.at.Load()
	for k := range nw + 1 {
		wi := (start/64 + k) % nw
		b := p.wait[wi].Load()
		if k == 0 {
			b &^= 1<<(start%64) - 1 // bits before the cursor wait for the wrap-around
		}
		if b == 0 {
			continue
		}
		set := wi*64 + uint64(bits.TrailingZeros64(b))
		p.at.Store(set + 1)
		if p == f.room.Load() {
			f.placeRoomSet(p, set)
		} else {
			f.placeSet(p, set, now)
		}
		return true
	}
	return false
}

// placeSet moves the waiting keys of table p's set i into the table. A key
// whose stripe is busy, or that loses a race for its slot, is left for a
// later step.
func (f *Filter) placeSet(p *stashTable, i, now uint64) {
	set, mu := p.e[i*stashWays:(i+1)*stashWays], &p.mu[i%stashStripes]
	var ids [stashWays]uint64
	n := 0
	mu.Lock()
	for k := range set {
		if e := &set[k]; e.id != 0 && e.w0&deferMark != 0 {
			if age(now, e.w0&tsMask) > f.horizon {
				f.stashWrite(p, e, stashEntry{}) // expired while it waited
				continue
			}
			ids[n], n = e.id, n+1
		}
	}
	if n == 0 {
		p.wait[i/64].And(^(1 << (i % 64)))
	}
	mu.Unlock()
	for _, id := range ids[:n] {
		fp, tg := id>>tagBits&(1<<16-1), id&(1<<tagBits-1)
		stripe := fp >> (16 - claimStripeBits)
		cm := &f.claimMu[stripe]
		if !cm.TryLock() {
			continue
		}
		if w0, w1, ok := f.tableGet(p, id, now, true); ok {
			pl := id >> 28
			al := f.alt(pl, fp)
			switch {
			case f.find(pl, fp<<1, tg) >= 0 || f.find(al, fp<<1|1, tg) >= 0:
				f.stashPut(id, w0, w1, now) // the table holds the key: no longer waiting
			case f.claimAt([2]uint64{pl, al}, [2]uint64{w0 &^ dispBit, w0 | dispBit}, w1, now, stripe, claimFull, nil) < 0:
				f.stashPutAs(id, w0, w1, now, true)
			}
		}
		cm.Unlock()
	}
}

// placeRoomSet places the waiting keys of the wait room's set i
// (Background).
func (f *Filter) placeRoomSet(p *stashTable, i uint64) {
	set, mu := p.e[i*stashWays:(i+1)*stashWays], &p.mu[i%stashStripes]
	var waiting [stashWays]stashEntry
	n := 0
	mu.Lock()
	for k := range set {
		if e := set[k]; e.id != 0 && e.w0&deferMark != 0 {
			waiting[n], n = e, n+1
		}
	}
	if n == 0 {
		p.wait[i/64].And(^(1 << (i % 64)))
	}
	mu.Unlock()
	for _, e := range waiting[:n] {
		f.placeRoomKey(p, e)
	}
}

// touchCT is touch in ConstantTime mode: its work (touch with ctInside:
// under the key's stripe, the same lookups whatever the key's state, then
// the count or the insert, then the overflow table) padded.
func (f *Filter) touchCT(h, window, now uint64, wantCount bool) Result {
	checkTick(now) // before the stripe: realign's check would panic holding it
	t0, pad := f.ctBegin(opTouch, now)
	s := time.Now()
	r := f.touch(h, window, now, wantCount, ctInside)
	f.ctEnd(opTouch, h, t0, s, pad)
	return r
}
