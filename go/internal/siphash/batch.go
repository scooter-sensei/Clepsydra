package siphash

// HashUint64s sets out[i] = k.HashUint64(keys[i]) for every i (out must be
// at least len(keys)): 8 keys at once with AVX-512F where the CPU and OS
// have it (hash8x), the rest through HashUint64.
func (k Key) HashUint64s(keys, out []uint64) {
	out = out[:len(keys)]
	n := 0
	if useAVX512 {
		if n = len(keys) &^ 7; n > 0 {
			hash8x(k.k0, k.k1, &keys[0], &out[0], n)
		}
	}
	for i := n; i < len(keys); i++ {
		out[i] = k.HashUint64(keys[i])
	}
}
