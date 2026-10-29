package temporalfilter

import (
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStressSoak runs every operation from many goroutines against one
// filter for TF_STRESS (a duration, e.g. 30s; skipped when unset), cycling
// the load from half the capacity to 4x it, and checks the fail-closed
// contract on every Count: unless the answer is flagged Saturated, Upper
// is never below the key's true count in the window. One exception is
// documented (docs/SECURITY.md): a key that left more than one history in
// an overflow bucket's floor can read up to all but the largest of them
// low, so a low answer whose bucket floor is live is counted, not failed. Each goroutine owns
// its keys, so it knows their true counts exactly; the clock is shared and
// moved by its own goroutine. Run it under -race too (go/test.sh stress).
// TF_STRESS_TAGS=n amplifies tag coincidences as the concurrent fuzzer's
// TF_CFUZZ_TAGS does (tagAmp; the soak uses fewer than 20,000 keys).
func TestStressSoak(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv("TF_STRESS"))
	if err != nil || d <= 0 {
		t.Skip("set TF_STRESS=<duration> to run")
	}
	const capacity = 4096
	workers := max(4, 2*runtime.GOMAXPROCS(0))
	var clock atomic.Uint64
	clock.Store(1)
	// Hot entries for every heavy key (8 per worker) and most of the pool's
	// keys that pass a register's 31 events in a unit, so their answers are
	// checked rather than Saturated (refused promotions are the fuzzer's
	// few-hot-entries cases and the attack tests'). 10 per worker left 63%
	// of answers Saturated, nearly all refused promotions; 80 leaves 24%,
	// mostly histories in the overflow table while the pool is 4-8x the
	// table (2026-10-28, 30 s soaks).
	f, err := New(Config{Capacity: capacity, Horizon: 10 * time.Second, Resolution: time.Millisecond,
		Clock: clock.Load, SpillMin: 1, Hot: 80 * workers})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	horizon := uint64(10_000)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	t0 := time.Now()
	go func() { // 1 ms of filter time per 20 µs of wall time: the horizon turns over every 0.2 s
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			clock.Store(1 + uint64(time.Since(t0)/(20*time.Microsecond)))
			time.Sleep(20 * time.Microsecond)
		}
	}()
	var ops, queries, sat, low, floored atomic.Int64
	amp := newTagAmp("TF_STRESS_TAGS")
	var firstLow atomic.Value
	for g := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 1))
			// Owned keys: a few heavy ones that always come back, plus a
			// pool whose size cycles to push the table past capacity.
			cache := map[uint64]uint64{}
			heavy := make([]uint64, 8)
			for i := range heavy {
				heavy[i] = amp.h(rng.Uint64(), cache)
			}
			logs := map[uint64][]uint64{}
			hs := make([]uint64, 16)
			res := make([]bool, 16)
			cnt := make([]int, 16)
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				phase := (time.Now().UnixMilli() / 500) % 4 // pool: 0.5x, 1x, 2x, 4x capacity in total
				pool := uint64(capacity/2<<phase) / uint64(workers)
				var h uint64
				if rng.IntN(32) == 0 {
					h = heavy[rng.IntN(len(heavy))]
				} else {
					h = amp.h(mix64(uint64(g)<<40|rng.Uint64N(pool)), cache)
				}
				now := clock.Load()
				switch op := rng.IntN(10); {
				case op < 6:
					f.TouchAt(h, 0, now)
					logs[h] = append(logs[h], now)
				case op < 9:
					w := 1 + rng.Uint64N(horizon)
					r := f.CountAt(h, w, now)
					// Other goroutines may have run at a later clock and
					// rightly expired events that are older than the horizon
					// by then; the clock read after the call bounds theirs.
					after := clock.Load()
					exact := 0
					for _, ts := range logs[h] {
						if now-ts <= w && after-ts <= horizon {
							exact++
						}
					}
					queries.Add(1)
					if r.Saturated {
						sat.Add(1)
					} else if r.Upper < exact && f.floorLive(h, w, now) {
						floored.Add(1)
					} else if r.Upper < exact {
						if low.Add(1) == 1 {
							again := f.CountAt(h, w, now)
							firstLow.Store(fmt.Sprintf("got %+v; asked again: %+v; window %d ms: exact %d (now %d, events %v); %s", r, again, w, exact, now, logs[h], f.whereIs(h, now)))
						}
					}
				default:
					for i := range hs {
						hs[i] = heavy[rng.IntN(len(heavy))]
					}
					f.CheckAndTouchBatch(hs, horizon, now, res)
					for _, x := range hs {
						logs[x] = append(logs[x], now)
					}
					f.VelocityCountBatch(hs, horizon, now, cnt)
				}
				ops.Add(1)
				if n%4096 == 0 { // forget events older than the horizon
					f.ContinuousDecay()
					for k, l := range logs {
						i := 0
						for i < len(l) && now-l[i] > horizon {
							i++
						}
						if i == len(l) {
							delete(logs, k)
						} else {
							logs[k] = l[i:]
						}
					}
				}
			}
		}()
	}
	time.Sleep(d)
	close(stop)
	wg.Wait()
	t.Logf("%d workers, %v: %d ops, %d counts checked (%d Saturated, %d low behind a live bucket floor); evictions %d, restored %d, lost %d, promoted %d, hot refused %d",
		workers, d, ops.Load(), queries.Load(), sat.Load(), floored.Load(), f.Evictions(), f.Restored(), f.Lost(), f.Promoted(), f.HotRefused())
	if n := low.Load(); n > 0 {
		t.Errorf("%d unflagged answers with Upper below the true count; first: %v", n, firstLow.Load())
	}
	if occ := f.Occupancy(clock.Load()); occ > f.Slots() {
		t.Errorf("occupancy %d > %d slots", occ, f.Slots())
	}
}

func init() { describeKey = (*Filter).whereIs }

// whereIs describes where h's history is: its slots (with the register's or
// hot entry's counts), its stash entry, its overflow entries and floor.
func (f *Filter) whereIs(h, now uint64) string {
	fp, tg := fingerprint(h), tag(h)
	id := f.stashID(h, fp, tg)
	p := f.primary(h)
	s := ""
	for li, l := range []uint64{p, f.alt(p, fp)} {
		base := int(l * wordsPerLine)
		for k := 0; k < wordsPerLine; k += 2 {
			w0, w1 := atomic.LoadUint64(&f.words[base+k]), atomic.LoadUint64(&f.words[base+k+1])
			if w0>>48 == fp && w1>>tagShift == tg {
				v, ok := f.viewOf(id, w1, w0&tsMask)
				s += fmt.Sprintf("slot line%d/%d disp%v last %d hot %v ok %v epoch %d counts %v; ", li, k/2, w0&dispBit != 0, w0&tsMask, isHot(w1), ok, v.e, v.c)
			}
		}
	}
	if f.stashN > 0 {
		set, _ := f.stashSet(id)
		for _, e := range set {
			if e.id == id {
				v, ok := f.viewOf(id, e.w1, e.w0&tsMask)
				s += fmt.Sprintf("stash last %d hot %v ok %v epoch %d counts %v; ", e.w0&tsMask, isHot(e.w1), ok, v.e, v.c)
			}
		}
	}
	if p := f.room.Load(); p != nil {
		set, _ := p.set(id)
		for _, e := range set {
			if e.id == id {
				v, ok := f.viewOf(id, e.w1, e.w0&tsMask)
				s += fmt.Sprintf("room last %d busy %v hot %v ok %v epoch %d counts %v (wait bit %v); ", e.w0&tsMask, e.w1>>tagShift == tagBusy, isHot(e.w1), ok, v.e, v.c, p.waiting(id))
			}
		}
	}
	if g := f.gh.Load(); g != nil {
		r, _ := g.estimate(id, f.horizon, now)
		s += fmt.Sprintf("ghost %+v", r)
	}
	return s
}

// floorLive: one of h's overflow buckets has a floor live within window
// that covers h's fingerprint, so h may have histories there that its
// answer can't attribute to it.
func (f *Filter) floorLive(h, window, now uint64) bool {
	g := f.gh.Load()
	if g == nil {
		return false
	}
	ep := g.epoch.Load()
	bases, nb, fp := g.slot(f.stashID(h, fingerprint(h), tag(h)))
	for _, base := range bases[:nb] {
		e := atomic.LoadUint64(&g.words[base]) >> g.floorShift(fp) & ghSubMask
		if ep != 0 && e != 0 && g.live(g.real(e, ep), min(window, g.horizon), now) {
			return true
		}
	}
	return false
}
