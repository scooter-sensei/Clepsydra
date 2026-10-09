//go:build !accuracyonly

package main

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	tf "github.com/scooter-sensei/clepsydra/go"
)

const benchKeys = 1 << 20

func filled() (*tf.Filter, []uint64) {
	f, _ := tf.New(tf.Config{Capacity: benchKeys, Horizon: horizon})
	hs := make([]uint64, benchKeys)
	for i := range hs {
		hs[i] = mix64(uint64(i))
		f.CheckAndTouchAt(hs[i], 0, 1000)
	}
	return f, hs
}

func nsop(fn func(b *testing.B)) float64 {
	best := 0.0
	for range 3 { // best of 3: least disturbed by noisy neighbours
		r := testing.Benchmark(fn)
		v := float64(r.T.Nanoseconds()) / float64(r.N)
		if best == 0 || v < best {
			best = v
		}
	}
	return best
}

func speed() speeds {
	f, hs := filled()
	w := f.Ticks(30 * time.Second)
	var s speeds
	s.hitCold = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f.CheckAndTouchAt(hs[i&(benchKeys-1)], w, 2000+uint64(i>>10))
		}
	})
	out := make([]bool, 64)
	s.batch = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i += 64 {
			lo := i & (benchKeys - 1)
			f.CheckAndTouchBatch(hs[lo:lo+64], w, 2000+uint64(i>>10), out)
		}
	})
	s.velocity = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f.VelocityCountAt(hs[i&(benchKeys-1)], w, 2000)
		}
	})
	s.parallel = nsop(func(b *testing.B) {
		var seed atomic.Uint64
		b.RunParallel(func(pb *testing.PB) {
			i := seed.Add(1) * 7919
			for pb.Next() {
				i++
				f.CheckAndTouchAt(hs[i&(benchKeys-1)], w, 2000+i>>10)
			}
		})
	})
	// Steady-state insert: time advances so keys expire as fast as they
	// arrive, holding ~1.0x capacity live.
	var ctr uint64
	perKey := f.Ticks(horizon) / benchKeys
	s.insert = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ctr++
			f.CheckAndTouchAt(mix64(benchKeys+ctr), w, 1000+ctr*perKey)
		}
	})
	// Saturated insert: worst case, every insert lands in full lines.
	sat, _ := tf.New(tf.Config{Capacity: benchKeys / 4, Horizon: horizon}) // 512K slots
	for i := range benchKeys {                                             // 2x overfilled
		sat.CheckAndTouchAt(mix64(uint64(i)), 0, 1000)
	}
	s.insertSat = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ctr++
			sat.CheckAndTouchAt(mix64(1<<40+ctr), w, 1000)
		}
	})
	hot, _ := tf.New(tf.Config{Capacity: 4096, Horizon: horizon})
	for i := range 4096 {
		hot.CheckAndTouchAt(hs[i], 0, 1000)
	}
	s.hitHot = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			hot.CheckAndTouchAt(hs[i&4095], w, 2000+uint64(i>>10))
		}
	})
	// String keys on fresh 1M-key tables (the tables above have been
	// written far into the future by the insert benchmarks).
	keys := make([]string, benchKeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("10.%d.%d.%d", i>>16, i>>8&255, i&255)
	}
	strBench := func(cfg tf.Config) float64 {
		sf, _ := tf.New(cfg)
		defer sf.Close()
		for _, k := range keys {
			sf.CheckAndTouch(k, time.Minute)
		}
		return nsop(func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				sf.CheckAndTouch(keys[i&(benchKeys-1)], 30*time.Second)
			}
		})
	}
	s.str = strBench(tf.Config{Capacity: benchKeys, Horizon: horizon})
	s.strCoarse = strBench(tf.Config{Capacity: benchKeys, Horizon: horizon, CoarseClock: 100 * time.Microsecond})
	// The 1M-key table (20 MB) fits this host's 260 MB L3. A 32M-key table
	// (~600 MB) makes every lookup a DRAM miss, as in large deployments.
	const hugeKeys = 1 << 25
	big, _ := tf.New(tf.Config{Capacity: hugeKeys, Horizon: horizon})
	for i := range uint64(hugeKeys) {
		big.CheckAndTouchAt(mix64(i), 0, 1000)
	}
	s.hitDRAM = nsop(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			big.CheckAndTouchAt(mix64(uint64(i*7919)%hugeKeys), w, 2000)
		}
	})
	return s
}
