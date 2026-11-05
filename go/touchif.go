package temporalfilter

import "time"

// condMu serializes conditional touches (TouchIfUnderAt) of keys sharing
// its stripe (the hash's top 6 bits), across filters. The claim stripes
// can't be used: touch takes them on a first touch.
var condMu [1 << 6]paddedMutex

// TouchIfUnder records an occurrence of key now only if the key is under
// limit within window by the strict fail-closed recipe (Upper below limit,
// not Saturated, and without an overflow table not an unseen key read
// Degraded, whose history may have been lost), and reports whether it did.
// Refused, the filter is unchanged.
func (f *Filter) TouchIfUnder(key string, window time.Duration, limit uint64) (Result, bool) {
	return f.TouchIfUnderAt(f.Hash(key), f.Ticks(window), limit, f.clock())
}

// TouchIfUnderAt is TouchIfUnder on a pre-hashed key, in ticks (claims
// A45). Conditional touches of one key are serialized (a lock across the
// read and the touch), so concurrent callers never admit more than limit
// in any window; plain touches of the key are not, and count against it.
// Upper counts a bucket the window cuts as a whole, so on windows shorter
// than a few units it admits fewer than limit.
func (f *Filter) TouchIfUnderAt(h, window, limit, now uint64) (Result, bool) {
	mu := &condMu[h>>(64-6)]
	mu.Lock()
	defer mu.Unlock()
	r := f.CountAt(h, window, now)
	if r.Saturated || f.skW == 0 && r.Degraded && !r.Seen || uint64(max(r.Upper, 0)) >= limit {
		return r, false
	}
	return f.TouchAt(h, window, now), true
}
