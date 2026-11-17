package temporalfilter

// lab/sw-2026-11, arm W7: a two-sided check of every answer under
// concurrency, at windows that cut a bucket (SW-RESULTS.md).

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDripConcurrentCounts (TF_EXP=1) is lab/sw's W7.
// TestBackgroundConcurrentCounts's scenario in each mode of TF_DRIP_MODES
// (default "plain,bg,ct") and shape of TF_DRIP_SHAPES (default
// "spread,burst"), TF_DRIP_ROUNDS rounds each (default 200): 8 workers each
// own 4 of 32 keys and touch each 20 times at ticks 25 apart from a shared
// clock, in a filter of Capacity 64 under a flood of other keys, so the keys
// wait, are evicted to the stash, come back and are moved while their owners
// touch them. In the burst shape every fifth of a key's touches (its 3rd,
// 8th, 13th, 18th) is 12 touches at one tick: 11 zero codes, a full field.
// After each round a shadow filter (Capacity 4,096, the same Horizon,
// Resolution, Stash, SpillMin and mode, no Background, no flood) is given
// the 32 keys' events in tick order on one goroutine, and every key is read
// in both at the round's end at nine windows from 250 ms to 60 s: Seen,
// Count, Upper and Saturated must be equal. A key touched by one goroutine
// at increasing ticks has slot words that are a function of its own events
// alone, wherever the filter moves them, and an answer is a function of the
// words, the window and the tick.
//
// A difference is "log lost" if the filter's drip log for the key says
// nothing where the shadow's does not, and "wrong" otherwise (Count high,
// Count low, or another field). Set apart: a key whose fingerprint and tag
// another key of the round shares (a collision), and every difference in a
// round whose filter folded a history into the overflow table or lost one
// (Folded, Lost: the key comes back from the overflow table's floors with a
// fresh log). Answers given while touches race are not judged: only the
// state the concurrent writes left.
func TestDripConcurrentCounts(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	rounds := 200
	if n, err := strconv.Atoi(os.Getenv("TF_DRIP_ROUNDS")); err == nil {
		rounds = n
	}
	modes := []string{"plain", "bg", "ct"}
	if m := os.Getenv("TF_DRIP_MODES"); m != "" {
		modes = strings.Split(m, ",")
	}
	shapes := []string{"spread", "burst"}
	if s := os.Getenv("TF_DRIP_SHAPES"); s != "" {
		shapes = strings.Split(s, ",")
	}
	wins := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second,
		4 * time.Second, 8 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}
	const workers, nkeys, perKey = 8, 32, 20
	build := "default"
	if dripOn {
		build = "tfdrip"
	}
	for _, mode := range modes {
		mk := func(capacity int, clock func() uint64, bg bool) *Filter {
			f, err := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond,
				Clock: clock, Stash: 1 << 12, SpillMin: 1, Background: bg})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ct" {
				f.ct = true // functional, as the concurrent fuzzer's ConstantTime cases
			}
			return f
		}
		for _, shape := range shapes {
			var answers, high, low, other, logLost, collided, apart, apartRounds, degraded int
			var waited, evictions, restored, relocations, folded, lost uint64
			byWin := make([]int, len(wins))
			var firsts []string
			for round := range rounds {
				var clock atomic.Uint64
				clock.Store(1)
				f := mk(64, clock.Load, mode == "bg")
				keys := make([]uint64, nkeys)
				for i := range keys {
					keys[i] = mix64(uint64(round)<<32 | uint64(i))
				}
				events := make([][]uint64, nkeys)
				floods := make([][]uint64, workers)
				var wg sync.WaitGroup
				for w := range workers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						rng := rand.New(rand.NewPCG(uint64(round), uint64(w)))
						for i := range nkeys * perKey / workers {
							now := clock.Add(25)
							k := (i*workers + w) % nkeys
							reps := 1
							if shape == "burst" && (i/(nkeys/workers))%5 == 2 {
								reps = 12
							}
							for range reps {
								f.TouchAt(keys[k], 0, now)
								events[k] = append(events[k], now)
							}
							if i%2 == 0 {
								h := rng.Uint64()
								floods[w] = append(floods[w], h)
								for range 25 {
									f.TouchAt(h, 0, now)
								}
							}
						}
					}()
				}
				wg.Wait()
				now := clock.Load()
				type ev struct {
					t uint64
					k int
				}
				var evs []ev
				for k, ts := range events {
					for _, tk := range ts {
						evs = append(evs, ev{tk, k})
					}
				}
				slices.SortStableFunc(evs, func(a, b ev) int { return cmp.Compare(a.t, b.t) })
				g := mk(4096, nil, false)
				for _, e := range evs {
					g.TouchAt(keys[e.k], 0, e.t)
				}
				shares := func(h uint64) bool {
					for _, fl := range append(floods, keys) {
						for _, h2 := range fl {
							if h2 != h && fingerprint(h2) == fingerprint(h) && tag(h2) == tag(h) {
								return true
							}
						}
					}
					return false
				}
				setApart := f.Folded() > 0 || f.Lost() > 0
				if setApart {
					apartRounds++
				}
				for k, h := range keys {
					for wi, w := range wins {
						wt := f.Ticks(w)
						a, b := f.CountAt(h, wt, now), g.CountAt(h, wt, now)
						answers++
						if a.Degraded {
							degraded++
						}
						if a.Seen == b.Seen && a.Count == b.Count && a.Upper == b.Upper && a.Saturated == b.Saturated {
							continue
						}
						switch {
						case shares(h):
							collided++
							continue
						case setApart:
							apart++
							continue
						}
						fw0, _, _ := f.read(h, now, -1)
						gw0, _, _ := g.read(h, now, -1)
						kind := ""
						switch {
						case dripOf(fw0) == 0 && dripOf(gw0) != 0:
							logLost++
							kind = "log lost"
						case a.Count > b.Count:
							high++
							kind = "Count high"
						case a.Count < b.Count:
							low++
							kind = "Count low"
						default:
							other++
							kind = "another field"
						}
						byWin[wi]++
						if len(firsts) < 5 {
							firsts = append(firsts, fmt.Sprintf("%s: round %d key %d (%d events) window %v: %+v, shadow %+v; log %#x, shadow's %#x",
								kind, round, k, len(events[k]), w, a, b, dripOf(fw0), dripOf(gw0)))
						}
					}
				}
				waited += f.Waited()
				evictions += f.Evictions()
				restored += f.Restored()
				relocations += f.Relocations()
				folded += f.Folded()
				lost += f.Lost()
				f.Close()
				g.Close()
			}
			t.Logf("%s build, mode %s, shape %s: %d rounds, %d answers; wrong: Count high %d, Count low %d, another field %d; log lost %d; set apart: collisions %d, in %d rounds that folded or lost a history %d; Degraded %d; waited %d, evictions %d, restored %d, relocations %d, folded %d, lost %d",
				build, mode, shape, rounds, answers, high, low, other, logLost, collided, apartRounds, apart, degraded, waited, evictions, restored, relocations, folded, lost)
			s := ""
			for wi, w := range wins {
				s += fmt.Sprintf(" %v:%d", w, byWin[wi])
			}
			t.Logf("%s build, mode %s, shape %s: differences by window:%s", build, mode, shape, s)
			for _, d := range firsts {
				t.Logf("  %s", d)
			}
			if high+low+other+logLost > 0 {
				t.Errorf("%s build, mode %s, shape %s: %d answers wrong, %d with the log lost", build, mode, shape, high+low+other, logLost)
			}
		}
	}
}
