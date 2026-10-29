package temporalfilter

import (
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFuzzUpper (TF_FUZZ=<operations>, e.g. 1e10) checks the fail-closed
// contract over random configurations and operation streams: unless an
// answer is Saturated, its Upper is never below the key's true count in the
// window (docs/DESIGN.md, Trust flags). It checks Count's answers and
// Touch's own (which include the touch). The documented exceptions are
// counted, not failed: a low answer behind a live overflow floor (a key
// floored more than once), and, with the overflow table off, a low answer
// within a horizon of a lost history (Lost counts them; the key may have
// come back since, so the answer itself need not be Degraded). Histories
// are never shed (SpillMin 1), as in the soak.
//
// Each case is a fresh filter driven by one goroutine from a seeded stream,
// so a failure replays exactly: TF_FUZZ_CASE=<seed> runs one case.
// Callers' clocks lag the newest by up to a few units, as concurrent
// callers' do (an eviction's clock can be behind the key's last touch).
// Cases run GOMAXPROCS at a time from seed TF_FUZZ_SEED (default 1) until
// TF_FUZZ operations have run. The concurrent soak (TestStressSoak) covers
// races; this covers configurations, clocks and operation mixes.
func TestFuzzUpper(t *testing.T) {
	if s := os.Getenv("TF_FUZZ_CASE"); s != "" {
		seed, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		r := fuzzCase(seed)
		t.Logf("case %d: %s", seed, r.summary())
		if r.fail != "" {
			t.Fatal(r.fail)
		}
		return
	}
	total, err := strconv.ParseFloat(os.Getenv("TF_FUZZ"), 64)
	if err != nil || total <= 0 {
		t.Skip("set TF_FUZZ=<operations> to run")
	}
	first := uint64(1)
	if s := os.Getenv("TF_FUZZ_SEED"); s != "" {
		if first, err = strconv.ParseUint(s, 10, 64); err != nil {
			t.Fatal(err)
		}
	}
	var next, done atomic.Uint64
	next.Store(first)
	var mu sync.Mutex
	var sum fuzzResult
	var fails []string
	t0 := time.Now()
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for done.Load() < uint64(total) {
				seed := next.Add(1) - 1
				r := fuzzCase(seed)
				done.Add(r.ops)
				mu.Lock()
				sum.add(r)
				if r.fail != "" {
					fails = append(fails, fmt.Sprintf("case %d: %s", seed, r.fail))
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	el := time.Since(t0)
	t.Logf("%d cases from seed %d in %v (%.1f M ops/s): %s", next.Load()-first, first, el.Round(time.Second),
		float64(sum.ops)/el.Seconds()/1e6, sum.summary())
	for _, f := range fails {
		t.Error(f)
	}
}

// fuzzResult tallies one case (or several).
type fuzzResult struct {
	ops, checks, sat, floored, lostLow, shared                  uint64
	ct, bg, noStash, noOverflow, smallOverflow, noHot, smallHot uint64 // cases by configuration
	evictions, folded, promoted, waited                         uint64
	fail                                                        string
}

func (r *fuzzResult) add(o fuzzResult) {
	r.ops += o.ops
	r.checks += o.checks
	r.sat += o.sat
	r.floored += o.floored
	r.lostLow += o.lostLow
	r.shared += o.shared
	r.ct += o.ct
	r.bg += o.bg
	r.noStash += o.noStash
	r.noOverflow += o.noOverflow
	r.smallOverflow += o.smallOverflow
	r.noHot += o.noHot
	r.smallHot += o.smallHot
	r.evictions += o.evictions
	r.folded += o.folded
	r.promoted += o.promoted
	r.waited += o.waited
}

func (r *fuzzResult) summary() string {
	return fmt.Sprintf("%d ops, %d answers checked (%d Saturated, %d low behind a live floor, %d low after a loss with the overflow off, %d off for a key whose identity another shares); "+
		"cases with ConstantTime %d, Background %d, no stash %d, no overflow %d, small overflow %d, no hot entries %d, few hot entries %d; "+
		"evictions %d, folded %d, promoted %d, waited %d",
		r.ops, r.checks, r.sat, r.floored, r.lostLow, r.shared, r.ct, r.bg, r.noStash, r.noOverflow, r.smallOverflow, r.noHot, r.smallHot,
		r.evictions, r.folded, r.promoted, r.waited)
}

// fuzzTruth keeps every key's event times (sorted) within the horizon of
// the newest clock any call has used, and keep ticks more (for the checks
// of answers that read high, which reach behind a window).
type fuzzTruth struct {
	ev      map[uint64][]uint64
	horizon uint64
	newest  uint64
	keep    uint64
}

// since is the number of h's events at lo or later, later than any clock
// included (a lagging caller's window holds them).
func (tr *fuzzTruth) since(h, lo uint64) int {
	l := tr.ev[h]
	a, _ := slices.BinarySearch(l, lo)
	return len(l) - a
}

func (tr *fuzzTruth) add(h, ts uint64) {
	l := append(tr.ev[h], ts)
	for i := len(l) - 1; i > 0 && l[i-1] > l[i]; i-- { // a lagging clock: keep the times sorted
		l[i-1], l[i] = l[i], l[i-1]
	}
	tr.ev[h] = l
}

// count is the true count of h's events in [now-window, now] that are
// within the horizon of the newest clock (a call at that clock may have
// expired older ones), window capped at the horizon as the filter caps it.
func (tr *fuzzTruth) count(h, window, now uint64) int {
	l := tr.ev[h]
	lo := tr.newest - min(tr.newest, tr.horizon)
	if w := min(window, tr.horizon); now-min(now, w) > lo {
		lo = now - w
	}
	a, _ := slices.BinarySearch(l, lo)
	b, _ := slices.BinarySearch(l, now+1)
	return max(0, b-a)
}

// trim drops events past the horizon of the newest clock and keep, from
// every key.
func (tr *fuzzTruth) trim() {
	lo := tr.newest - min(tr.newest, tr.horizon+tr.keep)
	for h, l := range tr.ev {
		i, _ := slices.BinarySearch(l, lo)
		if i == len(l) {
			delete(tr.ev, h)
		} else if i > 0 {
			tr.ev[h] = slices.Clip(l[i:])
		}
	}
}

// fuzzConfig draws a case's configuration (horizon H in ticks of 1 ms) and
// tallies it in res.
func fuzzConfig(rng *rand.Rand, res *fuzzResult) (cfg Config, capacity int, H uint64) {
	capacity = 1 << (6 + rng.IntN(9)) // 64 .. 32K keys
	capacity += rng.IntN(capacity)
	horizons := []uint64{48, 96, 240, 1_000, 10_000, 60_000}
	H = horizons[rng.IntN(len(horizons))]
	cfg = Config{Capacity: capacity, Horizon: time.Duration(H) * time.Millisecond, Resolution: time.Millisecond, SpillMin: 1}
	if rng.IntN(4) == 0 {
		cfg.Load = 0.5 + 0.5*rng.Float64()
	}
	switch rng.IntN(8) {
	case 0:
		cfg.Stash, res.noStash = -1, 1
	case 1, 2:
		cfg.Stash = 1 + rng.IntN(64)
	}
	switch rng.IntN(8) {
	case 0:
		cfg.Overflow, res.noOverflow = -1, 1
	case 1, 2:
		cfg.Overflow, res.smallOverflow = 1+rng.IntN(256), 1
	}
	switch rng.IntN(8) {
	case 0:
		cfg.Hot, res.noHot = -1, 1
	case 1, 2:
		cfg.Hot, res.smallHot = 1+rng.IntN(8), 1
	}
	return cfg, capacity, H
}

// fuzzCase runs one case: a configuration and an operation stream drawn
// from seed.
func fuzzCase(seed uint64) (res fuzzResult) {
	rng := rand.New(rand.NewPCG(seed, 0x5eed_f022))
	cfg, capacity, H := fuzzConfig(rng, &res)
	var T uint64 = 1 + rng.Uint64N(1<<30) // the newest clock
	cfg.Clock = func() uint64 { return T }
	f, err := New(cfg)
	if err != nil {
		res.fail = fmt.Sprintf("New(%+v): %v", cfg, err)
		return res
	}
	if rng.IntN(5) == 0 {
		f.ct, res.ct = true, 1 // functional ConstantTime (no pad), as TF_CT=1 runs it
	}
	// Background cases come from their own stream, so every other case is
	// drawn as before. The placer runs at seeded points instead of in a
	// goroutine, so a case still replays exactly.
	bgRng := rand.New(rand.NewPCG(seed, 0xb6))
	placeEvery := 0
	if bgRng.IntN(4) == 0 && !f.ct {
		f.bg, f.bgManual, res.bg = true, true, 1
		placeEvery = []int{1, 4, 32, 512}[bgRng.IntN(4)]
	}
	defer func() {
		res.evictions, res.folded, res.promoted, res.waited = f.Evictions(), f.Folded(), f.Promoted(), f.Waited()
	}()
	ops := 1 << (16 + rng.IntN(7)) // 64K .. 4M
	unit := f.unit
	maxLag := []uint64{0, 2, unit, 2 * unit}[rng.IntN(4)]
	// Operations per horizon, from idle to a flood of 64x the capacity.
	perH := float64(capacity) * []float64{0.25, 1, 4, 16, 64}[rng.IntN(5)]
	tick, acc := float64(H)/perH, 0.0
	salt := rng.Uint64()
	key := func(k uint64) uint64 { return mix64(salt ^ k) }
	heavy := make([]uint64, 1+rng.IntN(32))
	for i := range heavy {
		heavy[i] = key(1<<62 | uint64(i))
	}
	// An answer from a register (the table, the stash or the wait room)
	// counts a bucket the window's edge cuts whole: buckets are at most 8
	// units wide. A touch at a clock behind the register's unit counts in
	// that unit, so an event can sit up to a unit and maxLag later than it
	// happened. No answer counts an event further back than that.
	reach := 9*unit + maxLag + 1
	tr := &fuzzTruth{ev: map[uint64][]uint64{}, horizon: H, newest: T, keep: reach + maxLag}
	pools := []float64{0.25, 0.5, 1, 2, 4, 8}
	pool, phaseLeft := uint64(capacity), 0
	trimAt := T + H
	hs := make([]uint64, 16)
	out := make([]bool, 16)
	cnt := make([]int, 16)
	desc := func() string {
		return fmt.Sprintf("config %+v ct %v bg %v place 1/%d horizon %d ticks unit %d lag <= %d ops/horizon %.0f", cfg, f.ct, f.bg, placeEvery, H, unit, maxLag, perH)
	}
	// shared: another key of the case has h's identity (line, fingerprint
	// and tag), so the filter can't tell their events apart.
	shared := func(h uint64) bool {
		id := f.stashID(h, fingerprint(h), tag(h))
		for k := range tr.ev {
			if k != h && f.stashID(k, fingerprint(k), tag(k)) == id {
				return true
			}
		}
		return false
	}
	// high checks the other direction, on what the register answers (the
	// overflow table's part, which counts a dropped history's events whole
	// and other keys' through floors and fingerprint matches, taken out):
	// Upper counts no event from further back than the window reaches by
	// reach, and Seen says whether the key has an event in the window
	// (before this touch, for Touch). Events older than the newest clock's
	// horizon may have been expired by a call at that clock, so they
	// needn't be seen.
	high := func(op string, h, window, now uint64, r Result) bool {
		var s Result
		if g := f.gh.Load(); g != nil {
			s, _ = g.estimate(f.stashID(h, fingerprint(h), tag(h)), min(window, H), now)
		}
		own := 0
		if op == "Touch" {
			own = 1 // the model has this touch already
		}
		w := min(window, H)
		lo := now - min(now, w)
		bad := ""
		switch upper := r.Upper - s.Upper; {
		case upper > tr.since(h, now-min(now, w+reach)):
			bad = fmt.Sprintf("Upper %d (the overflow's %d) > %d events back to %d", r.Upper, s.Upper, tr.since(h, now-min(now, w+reach)), now-min(now, w+reach))
		case r.Seen && !s.Seen && tr.since(h, lo) <= own:
			bad = fmt.Sprintf("Seen with no event since %d", lo)
		case !r.Seen && tr.since(h, max(lo, tr.newest-min(tr.newest, H))) > own && !f.floorLive(h, window, now) && !(cfg.Overflow < 0 && f.lostRecently(now)):
			bad = fmt.Sprintf("not Seen with %d events since %d", tr.since(h, max(lo, tr.newest-min(tr.newest, H)))-own, max(lo, tr.newest-min(tr.newest, H)))
		default:
			return true
		}
		if shared(h) {
			res.shared++
			return true
		}
		res.fail = fmt.Sprintf("op %d %s: %s (result %+v); key %x window %d now %d newest %d; events %v; %s; %s",
			res.ops, op, bad, r, h, window, now, tr.newest, tail(tr.ev[h], 12), f.whereIs(h, now), desc())
		return false
	}
	check := func(op string, h, window, now uint64, r Result) bool {
		res.checks++
		if !high(op, h, window, now, r) {
			return false
		}
		if r.Saturated {
			res.sat++
			return true
		}
		exact := tr.count(h, window, now)
		if r.Upper >= exact {
			return true
		}
		switch {
		case f.floorLive(h, window, now):
			res.floored++
		case cfg.Overflow < 0 && f.lostRecently(now):
			res.lostLow++ // without the overflow table a dropped history is lost (Lost), held key or not
		default:
			res.fail = fmt.Sprintf("op %d %s: Upper %d < true %d (result %+v); key %x window %d now %d newest %d; events %v; %s; %s",
				res.ops, op, r.Upper, exact, r, h, window, now, tr.newest, tail(tr.ev[h], 12), f.whereIs(h, now), desc())
			return false
		}
		return true
	}
	// TF_FUZZ_TRACE=<key in hex> prints every operation on that key with
	// where its history is afterwards (for replaying a failing case).
	traceKey, _ := strconv.ParseUint(os.Getenv("TF_FUZZ_TRACE"), 16, 64)
	traceIn := false
	trace := func(op string, h, now uint64, detail any) {
		if traceKey != 0 && h == traceKey {
			fmt.Fprintf(os.Stderr, "op %d %s now %d (T %d) %v: %s; true %v\n", res.ops, op, now, T, detail, f.whereIs(h, now), tail(tr.ev[h], 8))
		}
	}
	for ; res.ops < uint64(ops); res.ops++ {
		if phaseLeft == 0 { // the pool of live keys cycles across the table's capacity
			pool = max(1, uint64(pools[rng.IntN(len(pools))]*float64(capacity)))
			phaseLeft = 1 + rng.IntN(4*capacity)
		}
		phaseLeft--
		acc += tick * 2 * rng.Float64() // the clock moves on, perH operations per horizon on average
		if acc >= 1 {
			T += uint64(acc)
			acc -= float64(uint64(acc))
		}
		if rng.Float64()*perH < 0.1 { // about once per 10 horizons, a jump of up to 3 horizons
			T += 1 + rng.Uint64N(3*H)
		}
		now := T
		if maxLag > 0 && rng.IntN(4) == 0 {
			now -= rng.Uint64N(maxLag + 1)
		}
		tr.newest = max(tr.newest, now)
		var h uint64
		if rng.IntN(16) == 0 {
			h = heavy[rng.IntN(len(heavy))]
		} else {
			h = key(rng.Uint64N(pool))
		}
		window := 1 + rng.Uint64N(H+H/4) // windows past the horizon too
		switch op := rng.IntN(100); {
		case op < 40:
			tr.add(h, now)
			r := f.TouchAt(h, window, now)
			trace("Touch", h, now, r)
			if !check("Touch", h, window, now, r) {
				return res
			}
		case op < 45: // a burst in one tick (promotes heavy keys)
			n := 1 + rng.IntN(64)
			for range n {
				f.TouchAt(h, 0, now)
				tr.add(h, now)
			}
			trace("burst", h, now, n)
			res.ops += uint64(n - 1)
		case op < 50:
			s := f.CheckAndTouchAt(h, window, now)
			tr.add(h, now)
			trace("CheckAndTouch", h, now, s)
		case op < 55:
			s, c := f.TouchAndCountAt(h, window, now)
			tr.add(h, now)
			trace("TouchAndCount", h, now, [2]any{s, c})
		case op < 85:
			r := f.CountAt(h, window, now)
			trace("Count", h, now, r)
			if !check("Count", h, window, now, r) {
				return res
			}
		case op < 90:
			c, v := f.ContainsAt(h, window, now), f.VelocityCountAt(h, window, now)
			trace("Contains+VelocityCount", h, now, [2]any{c, v})
			res.ops++
		case op < 97:
			for i := range hs {
				if rng.IntN(2) == 0 {
					hs[i] = heavy[rng.IntN(len(heavy))]
				} else {
					hs[i] = key(rng.Uint64N(pool))
				}
			}
			if rng.IntN(2) == 0 {
				f.CheckAndTouchBatch(hs, window, now, out)
			} else {
				f.TouchAndCountBatch(hs, window, now, out, cnt)
			}
			for _, x := range hs {
				tr.add(x, now)
				trace("batch", x, now, "")
			}
			f.VelocityCountBatch(hs, window, now, cnt)
			res.ops++ // two calls (of 16 keys each)
		default:
			f.decayStep(now, uint64(1+rng.IntN(16)))
			if traceKey != 0 {
				trace("decay", traceKey, now, "")
			}
		}
		if placeEvery > 0 && bgRng.IntN(placeEvery) == 0 {
			f.placeNext(f.room.Load(), 0)
		}
		if traceKey != 0 { // report the operation after which the traced key's slot or stash entry is gone
			w := f.whereIs(traceKey, T)
			if in := strings.Contains(w, "slot") || strings.Contains(w, "stash"); in != traceIn {
				fmt.Fprintf(os.Stderr, "op %d (key %x now %d T %d; a batch's keys: %x): traced key in table or stash: %v; %s\n", res.ops, h, now, T, hs, in, w)
				traceIn = in
			}
		}
		if tr.newest >= trimAt && len(tr.ev) > 0 {
			tr.trim()
			trimAt = tr.newest + H
		}
	}
	if occ := f.Occupancy(tr.newest); occ > f.Slots() {
		res.fail = fmt.Sprintf("occupancy %d > %d slots; %s", occ, f.Slots(), desc())
	}
	return res
}

func tail(l []uint64, n int) []uint64 { return l[max(0, len(l)-n):] }
