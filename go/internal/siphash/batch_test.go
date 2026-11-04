package siphash

import (
	"encoding/binary"
	"testing"
)

// TestHashUint64sMatches: HashUint64s equals HashUint64 on every batch
// length 0-130 and on 1M keys in batches of random lengths, under random
// keys, through hash8x (where the CPU has AVX-512) and with it off.
func TestHashUint64sMatches(t *testing.T) {
	t.Logf("AVX-512F: %v", useAVX512)
	defer func(u bool) { useAVX512 = u }(useAVX512)
	x := uint64(0x13198a2e03707344)
	next := func() uint64 { x ^= x << 13; x ^= x >> 7; x ^= x << 17; return x }
	keys, out := make([]uint64, 130), make([]uint64, 130)
	for _, avx := range []bool{useAVX512, false} {
		useAVX512 = avx
		total := 0
		for c := 0; total < 1_000_000; c++ {
			var kb [16]byte
			binary.LittleEndian.PutUint64(kb[:8], next())
			binary.LittleEndian.PutUint64(kb[8:], next())
			k := NewKey(&kb)
			n := c % 131 // every length 0-130 first, then the same again
			if c > 131 {
				n = int(next() % 131)
			}
			for i := range keys[:n] {
				keys[i] = next()
			}
			for i := range out {
				out[i] = 0xdeadbeef
			}
			k.HashUint64s(keys[:n], out)
			for i := range n {
				if want := k.HashUint64(keys[i]); out[i] != want {
					t.Fatalf("avx %v, batch of %d, key %d (%#x): %#x, want %#x", avx, n, i, keys[i], out[i], want)
				}
			}
			for i := n; i < len(out); i++ {
				if out[i] != 0xdeadbeef {
					t.Fatalf("avx %v, batch of %d: out[%d] written", avx, n, i)
				}
			}
			total += n
		}
	}
}
