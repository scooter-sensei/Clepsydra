//go:build !linux

package temporalfilter

import "testing"

// cycleCounter reports nothing off Linux (cycles_linux_test.go).
type cycleCounter struct{}

func startCycles() *cycleCounter { return &cycleCounter{} }

func (c *cycleCounter) report(b *testing.B) {}
