package main

import (
	"math"
	"math/bits"
	"time"
)

// Latency is timed per operation in TSC ticks (RDTSCP, tsc_amd64.s; the
// Rust runner uses the __rdtscp intrinsic) and binned in a log-linear
// histogram: values below 128 exactly, above that 64 bins per power of two
// (bins at most 1.6% wide). bench/h2h/src/main.rs has the same code.

const (
	subBits = 6
	sub     = 1 << subBits
	nBins   = 2*sub + (63-subBits)*sub
)

type hist [nBins]uint64

func bin(v uint64) int {
	if v < 2*sub {
		return int(v)
	}
	e := 63 - bits.LeadingZeros64(v) // >= subBits+1
	return 2*sub + (e-subBits-1)*sub + int(v>>(e-subBits)) - sub
}

// top is the largest value in bin b.
func top(b int) uint64 {
	if b < 2*sub {
		return uint64(b)
	}
	e := (b-2*sub)/sub + subBits + 1
	m := uint64((b-2*sub)%sub + sub)
	return (m+1)<<(e-subBits) - 1
}

func (h *hist) add(v uint64) { h[bin(v)]++ }

func (h *hist) merge(o *hist) {
	for i := range h {
		h[i] += o[i]
	}
}

// quantile is the top of the bin holding the q-th value, in ns.
func (h *hist) quantile(q, ticksPerNs float64) float64 {
	var n uint64
	for _, c := range h {
		n += c
	}
	rank := uint64(math.Ceil(q * float64(n)))
	rank = max(rank, 1)
	var cum uint64
	for b, c := range h {
		cum += c
		if cum >= rank {
			return float64(top(b)) / ticksPerNs
		}
	}
	return math.NaN()
}

// tscPerNs calibrates the TSC against the monotonic clock over 200 ms.
func tscPerNs() float64 {
	t0, c0 := time.Now(), rdtscp()
	for time.Since(t0) < 200*time.Millisecond {
	}
	c1, t1 := rdtscp(), time.Now()
	return float64(c1-c0) / float64(t1.Sub(t0).Nanoseconds())
}

// timerFloor times an empty region n times: the timer's own cost.
func timerFloor(n int) *hist {
	h := new(hist)
	for range n {
		t0 := rdtscp()
		t1 := rdtscp()
		h.add(t1 - t0)
	}
	return h
}
