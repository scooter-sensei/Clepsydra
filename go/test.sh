#!/usr/bin/env bash
# Test tiers for the Go implementation. docs/specification/conformance.md defines them for
# every language.
#
#   ./test.sh smoke        build, vet, format, one call of every operation (~2 s)
#   ./test.sh sanity       smoke + the unit suite (short) + ConstantTime and Background modes + realign's hold
#                          on amd64 (TF_NOCAS2=1) + conformance vectors (~40 s)
#   ./test.sh regression   bug-fix tests and every deterministic result against verify/expected/ (~1 min)
#                          --full also replays fixes, history and old harnesses (verify/run.sh, ~25 min)
#   ./test.sh perf [A]     benchmarks of A (default: HEAD, or HEAD~1 on a clean tree) vs the
#                          working tree, alternated on this host; fails if one is > PERF_MAX (1.20) x slower
#   ./test.sh stress       race detector on the full suite, racy regression tests 300x,
#                          concurrent soak with the Upper-bound check (TF_STRESS, default 60s), plain, -race and Background,
#                          and Background with tags shared 8 ways (TF_STRESS_TAGS=8), the Upper-contract fuzzer over random
#                          configurations (TF_FUZZ, default 1e9 operations), and the concurrent fuzzer
#                          (TF_CFUZZ, default 5m), plain and with tags shared 8 ways (TF_CFUZZ_TAGS=8)
#   ./test.sh all          smoke, sanity, regression, stress (perf is host-dependent: run it on purpose)
#
# Reports go to $OUT (default ./verify-report/<tier>).
set -uo pipefail
cd "$(dirname "$0")"
. verify/lib/lib.sh
tier=${1:-}
[ -n "$tier" ] || { sed -n '2,18p' "$0"; exit 2; }
shift
OUT=${OUT:-verify-report/$tier}
. verify/lib/checks.sh
env_info >"$OUT/env.txt"

fmt_check() { # gofmt lists nothing
	local f
	f=$(gofmt -l .)
	[ -z "$f" ] || { echo "not gofmt-ed: $f"; return 1; }
}

smoke() {
	check build - go build ./...
	check vet - go vet ./...
	check gofmt - fmt_check
	check smoke - go test -count=1 -run '^(TestSmoke|TestSipHashVectors)$' . ./internal/siphash
}

sanity() {
	smoke
	check unit - go test -short -count=1 ./...
	check constant-time - env TF_CT=1 go test -short -count=1 .
	check background - env TF_BG=1 go test -short -count=1 .
	check no-cas2 - env TF_NOCAS2=1 go test -short -count=1 .
	check conformance - go test -count=1 -run '^TestConformance$' -v .
}

regression() {
	check conformance - go test -count=1 -run '^TestConformance$' .
	check regress - go test -count=1 -run '^TestRegress' -v .
	check exact verify/expected/exact.txt go run ./cmd/tfbench -exact
	check placement verify/expected/placement.txt placement
	check register-experiment verify/expected/register-experiment.txt register_experiment
	check velocity-bound verify/expected/velocity-bound.txt velocity_bound
	check attacks verify/expected/attacks.txt attacks
	check rates verify/expected/rates.txt rates
	check sketch-experiments verify/expected/sketch-experiments.txt sketch_experiments
	check flood-sizing verify/expected/flood-sizing.txt flood_sizing
	if [ "${1:-}" = --full ]; then
		check fixes - verify/replay.sh fixes
		check history - verify/replay.sh history
		check harness verify/expected/harness.txt verify/replay.sh harness
		check experiments verify/expected/experiments.txt verify/experiments.sh
		check h2h-ecm verify/expected/h2h-ecm.txt go run ./cmd/h2h ecm
	fi
}

# The benchmarks that run in seconds and cover every path; Huge* (600 MB
# tables) only on request: PERF_BENCH='Huge' ./test.sh perf.
PERF_BENCH=${PERF_BENCH:-'^Benchmark(CheckAndTouchHit|CheckAndTouchHitHot|CheckAndTouchMissInsert|TouchAndCount|VelocityCount|VelocityCountHot|ContainsAbsent|InsertSteady|InsertNearFull|InsertSaturated|CheckAndTouchParallel|CheckAndTouchBatch)$'}

perf_gate() { # perf_gate <A>: fail when both the min and the median of B run over PERF_MAX x A's.
	# A/A runs of one build differ by up to ~15% here, so the gate catches only clear regressions;
	# measure smaller effects with verify/ab.sh and more rounds.
	verify/ab.sh "$1" . "$PERF_BENCH" "${PERF_ROUNDS:-6}" | tee /dev/stderr |
		awk -v max="${PERF_MAX:-1.20}" 'NR > 3 && $NF + 0 > max && $4 / $2 > max { print "slower than " max "x (min and median): " $1; bad = 1 } END { exit bad }'
}

perf() {
	local a=${1:-}
	if [ -z "$a" ]; then
		if [ -z "$(git status --porcelain --untracked-files=no -- .)" ]; then a=HEAD~1; else a=HEAD; fi
	fi
	say "perf: $a vs working tree (B/A > ${PERF_MAX:-1.20} fails; speed is host-dependent)"
	check perf - perf_gate "$a"
}

stress() {
	check race-full - go test -race -count=1 ./...
	check racy-regressions - verify/replay.sh stress "${STRESS_RUNS:-300}"
	check soak - env TF_STRESS="${TF_STRESS:-60s}" go test -count=1 -run '^TestStressSoak$' -v -timeout 0 .
	check soak-race - env TF_STRESS="${TF_STRESS_RACE:-20s}" go test -race -count=1 -run '^TestStressSoak$' -v -timeout 0 .
	check soak-background - env TF_BG=1 TF_STRESS="${TF_STRESS:-60s}" go test -count=1 -run '^TestStressSoak$' -v -timeout 0 .
	check soak-background-tags - env TF_BG=1 TF_STRESS="${TF_STRESS:-60s}" TF_STRESS_TAGS=8 go test -count=1 -run '^TestStressSoak$' -v -timeout 0 .
	check fuzz - env TF_FUZZ="${TF_FUZZ:-1e9}" go test -count=1 -run '^TestFuzzUpper$' -v -timeout 0 .
	check fuzz-concurrent - env TF_CFUZZ="${TF_CFUZZ:-5m}" go test -count=1 -run '^TestFuzzConcurrent$' -v -timeout 0 .
	check fuzz-concurrent-tags - env TF_CFUZZ="${TF_CFUZZ:-5m}" TF_CFUZZ_TAGS=8 go test -count=1 -run '^TestFuzzConcurrent$' -v -timeout 0 .
}

case $tier in
smoke | sanity | regression | perf | stress) "$tier" "$@" ;;
all)
	sanity
	regression
	stress
	;;
*) sed -n '2,18p' "$0"; exit 2 ;;
esac
if [ $failed = 0 ]; then say "ALL PASS ($tier)"; else say "SOME CHECKS FAILED ($tier; logs in $OUT)"; fi
exit $failed
