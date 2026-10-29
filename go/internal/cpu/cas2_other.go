//go:build !amd64

// Package cpu holds the filter's platform-specific primitives: a 16-byte
// compare-and-swap and line prefetches, in assembly where the architecture
// has them and in plain Go elsewhere.
package cpu

// HasCAS2: realign holds the slot (arm64's CASP would do; not yet measured).
const HasCAS2 = false

func CAS2(p *uint64, old0, old1, new0, new1 uint64) bool { panic("unreachable") }
