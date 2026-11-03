package temporalfilter

import (
	"unsafe"

	"github.com/scooter-sensei/clepsydra/go/internal/cpu"
)

// CheckAndTouchBatch is CheckAndTouchAt over many pre-hashed keys, in the
// order of hs, a chunk at a time with the chunk's cache misses overlapped
// (see warm). out must be at least len(hs).
func (f *Filter) CheckAndTouchBatch(hs []uint64, window, now uint64, out []bool) {
	out = out[:len(hs)]
	var at [batchChunk]int
	for lo := 0; lo < len(hs); lo += batchChunk {
		hi := min(lo+batchChunk, len(hs))
		f.warm(hs[lo:hi], &at)
		for i := lo; i < hi; i++ {
			out[i] = f.touch(hs[i], window, now, false, at[i-lo]).Seen
		}
	}
}

// TouchAndCountBatch is TouchAndCountAt over many pre-hashed keys, with the
// overlapped misses of CheckAndTouchBatch. seen and counts must be at least
// len(hs).
func (f *Filter) TouchAndCountBatch(hs []uint64, window, now uint64, seen []bool, counts []int) {
	seen, counts = seen[:len(hs)], counts[:len(hs)]
	var at [batchChunk]int
	for lo := 0; lo < len(hs); lo += batchChunk {
		hi := min(lo+batchChunk, len(hs))
		f.warm(hs[lo:hi], &at)
		for i := lo; i < hi; i++ {
			r := f.touch(hs[i], window, now, true, at[i-lo])
			seen[i], counts[i] = r.Seen, r.Count
		}
	}
}

// VelocityCountBatch is VelocityCountAt over many pre-hashed keys, with the
// overlapped misses of CheckAndTouchBatch. counts must be at least len(hs).
func (f *Filter) VelocityCountBatch(hs []uint64, window, now uint64, counts []int) {
	counts = counts[:len(hs)]
	var at [batchChunk]int
	for lo := 0; lo < len(hs); lo += batchChunk {
		hi := min(lo+batchChunk, len(hs))
		f.warm(hs[lo:hi], &at)
		for i := lo; i < hi; i++ {
			counts[i] = f.count(hs[i], window, now, at[i-lo]).Count
		}
	}
}

// batchChunk is how many keys warm overlaps (S48: 32 beat 64 on tables
// near the last-level cache's size and matched it past them).
const batchChunk = 32

// warm overlaps the cache misses of a chunk's keys (memory-level
// parallelism). It prefetches every key's primary line, looks for each key
// there (at[k]: its slot, or -1), then prefetches the alternate lines of the
// keys it didn't find there, so each key's operation, in order, finds its
// lines cached or on their way, and a touch or count starts from its slot.
// Prefetch instructions, unlike loads, don't hold up retirement, and the
// touches' locked CASes don't wait for them. S48: 1.2x the loads it replaces
// at 90% load on tables past the last-level cache; 0.9x on a table that
// fits it.
//
// In ConstantTime it looks nothing up and prefetches both lines of every
// key: its work, outside the calls' pads, doesn't depend on what the table
// holds (touchCT reads both lines anyway).
func (f *Filter) warm(hs []uint64, at *[batchChunk]int) {
	var lines [2 * batchChunk]uint64
	w := unsafe.Pointer(&f.words[0])
	if f.ct {
		for k, h := range hs {
			p := f.primary(h)
			lines[2*k], lines[2*k+1], at[k] = p, f.alt(p, fingerprint(h)), -1
		}
		cpu.PrefetchLines(w, &lines[0], 2*len(hs))
		return
	}
	for k, h := range hs {
		lines[k] = f.primary(h)
	}
	cpu.PrefetchLines(w, &lines[0], len(hs))
	m := 0
	for k, h := range hs {
		p, fp := lines[k], fingerprint(h)
		if at[k] = f.find(p, fp<<1, tag(h)); at[k] < 0 {
			lines[m], m = f.alt(p, fp), m+1 // m <= k: the entries before k's are used
		}
	}
	cpu.PrefetchLines(w, &lines[0], m)
}
