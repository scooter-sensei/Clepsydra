package temporalfilter

import (
	"math/rand/v2"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestThroughput (TF_EXP=1): tables of 2M keys filled to 90% of their
// slots, a fresh one for every measurement.
//
//   - batched and single: one goroutine, CheckAndTouchBatch (64 keys at a
//     time) or CheckAndTouchAt over present keys drawn uniformly.
//   - scaling: 1 to GOMAXPROCS goroutines, each making 1M CheckAndTouchAt
//     calls on keys drawn from a Zipf distribution (s 1.1, so the heaviest
//     key takes ~9% of all calls and is promoted to a hot entry) or
//     uniformly, with 1% of calls on keys never seen (inserts: at most 5%
//     of the slots across a run).
//
// Key streams are drawn before the clock starts. Throughput is reported,
// not checked: it depends on the host.
func TestThroughput(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	const capacity = 1 << 21
	fill := func() (*Filter, []uint64) {
		f, err := New(Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond, Clock: func() uint64 { return 1000 }})
		if err != nil {
			t.Fatal(err)
		}
		keys := make([]uint64, f.Slots()*90/100)
		for i := range keys {
			keys[i] = mix64(uint64(i) + 1<<44)
			f.TouchAt(keys[i], 0, 1000)
		}
		return f, keys
	}
	f, keys := fill()
	t.Logf("table: %d slots, %d keys (90%% load), %d MB", f.Slots(), len(keys), f.Bytes()>>20)
	stream := make([]uint64, 1<<22)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range stream {
		stream[i] = keys[rng.IntN(len(keys))]
	}
	out := make([]bool, 64)
	start := time.Now()
	for i := 0; i+64 <= len(stream); i += 64 {
		f.CheckAndTouchBatch(stream[i:i+64], 60_000, 1000, out)
	}
	t.Logf("batched (64, uniform, present), one goroutine: %.2f M keys/s", float64(len(stream))/time.Since(start).Seconds()/1e6)
	start = time.Now()
	for _, h := range stream {
		f.CheckAndTouchAt(h, 60_000, 1000)
	}
	t.Logf("single (uniform, present), one goroutine: %.2f M ops/s", float64(len(stream))/time.Since(start).Seconds()/1e6)

	const per, unseen = 1 << 20, 10 // calls per goroutine; unseen per 1000
	for _, zipf := range []bool{true, false} {
		var base float64
		for _, workers := range []int{1, 2, 3, 4, 6, 8, 12} {
			if workers > runtime.GOMAXPROCS(0) {
				break
			}
			f, keys := fill()
			streams := make([][]uint64, workers)
			for w := range streams {
				rng := rand.New(rand.NewPCG(uint64(w), 7))
				z := rand.NewZipf(rng, 1.1, 1, uint64(len(keys)-1))
				streams[w] = make([]uint64, per)
				for i := range streams[w] {
					if zipf {
						streams[w][i] = keys[z.Uint64()]
					} else {
						streams[w][i] = keys[rng.IntN(len(keys))]
					}
					if rng.IntN(1000) < unseen {
						streams[w][i] = rng.Uint64()
					}
				}
			}
			var wg sync.WaitGroup
			start := time.Now()
			for w := range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for _, h := range streams[w] {
						f.CheckAndTouchAt(h, 60_000, 1000)
					}
				}()
			}
			wg.Wait()
			mops := float64(workers*per) / time.Since(start).Seconds() / 1e6
			if workers == 1 {
				base = mops
			}
			name := "uniform"
			if zipf {
				name = "zipf s=1.1"
			}
			t.Logf("%-10s 1%% unseen, %2d goroutines: %6.2f M ops/s (%.2fx one goroutine); promoted %d, evictions %d",
				name, workers, mops, mops/base, f.Promoted(), f.Evictions())
		}
	}
	t.Logf("host: %d logical CPUs", runtime.NumCPU())
}
