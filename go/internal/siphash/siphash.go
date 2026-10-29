// Package siphash is SipHash-2-4 (Aumasson & Bernstein), the keyed PRF
// behind the filter's Config.Key.
package siphash

import (
	"encoding/binary"
	"math/bits"
	"unsafe"
)

// Key is a 128-bit SipHash key.
type Key struct{ k0, k1 uint64 }

// NewKey reads k as two little-endian words.
func NewKey(k *[16]byte) Key {
	return Key{binary.LittleEndian.Uint64(k[:8]), binary.LittleEndian.Uint64(k[8:])}
}

func sipRound(v0, v1, v2, v3 uint64) (uint64, uint64, uint64, uint64) {
	v0 += v1
	v1 = bits.RotateLeft64(v1, 13) ^ v0
	v0 = bits.RotateLeft64(v0, 32)
	v2 += v3
	v3 = bits.RotateLeft64(v3, 16) ^ v2
	v0 += v3
	v3 = bits.RotateLeft64(v3, 21) ^ v0
	v2 += v1
	v1 = bits.RotateLeft64(v1, 17) ^ v2
	v2 = bits.RotateLeft64(v2, 32)
	return v0, v1, v2, v3
}

func (k Key) init() (uint64, uint64, uint64, uint64) {
	return k.k0 ^ 0x736f6d6570736575, k.k1 ^ 0x646f72616e646f6d, k.k0 ^ 0x6c7967656e657261, k.k1 ^ 0x7465646279746573
}

func sipFinish(v0, v1, v2, v3, last uint64) uint64 {
	v3 ^= last
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0 ^= last
	v2 ^= 0xff
	for range 4 {
		v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	}
	return v0 ^ v1 ^ v2 ^ v3
}

// HashString hashes s without copying it.
func (k Key) HashString(s string) uint64 {
	b := unsafe.Slice(unsafe.StringData(s), len(s))
	v0, v1, v2, v3 := k.init()
	n := len(b)
	for ; len(b) >= 8; b = b[8:] {
		m := binary.LittleEndian.Uint64(b)
		v3 ^= m
		v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
		v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
		v0 ^= m
	}
	last := uint64(n) << 56
	for i, c := range b {
		last |= uint64(c) << (8 * i)
	}
	return sipFinish(v0, v1, v2, v3, last)
}

// HashUint64 hashes x's 8 little-endian bytes.
func (k Key) HashUint64(x uint64) uint64 {
	v0, v1, v2, v3 := k.init()
	v3 ^= x
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0 ^= x
	return sipFinish(v0, v1, v2, v3, 8<<56)
}
