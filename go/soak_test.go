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
func TestStressSoak(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv("TF_STRESS"))
	if err != nil || d <= 0 {
		t.Skip("set TF_STRESS=<duration> to run")
	}
	const capacity = 4096
	var clock atomic.Uint64
	clock.Store(1)
	f, err := New(Config{Capacity: capacity, Horizon: 10 * time.Second, Resolution: time.Millisecond,
		Clock: clock.Load, SpillMin: 1})
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
	workers := max(4, 2*runtime.GOMAXPROCS(0))
	var ops, queries, sat, low, floored atomic.Int64
	var firstLow atomic.Value
	for g := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 1))
			// Owned keys: a few heavy ones that always come back, plus a
			// pool whose size cycles to push the table past capacity.
			heavy := make([]uint64, 8)
			for i := range heavy {
				heavy[i] = rng.Uint64()
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
					h = mix64(uint64(g)<<40 | rng.Uint64N(pool))
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
							firstLow.Store(fmt.Sprintf("window %d ms: exact %d (events %v, now %d), got %+v; asked again: %+v", w, exact, logs[h], now, r, again))
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

// floorLive: h's overflow bucket has a floor live within window, so h may
// have histories there that its answer can't attribute to it.
func (f *Filter) floorLive(h, window, now uint64) bool {
	g := f.gh.Load()
	if g == nil {
		return false
	}
	ep := g.epoch.Load()
	base, _ := g.slot(f.stashID(h, fingerprint(h), tag(h)))
	e := atomic.LoadUint64(&g.words[base]) & ghEntryMask
	return ep != 0 && e != 0 && g.live(g.real(e, ep), min(window, g.horizon), now)
}
