package temporalfilter

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/scooter-sensei/clepsydra/go/internal/workload"
)

// TestPlacementLimit asks whether the scorecard's missed keys are forced by
// the table layout or caused by the filter's bounded cuckoo search.
//
// It replays the scorecard's churn stream (cmd/tfbench, same seeds) through
// an exact placement oracle with the filter's geometry: the same lines, 4
// slots each, and the same two candidate lines per key (primary, alt). The
// oracle searches without bound (BFS for a shortest augmenting path), so it
// places a new key whenever any arrangement of the live keys plus that key
// exists, and drops the key only when none does. Every drop is checked with
// a Hall-violation certificate: a set S of full lines whose keys all have
// both candidates in S, plus the new key whose candidates are in S, is
// 4|S|+1 keys for 4|S| slots. The real filter runs on the same stream.
//
// Output lines starting with "placement" are deterministic and are compared
// byte for byte by verify/run.sh.
//
//	go test -run TestPlacementLimit -v
func TestPlacementLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("replays three 4-horizon churn streams")
	}
	const capN, hTicks = 200_000, 60_000
	for _, c := range []struct {
		load float64
		seed uint64
	}{{1.0, 2}, {1.1, 3}, {1.15, 4}} {
		f, err := New(Config{Capacity: capN, Horizon: time.Minute, Resolution: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		o := newOracle(f, hTicks)
		last := map[uint64]uint64{}
		var pos, filterFN, oracleFN int
		var pruned uint64
		workload.Churn(c.load, c.seed, capN, hTicks, mix64, func(h, w, now uint64) {
			if now%hTicks == 0 && now != pruned {
				pruned = now
				for k, lt := range last {
					if now-lt > 2*hTicks {
						delete(last, k)
					}
				}
			}
			got := f.CheckAndTouchAt(h, w, now)
			ogot := o.touch(t, h, w, now)
			lt, ok := last[h]
			want := ok && now-lt <= w
			last[h] = now
			if now < hTicks || !want {
				return
			}
			pos++
			if !got {
				filterFN++
			}
			if !ogot {
				oracleFN++
			}
		})
		hist := ""
		for d, n := range o.moves {
			if n > 0 {
				hist += fmt.Sprintf(" %d:%d", d, n)
			}
		}
		t.Logf("placement load=%.2f seed=%d inserts=%d oracle_drops=%d oracle_peak_slot_load=%.4f oracle_missed=%d filter_evictions=%d filter_missed=%d positives=%d",
			c.load, c.seed, o.inserts, o.drops, float64(o.peak)/float64(4*f.lines), oracleFN, f.Evictions(), filterFN, pos)
		t.Logf("placement load=%.2f shortest_path_moves(moves:inserts)%s", c.load, hist)
	}
}

// oracle is an exact placement model of the filter's table: each live key
// sits in one of its two candidate lines, and inserts take a shortest
// augmenting path when one exists. Keys expire Horizon after their last touch,
// as in the filter.
type oracle struct {
	f       *Filter
	horizon uint64
	ids     map[uint64]int32 // live key hash -> id
	h, last []uint64
	cand    [][2]int32
	line    []int32 // line holding the key
	free    []int32
	slots   [][slotsPerLine]int32 // key ids, -1 = empty
	used    []uint8
	queue   []expiry // touches in time order
	qhead   int

	// BFS state, stamped to avoid clearing.
	seen    []uint32
	stamp   uint32
	from    []int32 // line the key in via moves out of
	via     []int32 // key that moves into this line
	depth   []int32
	bfs     []int32
	target  int32 // line place freed a slot in
	inserts int
	drops   int
	live    int
	peak    int
	moves   [32]int // shortest augmenting path length (moves) per insert; 31 = 31+
}

type expiry struct {
	id int32
	t  uint64
}

func newOracle(f *Filter, horizon uint64) *oracle {
	n := int(f.lines)
	o := &oracle{f: f, horizon: horizon, ids: map[uint64]int32{},
		slots: make([][slotsPerLine]int32, n), used: make([]uint8, n),
		seen: make([]uint32, n), from: make([]int32, n), via: make([]int32, n), depth: make([]int32, n)}
	for i := range o.slots {
		o.slots[i] = [slotsPerLine]int32{-1, -1, -1, -1}
	}
	return o
}

// touch returns whether h was seen within w, then records the touch.
func (o *oracle) touch(t *testing.T, h, w, now uint64) bool {
	o.expire(now)
	if id, ok := o.ids[h]; ok {
		seen := now-o.last[id] <= w
		o.last[id] = now
		o.queue = append(o.queue, expiry{id, now})
		return seen
	}
	o.inserts++
	p := o.f.primary(h)
	c := [2]int32{int32(p), int32(o.f.alt(p, fingerprint(h)))}
	moves, ok := o.place(t, c)
	if !ok {
		o.drops++
		return false
	}
	o.moves[min(moves, len(o.moves)-1)]++
	var id int32
	if n := len(o.free); n > 0 {
		id, o.free = o.free[n-1], o.free[:n-1]
		o.h[id], o.last[id], o.cand[id] = h, now, c
	} else {
		id = int32(len(o.h))
		o.h, o.last, o.cand, o.line = append(o.h, h), append(o.last, now), append(o.cand, c), append(o.line, -1)
	}
	o.ids[h] = id
	o.put(id, o.target)
	o.queue = append(o.queue, expiry{id, now})
	o.live++
	o.peak = max(o.peak, o.live)
	return false
}

// expire removes keys whose last touch is more than horizon before now.
func (o *oracle) expire(now uint64) {
	for o.qhead < len(o.queue) && now-o.queue[o.qhead].t > o.horizon {
		e := o.queue[o.qhead]
		o.qhead++
		if o.last[e.id] == e.t && o.line[e.id] >= 0 {
			o.remove(e.id)
		}
	}
	if o.qhead > 1<<20 {
		o.queue = append(o.queue[:0], o.queue[o.qhead:]...)
		o.qhead = 0
	}
}

func (o *oracle) put(id, line int32) {
	for s, k := range o.slots[line] {
		if k < 0 {
			o.slots[line][s] = id
			o.used[line]++
			o.line[id] = line
			return
		}
	}
	panic("oracle: put into a full line")
}

func (o *oracle) take(id int32) {
	l := o.line[id]
	for s, k := range o.slots[l] {
		if k == id {
			o.slots[l][s] = -1
			o.used[l]--
			o.line[id] = -1
			return
		}
	}
	panic("oracle: key not in its line")
}

func (o *oracle) remove(id int32) {
	o.take(id)
	delete(o.ids, o.h[id])
	o.free = append(o.free, id)
	o.live--
}

// place frees a slot in one of the candidate lines c, moving keys along a
// shortest augmenting path, and sets o.target to that line. It returns the
// number of moves, or false (with a verified certificate) if no placement
// exists.
func (o *oracle) place(t *testing.T, c [2]int32) (int, bool) {
	for _, l := range c {
		if o.used[l] < slotsPerLine {
			o.target = l
			return 0, true
		}
	}
	o.stamp++
	o.bfs = o.bfs[:0]
	for _, l := range c {
		if o.seen[l] != o.stamp {
			o.seen[l], o.from[l], o.depth[l] = o.stamp, -1, 0
			o.bfs = append(o.bfs, l)
		}
	}
	for i := 0; i < len(o.bfs); i++ {
		m := o.bfs[i]
		for _, k := range o.slots[m] {
			other := o.cand[k][0]
			if other == m {
				other = o.cand[k][1]
			}
			if other == m || o.seen[other] == o.stamp {
				continue
			}
			o.seen[other], o.from[other], o.via[other], o.depth[other] = o.stamp, m, k, o.depth[m]+1
			if o.used[other] < slotsPerLine { // augment back along the path
				moves := int(o.depth[other])
				cur := other
				for o.from[cur] >= 0 {
					k, prev := o.via[cur], o.from[cur]
					o.take(k)
					o.put(k, cur)
					cur = prev
				}
				o.target = cur
				return moves, true
			}
			o.bfs = append(o.bfs, other)
		}
	}
	// Certificate: every line reached is full, and every key in them has
	// both candidates among them, so they hold 4|S| keys that need these
	// 4|S| slots, plus the new key.
	for _, m := range o.bfs {
		if o.used[m] != slotsPerLine {
			t.Fatalf("oracle: line %d reached but not full", m)
		}
		for _, k := range o.slots[m] {
			for _, l := range o.cand[k] {
				if o.seen[l] != o.stamp {
					t.Fatalf("oracle: key %d in line %d has candidate %d outside the closed set", k, m, l)
				}
			}
		}
	}
	return 0, false
}

// TestOracleMatchesMaxFlow checks the oracle against an independent exact
// method on small overloaded tables: before every insert, a from-scratch
// maximum bipartite b-matching (Kuhn's augmenting paths) of the live keys
// plus the new key must place all of them exactly when the oracle can.
func TestOracleMatchesMaxFlow(t *testing.T) {
	const capN, hTicks = 170, 6_000 // 50 lines
	feasible, infeasible := 0, 0
	for seed := uint64(1); seed <= 20; seed++ {
		f, err := New(Config{Capacity: capN, Horizon: time.Minute, Resolution: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		o := newOracle(f, hTicks)
		workload.Churn(1.4, seed, capN, hTicks, mix64, func(h, w, now uint64) {
			o.expire(now) // as touch does first; then ask max-flow
			_, live := o.ids[h]
			var want bool
			if !live {
				p := f.primary(h)
				keys := [][2]int32{{int32(p), int32(f.alt(p, fingerprint(h)))}}
				for _, id := range o.ids {
					keys = append(keys, o.cand[id])
				}
				want = kuhnPlacesAll(keys, int(f.lines))
			}
			drops := o.drops
			o.touch(t, h, w, now)
			if !live {
				got := o.drops == drops
				if got != want {
					t.Fatalf("seed %d now %d: oracle placed=%v, max-flow placed=%v", seed, now, got, want)
				}
				if want {
					feasible++
				} else {
					infeasible++
				}
			}
		})
	}
	t.Logf("agreed on %d feasible and %d infeasible inserts", feasible, infeasible)
	if infeasible == 0 || feasible == 0 {
		t.Fatal("workload did not exercise both outcomes")
	}
}

// kuhnPlacesAll reports whether every key can sit in one of its two lines
// with at most slotsPerLine keys per line.
func kuhnPlacesAll(keys [][2]int32, lines int) bool {
	owner := make([][]int, lines)
	var try func(k int, seen []bool) bool
	try = func(k int, seen []bool) bool {
		for _, l := range keys[k] {
			if seen[l] {
				continue
			}
			seen[l] = true
			if len(owner[l]) < slotsPerLine {
				owner[l] = append(owner[l], k)
				return true
			}
			for i, other := range owner[l] {
				if try(other, seen) {
					owner[l][i] = k
					return true
				}
			}
		}
		return false
	}
	for k := range keys {
		if !try(k, make([]bool, lines)) {
			return false
		}
	}
	return true
}

// TestLoadThreshold computes the load threshold of 2-choice cuckoo hashing
// with buckets of l slots: c*/l, where x solves 2l = x·Q(x,l)/Q(x,l+1),
// c* = x/(2·Q(x,l)) and Q(x,y) = P(Poisson(x) >= y) (Cain, Sanders and
// Wormald 2007; Fernholz and Ramachandran 2007). Below it, a placement of
// all keys exists with high probability as the table grows. It checks the
// values tabulated in Walzer, "What if we tried Less Power?" (arXiv
// 2307.00644, §1.1.1) and the classic 1/2 for one slot.
func TestLoadThreshold(t *testing.T) {
	poissonTail := func(x float64, y int) float64 { // P(Poisson(x) >= y)
		term, cdf := math.Exp(-x), 0.0
		for j := 0; j < y; j++ {
			cdf += term
			term *= x / float64(j+1)
		}
		return 1 - cdf
	}
	threshold := func(l int) float64 {
		lo, hi := 1e-9, 100.0
		for range 200 {
			x := (lo + hi) / 2
			if x*poissonTail(x, l)/poissonTail(x, l+1) < float64(2*l) {
				lo = x
			} else {
				hi = x
			}
		}
		x := (lo + hi) / 2
		return x / (2 * poissonTail(x, l)) / float64(l)
	}
	for _, c := range []struct {
		l    int
		want float64
	}{{1, 0.5}, {2, 0.89701}, {3, 0.95915}, {4, 0.98037}} {
		got := threshold(c.l)
		t.Logf("placement threshold choices=2 slots=%d load=%.5f", c.l, got)
		if math.Abs(got-c.want) > 5e-6 {
			t.Errorf("slots=%d: threshold %.6f, want %.5f", c.l, got, c.want)
		}
	}
}

// TestComparedEntries measures how many stored entries an absent-key lookup
// compares its 28-bit identity with, at the scorecard's static-FPR setup
// (200k keys at the default 85% load). With the displaced bit it compares
// only entries homed at its primary line; without it, every entry in both
// candidate lines. Each comparison is a false positive with probability
// 2^-28, so the expected FPR is the mean count / 2^28.
func TestComparedEntries(t *testing.T) {
	const capN, probes = 200_000, 1_000_000
	f, err := New(Config{Capacity: capN, Horizon: time.Minute, Resolution: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for i := range uint64(capN) {
		f.CheckAndTouchAt(mix64(i), 0, 1)
	}
	var with, without int
	for i := range uint64(probes) {
		h := mix64(capN + i)
		fp := fingerprint(h)
		p := f.primary(h)
		a := f.alt(p, fp)
		for li, l := range []uint64{p, a} {
			if li == 1 && a == p {
				break
			}
			for s := 0; s < wordsPerLine; s += 2 {
				w0 := f.words[int(l)*wordsPerLine+s]
				if w0 == 0 {
					continue
				}
				without++
				if (w0&dispBit != 0) == (li == 1) {
					with++
				}
			}
		}
	}
	mw, mo := float64(with)/probes, float64(without)/probes
	t.Logf("placement compared_entries probes=%d with_displaced_bit=%.4f without=%.4f expected_fpr_with=%.3e without=%.3e",
		probes, mw, mo, mw/(1<<28), mo/(1<<28))
	stored, displaced := 0, 0
	for i := 0; i < len(f.words); i += 2 {
		if w0 := f.words[i]; w0 != 0 {
			stored++
			if w0&dispBit != 0 {
				displaced++
			}
		}
	}
	t.Logf("placement displaced_entries stored=%d in_alternate_line=%d (%.1f%%)", stored, displaced, 100*float64(displaced)/float64(stored))
}
