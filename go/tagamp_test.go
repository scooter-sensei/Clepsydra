package temporalfilter

import (
	"os"
	"strconv"
	"sync"
)

// tagAmp amplifies tag coincidences in the concurrent tests (TF_CFUZZ_TAGS,
// TF_STRESS_TAGS): it remaps each key's hash so its 12-bit tag is one of n
// values and its fingerprint is unique in the run, its primary line kept.
// Two keys that meet in a slot then share a tag one time in n instead of
// one in 4096, and no two keys share an identity. The races where a CAS
// from a value read earlier lands on a slot that changed hands, because a
// same-tag key's register can be bit-identical to the old occupant's
// (claims A28, A29), turn from one failure per 30 to 60 minutes of the
// concurrent fuzzer into a few a minute. The tags have distinct low bytes,
// so their keys' move counts stay on distinct stripes. Fingerprints are
// 16 bits: a run must use fewer than 65,536 keys.
type tagAmp struct {
	n    uint64
	mu   sync.Mutex
	fp   map[uint64]uint64
	next uint64
}

// newTagAmp is the amplifier the environment variable env asks for (n,
// up to 14), or nil.
func newTagAmp(env string) *tagAmp {
	n, _ := strconv.Atoi(os.Getenv(env))
	if n <= 0 {
		return nil
	}
	return &tagAmp{n: uint64(min(n, 14)), fp: map[uint64]uint64{}}
}

// h is x remapped (x itself on a nil amplifier); cache, if not nil, is the
// caller's own copy of the mapping.
func (a *tagAmp) h(x uint64, cache map[uint64]uint64) uint64 {
	if a == nil {
		return x
	}
	if y, ok := cache[x]; ok {
		return y
	}
	a.mu.Lock()
	fp, ok := a.fp[x]
	if !ok {
		a.next++
		fp = a.next * 0x9e37 & 0xffff // a bijection of 1..65535: no fingerprint 0, stripes spread
		a.fp[x] = fp
	}
	a.mu.Unlock()
	tg := 0x010 + x>>36%a.n*0x011
	y := fp<<48 | tg<<36 | x&(1<<36-1)
	if cache != nil {
		cache[x] = y
	}
	return y
}
