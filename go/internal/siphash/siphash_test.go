package siphash

import (
	"encoding/binary"
	"testing"
)

func TestSipHashVectors(t *testing.T) {
	var key [16]byte
	for i := range key {
		key[i] = byte(i)
	}
	k := NewKey(&key)
	msg := make([]byte, 15)
	for i := range msg {
		msg[i] = byte(i)
	}
	// Reference outputs from the SipHash paper and its test vectors.
	for _, c := range []struct {
		n    int
		want uint64
	}{{0, 0x726fdb47dd0e0e31}, {15, 0xa129ca6149be45e5}} {
		if got := k.HashString(string(msg[:c.n])); got != c.want {
			t.Errorf("siphash(%d bytes) = %#x, want %#x", c.n, got, c.want)
		}
	}
	var b [8]byte
	for _, x := range []uint64{0, 1, 0x0706050403020100, ^uint64(0)} {
		binary.LittleEndian.PutUint64(b[:], x)
		if k.HashUint64(x) != k.HashString(string(b[:])) {
			t.Errorf("uint64(%#x) differs from the byte path", x)
		}
	}
}
