package temporalfilter

import (
	"math/rand/v2"

	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTouchIfBurst (claims A45): G goroutines at one tick, each calling
// until refused, on fresh keys: admitted per key must equal the limit.
func TestTouchIfBurst(t *testing.T) {
	const G = 8
	for _, nk := range []int{1, 1000} {
		for _, limit := range []uint64{10, 100, 1000} {
			f, _ := New(Config{Capacity: 200_000, Horizon: time.Minute, Resolution: time.Millisecond})
			adm := make([]atomic.Int64, nk)
			var wg sync.WaitGroup
			for g := range G {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := rand.New(rand.NewPCG(uint64(g), 1))
					// until every key has refused this goroutine once (the
					// first reading stopped after 4 x keys refusals in all,
					// before every key was full)
					refused, left := make([]bool, nk), nk
					for left > 0 {
						k := r.IntN(nk)
						if _, ok := f.TouchIfUnderAt(mix64(uint64(k)+1<<45), 60_000, limit, 1000); ok {
							adm[k].Add(1)
						} else if !refused[k] {
							refused[k], left = true, left-1
						}
					}
				}()
			}
			wg.Wait()
			lo, hi := int64(1<<62), int64(0)
			for k := range adm {
				lo, hi = min(lo, adm[k].Load()), max(hi, adm[k].Load())
			}
			t.Logf("burst keys %4d limit %4d: admitted per key min %d max %d; evictions %d, promoted %d", nk, limit, lo, hi, f.Evictions(), f.Promoted())
			if lo != int64(limit) || hi != int64(limit) {
				t.Errorf("burst keys %d limit %d: admitted %d to %d per key", nk, limit, lo, hi)
			}
		}
	}
}

// TestTouchIfSustained (claims A45): G goroutines over three horizons on
// a shared clock (1 ms steps, 4 calls each per step); admitted per key at
// every window position must stay within the limit.
func TestTouchIfSustained(t *testing.T) {
	const G, hTicks = 8, 60_000
	for _, c := range []struct{ w, limit uint64 }{{1_000, 100}, {5_000, 500}} {
		for _, nk := range []int{1, 1000} {
			f, _ := New(Config{Capacity: 200_000, Horizon: time.Minute, Resolution: time.Millisecond})
			logs := make([][]uint64, G) // per goroutine: key<<32 | tick
			var sat atomic.Int64
			var wg sync.WaitGroup
			ticks := uint64(3 * hTicks)
			if nk > 1 || testing.Short() {
				ticks = hTicks / 2 // 1,000 keys: half a horizon (the per-position check is per key)
			}
			step := make([]chan uint64, G)
			for g := range G {
				step[g] = make(chan uint64, 1)
			}
			var done sync.WaitGroup
			for g := range G {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := rand.New(rand.NewPCG(uint64(g), 2))
					for now := range step[g] {
						for range 4 {
							k := r.IntN(nk)
							res, ok := f.TouchIfUnderAt(mix64(uint64(k)+1<<44), c.w, c.limit, now)
							if ok {
								logs[g] = append(logs[g], uint64(k)<<32|now)
							}
							if res.Saturated {
								sat.Add(1)
							}
						}
						done.Done()
					}
				}()
			}
			for now := uint64(1); now <= ticks; now++ {
				done.Add(G)
				for g := range G {
					step[g] <- now
				}
				done.Wait()
			}
			for g := range G {
				close(step[g])
			}
			wg.Wait()
			per := make([][]uint64, nk)
			for g := range G {
				for _, x := range logs[g] {
					per[x>>32] = append(per[x>>32], x&(1<<32-1))
				}
			}
			over, positions, sum := 0, 0, 0
			minW := int(1 << 30)
			for k := range per {
				ts := per[k]
				sort.Slice(ts, func(a, b int) bool { return ts[a] < ts[b] })
				lo := 0
				for hi := range ts { // window ending at each admission's tick
					for ts[lo]+c.w < ts[hi] {
						lo++
					}
					j := hi
					for j+1 < len(ts) && ts[j+1] == ts[hi] {
						j++
					}
					n := j - lo + 1
					positions++
					sum += n
					minW = min(minW, n)
					if n > int(c.limit) {
						over++
					}
				}
			}
			t.Logf("sustained keys %4d W=%ds limit %d: positions %d, over the limit %d, mean admitted per window %.1f (min %d); evictions %d, saturated answers %d",
				nk, c.w/1000, c.limit, positions, over, float64(sum)/float64(max(positions, 1)), minW, f.Evictions(), sat.Load())
			if over != 0 {
				t.Errorf("sustained keys %d W=%ds limit %d: over the limit at %d window positions", nk, c.w/1000, c.limit, over)
			}
		}
	}
}

// TestTouchIfVelocity (claims A45): one goroutine, tfbench's velocity
// workload's events as requests, against the filter's own admitted log and
// an exact sliding-log limiter fed the same requests.
func TestTouchIfVelocity(t *testing.T) {
	for _, c := range []struct{ w, limit uint64 }{{1_000, 5}, {5_000, 20}} {
		f, _ := New(Config{Capacity: 200_000, Horizon: time.Minute, Resolution: time.Millisecond})
		own := make([][]uint64, 2000)
		ex := make([][]uint64, 2000)
		inWin := func(ts []uint64, now uint64) uint64 {
			n := uint64(0)
			for i := len(ts) - 1; i >= 0 && ts[i]+c.w >= now; i-- {
				n++
			}
			return n
		}
		var reqs, agree, admitOver, refusedUnder, refusedUnderUpper, bothAdmit, exAdmit int
		velWorkload(func(k int, tk uint64) {
			reqs++
			h := mix64(uint64(k) + 1<<50)
			truth := inWin(own[k], tk)
			r, ok := f.TouchIfUnderAt(h, c.w, c.limit, tk)
			if ok {
				if truth >= c.limit {
					admitOver++
				}
				own[k] = append(own[k], tk)
			} else if truth < c.limit {
				refusedUnder++
				if uint64(r.Upper) > truth || r.Saturated {
					refusedUnderUpper++
				}
			}
			eok := inWin(ex[k], tk) < c.limit
			if eok {
				ex[k] = append(ex[k], tk)
				exAdmit++
			}
			if eok == ok {
				agree++
				if ok {
					bothAdmit++
				}
			}
		}, func(int, []uint64, int, uint64) {})
		t.Logf("velocity W=%ds limit %d: requests %d, agreement with exact %.4f (exact admits %d, both %d); admitted at/over own limit %d; refused under own limit %d, of which Upper > truth %d",
			c.w/1000, c.limit, reqs, float64(agree)/float64(reqs), exAdmit, bothAdmit, admitOver, refusedUnder, refusedUnderUpper)
		if admitOver != 0 || refusedUnderUpper != refusedUnder {
			t.Errorf("velocity W=%ds limit %d: %d admitted at or over the limit; %d refused under it without Upper > truth", c.w/1000, c.limit, admitOver, refusedUnder-refusedUnderUpper)
		}
	}
}

// TestTouchIfUnderLostHistory: without an overflow table a dropped history
// is lost, and an unseen key reads Degraded while its events may still be
// in a window (TestLostAtNewestEvent): Upper may then be below the truth,
// so TouchIfUnderAt refuses such a key; once the lost events are past a
// horizon it admits again.
func TestTouchIfUnderLostHistory(t *testing.T) {
	f, err := New(Config{Capacity: 64, Horizon: time.Second, Resolution: time.Millisecond, Overflow: -1})
	if err != nil {
		t.Fatal(err)
	}
	h := mix64(1)
	fp, tg := fingerprint(h), tag(h)
	w1, _ := f.nextReg(0, tg, 1000, 1000, true)
	f.spill(f.stashID(h, fp, tg), fp<<48|1000, w1, 1000)
	if r, ok := f.TouchIfUnderAt(h, 1000, 5, 1500); ok || !r.Degraded || r.Seen {
		t.Errorf("an unseen key read Degraded after a lost history: %+v, admitted %v; want refused", r, ok)
	}
	if r, ok := f.TouchIfUnderAt(h, 1000, 5, 2001); !ok {
		t.Errorf("past a horizon after the lost event: %+v, refused", r)
	}
}
