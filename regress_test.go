package temporalfilter

import (
	"testing"
	"time"
)

// Regression tests for fixed bugs. verify/replay.sh copies this file (and
// dup_test.go, reloc_race_test.go) into the commit before each fix to show
// the test fails there, so it uses only API those commits already had.

// Fixed in f9c9127: a negative window wrapped to a huge tick count, so every
// key read as seen.
func TestRegressNegativeTicks(t *testing.T) {
	f := newTest(t, 10, time.Minute)
	if got := f.Ticks(-time.Second); got != 0 {
		t.Fatalf("Ticks(-1s) = %d, want 0", got)
	}
}

// Fixed in f07b04e: CheckAndTouchBatch indexed out[len(hs)-1] and panicked
// on an empty batch.
func TestRegressEmptyBatch(t *testing.T) {
	f := newTest(t, 10, time.Minute)
	f.CheckAndTouchBatch(nil, 1, 1, nil)
}
