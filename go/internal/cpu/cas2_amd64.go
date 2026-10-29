// Package cpu holds the filter's platform-specific primitives: a 16-byte
// compare-and-swap and line prefetches, in assembly where the architecture
// has them and in plain Go elsewhere.
package cpu

// HasCAS2 reports CMPXCHG16B (every x86-64 CPU since about 2006, though
// GOAMD64=v1 doesn't promise it). Tests: TF_NOCAS2=1 turns it off, so
// realign holds the slot as on other architectures.
var HasCAS2 = cpuHasCX16()

// CAS2 atomically replaces the 16-byte pair at p (16-byte aligned: a slot)
// with new0, new1 if it still holds old0, old1.
//
//go:noescape
func CAS2(p *uint64, old0, old1, new0, new1 uint64) bool

func cpuHasCX16() bool
