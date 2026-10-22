//go:build linux

package temporalfilter

import (
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Host probes (TF_EXP=1): what a timing claim on this host rests on. Every
// latency and leak figure in verify/claims.md is measured on one host;
// these two say how that host behaves, so figures from another can be read
// against its own (claims S42, S43).

// TestHostGaps: how often this host takes the CPU
// away from a running goroutine, and for how long. A loop reads the clock
// back to back for 5 s; a gap between two reads longer than 1 µs is time
// the loop didn't run (an interrupt, a VM exit, the scheduler). A call of
// length d is hit with probability ~ rate x d.
func TestHostGaps(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const run = 5 * time.Second
	var gaps []float64
	start := time.Now()
	prev := start
	reads := 0
	for {
		now := time.Now()
		reads++
		if d := now.Sub(prev); d > time.Microsecond {
			gaps = append(gaps, float64(d.Nanoseconds()))
		}
		prev = now
		if now.Sub(start) > run {
			break
		}
	}
	slices.Sort(gaps)
	q := func(p float64) float64 {
		if len(gaps) == 0 {
			return 0
		}
		return gaps[int(p*float64(len(gaps)-1))]
	}
	stolen := 0.0
	for _, g := range gaps {
		stolen += g
	}
	rate := float64(len(gaps)) / run.Seconds()
	t.Logf("%d clock reads in %v; %d gaps > 1 µs (%.0f per s), %.2f%% of the time; gap p50/p90/p99/max %.1f/%.1f/%.1f/%.1f µs",
		reads, run, len(gaps), rate, 100*stolen/float64(run.Nanoseconds()), q(.5)/1e3, q(.9)/1e3, q(.99)/1e3, q(1)/1e3)
	for _, d := range []float64{400, 800, 1400, 2000} {
		t.Logf("  a %4.0f ns call is hit with probability ~%.3f%%", d, 100*rate*d*1e-9)
	}
}

// TestMemProbe: this host's memory as the cuckoo
// search sees it. Over a 512 MiB array (4 KiB pages, then advised huge
// pages): a dependent chain of loads (latency), and batches of k independent
// random loads (k = 1, 8, 32), as warmAlts and warmAlts2 issue them.
func TestMemProbe(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	const n = 64 << 20 // words: 512 MiB
	for _, huge := range []bool{false, true} {
		buf := make([]uint64, n+(2<<20)/8)
		off := int(((2<<20)-uintptr(unsafe.Pointer(&buf[0]))%(2<<20))%(2<<20)) / 8
		a := buf[off : off+n]
		if huge {
			b := unsafe.Slice((*byte)(unsafe.Pointer(&a[0])), n*8)
			if err := syscall.Madvise(b, 14); err != nil {
				t.Fatal(err)
			}
		}
		rng := rand.New(rand.NewPCG(1, 2))
		// A random cyclic permutation over cache lines (8 words each).
		lines := n / 8
		perm := rng.Perm(lines)
		for i := range lines {
			a[perm[i]*8] = uint64(perm[(i+1)%lines] * 8)
		}
		// Dependent chain.
		const steps = 2_000_000
		p := uint64(perm[0] * 8)
		t0 := time.Now()
		for range steps {
			p = a[p]
		}
		lat := float64(time.Since(t0).Nanoseconds()) / steps
		t.Logf("huge %v: dependent load %.0f ns (%d)", huge, lat, p%2)
		// Batches of independent loads at random lines.
		idx := make([]int, 1<<20)
		for i := range idx {
			idx[i] = rng.IntN(lines) * 8
		}
		for _, k := range []int{1, 8, 32} {
			var sink uint64
			batches := len(idx) / k
			t0 := time.Now()
			for b := range batches {
				for _, j := range idx[b*k : b*k+k] {
					sink += a[j]
				}
				sink = a[sink%uint64(n)] // the next batch waits for this one, as the search's next step does
			}
			t.Logf("huge %v: batch of %2d independent loads %.0f ns (%.1f ns per load) (%d)", huge, k,
				float64(time.Since(t0).Nanoseconds())/float64(batches),
				float64(time.Since(t0).Nanoseconds())/float64(batches*k), sink%2)
		}
	}
}
