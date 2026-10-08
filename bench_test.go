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

func BenchmarkTouchAndCountBatch(b *testing.B) {
	f, hs := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	seen, counts := make([]bool, 64), make([]int, 64)
	b.ResetTimer()
	for i := 0; i < b.N; i += 64 {
		lo := i & (benchKeys - 1)
		f.TouchAndCountBatch(hs[lo:lo+64], w, 2000+uint64(i>>10), seen, counts)
	}
}

func BenchmarkVelocityCountBatch(b *testing.B) {
	f, hs := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	counts := make([]int, 64)
	b.ResetTimer()
	for i := 0; i < b.N; i += 64 {
		lo := i & (benchKeys - 1)
		f.VelocityCountBatch(hs[lo:lo+64], w, 2000, counts)
	}
}

// Steady state near the limit: ~98% slot load, keys expiring as fast as they
// arrive, so most inserts need a cuckoo path.
func BenchmarkInsertNearFull(b *testing.B) {
	const n = 1 << 18
	f, _ := New(Config{Capacity: n, Horizon: time.Minute})
	live := uint64(n * 115 / 100) // ~98% of slots
	per := f.Ticks(time.Minute) / live
	var ctr uint64
	for ; ctr < live; ctr++ {
		f.CheckAndTouchAt(HashUint64(ctr), 0, 1+ctr*per)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctr++
		f.CheckAndTouchAt(HashUint64(ctr), 0, 1+ctr*per)
	}
	b.ReportMetric(float64(f.Evictions())/float64(b.N), "evict/op")
}

// VelocityCount with the table cache-resident: the estimator's CPU cost.
func BenchmarkVelocityCountHot(b *testing.B) {
	const n = 1 << 12
	f, _ := New(Config{Capacity: n, Horizon: time.Minute})
	hs := make([]uint64, n)
	for i := range hs {
		hs[i] = HashUint64(uint64(i))
		for k := range 5 {
			f.CheckAndTouchAt(hs[i], 0, 1000+uint64(k)*3_000_000)
		}
	}
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.VelocityCountAt(hs[i&(n-1)], w, 20_000_000)
	}
}

// Lookup of keys never inserted, on a cold table at 85% slot load.
func BenchmarkContainsAbsent(b *testing.B) {
	f, _ := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.ContainsAt(HashUint64(uint64(1<<40+i)), w, 2000)
	}
}

// Steady-state insert: time advances so keys expire as fast as they arrive,
// holding ~1.0x capacity live (the scorecard's insert column).
func BenchmarkInsertSteady(b *testing.B) {
	f, _ := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	perKey := f.Ticks(time.Minute) / benchKeys
	ctr := uint64(0)
	for range 2 * benchKeys { // until the initial keys have expired
		ctr++
		f.CheckAndTouchAt(HashUint64(benchKeys+ctr), w, 1000+ctr*perKey)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctr++
		f.CheckAndTouchAt(HashUint64(benchKeys+ctr), w, 1000+ctr*perKey)
	}
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// Same, but each probe's key depends on the previous answer, so lookups
// can't overlap: per-request latency rather than throughput.
func BenchmarkContainsAbsentLatency(b *testing.B) {
	f, _ := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	dep := uint64(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dep += b2u(f.ContainsAt(HashUint64(uint64(1<<40+i)+dep), w, 2000))
	}
}

func BenchmarkHitLatency(b *testing.B) {
	f, hs := benchFilter(b)
	w := f.Ticks(30 * time.Second)
	dep := uint64(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dep += b2u(f.CheckAndTouchAt(hs[(uint64(i)+dep)&(benchKeys-1)], w, 2000+uint64(i>>10)))
	}
}

// Hits on a table shaped by churn (keys expiring as fast as they arrive),
// where more keys sit in their alternate line than after a plain fill.
func BenchmarkHitChurned(b *testing.B) {
	f, _ := New(Config{Capacity: benchKeys, Horizon: time.Minute})
	perKey := f.Ticks(time.Minute) / benchKeys
	ctr := uint64(0)
	for range 2 * benchKeys {
		ctr++
		f.CheckAndTouchAt(HashUint64(ctr), 0, 1000+ctr*perKey)
	}
	now := 1000 + ctr*perKey
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.CheckAndTouchAt(HashUint64(ctr-uint64(i*7919)%(benchKeys*3/4)), w, now)
	}
}

// A table far larger than any cache (32M keys, ~600 MB; this host's L3 is
// 260 MB), so every lookup is a DRAM miss. Built once and shared.
const hugeKeys = 1 << 25

var huge struct {
	once sync.Once
	f    *Filter
}

func hugeFilter(b *testing.B) *Filter {
	if testing.Short() {
		b.Skip("builds a 600 MB table")
	}
	huge.once.Do(func() {
		huge.f, _ = New(Config{Capacity: hugeKeys, Horizon: time.Minute})
		for i := range uint64(hugeKeys) {
			huge.f.CheckAndTouchAt(HashUint64(i), 0, 1000)
		}
	})
	return huge.f
}

func BenchmarkHugeHit(b *testing.B) {
	f := hugeFilter(b)
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.CheckAndTouchAt(HashUint64(uint64(i*7919)%hugeKeys), w, 2000)
	}
}

func BenchmarkHugeBatch(b *testing.B) {
	f := hugeFilter(b)
	w := f.Ticks(30 * time.Second)
	hs, out := make([]uint64, 64), make([]bool, 64)
	b.ResetTimer()
	for i := 0; i < b.N; i += 64 {
		for j := range hs {
			hs[j] = HashUint64(uint64((i+j)*7919) % hugeKeys)
		}
		f.CheckAndTouchBatch(hs, w, 2000, out)
	}
}

func BenchmarkHugeAbsent(b *testing.B) {
	f := hugeFilter(b)
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.ContainsAt(HashUint64(uint64(1<<40+i)), w, 2000)
	}
}

func BenchmarkHugeVelocity(b *testing.B) {
	f := hugeFilter(b)
	w := f.Ticks(30 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.VelocityCountAt(HashUint64(uint64(i*7919)%hugeKeys), w, 2000)
	}
}
