//go:build !amd64 && !arm64

package temporalfilter

import (
	"sync/atomic"
	"unsafe"
)

// prefetchLines loads one word of each 64-byte line at words + 64*lines[i],
// i < n, and drops it: the misses are in flight together, but unlike
// prefetch instructions the loads hold up retirement.
func prefetchLines(words unsafe.Pointer, lines *uint64, n int) {
	for _, l := range unsafe.Slice(lines, n) {
		_ = atomic.LoadUint64((*uint64)(unsafe.Add(words, l*64)))
	}
}
