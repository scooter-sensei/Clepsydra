package temporalfilter

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

// Attacks on the filter (SECURITY.md), each measured against the defence:
// forged identities and lock-stripe floods need the hash to be unkeyed;
// eviction floods need the stash to be off or overrun; heavy hitters need
// the count to read low without saying so.

func TestSipHashVectors(t *testing.T) {
	var key [16]byte
	for i := range key {
		key[i] = byte(i)
	}
	k := newSipKey(&key)
	msg := make([]byte, 15)
	for i := range msg {
		msg[i] = byte(i)
	}
	// Reference outputs from the SipHash paper and its test vectors.
	for _, c := range []struct {
		n    int
		want uint64
	}{{0, 0x726fdb47dd0e0e31}, {15, 0xa129ca6149be45e5}} {
		if got := k.string(string(msg[:c.n])); got != c.want {
			t.Errorf("siphash(%d bytes) = %#x, want %#x", c.n, got, c.want)
		}
	}
	var b [8]byte
	for _, x := range []uint64{0, 1, 0x0706050403020100, ^uint64(0)} {
		binary.LittleEndian.PutUint64(b[:], x)
		if k.uint64(x) != k.string(string(b[:])) {
			t.Errorf("uint64(%#x) differs from the byte path", x)
		}
	}
}

// unmix64 inverts mix64: the unkeyed hash is a bijection anyone can run
// backwards.
func unmix64(y uint64) uint64 {
	inv := func(c uint64) uint64 {
		x := c
		for range 6 {
			x *= 2 - c*x
		}
		return x
	}
	y ^= y>>31 ^ y>>62
	y *= inv(0x94d049bb133111eb)
	y ^= y>>27 ^ y>>54
	y *= inv(0xbf58476d1ce4e5b9)
	return y ^ y>>30 ^ y>>60
}

func attackFilter(t testing.TB, capacity, stash int, opts ...func(*Config)) (*Filter, *uint64) {
	t.Helper()
	now := new(uint64)
	*now = 1
	cfg := Config{Capacity: capacity, Horizon: time.Minute, Resolution: time.Millisecond,
		Clock: func() uint64 { return *now }, Stash: stash}
	for _, o := range opts {
		o(&cfg)
	}
	f, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f, now
}

// TestAttackForgedIdentity: with the old unkeyed integer hash an attacker
// computes ids whose hashes differ from a victim's only in bit 0..3. They
// land on the victim's line with its identity, so the attacker's events count
// as the victim's (framing it) and the victim reads as seen. Keyed, the same
// ids land elsewhere.
func TestAttackForgedIdentity(t *testing.T) {
	const victim, perForgery = 4242, 25
	run := func(hash func(uint64) uint64) (count int, seen bool) {
		f, now := attackFilter(t, 1<<14, 0)
		hv := hash(victim)
		for b := range 4 {
			forged := unmix64(mix64(victim) ^ 1<<b) // chosen against the unkeyed hash
			for range perForgery {
				*now++
				f.TouchAt(hash(forged), 0, *now)
			}
		}
		r := f.CountAt(hv, f.Ticks(time.Minute), *now)
		return r.Count, r.Seen
	}
	before, seenBefore := run(mix64)
	f, _ := attackFilter(t, 1, 0)
	after, seenAfter := run(f.HashUint64)
	t.Logf("victim never touched; attacker sent 4 forged ids x %d: unkeyed reads %d (seen %v), keyed reads %d (seen %v)",
		perForgery, before, seenBefore, after, seenAfter)
	if before != 4*perForgery || !seenBefore {
		t.Errorf("the forgery should work on the unkeyed hash: read %d", before)
	}
	if after != 0 || seenAfter {
		t.Errorf("keyed hash: victim reads %d, seen %v; want 0, false", after, seenAfter)
	}
}

// TestAttackStripeFlood: unkeyed, an attacker picks ids whose hashes share
// the top 8 bits, so every first touch queues on one of the 256 claim locks.
// Keyed, the same ids spread over the stripes.
func TestAttackStripeFlood(t *testing.T) {
	f, _ := attackFilter(t, 1, 0)
	unkeyed, keyed := map[uint64]bool{}, map[uint64]bool{}
	for i := range uint64(4096) {
		id := unmix64(0xAB<<56 | i*0x9e3779b97f4a7c15>>8)
		unkeyed[mix64(id)>>(64-claimStripeBits)] = true
		keyed[f.HashUint64(id)>>(64-claimStripeBits)] = true
	}
	t.Logf("4096 crafted ids hit %d stripe(s) unkeyed, %d keyed", len(unkeyed), len(keyed))
	if len(unkeyed) != 1 || len(keyed) < 240 {
		t.Errorf("stripes: unkeyed %d (want 1), keyed %d (want ~256)", len(unkeyed), len(keyed))
	}
}

// evictionSetup fills a table to its design load with one-touch keys and
// adds an offender with k events.
func evictionSetup(t *testing.T, stash, k int, opts ...func(*Config)) (f *Filter, now *uint64, rng *rand.Rand, off uint64) {
	f, now = attackFilter(t, 4096, stash, opts...)
	rng = rand.New(rand.NewPCG(7, uint64(stash+1)))
	for range f.Slots() * 85 / 100 {
		*now++
		f.TouchAt(rng.Uint64(), 0, *now)
	}
	off = rng.Uint64()
	for range k {
		*now++
		f.TouchAt(off, 0, *now)
	}
	return f, now, rng, off
}

// sprayUntilEvicted touches each key from next k+1 times (so it outranks the
// offender for eviction) until the offender is out of the table, then
// returns the attacker's event count.
func sprayUntilEvicted(f *Filter, now *uint64, off uint64, k int, next func() uint64) int {
	fp, tg := fingerprint(off), tag(off)
	events := 0
	for f.locate(off, fp, tg) >= 0 && events < 1<<24 {
		h := next()
		for range k + 1 {
			f.TouchAt(h, 0, *now)
			events++
		}
		*now++
	}
	return events
}

// noSketch turns the overflow sketch off.
func noSketch(c *Config) { c.Overflow = -1 }

// TestAttackEvictionReset: an offender with k events gets evicted so that it
// comes back as a first-time key. With the unkeyed hash the attacker builds
// keys on the offender's two lines (same primary line and fingerprint,
// other tags): a few hundred events. Keyed, the spray lands at random and
// needs about a table's worth of keys, each outranking the offender. The
// stash then brings the offender back with its history. A flood that also
// overruns the stash with heavier keys pushes the history into the overflow
// sketch: Upper still covers it, and Count does when the history stands out
// from the flood's noise (then Degraded). Without the sketch the history is
// lost, and every new key reads Degraded.
func TestAttackEvictionReset(t *testing.T) {
	const k = 20
	window := uint64(60_000)
	report := func(name string, f *Filter, events int, r Result) {
		t.Logf("%-30s attacker events %7d; offender returns: count %2d upper %3d (true %d) seen %-5v restored %-5v degraded %-5v; evictions %d folded %d lost %d",
			name, events, r.Count, r.Upper, k+1, r.Seen, r.Restored, r.Degraded, f.Evictions(), f.Folded(), f.Lost())
	}
	var targeted, sprayed int
	for _, stash := range []int{-1, 0} {
		f, now, _, off := evictionSetup(t, stash, k, noSketch)
		tagN := uint64(0)
		events := sprayUntilEvicted(f, now, off, k, func() uint64 { // forge: flip tag bits only
			tagN++
			return off ^ tagN<<36
		})
		*now++
		r := f.TouchAt(off, window, *now)
		report(fmt.Sprintf("unkeyed targeted, stash %v", stash >= 0), f, events, r)
		targeted = events
		if stash < 0 && (r.Count != 1 || !r.Degraded) {
			t.Errorf("no stash: expected a reset reported as Degraded, got %+v", r)
		}
		if stash >= 0 && (!r.Restored || r.Count != k+1) {
			t.Errorf("stash: offender not restored after a targeted eviction: %+v", r)
		}
	}
	modes := []struct {
		name  string
		stash int
		opts  []func(*Config)
	}{
		{"keyed spray, no stash/sketch", -1, []func(*Config){noSketch}},
		{"keyed spray, stash only", 0, []func(*Config){noSketch}},
		{"keyed spray, stash + sketch", 0, nil},
	}
	for _, m := range modes {
		f, now, rng, off := evictionSetup(t, m.stash, k, m.opts...)
		events := sprayUntilEvicted(f, now, off, k, rng.Uint64)
		*now++
		r := f.TouchAt(off, window, *now)
		report(m.name, f, events, r)
		sprayed = max(sprayed, events)
		if !r.Restored && !r.Degraded && r.Upper < k+1 {
			t.Errorf("%s: history lost silently: %+v", m.name, r)
		}
		if m.opts == nil && (f.Lost() != 0 || r.Upper < k+1) {
			t.Errorf("%s: lost %d, Upper %d < %d", m.name, f.Lost(), r.Upper, k+1)
		}
	}
	t.Logf("keyed hashing raises the cost of evicting a %d-event key from %d to %d attacker events (%.0fx; table %d slots)",
		k, targeted, sprayed, float64(sprayed)/float64(targeted), 4096*100/85)
}

// TestAttackFloodCollateral: offenders with k events each, then a flood of
// keys sent k+1 times each (heavier than the offenders, so they evict them)
// at 1x to 16x the table's slots. Afterwards: how the offenders read (Upper
// never below the truth; Count's shortfall), and how keys never sent read
// (Degraded, Seen, Upper). Without the sketch every new key reads Degraded
// once any history is lost.
func TestAttackFloodCollateral(t *testing.T) {
	const k, offenders, probes = 20, 200, 20_000
	window := uint64(60_000)
	for _, sk := range []bool{false, true} {
		for _, mult := range []int{1, 4, 16} {
			var opts []func(*Config)
			if !sk {
				opts = append(opts, noSketch)
			}
			f, now := attackFilter(t, 4096, 0, opts...)
			rng := rand.New(rand.NewPCG(11, uint64(mult)))
			for range f.Slots() * 85 / 100 {
				*now++
				f.TouchAt(rng.Uint64(), 0, *now)
			}
			offs := make([]uint64, offenders)
			for i := range offs {
				offs[i] = rng.Uint64()
				for range k {
					f.TouchAt(offs[i], 0, *now)
				}
				*now++
			}
			for range f.Slots() * mult {
				h := rng.Uint64()
				for range k + 1 {
					f.TouchAt(h, 0, *now)
				}
				*now++
			}
			*now++
			low, below, shortfall := 0, 0, 0
			for _, h := range offs {
				r := f.CountAt(h, window, *now)
				if r.Upper < k {
					below++
				}
				if r.Count < k {
					low++
					shortfall += k - r.Count
				}
			}
			deg, seen, upper, strict := 0, 0, 0, 0
			for range probes {
				r := f.CountAt(rng.Uint64(), window, *now)
				if r.Degraded {
					deg++
				}
				if r.Degraded || r.Upper > k {
					strict++ // a strict limit of k turns it away
				}
				if r.Seen {
					seen++
				}
				upper += r.Upper
			}
			t.Logf("sketch %-5v flood %2dx slots (%7d events): offenders Upper<true %d/%d, Count low %3d/%d (mean shortfall %4.1f); new keys Degraded %5.1f%% Seen %5.2f%% mean Upper %6.1f, denied by a strict limit of %d: %5.1f%%; folded %d lost %d",
				sk, mult, f.Slots()*mult*(k+1), below, offenders, low, offenders, float64(shortfall)/float64(max(low, 1)),
				100*float64(deg)/probes, 100*float64(seen)/probes, float64(upper)/probes, k, 100*float64(strict)/probes, f.Folded(), f.Lost())
			if sk && (below > 0 || f.Lost() > 0) {
				t.Errorf("sketch: %d offenders read Upper below the truth, %d lost", below, f.Lost())
			}
			if sk && deg*100 > probes {
				t.Errorf("sketch: %d of %d new keys read Degraded", deg, probes)
			}
		}
	}
}

// TestAttackHeavyHitter: a key sending faster than the register holds reads
// low (DESIGN.md, count limits). That must never happen silently: whenever
// Saturated is false, Upper must be >= the exact count. Steady and bursty
// senders from 1 to 500 events/s, queried for 1-60 s windows at random
// times, against an exact log of their events.
func TestAttackHeavyHitter(t *testing.T) {
	type stats struct{ queries, sat, silentLow, upperLow, countLow int }
	rates := []float64{1, 5, 12, 13, 20, 31, 50, 100, 500}
	var all stats
	for _, burst := range []bool{false, true} {
		for _, rate := range rates {
			var s stats
			for key := range 8 {
				f, now := attackFilter(t, 1024, 0)
				rng := rand.New(rand.NewPCG(uint64(rate*1000), uint64(key)))
				h := rng.Uint64()
				var log []uint64
				end := uint64(180_000) + rng.Uint64N(60_000)
				for *now < end {
					gap := rng.ExpFloat64() * 1000 / rate
					if burst && rng.IntN(20) == 0 {
						gap *= 20 // quiet spells, then catch-up bursts
					}
					*now += max(1, uint64(gap))
					n := 1
					if burst {
						n = 1 + rng.IntN(4)
					}
					for range n {
						f.TouchAt(h, 0, *now)
						log = append(log, *now)
					}
					if rng.IntN(25) == 0 {
						w := uint64(1000 * (1 + rng.IntN(60)))
						exact := 0
						for _, ts := range log {
							if *now-ts <= w {
								exact++
							}
						}
						r := f.CountAt(h, w, *now)
						s.queries++
						if r.Saturated {
							s.sat++
							continue
						}
						if float64(r.Count) < 0.9*float64(exact)-1 {
							s.countLow++
						}
						if r.Upper < exact {
							s.upperLow++
							if s.upperLow <= 3 {
								t.Logf("rate %v burst %v: window %ds exact %d, Count %d, Upper %d, not Saturated", rate, burst, w/1000, exact, r.Count, r.Upper)
							}
						}
					}
				}
			}
			t.Logf("burst %-5v %4.0f ev/s: %5d queries, %5.1f%% Saturated; unflagged: Upper < exact %d, Count >10%% low %d",
				burst, rate, s.queries, 100*float64(s.sat)/float64(max(1, s.queries)), s.upperLow, s.countLow)
			all.queries += s.queries
			all.sat += s.sat
			all.upperLow += s.upperLow
			all.countLow += s.countLow
		}
	}
	fmt.Printf("heavy hitter: %d queries, %d Saturated, %d unflagged with Upper < exact, %d unflagged with Count >10%% low\n",
		all.queries, all.sat, all.upperLow, all.countLow)
	if all.upperLow > 0 {
		t.Errorf("%d unflagged queries had Upper below the exact count", all.upperLow)
	}
}

// TestUpperBoundRandom is TestAttackHeavyHitter over random senders: each
// key switches between random rates (0.2-60 events/s), silences and bursts
// of up to 200 events in one tick, with horizons of 10 s and 60 s. It checks
// the same contract (unflagged Upper >= exact) and reports how loose Upper
// is.
func TestUpperBoundRandom(t *testing.T) {
	keys := 3000
	if testing.Short() {
		keys = 300
	}
	var queries, sat, low int
	var slack, exactSum float64
	for key := range keys {
		rng := rand.New(rand.NewPCG(99, uint64(key)))
		hz := []time.Duration{10 * time.Second, time.Minute}[key%2]
		now := uint64(1)
		f, err := New(Config{Capacity: 64, Horizon: hz, Resolution: time.Millisecond, Clock: func() uint64 { return now }})
		if err != nil {
			t.Fatal(err)
		}
		hMs := uint64(hz / time.Millisecond)
		h := rng.Uint64()
		var log []uint64
		rate := 0.0
		for now < 6*hMs {
			if rng.IntN(200) == 0 || rate == 0 {
				rate = []float64{0.2, 1, 3, 8, 12, 20, 40, 60}[rng.IntN(8)]
			}
			now += max(1, uint64(rng.ExpFloat64()*1000/rate))
			if rng.IntN(500) == 0 {
				now += uint64(rng.IntN(int(hMs))) // silence
			}
			n := 1
			if rng.IntN(100) == 0 {
				n = 1 + rng.IntN(200) // burst
			}
			for range n {
				f.TouchAt(h, 0, now)
				log = append(log, now)
			}
			if rng.IntN(10) == 0 {
				w := 1 + rng.Uint64N(hMs)
				exact := 0
				for _, ts := range log {
					if now-ts <= w {
						exact++
					}
				}
				r := f.CountAt(h, w, now)
				queries++
				if r.Saturated {
					sat++
					continue
				}
				if r.Upper < exact {
					low++
					if low <= 5 {
						t.Logf("key %d horizon %v window %dms: exact %d Count %d Upper %d unflagged", key, hz, w, exact, r.Count, r.Upper)
					}
				}
				slack += float64(r.Upper - exact)
				exactSum += float64(exact)
			}
		}
	}
	fmt.Printf("upper bound: %d queries, %d Saturated, %d unflagged with Upper < exact; unflagged Upper exceeds exact by %.1f%% of events\n",
		queries, sat, low, 100*slack/max(1, exactSum))
	if low > 0 {
		t.Errorf("%d unflagged queries had Upper below the exact count", low)
	}
}
