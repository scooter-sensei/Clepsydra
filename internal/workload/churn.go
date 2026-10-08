// Package workload generates the scorecard's churn stream. cmd/tfbench and
// the placement-limit test both replay it, so they see identical operations.
package workload

import (
	"math/rand/v2"
	"sort"
)

// Windows are the query windows (ticks) the churn stream draws from.
var Windows = []uint64{1_000, 10_000, 60_000}

// Churn calls op for each operation of a stream mixing new keys (60%) and
// repeats of keys born within the last 1.5 horizons, over 4 horizons of
// hTicks each, with one window drawn from Windows per operation. load sets
// the arrival rate so that distinct keys live per horizon ≈ load×capacity.
// hash maps a key number to its hash. The stream is a pure function of its
// arguments (seeded PCG).
func Churn(load float64, seed uint64, capacity int, hTicks uint64, hash func(uint64) uint64, op func(h, window, now uint64)) {
	r := rand.New(rand.NewPCG(seed, 1))
	const newFrac = 0.6
	ratePerMs := load * float64(capacity) / (newFrac * float64(hTicks)) / 1.375 // calibrated: live keys per horizon ≈ load×capacity
	type born struct {
		h uint64
		t uint64
	}
	var keys []born
	var next uint64
	for now := uint64(1); now < 4*hTicks; now++ {
		n := int(ratePerMs)
		if r.Float64() < ratePerMs-float64(n) {
			n++
		}
		for range n {
			var h uint64
			if len(keys) == 0 || r.Float64() < newFrac {
				next++
				h = hash(next)
				keys = append(keys, born{h, now})
			} else { // repeat a key born within the last 1.5 horizons
				lo := sort.Search(len(keys), func(i int) bool { return keys[i].t+hTicks*3/2 > now })
				h = keys[lo+r.IntN(len(keys)-lo)].h
			}
			op(h, Windows[r.IntN(len(Windows))], now)
		}
		if now%hTicks == 0 { // drop keys too old to be repeated
			cut := 0
			for cut < len(keys) && keys[cut].t+2*hTicks < now {
				cut++
			}
			keys = keys[cut:]
		}
	}
}
