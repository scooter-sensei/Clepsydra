package temporalfilter

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// findAny is find ignoring the displaced bit.
func (f *Filter) findAny(line, fp, tg uint64) int {
	if i := f.find(line, fp<<1, tg); i >= 0 {
		return i
	}
	return f.find(line, fp<<1|1, tg)
}

// Under concurrent churn with relocations, evictions and sweeps, every
// entry's displaced bit must say which of its two lines it is in; otherwise
// locate would miss it.
func TestDisplacedBitTracksLine(t *testing.T) {
	const capN, workers = 20_000, 8
	f := newTest(t, capN, time.Second)
	horizon := f.Ticks(time.Second)
	var clock atomic.Uint64
	clock.Store(1000)
	key := func(k, now uint64) uint64 { return HashUint64(k + now/horizon*capN) }
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300_000 {
				now := clock.Add(1) * horizon / 100_000 // ~24 horizons overall
				k := uint64(w*7919+i) % (capN * 6 / 5)  // 1.2x capacity live
				f.CheckAndTouchAt(key(k, now), horizon, now)
				if i%97 == 0 {
					f.decayStep(now, 4)
				}
			}
		}()
	}
	wg.Wait()
	t.Logf("relocations %d, evictions %d", f.Relocations(), f.Evictions())
	now := clock.Load() * horizon / 100_000
	found := 0
	for k := range uint64(capN * 6 / 5) {
		h := key(k, now)
		fp, tg := fingerprint(h), tag(h)
		p := f.primary(h)
		inP, inAlt := f.findAny(p, fp, tg), f.findAny(f.alt(p, fp), fp, tg)
		if inP >= 0 && atomic.LoadUint64(&f.words[inP])&dispBit != 0 {
			t.Fatalf("key %d in its primary line marked displaced", k)
		}
		if inP < 0 && inAlt >= 0 && atomic.LoadUint64(&f.words[inAlt])&dispBit == 0 {
			t.Fatalf("key %d in its alternate line not marked displaced", k)
		}
		if (f.locate(h, fp, tg) >= 0) != (inP >= 0 || inAlt >= 0) {
			t.Fatalf("key %d: locate disagrees with a two-line search", k)
		}
		if inP >= 0 || inAlt >= 0 {
			found++
		}
	}
	if found == 0 {
		t.Fatal("no keys survived; test exercises nothing")
	}
}
