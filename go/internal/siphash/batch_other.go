//go:build !amd64

package siphash

var useAVX512 = false

func hash8x(k0, k1 uint64, keys, out *uint64, n int) { panic("siphash: hash8x without AVX-512") }
