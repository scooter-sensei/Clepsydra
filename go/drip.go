package temporalfilter

// The drip log (lab/sw-2026-11, arm W4; a lab arm behind the build tag
// tfdrip). word0 keeps, beside the key's exact last touch, a shift register
// of dripBits bits that each event shifts its gap to the touch before into,
// as a unary code in quanta of unit/16 (the onset's grid): g quanta are g
// ones and a zero, the newest code in the low bits; bits pushed past the
// field's end fall off. Reading from the low end gives the quanta of the
// key's recent events newest first, and the code the field's end cuts
// bounds the next gap from below, so the events the log no longer holds lie
// before a known quantum. Count reads it with the register and the onset:
// logged events are taken out of their buckets, counted by their quanta,
// and each bucket's other events are spread below the bound (claims-to-be
// in SW-RESULTS.md, W1-W3). Upper never reads it.
//
// A fresh key's field is all ones: no older event within dripBits quanta.
// 0 says nothing (a merge, a snapshot of a filter without the log, or
// dripBits+1 events in one quantum, whose field holds only zero codes); it
// stays so until a gap of dripBits quanta or more, after which all ones is
// true again, and Count reads the register and onset alone meanwhile. An
// event behind the last touch (a caller whose clock lags) is pushed as a gap
// of 0.

// quantum is the drip log's quantum index of tick t: floor(16 t / unit).
func (f *Filter) quantum(t uint64) int64 { return f.unitOf(t << 4) }

// dripOf is word0's drip log (0 without the build tag).
func dripOf(w0 uint64) uint64 { return w0 >> dripShift & dripMask }

// next0 is word0 w0 after an event at now: the last touch moves to now if
// now is later, and the drip log takes the event's gap.
func (f *Filter) next0(w0, now uint64) uint64 {
	last := w0 & tsMask
	if !dripOn {
		if now > last {
			return w0&^tsMask | now
		}
		return w0
	}
	d := dripOf(w0)
	var g uint64
	if now > last {
		g = uint64(f.quantum(now) - f.quantum(last))
	}
	switch {
	case g >= dripBits:
		d = dripMask
	case d != 0:
		d = (d<<(g+1) | (1<<g - 1)) & dripMask
	}
	return w0&^(tsMask|dripMask<<dripShift) | d<<dripShift | max(now, last)
}

// dripFresh is a new key's word0 low bits at now: its last touch, and a
// drip log that holds no older event.
func dripFresh(now uint64) uint64 { return dripMask<<dripShift | now }

// dripCount is Count's estimate from the register view v (at the unit of
// last), the last touch and the drip log d (non-zero): the count in
// [now-window, now], as W1's interval model reads it (swIvals, swCount).
func (f *Filter) dripCount(v *view, last, now, window, d uint64) float64 {
	unit := float64(f.unit)
	r := unit / 16
	from := float64(now) - float64(window)
	L := float64(last)
	var qs [16]int64
	n := 0
	qPrev := f.quantum(last)
	var lb int64
	for avail := dripBits; ; {
		if avail == 0 {
			break // the next code is cut at its first bit: a gap of 0 or more
		}
		ones := 0
		for ones < avail && d>>uint(ones)&1 == 1 {
			ones++
		}
		if ones == avail {
			lb = int64(ones) // a cut code: at least that many quanta
			break
		}
		qPrev -= int64(ones)
		qs[n] = qPrev
		n++
		d >>= uint(ones + 1)
		avail -= ones + 1
	}
	hiCap := float64(qPrev-lb+1) * r
	// the event at the last touch, and the logged ones
	est := 0.0
	if from <= L {
		est = 1
	}
	in := func(lo, hi, c float64) float64 {
		switch {
		case c <= 0:
			return 0
		case from <= lo:
			return c
		case from < hi:
			return c * (hi - from) / (hi - lo)
		}
		return 0
	}
	for _, q := range qs[:n] {
		est += in(float64(q)*r, min(float64(q+1)*r, L), 1)
	}
	// each bucket's other events, spread below the log's bound
	top := v.e + 1
	for j := range nBuckets {
		start := bucketStart(v.e, j)
		lo, hi := float64(start)*unit, float64(top)*unit
		c := float64(v.c[j])
		if j == 0 {
			c--
			hi = L
			if v.on != 0 {
				lo += float64(v.on) * unit / 16
			}
		}
		for _, q := range qs[:n] {
			if u := q >> 4; u >= start && u < top {
				c--
			}
		}
		hi = min(hi, hiCap)
		if hi < lo {
			hi = lo
		}
		est += in(lo, hi, c)
		top = start
	}
	return est
}
