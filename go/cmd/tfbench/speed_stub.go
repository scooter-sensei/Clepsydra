//go:build accuracyonly

package main

// Built with -tags accuracyonly (verify/replay.sh, on commits whose API
// predates the speed benchmarks): no speed suite.
func speed() speeds { return speeds{} }
