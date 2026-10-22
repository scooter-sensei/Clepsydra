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
		// Keys waiting for a slot: ConstantTime's stay in the stash, and
		// Background's placer goroutine drains its wait room (moving entries
		// as it goes), so first wait until the count is 0 or has held still
		// for 50 ms, then count slots between two equal reads of it.
		for n, still := w.Waiting(), 0; n > 0 && still < 50; still++ {
			time.Sleep(time.Millisecond)
			if m := w.Waiting(); m != n {
				n, still = m, 0
			}
		}
		for {
			n := w.Waiting()
			occ = f.Occupancy(1000)
			if w.Waiting() == n {
				occ += n
				break
			}
		}
	}
	if s, ok := any(f).(interface{ Restored() uint64 }); ok {
		// Keys in the stash: Background's placer races the workers' touches
		// for its moves and, under the race detector, now and then runs out
		// of search and evicts one; its history waits in the stash.
		occ += int(f.Evictions() - s.Restored())
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
