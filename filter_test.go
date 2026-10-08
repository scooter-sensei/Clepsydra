package temporalfilter

import (
	"math/rand/v2"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func newTest(t testing.TB, capacity int, horizon time.Duration) *Filter {
	t.Helper()
	f, err := New(Config{Capacity: capacity, Horizon: horizon, Resolution: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAlignment(t *testing.T) {
	f := newTest(t, 1000, time.Minute)
	if p := uintptr(unsafe.Pointer(&f.words[0])); p%64 != 0 {
		t.Fatalf("table not cache-line aligned: %#x", p)
	}
}

func TestCheckAndTouchWindows(t *testing.T) {
	f := newTest(t, 1000, time.Minute)
	ms := func(d time.Duration) uint64 { return f.Ticks(d) }
	h := HashUint64(42)
	now := uint64(1000)

	if f.CheckAndTouchAt(h, ms(10*time.Second), now) {
		t.Fatal("first touch reported seen")
	}
	if !f.CheckAndTouchAt(h, ms(10*time.Second), now+ms(9*time.Second)) {
		t.Fatal("touch 9s later not seen within 10s")
	}
	now += ms(9 * time.Second)
	// 20s gap: outside a 10s window, inside a 30s one. The window is a query
	// parameter, not a table property.
	k2 := HashUint64(43)
	f.CheckAndTouchAt(k2, 0, now)
	if f.CheckAndTouchAt(h, ms(10*time.Second), now+ms(20*time.Second)) {
		t.Fatal("20s gap reported seen within 10s")
	}
	if !f.CheckAndTouchAt(k2, ms(30*time.Second), now+ms(20*time.Second)) {
		t.Fatal("20s gap not seen within 30s")
	}
	// Past the horizon the key is gone regardless of window.
	if f.CheckAndTouchAt(k2, ms(time.Hour), now+ms(20*time.Second)+ms(61*time.Second)) {
		t.Fatal("key seen after horizon expired")
	}
}

func TestExactBoundary(t *testing.T) {
	f := newTest(t, 1000, time.Minute)
	h := HashUint64(7)
	f.CheckAndTouchAt(h, 0, 5000)
	if !f.CheckAndTouchAt(h, 3000, 8000) {
		t.Fatal("age == window must be seen")
	}
	if f.CheckAndTouchAt(h, 2999, 11000) {
		t.Fatal("age == window+1 must not be seen")
	}
}

// Velocity estimate vs exact sliding count for Poisson and bursty traffic.
// The interpolation error is bounded by the count in the one bucket the
// window's trailing edge cuts; at age W that bucket lies within ages
// (W/2 - unit, 2W + unit].
func TestVelocityAccuracy(t *testing.T) {
	const horizon = 60 * time.Second
	f := newTest(t, 1000, horizon)
	r := rand.New(rand.NewPCG(1, 2))
	u := f.unit

	for trial := 0; trial < 200; trial++ {
		h := HashUint64(uint64(trial) + 1000)
		rate := 0.5 + r.Float64()*3 // events per second
		var events []uint64
		now := uint64(10_000_000) + uint64(trial)*1_000_000
		end := now + f.Ticks(horizon)*2
		for now < end {
			now += uint64(r.ExpFloat64() / rate * 1000)
			if r.IntN(10) == 0 { // occasional burst
				for range 5 {
					events = append(events, now)
					f.CheckAndTouchAt(h, 0, now)
				}
				continue
			}
			events = append(events, now)
			f.CheckAndTouchAt(h, 0, now)
		}
		for _, w := range []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, horizon} {
			wt := f.Ticks(w)
			exact, edge := 0, 0
			for _, e := range events {
				if e+wt > now {
					exact++
				}
				if a := now - e; a+2*u > wt/2 && a <= 2*wt+2*u {
					edge++
				}
			}
			got := f.VelocityCountAt(h, wt, now)
			if d := got - exact; d > edge+1 || -d > edge+1 {
				t.Fatalf("trial %d w=%v: est %d exact %d (edge bound %d)", trial, w, got, exact, edge)
			}
		}
	}
}

func TestVelocityBurstAndDecay(t *testing.T) {
	f := newTest(t, 1000, time.Minute)
	h := HashUint64(99)
	for i := range 10 {
		f.CheckAndTouchAt(h, 0, 100_000+uint64(i)*100) // 10 hits in 1s
	}
	now := uint64(101_000)
	if got := f.VelocityCountAt(h, f.Ticks(time.Minute), now); got != 10 {
		t.Fatalf("60s count = %d, want 10", got)
	}
	// Windows at or above one sub-window (Horizon/5 = 12s) resolve the burst.
	if got := f.VelocityCountAt(h, f.Ticks(30*time.Second), now+f.Ticks(20*time.Second)); got < 9 || got > 10 {
		t.Fatalf("30s count 20s later = %d, want ~10", got)
	}
	// Below sub-window resolution the estimate is bounded, never zero while
	// the key is fresh, and exactly zero once the window passes the last touch.
	if got := f.VelocityCountAt(h, f.Ticks(time.Second), now); got < 1 || got > 10 {
		t.Fatalf("1s count = %d, want within [1,10]", got)
	}
	if got := f.VelocityCountAt(h, f.Ticks(time.Second), now+f.Ticks(2*time.Second)); got != 0 {
		t.Fatalf("1s count 2s after last touch = %d, want 0", got)
	}
	if got := f.VelocityCountAt(h, f.Ticks(time.Minute), now+f.Ticks(61*time.Second)); got != 0 {
		t.Fatalf("count after horizon = %d, want 0", got)
	}
	// A returning key starts a fresh run.
	_, c := f.TouchAndCountAt(h, f.Ticks(time.Minute), now+f.Ticks(90*time.Second))
	if c != 1 {
		t.Fatalf("count after return = %d, want 1", c)
	}
}

func TestHeavyHitterSpill(t *testing.T) {
	f := newTest(t, 100, time.Minute)
	h := HashUint64(5)
	for range 1000 {
		f.CheckAndTouchAt(h, 0, 1000)
	}
	// A full bucket spills into older ones, so a burst keeps its count up to
	// 1273 events (all buckets full; tfbench -exact, heavy_hitter).
	if got := f.VelocityCountAt(h, f.Ticks(time.Minute), 1000); got < 950 || got > 1000 {
		t.Fatalf("1000 hits counted as %d", got)
	}
}

func TestFalsePositiveRate(t *testing.T) {
	const n = 100_000
	f := newTest(t, n, time.Minute)
	now := uint64(1000)
	for i := range n {
		f.CheckAndTouchAt(HashUint64(uint64(i)), 0, now)
	}
	fp := 0
	const probes = 1_000_000
	for i := range probes {
		if f.ContainsAt(HashUint64(uint64(n+i)), f.Ticks(time.Minute), now) {
			fp++
		}
	}
	rate := float64(fp) / probes
	t.Logf("FPR at capacity: %.2e (%d/%d), slots=%d", rate, fp, probes, f.Slots())
	if rate > 1e-5 {
		t.Fatalf("FPR %.2e too high", rate)
	}
}

func TestRetentionAtCapacity(t *testing.T) {
	const n = 100_000
	f := newTest(t, n, time.Minute)
	now := uint64(1000)
	for i := range n {
		f.CheckAndTouchAt(HashUint64(uint64(i)), 0, now)
	}
	lost := 0
	for i := range n {
		if !f.CheckAndTouchAt(HashUint64(uint64(i)), f.Ticks(time.Minute), now+1) {
			lost++
		}
	}
	t.Logf("false negatives at capacity: %d/%d, evictions=%d", lost, n, f.Evictions())
	if lost > n/1000 {
		t.Fatalf("lost %d of %d live keys", lost, n)
	}
}

func TestContinuousDecayReclaims(t *testing.T) {
	var clock uint64 = 1
	f, err := New(Config{Capacity: 10_000, Horizon: time.Second, Resolution: time.Millisecond,
		Clock: func() uint64 { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10_000 {
		f.CheckAndTouchAt(HashUint64(uint64(i)), 0, clock)
	}
	if f.Occupancy(clock) == 0 {
		t.Fatal("nothing inserted")
	}
	// Advance the clock in small steps, as a live system would; each step
	// sweeps only the lines its elapsed ticks pay for.
	maxStep := 0
	for clock < 3000 {
		clock++
		for {
			n := f.ContinuousDecay()
			maxStep = max(maxStep, n)
			if n == 0 {
				break
			}
		}
	}
	live := 0
	for i := 0; i < len(f.words); i += 2 {
		if f.words[i] != 0 {
			live++
		}
	}
	if live != 0 {
		t.Fatalf("%d expired slots not reclaimed", live)
	}
	if maxStep > 4 {
		t.Fatalf("decay step swept %d lines; must stay bounded", maxStep)
	}
}

func TestConcurrentTouchNoLostUpdates(t *testing.T) {
	f := newTest(t, 1000, time.Minute)
	h := HashUint64(1)
	f.CheckAndTouchAt(h, 0, 1000)
	const workers, per = 8, 25
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				f.CheckAndTouchAt(h, 0, 1000)
			}
		}()
	}
	wg.Wait()
	if got := f.VelocityCountAt(h, f.Ticks(time.Minute), 1000); got != 1+workers*per {
		t.Fatalf("count = %d, want %d", got, 1+workers*per)
	}
}

func TestConcurrentMixedRace(t *testing.T) {
	f := newTest(t, 1000, 100*time.Millisecond)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 9))
			for range 20_000 {
				h := HashUint64(uint64(r.IntN(3000)))
				now := f.Now()
				switch r.IntN(3) {
				case 0:
					f.CheckAndTouchAt(h, f.Ticks(50*time.Millisecond), now)
				case 1:
					f.VelocityCountAt(h, f.Ticks(50*time.Millisecond), now)
					f.CheckAndTouchBatch([]uint64{h, h ^ 1, h ^ 2}, f.Ticks(50*time.Millisecond), now, make([]bool, 3))
				default:
					f.ContinuousDecay()
				}
			}
		}()
	}
	wg.Wait()
}

func TestStringAPI(t *testing.T) {
	f, _ := New(Config{Capacity: 100, Horizon: time.Minute})
	if f.CheckAndTouch("10.0.0.1", time.Minute) {
		t.Fatal("first seen")
	}
	if !f.CheckAndTouch("10.0.0.1", time.Minute) {
		t.Fatal("second not seen")
	}
	if seen, c := f.TouchAndCount("10.0.0.1", time.Minute); !seen || c != 3 {
		t.Fatalf("TouchAndCount = %v,%d", seen, c)
	}
	if c := f.VelocityCount("10.0.0.1", time.Minute); c != 3 {
		t.Fatalf("VelocityCount = %d", c)
	}
}

// An attacker who sprays fresh keys to overflow the table must not be able to
// evict their own repeat key: eviction takes the lowest-velocity slot.
func TestSprayCannotEvictRepeatOffender(t *testing.T) {
	const n = 10_000
	f := newTest(t, n, time.Minute)
	now := uint64(1000)
	offenders := make([]uint64, 50)
	for i := range offenders {
		offenders[i] = HashUint64(uint64(1<<40 + i))
		for range 20 {
			f.CheckAndTouchAt(offenders[i], 0, now)
		}
	}
	for i := range 20 * n { // 20x capacity of one-off keys, all inside the window
		f.CheckAndTouchAt(HashUint64(uint64(i)), 0, now+uint64(i%30_000))
	}
	now += 30_000
	for i, h := range offenders {
		if c := f.VelocityCountAt(h, f.Ticks(time.Minute), now); c != 20 {
			t.Fatalf("offender %d: count %d after spray, want 20", i, c)
		}
	}
	t.Logf("evictions during spray: %d", f.Evictions())
}

func TestBatchMatchesSingle(t *testing.T) {
	a, b := newTest(t, 10_000, time.Minute), newTest(t, 10_000, time.Minute)
	hs := make([]uint64, 5000)
	for i := range hs {
		hs[i] = HashUint64(uint64(i % 3000)) // repeats inside one batch too
	}
	out := make([]bool, len(hs))
	for round := range 3 {
		now := uint64(1000 + round*2000)
		a.CheckAndTouchBatch(hs, 3000, now, out)
		for i, h := range hs {
			if want := b.CheckAndTouchAt(h, 3000, now); out[i] != want {
				t.Fatalf("round %d key %d: batch %v single %v", round, i, out[i], want)
			}
		}
	}
}

func TestCountBatchesMatchSingle(t *testing.T) {
	a, b := newTest(t, 10_000, time.Minute), newTest(t, 10_000, time.Minute)
	hs := make([]uint64, 5000)
	for i := range hs {
		hs[i] = HashUint64(uint64(i % 3000)) // repeats inside one batch too
	}
	seen, counts, vel := make([]bool, len(hs)), make([]int, len(hs)), make([]int, len(hs))
	for round := range 3 {
		now := uint64(1000 + round*2000)
		a.TouchAndCountBatch(hs, 3000, now, seen, counts)
		for i, h := range hs {
			if s, c := b.TouchAndCountAt(h, 3000, now); seen[i] != s || counts[i] != c {
				t.Fatalf("round %d key %d: batch %v/%d single %v/%d", round, i, seen[i], counts[i], s, c)
			}
		}
		a.VelocityCountBatch(hs, 5000, now+500, vel)
		for i, h := range hs {
			if want := b.VelocityCountAt(h, 5000, now+500); vel[i] != want {
				t.Fatalf("round %d key %d: velocity batch %d single %d", round, i, vel[i], want)
			}
		}
	}
	a.CheckAndTouchBatch(nil, 1, 1, nil) // empty batches are fine
	a.TouchAndCountBatch(nil, 1, 1, nil, nil)
	a.VelocityCountBatch(nil, 1, 1, nil)
}

func TestCoarseClock(t *testing.T) {
	f, err := New(Config{Capacity: 100, Horizon: time.Minute, CoarseClock: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t0 := f.Now()
	time.Sleep(20 * time.Millisecond)
	if d := f.Now() - t0; d < f.Ticks(10*time.Millisecond) || d > f.Ticks(200*time.Millisecond) {
		t.Fatalf("coarse clock advanced %d ticks over 20ms", d)
	}
	if f.CheckAndTouch("k", time.Second) || !f.CheckAndTouch("k", time.Second) {
		t.Fatal("string API with coarse clock")
	}
	f.Close()
	f.Close() // idempotent
}
