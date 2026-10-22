package temporalfilter

import (
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFuzzConcurrent (TF_CFUZZ=<duration>, e.g. 10m) is the soak
// (TestStressSoak) over the fuzzer's configurations (fuzzConfig: load,
// stash, overflow table, hot entries, horizon), with ConstantTime
// (functional, as TF_CT=1 runs it) or Background (its placer a goroutine)
// in some cases. A case runs 2 to 2×GOMAXPROCS goroutines on one filter.
// Each owns its keys, so it knows their true counts: it touches, counts and
// batches them (repeats included) at a shared clock that the goroutines move
// on, reading it with a lag of up to two units, and checks Upper on every
// Touch and Count as TestFuzzUpper does, with its documented exceptions.
// One operation in eight is on a few keys every goroutine shares, so a key's
// reads race its touches; a Count of a shared key must cover the events whose
// touches returned before it began. Events a call at the newest clock any
// call has used could have expired are left out of the truth. A case's configuration and streams come from
// its seed (TF_CFUZZ_SEED, default 1, for the first), but its run depends
// on its races: TF_CFUZZ_CASE=<seed> runs one case TF_CFUZZ_RUNS times
// (default 20).
func TestFuzzConcurrent(t *testing.T) {
	if s := os.Getenv("TF_CFUZZ_CASE"); s != "" {
		seed, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		runs, _ := strconv.Atoi(os.Getenv("TF_CFUZZ_RUNS"))
		if runs <= 0 {
			runs = 20
		}
		var sum fuzzResult
		for range runs {
			r := cfuzzCase(seed)
			sum.add(r)
			if r.fail != "" {
				t.Fatalf("case %d: %s", seed, r.fail)
			}
		}
		t.Logf("case %d: %s", seed, sum.summary())
		return
	}
	d, err := time.ParseDuration(os.Getenv("TF_CFUZZ"))
	if err != nil || d <= 0 {
		t.Skip("set TF_CFUZZ=<duration> to run")
	}
	seed := uint64(1)
	if s := os.Getenv("TF_CFUZZ_SEED"); s != "" {
		if seed, err = strconv.ParseUint(s, 10, 64); err != nil {
			t.Fatal(err)
		}
	}
	first := seed
	var sum fuzzResult
	t0 := time.Now()
	for ; time.Since(t0) < d; seed++ {
		r := cfuzzCase(seed)
		sum.add(r)
		if r.fail != "" {
			t.Errorf("case %d: %s", seed, r.fail)
		}
	}
	el := time.Since(t0)
	t.Logf("%d cases from seed %d in %v (%.1f M ops/s): %s", seed-first, first, el.Round(time.Second),
		float64(sum.ops)/el.Seconds()/1e6, sum.summary())
}

// cfuzzCase runs one concurrent case.
func cfuzzCase(seed uint64) (res fuzzResult) {
	rng := rand.New(rand.NewPCG(seed, 0xc0_f022))
	cfg, capacity, H := fuzzConfig(rng, &res)
	var clock, newest atomic.Uint64 // the clock callers read; the newest any call has used
	clock.Store(1 + rng.Uint64N(1<<30))
	newest.Store(clock.Load())
	cfg.Clock = clock.Load
	mode := rng.IntN(5)
	if mode == 1 {
		cfg.Background, res.bg = true, 1
	}
	f, err := New(cfg)
	if err != nil {
		res.fail = fmt.Sprintf("New(%+v): %v", cfg, err)
		return res
	}
	defer f.Close()
	if mode == 0 {
		f.ct, res.ct = true, 1
	}
	workers := 2 + rng.IntN(2*runtime.GOMAXPROCS(0)-1)
	ops := 1 << (13 + rng.IntN(5)) // per goroutine: 8K .. 128K
	maxLag := []uint64{0, 2, f.unit, 2 * f.unit}[rng.IntN(4)]
	// Operations per horizon, all goroutines together, from idle to a flood
	// of 64x the capacity; the pool of live keys cycles across the capacity.
	perH := float64(capacity) * []float64{0.25, 1, 4, 16, 64}[rng.IntN(5)]
	desc := fmt.Sprintf("config %+v ct %v bg %v goroutines %d horizon %d ticks unit %d lag <= %d ops/horizon %.0f",
		cfg, f.ct, f.bg, workers, H, f.unit, maxLag, perH)
	sh := &cfuzzShared{keys: make([]uint64, 1+rng.IntN(16)), tr: &fuzzTruth{ev: map[uint64][]uint64{}, horizon: H}}
	for i := range sh.keys {
		sh.keys[i] = mix64(1<<62 | uint64(i))
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for g := range workers {
		wseed := rng.Uint64()
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := cfuzzWorker(f, uint64(g), wseed, ops, capacity/workers+1, H, maxLag, perH, &clock, &newest, sh)
			mu.Lock()
			defer mu.Unlock()
			res.add(r)
			if r.fail != "" && res.fail == "" {
				res.fail = r.fail + "; " + desc
			}
		}()
	}
	wg.Wait()
	res.evictions, res.folded, res.promoted, res.waited = f.Evictions(), f.Folded(), f.Promoted(), f.Waited()
	if occ := f.Occupancy(newest.Load()); occ > f.Slots() && res.fail == "" {
		res.fail = fmt.Sprintf("occupancy %d > %d slots; %s", occ, f.Slots(), desc)
	}
	return res
}

// cfuzzShared: the keys every goroutine of a case touches and counts, and
// their events whose touches have returned.
type cfuzzShared struct {
	keys []uint64
	mu   sync.Mutex
	tr   *fuzzTruth
}

func (s *cfuzzShared) add(h, ts uint64) {
	s.mu.Lock()
	s.tr.add(h, ts)
	s.mu.Unlock()
}

// snapshot is h's events so far, as a truth of its own.
func (s *cfuzzShared) snapshot(h uint64) *fuzzTruth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &fuzzTruth{ev: map[uint64][]uint64{h: append([]uint64(nil), s.tr.ev[h]...)}, horizon: s.tr.horizon}
}

// cfuzzWorker is one goroutine of a concurrent case: ops operations on its
// own keys (g in the top bits), share its share of the capacity, and on the
// shared keys.
func cfuzzWorker(f *Filter, g, seed uint64, ops, share int, H, maxLag uint64, perH float64, clock, newest *atomic.Uint64, sh *cfuzzShared) (res fuzzResult) {
	rng := rand.New(rand.NewPCG(seed, g))
	key := func(k uint64) uint64 { return mix64(g<<48 ^ k) }
	heavy := make([]uint64, 1+rng.IntN(4))
	for i := range heavy {
		heavy[i] = key(1<<40 | uint64(i))
	}
	tr := &fuzzTruth{ev: map[uint64][]uint64{}, horizon: H, newest: newest.Load()}
	pools := []float64{0.25, 0.5, 1, 2, 4, 8}
	pool, phaseLeft := uint64(share), 0
	tick, acc := float64(H)/perH, 0.0
	trimAt := tr.newest + H
	hs := make([]uint64, 16)
	out := make([]bool, 16)
	cnt := make([]int, 16)
	check := func(op string, h, window, now uint64, r Result) bool {
		res.checks++
		if r.Saturated {
			res.sat++
			return true
		}
		tr.newest = max(tr.newest, newest.Load()) // a call at this clock may have expired older events
		exact := tr.count(h, window, now)
		if r.Upper >= exact {
			return true
		}
		switch {
		case f.floorLive(h, window, now):
			res.floored++
		case f.skW == 0 && f.lostRecently(now):
			res.lostLow++
		default:
			res.fail = fmt.Sprintf("goroutine %d op %d %s: Upper %d < true %d (result %+v); key %x window %d now %d newest %d; events %v; %s",
				g, res.ops, op, r.Upper, exact, r, h, window, now, tr.newest, tail(tr.ev[h], 12), f.whereIs(h, now))
			return false
		}
		return true
	}
	for ; res.ops < uint64(ops); res.ops++ {
		if phaseLeft == 0 {
			pool = max(1, uint64(pools[rng.IntN(len(pools))]*float64(share)))
			phaseLeft = 1 + rng.IntN(4*share)
		}
		phaseLeft--
		acc += tick * 2 * rng.Float64()
		if acc >= 1 {
			clock.Add(uint64(acc))
			acc -= float64(uint64(acc))
		}
		if rng.Float64()*perH < 0.1 {
			clock.Add(1 + rng.Uint64N(3*H))
		}
		now := clock.Load()
		if maxLag > 0 && rng.IntN(4) == 0 {
			now -= rng.Uint64N(maxLag + 1)
		}
		for n := newest.Load(); now > n && !newest.CompareAndSwap(n, now); n = newest.Load() {
		}
		tr.newest = max(tr.newest, now)
		window := 1 + rng.Uint64N(H+H/4)
		if rng.IntN(8) == 0 { // a shared key
			h := sh.keys[rng.IntN(len(sh.keys))]
			op := rng.IntN(4)
			switch op {
			case 0:
				f.TouchAt(h, window, now)
			case 1:
				f.CheckAndTouchAt(h, window, now)
			case 2:
				f.TouchAndCountAt(h, window, now)
			default:
				before := sh.snapshot(h) // touches that returned before the count began
				r := f.CountAt(h, window, now)
				if r.Saturated {
					res.checks++
					res.sat++
					break
				}
				before.newest = max(tr.newest, newest.Load())
				if exact := before.count(h, window, now); r.Upper < exact && !f.floorLive(h, window, now) && !(f.skW == 0 && f.lostRecently(now)) {
					res.checks++
					res.fail = fmt.Sprintf("goroutine %d op %d shared Count: Upper %d < true %d (result %+v); key %x window %d now %d newest %d; events %v; %s",
						g, res.ops, r.Upper, exact, r, h, window, now, before.newest, tail(before.ev[h], 12), f.whereIs(h, now))
					return res
				}
				res.checks++
			}
			if op < 3 {
				sh.add(h, now)
			}
			if res.ops%4096 == 0 {
				sh.mu.Lock()
				sh.tr.newest = max(sh.tr.newest, newest.Load())
				sh.tr.trim()
				sh.mu.Unlock()
			}
			continue
		}
		var h uint64
		if rng.IntN(16) == 0 {
			h = heavy[rng.IntN(len(heavy))]
		} else {
			h = key(rng.Uint64N(pool))
		}
		switch op := rng.IntN(100); {
		case op < 40:
			tr.add(h, now)
			if !check("Touch", h, window, now, f.TouchAt(h, window, now)) {
				return res
			}
		case op < 45:
			n := 1 + rng.IntN(64)
			for range n {
				f.TouchAt(h, 0, now)
				tr.add(h, now)
			}
			res.ops += uint64(n - 1)
		case op < 50:
			f.CheckAndTouchAt(h, window, now)
			tr.add(h, now)
		case op < 55:
			f.TouchAndCountAt(h, window, now)
			tr.add(h, now)
		case op < 85:
			if !check("Count", h, window, now, f.CountAt(h, window, now)) {
				return res
			}
		case op < 90:
			f.ContainsAt(h, window, now)
			f.VelocityCountAt(h, window, now)
			res.ops++
		case op < 97: // a batch of 16, drawn from 4 to 16 keys
			ks := make([]uint64, 4+rng.IntN(13))
			for i := range ks {
				if rng.IntN(2) == 0 {
					ks[i] = heavy[rng.IntN(len(heavy))]
				} else {
					ks[i] = key(rng.Uint64N(pool))
				}
			}
			for i := range hs {
				hs[i] = ks[rng.IntN(len(ks))]
			}
			if rng.IntN(2) == 0 {
				f.CheckAndTouchBatch(hs, window, now, out)
			} else {
				f.TouchAndCountBatch(hs, window, now, out, cnt)
			}
			for _, x := range hs {
				tr.add(x, now)
			}
			f.VelocityCountBatch(hs, window, now, cnt)
			res.ops++
		default:
			f.decayStep(now, uint64(1+rng.IntN(16)))
		}
		if tr.newest >= trimAt && len(tr.ev) > 0 {
			tr.trim()
			trimAt = tr.newest + H
		}
	}
	return res
}
