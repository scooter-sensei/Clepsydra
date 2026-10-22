package temporalfilter

import (
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"
)

// At 90% load, where a quarter of the keys live in their alternate line,
// batches of repeated, present and new keys answer as the same calls made
// one at a time, in order.
func TestBatchAtHighLoadMatchesSingle(t *testing.T) {
	a, b := newTest(t, 1<<14, time.Minute), newTest(t, 1<<14, time.Minute)
	pool := make([]uint64, a.Slots()*90/100)
	for i := range pool {
		pool[i] = mix64(uint64(i) + 1<<44)
		a.TouchAt(pool[i], 0, 1000)
		b.TouchAt(pool[i], 0, 1000)
	}
	rng := rand.New(rand.NewPCG(3, 4))
	hs := make([]uint64, 4096)
	seen, counts := make([]bool, len(hs)), make([]int, len(hs))
	missed, fresh := 0, uint64(1<<50)
	for round := range 8 {
		now := uint64(2000 + round*500)
		for c := 0; c < len(hs); c += 64 { // 16 keys, 4 times each, a quarter of them new
			for i := range 16 {
				k := pool[rng.IntN(len(pool))]
				if rng.IntN(4) == 0 {
					k, fresh = mix64(fresh), fresh+1
				}
				for j := range 4 {
					hs[c+i*4+j] = k
				}
			}
			rng.Shuffle(64, func(i, j int) { hs[c+i], hs[c+j] = hs[c+j], hs[c+i] })
		}
		for _, h := range hs {
			if a.find(a.primary(h), fingerprint(h)<<1, tag(h)) < 0 {
				missed++
			}
		}
		if round%2 == 0 {
			a.TouchAndCountBatch(hs, 3000, now, seen, counts)
		} else {
			a.CheckAndTouchBatch(hs, 3000, now, seen)
		}
		for i, h := range hs {
			s, c := b.TouchAndCountAt(h, 3000, now)
			if seen[i] != s || round%2 == 0 && counts[i] != c {
				t.Fatalf("round %d key %d: batch %v/%d single %v/%d", round, i, seen[i], counts[i], s, c)
			}
		}
	}
	t.Logf("%.1f%% of keys not in their primary line before their batch; evictions %d, relocations %d",
		100*float64(missed)/float64(8*len(hs)), a.Evictions(), a.Relocations())
}

// warm hands each touch the slot where it found the key, and the chunk's
// earlier touches can move or replace the entry meanwhile, so a touch first
// checks that the slot still holds its key. Handed key b's slot (b shares
// a's fingerprint and lines, not its tag) or an empty one, a touch of a
// counts in a's own slot and leaves b's alone.
func TestTouchStaleSlot(t *testing.T) {
	f := newTest(t, 1000, time.Minute)
	a := mix64(7)
	b := a ^ 1<<36 // another tag (hash bits 36..47); the same fingerprint and lines
	if fingerprint(a) != fingerprint(b) || tag(a) == tag(b) || f.primary(a) != f.primary(b) {
		t.Fatal("b must share a's fingerprint and lines and differ in tag")
	}
	f.TouchAt(a, 0, 1000)
	f.TouchAt(b, 0, 1000)
	ib := f.locate(b, fingerprint(b), tag(b))
	empty := -1
	for i := 0; i < len(f.words) && empty < 0; i += 2 {
		if atomic.LoadUint64(&f.words[i]) == 0 {
			empty = i
		}
	}
	w0, w1 := atomic.LoadUint64(&f.words[ib]), atomic.LoadUint64(&f.words[ib+1])
	for n, i0 := range []int{ib, empty} {
		if r := f.touch(a, 60_000, 2000, true, i0); r.Count != 2+n || !r.Seen {
			t.Fatalf("a handed slot %d: %+v, want count %d", i0, r, 2+n)
		}
	}
	if atomic.LoadUint64(&f.words[ib]) != w0 || atomic.LoadUint64(&f.words[ib+1]) != w1 {
		t.Fatal("a's touch changed b's slot")
	}
	if r := f.CountAt(b, 60_000, 2000); r.Count != 1 {
		t.Fatalf("b: %+v, want count 1", r)
	}
}
