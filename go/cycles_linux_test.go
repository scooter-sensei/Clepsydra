//go:build linux

package temporalfilter

import (
	"runtime"
	"syscall"
	"testing"
	"unsafe"
)

// cycleCounter counts the user-mode CPU cycles of a benchmark's timed loop,
// on the goroutine's thread, through perf_event_open. ns/op moves with the
// host's clock frequency (boost, thermal limits); cycles/op does not, so
// A/B readings on one host compare the code more than the moment it ran
// (verify/lrab.sh and verify/ab.sh take METRIC=cycles/op; claims S56).
// Where perf events are unavailable (perf_event_paranoid above 2,
// containers, other systems) it reports nothing.
type cycleCounter struct {
	fd    int
	start uint64
}

// perfEventAttr is Linux's struct perf_event_attr up to bp_len
// (PERF_ATTR_SIZE_VER1, 72 bytes).
type perfEventAttr struct {
	typ, size    uint32
	config       uint64
	samplePeriod uint64
	sampleType   uint64
	readFormat   uint64
	flags        uint64
	wakeup       uint32
	bpType       uint32
	bpAddr       uint64
	bpLen        uint64
}

// startCycles locks the goroutine to its thread and starts counting: call
// it right after b.ResetTimer, and report after the loop.
func startCycles() *cycleCounter {
	runtime.LockOSThread()
	// PERF_TYPE_HARDWARE, PERF_COUNT_HW_CPU_CYCLES; exclude_kernel, exclude_hv.
	attr := perfEventAttr{size: uint32(unsafe.Sizeof(perfEventAttr{})), flags: 1<<5 | 1<<6}
	fd, _, errno := syscall.Syscall6(syscall.SYS_PERF_EVENT_OPEN, uintptr(unsafe.Pointer(&attr)), 0, ^uintptr(0), ^uintptr(0), 0, 0)
	c := &cycleCounter{fd: -1}
	if errno == 0 {
		c.fd = int(fd)
		c.start = c.read()
	}
	return c
}

func (c *cycleCounter) read() uint64 {
	var v uint64
	if n, err := syscall.Read(c.fd, (*[8]byte)(unsafe.Pointer(&v))[:]); n != 8 || err != nil {
		return 0
	}
	return v
}

// report stops counting and adds cycles/op to b's result.
func (c *cycleCounter) report(b *testing.B) {
	if c.fd >= 0 {
		end := c.read()
		syscall.Close(c.fd)
		if end > c.start && b.N > 0 {
			b.ReportMetric(float64(end-c.start)/float64(b.N), "cycles/op")
		}
	}
	runtime.UnlockOSThread()
}
