//go:build amd64 || arm64

package cpu

import "unsafe"

// PrefetchLines asks for the 64-byte lines at words + 64*lines[i], i < n
// (PREFETCHT0, PRFM PLDL1KEEP), without waiting for them. Unlike a load, a
// prefetch doesn't hold up retirement, and the locked instructions of the
// touches that follow (their CASes) don't wait for it to land. One call
// takes a chunk: an assembly call costs a few ns.
//
//go:noescape
func PrefetchLines(words unsafe.Pointer, lines *uint64, n int)
