//go:build tfdrip

package temporalfilter

// lab/sw-2026-11, W4d: a probe that reads the drip log itself.

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dripAudit checks key h's drip log against its register, read together
// (table or stash): no bucket may hold more logged events than the register
// counts there, the event at the last touch counted in bucket 0. A log that
// says nothing passes.
func (f *Filter) dripAudit(h, now uint64) error {
	w0, w1, src := f.read(h, now, -1)
	if src == 0 || w0 == 0 {
		return nil
	}
	d := dripOf(w0)
	if d&dripV == 0 {
		return nil
	}
	last := w0 & tsMask
	v, ok := f.viewOf(f.stashID(h, fingerprint(h), tag(h)), w1, last)
	if !ok || v.e != f.unitOf(last) {
		return nil
	}
	field := d & dripMask
	q := f.quantum(last)
	var logged [nBuckets]uint64
	logged[0] = 1 // the last touch
	bucketOf := func(u int64) int {
		top := v.e + 1
		for j := range nBuckets {
			start := bucketStart(v.e, j)
			if u >= start && u < top {
				return j
			}
			top = start
		}
		return -1
	}
	for avail := dripBits; avail > 0; {
		ones := 0
		for ones < avail && field>>uint(ones)&1 == 1 {
			ones++
		}
		if ones == avail {
			break
		}
		q -= int64(ones)
		if j := bucketOf(q >> 4); j >= 0 {
			logged[j]++
		}
		field >>= uint(ones + 1)
		avail -= ones + 1
	}
	for j := range nBuckets {
		if logged[j] > v.c[j] {
			return fmt.Errorf("bucket %d: %d logged, register %d (log %#x, register %v, last %d)", j, logged[j], v.c[j], d, v.c, last)
		}
	}
	return nil
}

// TestDripPushOnce: TestBackgroundConcurrentCounts's scenario (8 workers,
// 32 keys touched 20 times each in ~76 slots under a flood, Background on:
// waits, evictions, restores and the placer's moves, all concurrent with
// the keys' own touches), with every key's log audited against its
// register after each round. A touch that pushes its event twice leaves a
// bucket with more logged events than counted. TF_DRIP_ROUNDS sets the
// rounds (default 200).
func TestDripPushOnce(t *testing.T) {
	const workers, hot, perKey = 8, 32, 20
	rounds := 200
	if n, err := strconv.Atoi(os.Getenv("TF_DRIP_ROUNDS")); err == nil {
		rounds = n
	}
	bad, checked := 0, 0
	var first error
	for round := range rounds {
		var clock atomic.Uint64
		clock.Store(1)
		f, err := New(Config{Capacity: 64, Horizon: time.Minute, Resolution: time.Millisecond,
			Clock: clock.Load, Stash: 1 << 12, SpillMin: 1, Background: true})
		if err != nil {
			t.Fatal(err)
		}
		keys := make([]uint64, hot)
		for i := range keys {
			keys[i] = mix64(uint64(round)<<32 | uint64(i))
		}
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(uint64(round), uint64(w)))
				for i := range hot * perKey / workers {
					now := clock.Add(25)
					f.TouchAt(keys[(i*workers+w)%hot], 0, now)
					if i%2 == 0 {
						h := rng.Uint64()
						for range 25 {
							f.TouchAt(h, 0, now)
						}
					}
				}
			}()
		}
		wg.Wait()
		now := clock.Load()
		for _, h := range keys {
			checked++
			if err := f.dripAudit(h, now); err != nil {
				bad++
				if first == nil {
					first = fmt.Errorf("round %d: %v", round, err)
				}
			}
		}
		f.Close()
	}
	t.Logf("%d of %d keys' logs hold more events than their registers (rounds %d)", bad, checked, rounds)
	if bad > 0 {
		t.Errorf("first: %v", first)
	}
}
