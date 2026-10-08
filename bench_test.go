package temporalfilter

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const benchKeys = 1 << 20 // 1M live keys: table is 32 MiB, far beyond L2/L3

func benchFilter(b *testing.B) (*Filter, []uint64) {
	f, err := New(Config{Capacity: benchKeys, Horizon: time.Minute})
	if err != nil {
		b.Fatal(err)
	}
	hs := make([]uint64, benchKeys)
	for i := range hs {
		hs[i] = HashUint64(uint64(i))
		f.CheckAndTouchAt(hs[i], 0, 1000)
	}
	return f, hs
}

// Hit path on a cold table: one random cache-line miss per op.
func BenchmarkCheckAndTouchHit(b *testing.B) {
	f, hs := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.CheckAndTouchAt(hs[i&(benchKeys-1)], w, 2000+uint64(i>>10))
	}
}

func BenchmarkCheckAndTouchMissInsert(b *testing.B) {
	f, _ := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.CheckAndTouchAt(HashUint64(uint64(benchKeys+i)), w, 2000+uint64(i>>10))
	}
}

func BenchmarkTouchAndCount(b *testing.B) {
	f, hs := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.TouchAndCountAt(hs[i&(benchKeys-1)], w, 2000+uint64(i>>10))
	}
}

func BenchmarkVelocityCount(b *testing.B) {
	f, hs := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.VelocityCountAt(hs[i&(benchKeys-1)], w, 2000)
	}
}

func BenchmarkCheckAndTouchParallel(b *testing.B) {
	f, hs := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	var seed atomic.Uint64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := seed.Add(1) * 7919
		for pb.Next() {
			i++
			f.CheckAndTouchAt(hs[i&(benchKeys-1)], w, 2000+i>>10)
		}
	})
}

// End-to-end: string hashing plus the built-in monotonic clock.
func BenchmarkCheckAndTouchString(b *testing.B) {
	f, _ := New(Config{Capacity: benchKeys, Horizon: time.Minute})
	keys := make([]string, benchKeys)
	for i := range keys {
		keys[i] = "10." + strconv.Itoa(i>>16) + "." + strconv.Itoa(i>>8&255) + "." + strconv.Itoa(i&255)
		f.CheckAndTouch(keys[i], time.Minute)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.CheckAndTouch(keys[i&(benchKeys-1)], 30*time.Second)
	}
}

// Baseline: exact map of last-seen timestamps behind a mutex, the usual
// in-process alternative. No velocity, no eviction, unbounded memory.
func BenchmarkBaselineMutexMap(b *testing.B) {
	var mu sync.Mutex
	m := make(map[uint64]uint64, benchKeys)
	hs := make([]uint64, benchKeys)
	for i := range hs {
		hs[i] = HashUint64(uint64(i))
		m[hs[i]] = 1000
	}
	w := uint64(30_000_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now := 2000 + uint64(i>>10)
		mu.Lock()
		last, ok := m[hs[i&(benchKeys-1)]]
		_ = ok && now-last <= w
		m[hs[i&(benchKeys-1)]] = now
		mu.Unlock()
	}
}

func BenchmarkBaselineMutexMapParallel(b *testing.B) {
	var mu sync.Mutex
	m := make(map[uint64]uint64, benchKeys)
	hs := make([]uint64, benchKeys)
	for i := range hs {
		hs[i] = HashUint64(uint64(i))
		m[hs[i]] = 1000
	}
	var seed atomic.Uint64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := seed.Add(1) * 7919
		for pb.Next() {
			i++
			mu.Lock()
			m[hs[i&(benchKeys-1)]] = 2000 + i>>10
			mu.Unlock()
		}
	})
}

// Hit path with a cache-resident working set (4K keys, 128 KiB table):
// isolates the algorithm's CPU cost from DRAM latency.
func BenchmarkCheckAndTouchHitHot(b *testing.B) {
	const n = 1 << 12
	f, _ := New(Config{Capacity: n, Horizon: time.Minute})
	hs := make([]uint64, n)
	for i := range hs {
		hs[i] = HashUint64(uint64(i))
		f.CheckAndTouchAt(hs[i], 0, 1000)
	}
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.CheckAndTouchAt(hs[i&(n-1)], w, 2000+uint64(i>>10))
	}
}

// Worst case: every insert lands in two full lines (no expiry).
func BenchmarkInsertSaturated(b *testing.B) {
	f, _ := New(Config{Capacity: benchKeys / 4, Horizon: time.Minute}) // 512K slots
	for i := range benchKeys {
		f.CheckAndTouchAt(HashUint64(uint64(i)), 0, 1000)
	}
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.CheckAndTouchAt(HashUint64(uint64(1<<40+i)), w, 1000)
	}
}

func BenchmarkCheckAndTouchBatch(b *testing.B) {
	f, hs := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	out := make([]bool, 64)
	b.ResetTimer()
	for i := 0; i < b.N; i += 64 {
		lo := i & (benchKeys - 1)
		f.CheckAndTouchBatch(hs[lo:lo+64], w, 2000+uint64(i>>10), out)
	}
}
