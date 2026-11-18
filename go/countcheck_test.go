package temporalfilter

// lab/sw-2026-11, W10: Count's and the drip log's checks for the soak and
// the concurrent fuzzer, on keys with one writer. Upper (so Count, never
// above it) counts no event from further back than its window reaches, the
// overflow table's part aside: TestFuzzUpper's high, under concurrency. And
// at rest a key's drip log holds no more events from any quantum on than
// the key had: a touch that pushes its event twice leaves one more (W9).

import (
	"fmt"
	"slices"
)

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
