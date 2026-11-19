package temporalfilter

// lab/sw-2026-11, W10: Count's and the drip log's checks for the soak and
// the concurrent fuzzer, on keys with one writer. Upper (so Count, never
// above it) counts no event from further back than its window reaches, the
// overflow table's part aside: TestFuzzUpper's high, under concurrency. And
// at rest a key's drip log holds no more events from any quantum on than
// the key had: a touch that pushes its event twice leaves one more (W9).
//
// W10b: the overflow table's part is the one the call itself added
// (testHookSketchPart, recorded in partCells), not the larger of two reads
// around the call, which other callers' sweeps of the table made miss it
// (W10's reading, case 865).

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// partCells record, for a key a check arms, the overflow table's part of
// its call's answer as addSketch adds it: the key's 48 high bits beside the
// part (16 bits, saturating), in the cell its 18 low bits pick. A cell
// another key armed since reads as taken over, and that answer is set apart.
// Lock-free and allocation-free, so the record costs the callers little.
var partCells [1 << 18]atomic.Uint64

func partCell(h uint64) *atomic.Uint64 { return &partCells[h&(1<<18-1)] }

// recordPart is testHookSketchPart while a test records parts.
func recordPart(h uint64, upper int) {
	c := partCell(h)
	for {
		old := c.Load()
		if old>>16 != h>>16 {
			return // not armed for h
		}
		if c.CompareAndSwap(old, h>>16<<16|uint64(min(max(upper, 0), 0xffff))) {
			return
		}
	}
}

// armPart readies h's cell before a checked call (a part of 0, as for a call
// that adds none); partOf reads it after: the part, and false if another
// key's call took the cell over meanwhile.
func armPart(h uint64) { partCell(h).Store(h >> 16 << 16) }

func partOf(h uint64) (int, bool) {
	v := partCell(h).Load()
	return int(v & 0xffff), v>>16 == h>>16
}

// recordParts sets testHookSketchPart for t's duration (before t starts
// goroutines that call the filter; cleared after t ends).
func recordParts(t testing.TB) {
	testHookSketchPart = recordPart
	t.Cleanup(func() { testHookSketchPart = nil })
}

// countReach is how far behind a window an answer from a register can count
// an event: the bucket the window's edge cuts counts whole (buckets are at
// most 8 units wide), and a touch at a clock behind the register's unit
// counts in that unit, up to a unit and lag later than it happened
// (TestFuzzUpper's reach).
func (f *Filter) countReach(lag uint64) uint64 { return 9*f.unit + lag + 1 }

// ghostUpper is the overflow table's part of an answer's Upper for h.
func (f *Filter) ghostUpper(h, window, now uint64) int {
	if g := f.gh.Load(); g != nil {
		s, _ := g.estimate(f.stashID(h, fingerprint(h), tag(h)), min(window, f.horizon), now)
		return s.Upper
	}
	return 0
}

// countHigh is "" unless r, an answer for a key whose sorted event times
// are l, has Upper, less ghost (the overflow table's part), above the
// key's events back to the window's reach.
func (f *Filter) countHigh(r Result, ghost int, l []uint64, window, now, lag uint64) string {
	lo := now - min(now, min(window, f.horizon)+f.countReach(lag))
	a, _ := slices.BinarySearch(l, lo)
	if n := len(l) - a; r.Upper-ghost > n {
		return fmt.Sprintf("Upper %d (the overflow table's %d) > %d events back to %d", r.Upper, ghost, n, lo)
	}
	return ""
}

// logHigh is "" unless h's drip log, read at rest, holds more events from
// some quantum on than the key had: l are its sorted event times, each of
// which can be logged up to lag later than it happened (a touch behind the
// last touch pushes at the last touch's quantum). checked is false when
// the log says nothing, the build has none, or h has no history.
func (f *Filter) logHigh(h, now, lag uint64, l []uint64) (msg string, checked bool) {
	if !dripOn {
		return "", false
	}
	w0, _, src := f.read(h, now, -1)
	if src == 0 || w0 == 0 {
		return "", false
	}
	d := dripOf(w0)
	if d&dripV == 0 {
		return "", false
	}
	field := d & dripMask
	q := f.quantum(w0 & tsMask)
	k := 1 // the event at the last touch, then one per code
	over := func() string {
		n := 0
		for i := len(l) - 1; i >= 0 && f.quantum(l[i]+lag) >= q; i-- {
			n++
		}
		if n < k {
			return fmt.Sprintf("log %#x: %d events logged at quantum %d or later, the key had %d (last touch %d)", d, k, q, n, w0&tsMask)
		}
		return ""
	}
	if m := over(); m != "" {
		return m, true
	}
	for avail := dripBits; avail > 0; {
		ones := 0
		for ones < avail && field>>uint(ones)&1 == 1 {
			ones++
		}
		if ones == avail {
			break // the code the field's end cuts: no event
		}
		q -= int64(ones)
		k++
		if m := over(); m != "" {
			return m, true
		}
		field >>= uint(ones + 1)
		avail -= ones + 1
	}
	return "", true
}

// sharesIdentity: another key of keys has h's identity (line, fingerprint
// and tag), so the filter can't tell their events apart.
func (f *Filter) sharesIdentity(h uint64, keys map[uint64][]uint64) bool {
	id := f.stashID(h, fingerprint(h), tag(h))
	for k := range keys {
		if k != h && f.stashID(k, fingerprint(k), tag(k)) == id {
			return true
		}
	}
	return false
}

// TestPartCells checks partCells alone, with no filter (W10b): a cell reads
// what its key's record put there, 0 when armed and not recorded, ignores
// another key's record, saturates, and reads as taken over once another key
// with the same 18 low bits arms it; and, from goroutines each recording its
// own keys in distinct cells, every read is the goroutine's own value.
func TestPartCells(t *testing.T) {
	h := uint64(0x123456789abcdef0)
	other := h ^ 1<<40 // h's cell, another key
	want := func(k uint64, p int, ok bool) {
		t.Helper()
		if gp, gok := partOf(k); gp != p || gok != ok {
			t.Fatalf("partOf(%x) = %d, %v; want %d, %v", k, gp, gok, p, ok)
		}
	}
	armPart(h)
	want(h, 0, true)
	recordPart(other, 5) // not armed for other
	want(h, 0, true)
	recordPart(h, 7)
	want(h, 7, true)
	recordPart(h, 3) // a later add in the same call replaces it
	want(h, 3, true)
	recordPart(h, 1<<20)
	want(h, 0xffff, true)
	recordPart(h, -1)
	want(h, 0, true)
	armPart(other)
	want(h, 0, false)
	recordPart(h, 2) // h's record after the take-over
	want(other, 0, true)
	const workers, rounds = 16, 100_000
	var wg sync.WaitGroup
	var bad atomic.Int64
	for g := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				k := uint64(g)<<48 | uint64(i%64)<<20 | uint64(g*64+i%64) // distinct cells across goroutines
				armPart(k)
				recordPart(k, i&0xff)
				recordPart(k^1<<40, 9) // another key of the cell, never armed
				if p, ok := partOf(k); p != i&0xff || !ok {
					bad.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if n := bad.Load(); n > 0 {
		t.Errorf("%d reads of %d were not the goroutine's own record", n, workers*rounds)
	}
}
