package temporalfilter

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// TestInsertTail (TF_EXP=1) times first touches of unseen keys, each alone,
// in a 2M-key table larger than the last-level cache filled to a given slot
// load, and splits them by what the insert did: took a free slot, moved
// entries along a cuckoo path (by how many entries moved), or evicted. It
// shows which path makes the unseen-key tail (docs/paper/06-security.md, Timing).
// Each load point pools TF_TABLES (default 1) fresh tables of 40,000 probes,
// and "noise" times an empty call in the same loop: the host's own tail.
func TestInsertTail(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	const capacity, probes = 1 << 21, 40_000
	tables := 1
	fmt.Sscan(os.Getenv("TF_TABLES"), &tables)
	names := []string{"free slot", "moved 1", "moved 2", "moved 3", "moved 4+", "evicted", "noise", "waited", "spin 0.5us"}
	for _, load := range []float64{0.85, 0.90, 0.95} {
		ts := make([][]float64, len(names))
		var all []float64
		for tb := range tables {
			now := uint64(1)
			f, err := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond,
				Clock: func() uint64 { return now }})
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(5+uint64(tb), 6))
			for range int(float64(f.Slots()) * load) {
				f.TouchAt(f.HashUint64(rng.Uint64()), 0, now)
			}
			now += 1000
			hs := make([]uint64, probes)
			for i := range hs {
				hs[i] = f.HashUint64(rng.Uint64())
			}
			for _, h := range hs {
				r0, e0, w0 := f.Relocations(), f.Evictions(), f.Waited()
				t0 := time.Now()
				f.TouchAt(h, 60_000, now)
				d := float64(time.Since(t0).Nanoseconds())
				k := 0
				switch moved := f.Relocations() - r0; {
				case f.Waited() != w0:
					k = 7
				case f.Evictions() != e0:
					k = 5
				case moved > 0:
					k = int(min(moved, 4))
				}
				ts[k] = append(ts[k], d)
				all = append(all, d)
				t0 = time.Now()
				noop()
				ts[6] = append(ts[6], float64(time.Since(t0).Nanoseconds()))
				t0 = time.Now()
				for time.Since(t0) < 500*time.Nanosecond {
				}
				ts[8] = append(ts[8], float64(time.Since(t0).Nanoseconds()))
			}
		}
		for k := range ts {
			slices.Sort(ts[k])
		}
		slices.Sort(all)
		q := func(x []float64, p float64) float64 {
			if len(x) == 0 {
				return 0
			}
			return x[int(p*float64(len(x)-1))]
		}
		over := func(x []float64) float64 { // share over 2 µs, %
			i, _ := slices.BinarySearch(x, 2000)
			return 100 * float64(len(x)-i) / float64(max(1, len(x)))
		}
		t.Logf("load %.2f, %d tables, all: p50/p99/p99.9/max %5.0f/%6.0f/%6.0f/%7.0f ns, over 2 us %.3f%% (spin 0.5us: %.3f%%)", load, tables, q(all, .5), q(all, .99), q(all, .999), q(all, 1), over(all), over(ts[8]))
		for k, name := range names {
			share := 100 * float64(len(ts[k])) / float64(len(all))
			if k == 6 || k == 8 {
				share = 100
			}
			t.Logf("  %-10s %6.2f%%: p50/p99/p99.9/max %5.0f/%6.0f/%6.0f/%7.0f ns, over 2 us %.3f%%", name, share,
				q(ts[k], .5), q(ts[k], .99), q(ts[k], .999), q(ts[k], 1), over(ts[k]))
		}
	}
}

//go:noinline
func noop() {}

// TestPlacerInterference (TF_EXP=1) asks how much of Background's unseen-key
// tail is the placer goroutine's, by moving the same placements off the
// caller's timing without moving them onto another core. Unseen-key inserts
// at 85% load, each timed alone as TestInsertTail times them, in three arms
// interleaved by table (TF_ROUNDS rounds, default 3, each a fresh 2M-key
// table per arm):
//
//   - conc: Background as committed, the placer goroutine placing waiting
//     keys while the caller inserts;
//   - sync: the placer run by hand on the caller's goroutine after each
//     timed insert, outside its timing (bgManual): the same placements,
//     nothing running beside the caller;
//   - sync+dram: sync, with another goroutine loading random lines of the
//     same table in rounds of 8 independent loads, as the placer's search
//     does: the placer's memory traffic without the placer.
//
// "spin 0.5us" is a 0.5 µs busy loop timed in the same loop: the host's own
// share (claims S61).
func TestPlacerInterference(t *testing.T) {
	if os.Getenv("TF_EXP") == "" || forceCT {
		t.Skip("set TF_EXP=1 (and not TF_CT)")
	}
	const capacity, probes = 1 << 21, 40_000
	rounds := 3
	fmt.Sscan(os.Getenv("TF_ROUNDS"), &rounds)
	arms := []string{"conc", "sync", "sync+dram"}
	type times struct{ all, waited, other, spin []float64 }
	res := make([]times, len(arms))
	for rd := range rounds {
		for k := range arms {
			a := (k + rd) % len(arms)
			byHand := arms[a] != "conc"
			now := uint64(1)
			f, err := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond,
				Background: true, Clock: func() uint64 { return now }})
			if err != nil {
				t.Fatal(err)
			}
			f.bgManual = byHand
			drain := func() {
				for f.room.Load() != nil && f.placeNext(f.room.Load(), 0) {
				}
			}
			rng := rand.New(rand.NewPCG(5+uint64(rd), 6))
			for range int(float64(f.Slots()) * 0.85) {
				f.TouchAt(f.HashUint64(rng.Uint64()), 0, now)
				if byHand {
					drain()
				}
			}
			for f.Waiting() > 0 {
				time.Sleep(time.Millisecond)
			}
			now += 1000
			hs := make([]uint64, probes)
			for i := range hs {
				hs[i] = f.HashUint64(rng.Uint64())
			}
			stop, done := make(chan struct{}), make(chan struct{})
			if arms[a] == "sync+dram" {
				go func() {
					defer close(done)
					ws, x, sink := f.words, uint64(7), uint64(0)
					for {
						for range 64 {
							for j := range uint64(8) {
								x = x*0x9E3779B97F4A7C15 + j
								sink += atomic.LoadUint64(&ws[(x>>20)%uint64(len(ws))&^7])
							}
							x += sink & 1
						}
						select {
						case <-stop:
							return
						default:
						}
					}
				}()
			} else {
				close(done)
			}
			r := &res[a]
			for _, h := range hs {
				w0 := f.Waited()
				t0 := time.Now()
				f.TouchAt(h, 60_000, now)
				d := float64(time.Since(t0).Nanoseconds())
				r.all = append(r.all, d)
				if f.Waited() != w0 {
					r.waited = append(r.waited, d)
				} else {
					r.other = append(r.other, d)
				}
				if byHand {
					drain()
				}
				t0 = time.Now()
				for time.Since(t0) < 500*time.Nanosecond {
				}
				r.spin = append(r.spin, float64(time.Since(t0).Nanoseconds()))
			}
			close(stop)
			<-done
			for f.placing.Load() {
				time.Sleep(time.Millisecond)
			}
		}
	}
	q := func(x []float64, p float64) float64 {
		if len(x) == 0 {
			return 0
		}
		slices.Sort(x)
		return x[int(p*float64(len(x)-1))]
	}
	over := func(x []float64) float64 { // share over 2 µs, %
		slices.Sort(x)
		i, _ := slices.BinarySearch(x, 2000)
		return 100 * float64(len(x)-i) / float64(max(1, len(x)))
	}
	for a, name := range arms {
		r := res[a]
		t.Logf("%-9s %d inserts: p50/p99/p99.9 %4.0f/%5.0f/%6.0f ns, over 2 us %.3f%% | waited p50 %4.0f, over 2 us %.3f%% | others p50 %4.0f, over 2 us %.3f%% | spin 0.5us over 2 us %.3f%%",
			name, len(r.all), q(r.all, .5), q(r.all, .99), q(r.all, .999), over(r.all), q(r.waited, .5), over(r.waited), q(r.other, .5), over(r.other), over(r.spin))
	}
}
