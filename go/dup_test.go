package temporalfilter

import (
	"sync"
	"testing"
	"time"
)

// Concurrent first touches of the same keys must not leave duplicate slots
// (which would split a key's count).
func TestConcurrentFirstInsertNoDuplicates(t *testing.T) {
	const keys, workers = 20_000, 8
	f := newTest(t, keys, time.Minute)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for k := range keys {
				f.CheckAndTouchAt(mix64(uint64(k)), 0, 1000)
			}
		}()
	}
	close(start)
	wg.Wait()
	occ := f.Occupancy(1000)
	if w, ok := any(f).(interface{ Waiting() int }); ok { // verify/replay.sh runs this file on commits without it
		occ += w.Waiting() // ConstantTime: keys waiting in the stash for a slot
	}
	total := 0
	for k := range keys {
		total += f.VelocityCountAt(mix64(uint64(k)), f.Ticks(time.Minute), 1000)
	}
	t.Logf("slots used %d for %d keys; counted %d of %d touches", occ, keys, total, keys*workers)
	if occ != keys {
		t.Fatalf("%d duplicate slots", occ-keys)
	}
	if total != keys*workers {
		t.Fatalf("counts split or lost: %d of %d", total, keys*workers)
	}
}
