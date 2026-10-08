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
cd "$(git rev-parse --show-toplevel)"
OUT=${OUT:-verify-report}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
export WORK=${WORK:-$(mktemp -d)}
SUM="$OUT/summary.txt"
: >"$SUM"
failed=0

say() { printf '%s\n' "$*" | tee -a "$SUM"; }

# check <name> <expected file or -> <command...>: PASS if the command exits 0
# and, when an expected file is given, its output matches it exactly.
check() {
	local name=$1 want=$2
	shift 2
	local log="$OUT/$name.txt" t0=$SECONDS
	"$@" >"$log" 2>"$OUT/$name.stderr"
	local rc=$?
	local secs=$((SECONDS - t0))
	if [ $rc -ne 0 ]; then
		say "FAIL  $name (exit $rc, ${secs}s; see $name.txt, $name.stderr)"
		failed=1
	elif [ "$want" != - ] && ! diff -u "$want" "$log" >"$OUT/$name.diff"; then
		say "FAIL  $name (differs from $want, ${secs}s; see $name.diff)"
		failed=1
	else
		say "PASS  $name (${secs}s)"
		rm -f "$OUT/$name.diff"
	fi
}

env_info() {
	echo "commit:   $(git rev-parse HEAD)$([ -z "$(git status --porcelain --untracked-files=no)" ] || echo ' (uncommitted changes)')"
	echo "go:       $(go version)"
	echo "GOFLAGS=$(go env GOFLAGS) GOPROXY=$(go env GOPROXY) GOTOOLCHAIN=$(go env GOTOOLCHAIN) GOCACHE=$(go env GOCACHE)"
	echo "kernel:   $(uname -srm)"
	echo "cpu:      $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ //') x $(nproc), L3 $(cat /sys/devices/system/cpu/cpu0/cache/index3/size 2>/dev/null || echo '?')"
	if timeout 3 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null; then
		echo "network:  reachable"
	else
		echo "network:  unreachable"
	fi
}

placement() {
	go test -count=1 -run '^(TestPlacementLimit|TestOracleMatchesMaxFlow|TestLoadThreshold|TestComparedEntries)$' -v . |
		grep -E '^ *[a-z_]+\.go:[0-9]+: (placement|agreed on)' | sed -E 's/^ *[a-z_]+\.go:[0-9]+: //'
}

register_experiment() {
	go test -count=1 -run '^TestRegisterExperiment$' -v . | sed -n '/normalized MAE/,/^--- /p' | grep -v '^--- ' | sed -E 's/^ *[a-z_]+\.go:[0-9]+: *//; s/^ +//'
}

speed() {
	for i in 1 2 3; do go run ./cmd/tfbench 2>/dev/null | tail -1; done
}

env_info | tee "$OUT/env.txt" | sed 's/^/      /' | tee -a "$SUM" >/dev/null
check vet - go vet ./...
check tests - go test -count=1 ./...
check race - go test -race -short -count=1 ./...
check exact verify/expected/exact.txt go run ./cmd/tfbench -exact
check placement verify/expected/placement.txt placement
check register-experiment verify/expected/register-experiment.txt register_experiment
check fixes - verify/replay.sh fixes
check stress - verify/replay.sh stress 300
check history - verify/replay.sh history
check harness verify/expected/harness.txt verify/replay.sh harness
check experiments verify/expected/experiments.txt verify/experiments.sh
if [ "${1:-}" = --speed ]; then
	check speed - speed
fi
if [ $failed = 0 ]; then say "ALL PASS"; else say "SOME CHECKS FAILED"; fi
exit $failed
