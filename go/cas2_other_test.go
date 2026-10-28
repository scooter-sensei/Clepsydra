//go:build !amd64

package temporalfilter

import "testing"

// realignModes runs fn with realign's hold (no 16-byte CAS here).
func realignModes(t *testing.T, fn func(t *testing.T)) { t.Run("hold", fn) }
