package temporalfilter

import (
	"testing"
	"time"
)

// TestHashUint64sMatchesFilter: Filter.HashUint64s equals HashUint64 with
// and without Config.Key, on every batch length 0-130.
func TestHashUint64sMatchesFilter(t *testing.T) {
	key := [16]byte{9, 8, 7}
	for _, cfg := range []Config{{Capacity: 10, Horizon: time.Minute}, {Capacity: 10, Horizon: time.Minute, Key: &key}} {
		f, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		keys, out := make([]uint64, 130), make([]uint64, 130)
		for n := range 131 {
			for i := range keys[:n] {
				keys[i] = mix64(uint64(n<<8 + i))
			}
			f.HashUint64s(keys[:n], out)
			for i := range n {
				if want := f.HashUint64(keys[i]); out[i] != want {
					t.Fatalf("keyed %v, batch of %d, key %d: %#x, want %#x", cfg.Key != nil, n, i, out[i], want)
				}
			}
		}
	}
}
