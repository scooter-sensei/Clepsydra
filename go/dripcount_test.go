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

// dripInOverflow reports whether f's overflow table holds an entry for h at
// now: a folded history, h's own or one sharing its identity (W7b).
func dripInOverflow(f *Filter, h, now uint64) bool {
	if !f.sketchLive(now) {
		return false
	}
	id := f.stashID(h, fingerprint(h), tag(h))
	var s Result
	var ok bool
	if !f.cms {
		if gt := f.gh.Load(); gt != nil {
			s, ok = gt.estimate(id, f.horizon, now)
		}
	} else if sk := f.sk.Load(); sk != nil {
		s, ok = sk.estimate(id, f.horizon, now, f.ct)
	}
	return ok && (s.Seen || s.Count > 0 || s.Upper > 0)
}

// TestDripConcurrentCounts (TF_EXP=1) is lab/sw's W7, as W7b revised it.
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
// Count low, or another field). Set apart, answer by answer (W7b; W7 set
// apart whole rounds that folded or lost a history): an answer either
// filter flags Saturated or Degraded (a refused or shed hot entry, an
// answer from the overflow table: the API's own disclaimers); an answer for
// a key the filter's overflow table holds an entry for (a folded history,
// its own or one sharing its identity, which can add to Upper unflagged);
// a key whose fingerprint and tag another key of the round shares (a
// collision); and every difference in a round whose filter lost a history
// outright (Lost). Both filters have the Capacity-64 filter's 16 hot
// entries (W7b; in W7 the shadow had its own capacity's). Answers given
// while touches race are not judged: only the state the concurrent writes
// left.
//
// W10b: at rest the filter is also read against each key's own events, as
// the soak and the concurrent fuzzer read theirs (countcheck_test.go):
// every key's drip log (logHigh), and the Upper of every answer above
// (countHigh, the overflow table's part as the call added it taken out). A
// key whose fingerprint and tag another key of the round shares is counted,
// not failed.
func TestDripConcurrentCounts(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	recordParts(t)
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
				Clock: clock, Stash: 1 << 12, SpillMin: 1, Background: bg, Hot: 16})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ct" {
				f.ct = true // functional, as the concurrent fuzzer's ConstantTime cases
			}
			return f
		}
		for _, shape := range shapes {
			var answers, high, low, other, logLost, collided, apart, apartRounds, degraded, flagged, overflowed int
			var waited, evictions, restored, relocations, folded, lost uint64
			byWin := make([]int, len(wins))
			var firsts []string
			// W10b at rest
			var restLogs, restLogVacuous, restLogShared, restLogOver int
			var restHigh, restHighGhost, restHighShared, restHighTaken, restHighOver, restPartOver int
			var restFirsts []string
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
				setApart := f.Lost() > 0
				if setApart {
					apartRounds++
				}
				for k, h := range keys {
					inOver := dripInOverflow(f, h, now)
					sharesKey := shares(h)
					if m, ok := f.logHigh(h, now, 0, events[k]); !ok { // W10b: the key's drip log at rest
						restLogVacuous++
					} else {
						restLogs++
						switch {
						case m == "":
						case sharesKey:
							restLogShared++
						default:
							if restLogOver++; len(restFirsts) < 5 {
								restFirsts = append(restFirsts, fmt.Sprintf("log over: round %d key %d (%d events): %s; events %v", round, k, len(events[k]), m, tail(events[k], 12)))
							}
						}
					}
					for wi, w := range wins {
						wt := f.Ticks(w)
						armPart(h)
						a := f.CountAt(h, wt, now)
						if gh, mine := partOf(h); !mine { // W10b: Upper at rest against the key's events
							restHighTaken++
						} else {
							restHigh++
							if gh > 0 {
								restHighGhost++
							}
							if gh > a.Upper { // the record can't be the call's part: it would hide an over
								if restPartOver++; len(restFirsts) < 5 {
									restFirsts = append(restFirsts, fmt.Sprintf("part over Upper: round %d key %d window %v: part %d, result %+v", round, k, w, gh, a))
								}
							}
							if m := f.countHigh(a, gh, events[k], wt, now, 0); m != "" {
								switch {
								case sharesKey:
									restHighShared++
								default:
									if restHighOver++; len(restFirsts) < 5 {
										restFirsts = append(restFirsts, fmt.Sprintf("Upper over: round %d key %d (%d events) window %v: %s (result %+v)", round, k, len(events[k]), w, m, a))
									}
								}
							}
						}
						b := g.CountAt(h, wt, now)
						answers++
						if a.Degraded {
							degraded++
						}
						if a.Saturated || b.Saturated || a.Degraded || b.Degraded {
							flagged++
							continue
						}
						if inOver {
							overflowed++
							continue
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
			judged := answers - flagged - overflowed
			t.Logf("%s build, mode %s, shape %s: %d rounds, %d answers, %d judged (%.1f%%); wrong: Count high %d, Count low %d, another field %d; log lost %d; set apart: flagged Saturated or Degraded %d, a key in the overflow table %d, collisions %d, in %d rounds that lost a history %d; the filter's Degraded %d; waited %d, evictions %d, restored %d, relocations %d, folded %d, lost %d",
				build, mode, shape, rounds, answers, judged, 100*float64(judged)/float64(max(answers, 1)), high, low, other, logLost, flagged, overflowed, collided, apartRounds, apart, degraded, waited, evictions, restored, relocations, folded, lost)
			s := ""
			for wi, w := range wins {
				s += fmt.Sprintf(" %v:%d", w, byWin[wi])
			}
			t.Logf("%s build, mode %s, shape %s: differences by window:%s", build, mode, shape, s)
			for _, d := range firsts {
				t.Logf("  %s", d)
			}
			t.Logf("%s build, mode %s, shape %s: W10b at rest: %d keys' drip logs checked (%d saying nothing or absent; %d over for a key whose fingerprint and tag another key of the round shares), %d over; %d answers' Upper checked against the key's events in their reach (%d with an overflow table's part, as the call added it, taken out; %d over for a key whose fingerprint and tag another shares; %d set apart, their record taken over), %d over, %d with a part over Upper",
				build, mode, shape, restLogs, restLogVacuous, restLogShared, restLogOver, restHigh, restHighGhost, restHighShared, restHighTaken, restHighOver, restPartOver)
			for _, d := range restFirsts {
				t.Logf("  %s", d)
			}
			if high+low+other+logLost > 0 {
				t.Errorf("%s build, mode %s, shape %s: %d answers wrong, %d with the log lost", build, mode, shape, high+low+other, logLost)
			}
			if restLogOver+restHighOver+restPartOver > 0 {
				t.Errorf("%s build, mode %s, shape %s: W10b at rest: %d drip logs over, %d answers' Upper over, %d parts over Upper", build, mode, shape, restLogOver, restHighOver, restPartOver)
			}
		}
	}
}
