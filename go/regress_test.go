package temporalfilter

import (
	"testing"
	"time"
)

// Regression tests for fixed bugs. verify/replay.sh copies this file (and
// dup_test.go, reloc_race_test.go) into the commit before each fix to show
// the test fails there, so it uses only API those commits already had.

// Fixed in a01e3dc: a negative window wrapped to a huge tick count, so every
// key read as seen.
func TestRegressNegativeTicks(t *testing.T) {
	f := newTest(t, 10, time.Minute)
	if got := f.Ticks(-time.Second); got != 0 {
		t.Fatalf("Ticks(-1s) = %d, want 0", got)
	}
}

// Fixed in d1db914: CheckAndTouchBatch indexed out[len(hs)-1] and panicked
// on an empty batch.
func TestRegressEmptyBatch(t *testing.T) {
	f := newTest(t, 10, time.Minute)
	f.CheckAndTouchBatch(nil, 1, 1, nil)
}

// A tick past the 46 bits of a slot's last touch ran into its saturated,
// displaced and fingerprint bits: a caller passing Unix microseconds as
// the *At methods' clock got every key back unseen with no count (a rate
// limit failed open), and its slots were lost to lookups. Such a tick now
// panics.
func TestRegressTickPastRange(t *testing.T) {
	f := newTest(t, 1000, time.Minute)
	const h = 0x9e3779b97f4a7c15
	now := uint64(time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC).UnixMicro()) // ~1.8e15 > 2^46
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	f.CheckAndTouchAt(h, 60e6, now)
	seen := f.CheckAndTouchAt(h, 60e6, now+1)
	t.Fatalf("touches at Unix microseconds (%d) returned; the second read seen %v and the key then counts %d, want true and 2",
		now, seen, f.VelocityCountAt(h, 60e6, now+2))
}
