package siphash

// hash8x writes SipHash-2-4 of keys[0:n] to out[0:n], 8 at a time (n a
// multiple of 8), one zmm lane per key. AVX-512F only.
//
//go:noescape
func hash8x(k0, k1 uint64, keys, out *uint64, n int)

// cpuHasAVX512F: the CPU has AVX-512F and the OS saves its state (XCR0:
// SSE, AVX, opmask, the upper zmm halves and zmm16-31).
func cpuHasAVX512F() bool

// useAVX512 selects hash8x in HashUint64s (tests turn it off).
var useAVX512 = cpuHasAVX512F()
