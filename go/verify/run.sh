#!/usr/bin/env bash
# Checks every reproducible claim in this repository. Run from a clean
# checkout with full git history (verify/isolated.sh does this in a fresh,
# offline container). Results go to $OUT (default ./verify-report):
# summary.txt plus one log per check.
#
#   verify/run.sh           deterministic checks: they must match verify/expected/ byte for byte
#   verify/run.sh --speed   also the speed suite (host-dependent: reported, not checked)
#
# Deterministic results depend only on the code and the Go version: the
# workloads are seeded and single-threaded, and the integer-key hash is
# unkeyed. Expected files were produced on linux/amd64 with Go 1.24.7; other
# architectures may differ (Go may fuse floating-point multiply-adds there).
set -uo pipefail
cd "$(dirname "$0")/.."
. verify/lib.sh
. verify/checks.sh

env_info | tee "$OUT/env.txt" | sed 's/^/      /' | tee -a "$SUM" >/dev/null
check vet - go vet ./...
check tests - go test -count=1 ./...
check race - go test -race -short -count=1 ./...
check constant-time - env TF_CT=1 go test -count=1 .
check exact verify/expected/exact.txt go run ./cmd/tfbench -exact
check placement verify/expected/placement.txt placement
check register-experiment verify/expected/register-experiment.txt register_experiment
check velocity-bound verify/expected/velocity-bound.txt velocity_bound
check attacks verify/expected/attacks.txt attacks
check rates verify/expected/rates.txt rates
check sketch-experiments verify/expected/sketch-experiments.txt sketch_experiments
check flood-sizing verify/expected/flood-sizing.txt flood_sizing
check fixes - verify/replay.sh fixes
check stress - verify/replay.sh stress 300
check history - verify/replay.sh history
check harness verify/expected/harness.txt verify/replay.sh harness
check experiments verify/expected/experiments.txt verify/experiments.sh
if [ "${1:-}" = --speed ]; then
	check speed - speed
	check timing - timing
	check ct-pads - ct_pads
fi
if [ $failed = 0 ]; then say "ALL PASS"; else say "SOME CHECKS FAILED"; fi
exit $failed
