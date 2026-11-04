package temporalfilter

import "testing"

// TestGhostMatchExact: ghMatch flags exactly the lanes whose entry is
// non-empty with fingerprint fp, as the per-entry scan did, on random
// words and on words built from fp's neighbours, all-ones and empty fields.
func TestGhostMatchExact(t *testing.T) {
	check := func(w, fp uint64) {
		m := ghMatch(w, fp*ghLanes<<(ghCntBits+ghSlBits))
		for k := range uint64(ghPerWord) {
			e := w >> (k * ghEntryBits) & ghEntryMask
			want := e != 0 && ghFP(e) == fp
			if got := m>>((k+1)*ghEntryBits)&1 == 1; got != want {
				t.Fatalf("w %#x fp %#x lane %d: match %v, want %v", w, fp, k, got, want)
			}
		}
		if m&^ghCarry != 0 {
			t.Fatalf("w %#x fp %#x: bits outside the lanes' %#x", w, fp, m)
		}
	}
	x := uint64(1)
	for range 4_000_000 {
		x = mix64(x)
		fp := x>>50 | 1 // 14 bits, never 0
		w := mix64(x) &^ (1 << 63)
		check(w, fp)
		// Lanes holding fp, its neighbours and extremes, with random counts and slices.
		var v uint64
		for k := range uint64(ghPerWord) {
			y := mix64(x + k)
			f := [...]uint64{fp, fp ^ 1, fp ^ 1<<13, fp + 1, fp - 1, 0, 1<<ghFPBits - 1, y >> 50}[y%8] & (1<<ghFPBits - 1)
			v |= (f<<(ghCntBits+ghSlBits) | y>>20&(1<<(ghCntBits+ghSlBits)-1)) << (k * ghEntryBits)
		}
		check(v, fp)
		check(v, 1<<ghFPBits-1)
		check(v, 1)
	}
}

// TestGhostScansAgree: estimateSWAR and estimate (per entry) give the
// same answer for keys in, near and out of tables filled past their
// capacity (floors raised), across windows and clocks over several slices.
func TestGhostScansAgree(t *testing.T) {
	const horizon = 60_000
	for _, shape := range [][2]int{{2, 3}, {1, 1}, {2, 1}} {
		g := newGhost(64*ghWords*8, horizon, shape[0], shape[1])
		now := uint64(100_000)
		for i := range uint64(40_000) {
			if i%1000 == 0 {
				now += 7_000
			}
			g.add(mix64(i%3000+1<<40), 1+i%5, now-i%9_000, i%97 == 0, now)
			if i%50 != 0 {
				continue
			}
			for q := range uint64(40) {
				id := mix64((i+q*131)%3500 + 1<<40)
				for _, w := range []uint64{1, horizon / 4, horizon / 2, horizon} {
					for _, at := range []uint64{now, now + 5_000, now + horizon} {
						a, aok := g.estimateSWAR(id, w, at)
						b, bok := g.estimate(id, w, at)
						if a != b || aok != bok {
							t.Fatalf("shape %v, id %#x, window %d, at %d: swar %+v %v, per entry %+v %v", shape, id, w, at, a, aok, b, bok)
						}
					}
				}
			}
		}
		if g.floored.Load() == 0 {
			t.Fatalf("shape %v: no floor raised; the test misses the floors' path", shape)
		}
	}
}
