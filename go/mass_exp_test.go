package temporalfilter

import (
	"os"
	"testing"
	"time"
)

// TestMassMatchesView (TF_EXP=1): mass, read from word1 in place, equals
// the view's walk (viewOf, viewMass) on 20 M random registers biased to the
// edges: empty registers, bucket 0 alone, full and one-event counters, every
// epoch offset, clocks at, just past and behind the last touch, on unit
// boundaries and at the horizon.
func TestMassMatchesView(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	f := newTest(t, 1000, time.Minute)
	viewMass := func(w1, last, now uint64) uint64 {
		if age(now, last) > f.horizon || w1&regMask == 0 {
			return 0
		}
		v, _ := f.viewOf(0, w1, last)
		return f.viewMass(&v, now)
	}
	u, h := f.unit, f.horizon
	x := uint64(7)
	next := func() uint64 { x = mix64(x + 0x9e3779b97f4a7c15); return x }
	for i := range 20_000_000 {
		r := next()
		var reg uint64
		switch r % 8 {
		case 0: // empty
		case 1: // bucket 0 alone
			reg = next() % 32
		default:
			for j := range nBuckets {
				max := uint64(1)<<bucketBits[j] - 1
				var c uint64
				switch next() % 4 {
				case 1:
					c = 1
				case 2:
					c = max
				case 3:
					c = next() & max
				}
				reg |= c << bucketShift[j]
			}
		}
		w1 := (next()%(1<<tagBits-1))<<tagShift | (next()&epochMask)<<48 | reg
		last := 3*h + next()%(10*h)
		if next()%4 == 0 {
			last = last/u*u + []uint64{0, 1, u - 1}[next()%3] // on, just past, just before a unit boundary
		}
		var now uint64
		switch next() % 6 {
		case 0:
			now = last // an event at now
		case 1:
			now = last - next()%(2*u) // a caller behind the last touch
		case 2:
			now = last + h - 1 + next()%3 // at the horizon
		case 3:
			now = (last/u+next()%30)*u + []uint64{0, 1, u - 1}[next()%3] // unit boundaries
		default:
			now = last + next()%(h+2*u)
		}
		if got, want := f.mass(0, w1, last, now), viewMass(w1, last, now); got != want {
			t.Fatalf("case %d: w1 %#x last %d now %d: mass %d, view %d", i, w1, last, now, got, want)
		}
	}
}
