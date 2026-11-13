#!/usr/bin/env bash
# Test tiers for the C++ port. docs/specification/conformance.md defines them
# for every language; go/test.sh is the reference's.
#
#   ./test.sh smoke        configure, build with warnings as errors, every public operation once
#                          (TestSmoke, TestSmokeOptional), SipHash reference vectors
#   ./test.sh sanity       smoke + the unit suite (-short) + ConstantTime (TF_CT=1) and Background (TF_BG=1)
#                          modes + realign's hold instead of its 16-byte CAS on x86-64 (TF_NOCAS2=1) +
#                          conformance vectors
#   ./test.sh regression   conformance vectors, bug-fix tests (TestRegress*), and every deterministic result
#                          against the Go reference's go/verify/expected/ byte for byte (tfbench -exact, the
#                          placement oracle, the register, velocity, attack, rate, sketch and flood experiments,
#                          the answer digest)
#   ./test.sh perf [A]     the twelve benchmarks of go/test.sh's PERF_BENCH (bench/bench.cpp), A (default: HEAD,
#                          or HEAD~1 on a clean tree; `go`: the Go reference) vs the working tree, alternated
#                          on this host (bench/ab.sh); fails if one is > PERF_MAX (1.20) x slower
#   ./test.sh stress       ThreadSanitizer on the whole suite, plain and in each mode; the racy regression
#                          tests 300x (STRESS_RUNS); the concurrent soak (TF_STRESS, default 60s) plain, under
#                          TSan (TF_STRESS_RACE, 20s), in Background mode and with tags shared 8 ways; the Upper
#                          fuzzer (TF_FUZZ, 1e9 operations); the concurrent fuzzer (TF_CFUZZ, 5m), plain and
#                          with tags shared 8 ways
#   ./test.sh all          smoke, sanity, regression, stress (perf is host-dependent: run it on purpose)
#
# Environment: CXX (the compiler; default: CMake's choice), CMAKE (default cmake), GENERATOR (default
# Ninja if ninja is on PATH), BUILD (build directory root, default ./build), OUT (reports, default
# ./verify-report/<tier>). TSan needs ASLR off on kernels with high mmap entropy: the stress tier runs
# its binaries under `setarch -R` when setarch exists.
set -uo pipefail
cd "$(dirname "$0")"
tier=${1:-}
[ -n "$tier" ] || { sed -n '2,27p' "$0"; exit 2; }
shift

OUT=${OUT:-verify-report/$tier}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
BUILD=${BUILD:-build}
mkdir -p "$BUILD"
BUILD=$(cd "$BUILD" && pwd)
CMAKE=${CMAKE:-cmake}
if [ -z "${GENERATOR:-}" ] && command -v ninja >/dev/null; then GENERATOR=Ninja; fi
SUM="$OUT/summary.txt"
: >"$SUM"
failed=0

say() { printf '%s\n' "$*" | tee -a "$SUM"; }

# check <name> <command...>: PASS if the command exits 0; its output goes to $OUT/<name>.txt.
check() {
	local name=$1
	shift
	local log="$OUT/$name.txt" t0=$SECONDS
	"$@" >"$log" 2>&1
	local rc=$?
	local secs=$((SECONDS - t0))
	if [ $rc -ne 0 ]; then
		say "FAIL  $name (exit $rc, ${secs}s; see $name.txt)"
		failed=1
	else
		say "PASS  $name (${secs}s)"
	fi
}

env_info() {
	echo "commit:   $(git rev-parse HEAD 2>/dev/null)$([ -z "$(git status --porcelain --untracked-files=no -- . 2>/dev/null)" ] || echo ' (uncommitted changes)')"
	echo "cmake:    $("$CMAKE" --version | head -1)"
	echo "compiler: $("${CXX:-c++}" --version 2>/dev/null | head -1)"
	echo "kernel:   $(uname -srm)"
	echo "cpu:      $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //') x $(nproc 2>/dev/null)"
}
env_info >"$OUT/env.txt"

# build <variant> <cmake options...>: configure and build one variant in $BUILD/<variant>.
build() {
	local dir="$BUILD/$1"
	shift
	local gen=()
	[ -n "${GENERATOR:-}" ] && gen=(-G "$GENERATOR")
	local cxx=()
	[ -n "${CXX:-}" ] && cxx=(-DCMAKE_CXX_COMPILER="$CXX")
	"$CMAKE" -S . -B "$dir" "${gen[@]}" "${cxx[@]}" "$@" && "$CMAKE" --build "$dir" --parallel
}

# run <variant> <env...> -- <test args...>
run() {
	local dir="$BUILD/$1"
	shift
	local envs=()
	while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
	shift
	local pre=()
	if [ "${SETARCH:-0}" = 1 ] && command -v setarch >/dev/null; then pre=(setarch "$(uname -m)" -R); fi
	env "${envs[@]}" "${pre[@]}" "$dir/clepsydra_test" "$@"
}

x86=0
case "$(uname -m)" in x86_64 | amd64) x86=1 ;; esac

smoke() {
	check build build rel -DCMAKE_BUILD_TYPE=RelWithDebInfo -DCLEPSYDRA_WERROR=ON
	check smoke run rel -- -run '^(TestSmoke|TestSmokeOptional|TestSipHashVectors)$' -v
}

sanity() {
	smoke
	check unit run rel -- -short -v
	check constant-time run rel TF_CT=1 -- -short -v
	check background run rel TF_BG=1 -- -short -v
	if [ $x86 = 1 ]; then check no-cas2 run rel TF_NOCAS2=1 -- -short -v; fi
	check conformance run rel -- -run '^TestConformance$' -v
}

# expect <name> <command...>: PASS if the command exits 0 and its output equals the Go reference's
# ../go/verify/expected/<name>.txt byte for byte (go/verify/lib/checks.sh's check with an expected file).
EXPECTED=$(cd ../go/verify/expected 2>/dev/null && pwd)
expect() {
	local name=$1
	shift
	local log="$OUT/$name.txt" t0=$SECONDS
	"$@" >"$log" 2>"$OUT/$name.stderr"
	local rc=$?
	local secs=$((SECONDS - t0))
	if [ $rc -ne 0 ]; then
		say "FAIL  $name (exit $rc, ${secs}s; see $name.txt, $name.stderr)"
		failed=1
	elif ! diff -u "$EXPECTED/$name.txt" "$log" >"$OUT/$name.diff"; then
		say "FAIL  $name (differs from go/verify/expected/$name.txt, ${secs}s; see $name.diff)"
		failed=1
	else
		say "PASS  $name (${secs}s)"
		rm -f "$OUT/$name.diff"
	fi
}

# The experiments' outputs, extracted as go/verify/lib/checks.sh extracts the Go tests' (the runner's -v
# lines are the log indented by 4 spaces; t.Logf's file:line prefix has no counterpart here).
t_logs() { grep -vE '^(=== |--- |ok: |FAIL)' | sed -E 's/^    //'; }
t_table() { sed -n '/normalized MAE/,/^--- /p' | grep -v '^--- '; }
x_exact() { "$BUILD/rel/tfbench" -exact; }
x_placement() {
	run rel -- -run '^(TestPlacementLimit|TestOracleMatchesMaxFlow|TestLoadThreshold|TestComparedEntries)$' -v |
		grep -E '^    (placement|agreed on)' | sed -E 's/^    //'
}
x_register_experiment() { run rel -- -run '^TestRegisterExperiment$' -v | t_table | sed -E 's/^ +//'; }
x_velocity_bound() { run rel TF_EXP=1 -- -run '^TestVelocityBound$' -v | t_table | sed -E 's/^    //'; }
x_velocity_information() { run rel TF_EXP=1 -- -run '^TestVelocityInformation$' -v | t_table | sed -E 's/^    //'; }
x_attacks() {
	run rel -- -run '^(TestAttackForgedIdentity|TestAttackStripeFlood|TestAttackEvictionReset|TestAttackFloodCollateral|TestAttackHeavyHitter|TestUpperBoundRandom)$' -v | t_logs
}
x_rates() { run rel -- -run '^TestSustainedRate$' -v | t_logs; }
x_sketch_experiments() { run rel TF_EXP=1 -- -run '^(TestSketchShapes|TestSpillThreshold)$' -v | t_logs; }
x_flood_sizing() { run rel TF_EXP=1 -- -run '^TestFloodSizing$' -v | t_logs; }
x_answer_digest() { run rel TF_EXP=1 -- -run '^TestAnswerDigest$' -v | grep '^cell '; }

regression() {
	check build build rel -DCMAKE_BUILD_TYPE=RelWithDebInfo -DCLEPSYDRA_WERROR=ON
	check conformance run rel -- -run '^TestConformance$' -v
	check regress run rel -- -run '^TestRegress' -v
	expect exact x_exact
	expect placement x_placement
	expect register-experiment x_register_experiment
	expect velocity-bound x_velocity_bound
	expect velocity-information x_velocity_information
	expect attacks x_attacks
	expect rates x_rates
	expect sketch-experiments x_sketch_experiments
	expect flood-sizing x_flood_sizing
	expect answer-digest x_answer_digest
}

# racy_regressions [N]: the racy regression tests, N runs each (default 300), 4 at a time, each run its
# own process (go/verify/replay.sh stress): contention between runs makes most races show;
# TestAbandonedMoveKeepsCounts needs its eight workers truly parallel, so it runs alone. Any failure is
# a live bug.
racy_regressions() {
	local n=${1:-300} bad=0 re par d f r
	for re in '^TestConcurrentFirstInsertNoDuplicates$' '^TestTouchRacingRelocationKeepsCounts$' '^TestAbandonedMoveKeepsCounts$' '^TestStashConcurrentEvictions$'; do
		par=4
		[ "$re" = '^TestAbandonedMoveKeepsCounts$' ] && par=1
		d="$OUT/racy/${re//[^A-Za-z]/}"
		mkdir -p "$d"
		: >"$d/fails"
		seq "$n" | xargs -P "$par" -I{} sh -c 'env -u TF_CT -u TF_BG -u TF_NOCAS2 "$1" -run "$2" >"$3/run.$4.log" 2>&1 || echo "$4" >>"$3/fails"' _ "$BUILD/rel/clepsydra_test" "$re" "$d" {}
		f=$(wc -l <"$d/fails" | tr -d ' ')
		printf 'stress %-44s failed %s/%s\n' "$re" "$f" "$n"
		[ "$f" = 0 ] && continue
		bad=1
		for r in $(cat "$d/fails"); do
			grep -h -E '_test\.cpp:[0-9]+:|terminate|Assertion' "$d/run.$r.log" | head -3 | sed "s|^ *|    run $r: |"
		done
	done
	return $bad
}

# The twelve benchmarks of go/test.sh's PERF_BENCH (bench/bench.cpp ports them).
PERF_BENCH=${PERF_BENCH:-'^Benchmark(CheckAndTouchHit|CheckAndTouchHitHot|CheckAndTouchMissInsert|TouchAndCount|VelocityCount|VelocityCountHot|ContainsAbsent|InsertSteady|InsertNearFull|InsertSaturated|CheckAndTouchParallel|CheckAndTouchBatch)$'}

perf_gate() { # perf_gate <A>: fail when both the min and the median of B run over PERF_MAX x A's.
	CMAKE=$CMAKE GENERATOR=${GENERATOR:-} bench/ab.sh "$1" . "$PERF_BENCH" "${PERF_ROUNDS:-6}" | tee /dev/stderr |
		awk -v max="${PERF_MAX:-1.20}" 'NR > 3 && $NF + 0 > max && $4 / $2 > max { print "slower than " max "x (min and median): " $1; bad = 1 } END { exit bad }'
}

perf() {
	local a=${1:-}
	if [ -z "$a" ]; then
		if [ -z "$(git status --porcelain --untracked-files=all -- . 2>/dev/null)" ]; then a=HEAD~1; else a=HEAD; fi
	fi
	if [ "$a" != go ] && ! git cat-file -e "$a:cpp" 2>/dev/null; then
		say "perf: $a has no cpp/ to compare with (commit the port first, or name a revision: ./test.sh perf <rev>; ./test.sh perf go compares with the Go reference)"
		failed=1
		return
	fi
	say "perf: $a vs working tree (B/A > ${PERF_MAX:-1.20} fails; speed is host-dependent)"
	check perf perf_gate "$a"
}

stress() {
	check build build rel -DCMAKE_BUILD_TYPE=RelWithDebInfo -DCLEPSYDRA_WERROR=ON
	check build-tsan build tsan -DCMAKE_BUILD_TYPE=RelWithDebInfo -DCLEPSYDRA_TSAN=ON
	# The suite under the race detector, without the soak and the fuzzer (their own checks follow).
	local quiet=(TF_STRESS= TF_FUZZ= TF_FUZZ_CASE= TF_CFUZZ= TF_CFUZZ_CASE=)
	SETARCH=1 check race-full run tsan "${quiet[@]}" -- -v
	SETARCH=1 check race-constant-time run tsan "${quiet[@]}" TF_CT=1 -- -v
	SETARCH=1 check race-background run tsan "${quiet[@]}" TF_BG=1 -- -v
	if [ $x86 = 1 ]; then SETARCH=1 check race-no-cas2 run tsan "${quiet[@]}" TF_NOCAS2=1 -- -v; fi
	check racy-regressions racy_regressions "${STRESS_RUNS:-300}"
	local soak=(-run '^TestStressSoak$' -v)
	check soak run rel TF_STRESS="${TF_STRESS:-60s}" -- "${soak[@]}"
	SETARCH=1 check soak-race run tsan TF_STRESS="${TF_STRESS_RACE:-20s}" -- "${soak[@]}"
	check soak-background run rel TF_BG=1 TF_STRESS="${TF_STRESS:-60s}" -- "${soak[@]}"
	check soak-background-tags run rel TF_BG=1 TF_STRESS="${TF_STRESS:-60s}" TF_STRESS_TAGS=8 -- "${soak[@]}"
	check fuzz run rel TF_FUZZ="${TF_FUZZ:-1e9}" -- -run '^TestFuzzUpper$' -v
	check fuzz-concurrent run rel TF_CFUZZ="${TF_CFUZZ:-5m}" -- -run '^TestFuzzConcurrent$' -v
	check fuzz-concurrent-tags run rel TF_CFUZZ="${TF_CFUZZ:-5m}" TF_CFUZZ_TAGS=8 -- -run '^TestFuzzConcurrent$' -v
}

case $tier in
smoke | sanity | regression | perf | stress) "$tier" "$@" ;;
all)
	sanity
	regression
	stress
	;;
*) sed -n '2,27p' "$0"; exit 2 ;;
esac
if [ $failed = 0 ]; then say "ALL PASS ($tier)"; else say "SOME CHECKS FAILED ($tier; logs in $OUT)"; fi
exit $failed
