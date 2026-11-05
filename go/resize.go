package temporalfilter

import (
	"errors"
	"sync"
	"sync/atomic"
)

// Resizable is a filter that can change its Capacity without losing state
// (claims D28): Resize starts a new filter at the new size, and for one
// horizon the old one stays beside it. A touch of a key still live in the
// old table moves its slot to the new one first; reads add the old filter's
// answer (its stash and overflow table, and keys not touched since the
// resize); once a horizon has passed nothing in the old filter is live and
// it is dropped. Meanwhile both tables are held (Bytes counts both), and a
// key reads Seen if either filter has it.
//
// A resize keeps what the old filter holds, not what it had already lost:
// a filter that evicted before it grew carries those losses (histories
// shed below SpillMin) until the transition ends, so a grown filter answers
// as one built at the new size only once the old one is dropped, or from
// the start if the old one had not evicted. Hot entries must be off
// (Config.Hot < 0): a hot key's exact counters don't fit a register.
// ConstantTime and Background are refused. Calls are serialized by one
// mutex.
type Resizable struct {
	mu       sync.Mutex
	cfg      Config
	cur, old *Filter
	until    uint64 // the transition ends after this tick
	moved    int    // keys moved to the new table
}

// NewResizable is New for a Resizable.
func NewResizable(cfg Config) (*Resizable, error) {
	if cfg.Hot >= 0 {
		return nil, errors.New("temporalfilter: Resizable needs hot entries off (Config.Hot < 0)")
	}
	f, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if f.ct || f.bg {
		return nil, errors.New("temporalfilter: Resizable of a ConstantTime or Background filter")
	}
	return &Resizable{cfg: cfg, cur: f}, nil
}

// Resize starts a transition to a filter of the given Capacity at now.
func (z *Resizable) Resize(capacity int, now uint64) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.settle(now)
	if z.old != nil {
		return errors.New("temporalfilter: Resize during a transition")
	}
	cfg := z.cfg
	cfg.Capacity = capacity
	f, err := New(cfg)
	if err != nil {
		return err
	}
	z.old, z.cur, z.until = z.cur, f, now+z.cur.horizon
	return nil
}

func (z *Resizable) settle(now uint64) {
	if z.old != nil && now > z.until {
		z.old = nil
	}
}

// move takes h's slot, live in the old table and absent from the new one,
// into the new table (as Merge installs a key), and clears it in the old.
// Unlike Merge's install it doesn't recall the new filter's stash: with
// calls serialized a key reaches that stash only from the new table, which
// it enters by this move (the old slot cleared) or, with no live copy in
// the old table, by a first touch, so the stash never holds a key the old
// table holds live.
func (z *Resizable) move(h, now uint64) {
	fp, tg := fingerprint(h), tag(h)
	f, o := z.cur, z.old
	if i, _, _ := f.lookupStable(h, fp, tg, false); i >= 0 {
		return
	}
	i, w0, w1 := o.lookupStable(h, fp, tg, false)
	if i < 0 || w1>>tagShift == tagBusy || isHot(w1) || age(now, w0&tsMask) > o.horizon {
		return
	}
	mu := &f.claimMu[h>>(64-claimStripeBits)]
	mu.Lock()
	ok := false
	for range 8 {
		if f.claim(h, fp, w1, now, w0&tsMask, w0&satBit, claimFull) >= 0 {
			ok = true
			break
		}
	}
	mu.Unlock()
	if ok {
		atomic.StoreUint64(&o.words[i+1], 0)
		atomic.StoreUint64(&o.words[i], 0)
		z.moved++
	}
}

func sumResult(a, b Result) Result {
	a.Count += b.Count
	a.Upper += b.Upper
	a.Seen = a.Seen || b.Seen
	a.Saturated = a.Saturated || b.Saturated
	a.Restored = a.Restored || b.Restored
	a.Degraded = a.Degraded || b.Degraded
	return a
}

// TouchAt is Filter.TouchAt across the transition.
func (z *Resizable) TouchAt(h, window, now uint64) Result {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.settle(now)
	if z.old == nil {
		return z.cur.TouchAt(h, window, now)
	}
	z.move(h, now)
	return sumResult(z.cur.TouchAt(h, window, now), z.old.CountAt(h, window, now))
}

// CountAt is Filter.CountAt across the transition.
func (z *Resizable) CountAt(h, window, now uint64) Result {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.settle(now)
	r := z.cur.CountAt(h, window, now)
	if z.old != nil {
		r = sumResult(r, z.old.CountAt(h, window, now))
	}
	return r
}

// VelocityCountAt is CountAt's Count.
func (z *Resizable) VelocityCountAt(h, window, now uint64) int {
	return z.CountAt(h, window, now).Count
}

// ContainsAt is Filter.ContainsAt across the transition.
func (z *Resizable) ContainsAt(h, window, now uint64) bool {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.settle(now)
	return z.cur.ContainsAt(h, window, now) || z.old != nil && z.old.ContainsAt(h, window, now)
}

// Bytes is the memory both filters hold.
func (z *Resizable) Bytes() int {
	z.mu.Lock()
	defer z.mu.Unlock()
	n := z.cur.Bytes()
	if z.old != nil {
		n += z.old.Bytes()
	}
	return n
}

// Evictions sums both filters'.
func (z *Resizable) Evictions() uint64 {
	z.mu.Lock()
	defer z.mu.Unlock()
	n := z.cur.Evictions()
	if z.old != nil {
		n += z.old.Evictions()
	}
	return n
}
