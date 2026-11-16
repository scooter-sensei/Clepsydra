#!/usr/bin/env bash
# Test tiers for the Rust port. docs/specification/conformance.md defines them
# for every language; go/test.sh is the reference's, cpp/test.sh the C++ port's.
#
#   ./test.sh smoke        build every target, clippy with warnings as errors (the Go tier's vet), rustfmt, the
#                          doctest, every public operation once (TestSmoke, TestSmokeOptional), SipHash vectors
#   ./test.sh sanity       smoke + the unit suite (-short) + ConstantTime (TF_CT=1) and Background (TF_BG=1)
#                          modes + realign's hold instead of its 16-byte CAS on x86-64 (TF_NOCAS2=1) +
#                          conformance vectors
#   ./test.sh regression   conformance vectors, bug-fix tests (TestRegress*), and every deterministic result
#                          against the Go reference's go/verify/expected/ byte for byte (tfbench -exact, the
#                          placement oracle, the register, velocity, attack, rate, sketch and flood experiments,
#                          the answer digest)
#   ./test.sh perf [A]     the twelve benchmarks of go/test.sh's PERF_BENCH (bench/bench.rs), A (default: HEAD,
#                          or HEAD~1 on a clean tree; `go`: the Go reference) vs the working tree, alternated
#                          on this host (bench/ab.sh); fails if one is > PERF_MAX (1.20) x slower
#   ./test.sh stress       ThreadSanitizer on the whole suite, plain and in each mode (needs a nightly toolchain
#                          with rust-src; skipped, and said so, without one); the racy regression tests 300x
#                          (STRESS_RUNS); the concurrent soak (TF_STRESS, default 60s) plain, under TSan
#                          (TF_STRESS_RACE, 20s), in Background mode and with tags shared 8 ways; the Upper
#                          fuzzer (TF_FUZZ, 1e9 operations); the concurrent fuzzer (TF_CFUZZ, 5m), plain and
#                          with tags shared 8 ways
#   ./test.sh all          smoke, sanity, regression, stress (perf is host-dependent: run it on purpose)
#
# Environment: CARGO (default cargo; ./cg.sh on a Windows drive under WSL), NIGHTLY (the toolchain for
# TSan, default nightly), OUT (reports, default ./verify-report/<tier>). The suite's tests share
# process-wide hooks, so they run one at a time (RUST_TEST_THREADS=1, set here as .cargo/config.toml sets
# it for cargo test).
set -uo pipefail
cd "$(dirname "$0")"
tier=${1:-}
[ -n "$tier" ] || { sed -n '2,28p' "$0"; exit 2; }
shift

OUT=${OUT:-verify-report/$tier}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
CARGO=${CARGO:-cargo}
NIGHTLY=${NIGHTLY:-nightly}
export RUST_TEST_THREADS=1
SUM="$OUT/summary.txt"
: >"$SUM"
failed=0
skipped=()

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

skip() { # skip <name> <why>
	say "SKIP  $1 ($2)"
	skipped+=("$1")
}

env_info() {
	echo "commit:   $(git rev-parse HEAD 2>/dev/null)$([ -z "$(git status --porcelain --untracked-files=all -- . 2>/dev/null)" ] || echo ' (uncommitted changes)')"
	echo "cargo:    $($CARGO --version 2>/dev/null)"
	echo "rustc:    $(rustc --version 2>/dev/null)"
	echo "kernel:   $(uname -srm)"
	echo "cpu:      $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //') x $(nproc 2>/dev/null)"
}
env_info >"$OUT/env.txt"

# executable <cargo args...>: builds and prints the path of the one executable the build produces.
executable() {
	"$CARGO" "$@" --message-format=json-render-diagnostics | grep -o '"executable":"[^"]*"' | tail -1 | cut -d'"' -f4
}

BIN= TFBENCH=
build_suite() {
	"$CARGO" build --all-targets &&
		BIN=$(executable test --lib --no-run) && [ -x "$BIN" ] &&
		TFBENCH=$(executable build --release --bin tfbench) && [ -x "$TFBENCH" ]
}

# names <regex>: the suite's tests whose Go name (the last path segment) matches, as libtest's paths.
names() { "$BIN" --list --format=terse 2>/dev/null | sed -n 's/: test$//p' | awk -F'::' -v re="$1" '$NF ~ re'; }

# run <env...> -- [-run REGEX] [-v]: the suite (or its tests matching REGEX), one test at a time, as go test.
run() {
	local envs=() re= v=()
	while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
	shift
	while [ $# -gt 0 ]; do
		case $1 in
		-run) re=$2; shift 2 ;;
		-v) v=(--nocapture); shift ;;
		*) echo "run: unknown flag $1" >&2; return 2 ;;
		esac
	done
	local sel=()
	if [ -n "$re" ]; then
		mapfile -t sel < <(names "$re")
		[ ${#sel[@]} -gt 0 ] || { echo "run: no test matches $re" >&2; return 1; }
		sel=(--exact "${sel[@]}")
	fi
	env "${envs[@]}" "$BIN" --test-threads=1 "${v[@]}" "${sel[@]}"
}

# gov <env...> -- Name...: the named tests in the order given, each in its own process, printed as
# `go test -v` prints them (the suite's TF_LOG: `=== RUN`, the log indented by four spaces, `--- PASS`).
gov() {
	local envs=()
	while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
	shift
	local log rc=0 n p
	log=$(mktemp)
	for n in "$@"; do
		p=$(names "^$n\$")
		[ -n "$p" ] || { echo "gov: no test $n" >&2; rc=1; continue; }
		env "${envs[@]}" TF_LOG="$log" "$BIN" --test-threads=1 --exact "$p" >/dev/null 2>"$log.err" || {
			rc=1
			cat "$log.err" >&2
		}
	done
	cat "$log"
	rm -f "$log" "$log.err"
	return $rc
}

x86=0
case "$(uname -m)" in x86_64 | amd64) x86=1 ;; esac

smoke() {
	check build build_suite
	check clippy "$CARGO" clippy --all-targets -- -D warnings
	check rustfmt "$CARGO" fmt --check
	check doctest "$CARGO" test --doc
	[ -x "$BIN" ] || return
	check smoke run -- -run '^(TestSmoke|TestSmokeOptional|TestSipHashVectors)$' -v
}

sanity() {
	smoke
	[ -x "$BIN" ] || return
	check unit run TF_SHORT=1 -- -v
	check constant-time run TF_SHORT=1 TF_CT=1 -- -v
	check background run TF_SHORT=1 TF_BG=1 -- -v
	if [ $x86 = 1 ]; then check no-cas2 run TF_SHORT=1 TF_NOCAS2=1 -- -v; fi
	check conformance run -- -run '^TestConformance$' -v
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
		[ -s "$OUT/$name.stderr" ] || rm -f "$OUT/$name.stderr"
	fi
}

# The experiments' outputs, extracted as go/verify/lib/checks.sh extracts the Go tests' (the -v lines are
# the log indented by 4 spaces; t.Logf's file:line prefix has no counterpart here).
t_logs() { grep -vE '^(=== |--- |ok: |FAIL)' | sed -E 's/^    //'; }
t_table() { sed -n '/normalized MAE/,/^--- /p' | grep -v '^--- '; }
x_exact() { "$TFBENCH" -exact; }
x_placement() {
	gov -- TestPlacementLimit TestOracleMatchesMaxFlow TestLoadThreshold TestComparedEntries |
		grep -E '^    (placement|agreed on)' | sed -E 's/^    //'
}
x_register_experiment() { gov -- TestRegisterExperiment | t_table | sed -E 's/^ +//'; }
x_velocity_bound() { gov TF_EXP=1 -- TestVelocityBound | t_table | sed -E 's/^    //'; }
x_velocity_information() { gov TF_EXP=1 -- TestVelocityInformation | t_table | sed -E 's/^    //'; }
x_attacks() {
	gov -- TestAttackForgedIdentity TestAttackStripeFlood TestAttackEvictionReset TestAttackFloodCollateral \
		TestAttackHeavyHitter TestUpperBoundRandom | t_logs
}
x_rates() { gov -- TestSustainedRate | t_logs; }
x_sketch_experiments() { gov TF_EXP=1 -- TestSketchShapes TestSpillThreshold | t_logs; }
x_flood_sizing() { gov TF_EXP=1 -- TestFloodSizing | t_logs; }
x_answer_digest() { gov TF_EXP=1 -- TestAnswerDigest | grep '^cell '; }

regression() {
	check build build_suite
	[ -x "$BIN" ] && [ -x "$TFBENCH" ] || return
	check conformance run -- -run '^TestConformance$' -v
	check regress run -- -run '^TestRegress' -v
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
	local n=${1:-300} bad=0 t par d f r p
	for t in TestConcurrentFirstInsertNoDuplicates TestTouchRacingRelocationKeepsCounts TestAbandonedMoveKeepsCounts TestStashConcurrentEvictions; do
		par=4
		[ "$t" = TestAbandonedMoveKeepsCounts ] && par=1
		p=$(names "^$t\$")
		d="$OUT/racy/$t"
		mkdir -p "$d"
		: >"$d/fails"
		seq "$n" | xargs -P "$par" -I{} sh -c 'env -u TF_CT -u TF_BG -u TF_NOCAS2 "$1" --test-threads=1 --nocapture --exact "$2" >"$3/run.$4.log" 2>&1 || echo "$4" >>"$3/fails"' _ "$BIN" "$p" "$d" {}
		f=$(wc -l <"$d/fails" | tr -d ' ')
		printf 'stress %-44s failed %s/%s\n' "$t" "$f" "$n"
		if [ "$f" = 0 ]; then
			rm -rf "$d"
			continue
		fi
		bad=1
		for r in $(cat "$d/fails"); do
			grep -h -E '_test\.rs:[0-9]+:|panicked' "$d/run.$r.log" | head -3 | sed "s|^ *|    run $r: |"
		done
	done
	return $bad
}

# The twelve benchmarks of go/test.sh's PERF_BENCH (bench/bench.rs ports them).
PERF_BENCH=${PERF_BENCH:-'^Benchmark(CheckAndTouchHit|CheckAndTouchHitHot|CheckAndTouchMissInsert|TouchAndCount|VelocityCount|VelocityCountHot|ContainsAbsent|InsertSteady|InsertNearFull|InsertSaturated|CheckAndTouchParallel|CheckAndTouchBatch)$'}

perf_gate() { # perf_gate <A>: fail when both the min and the median of B run over PERF_MAX x A's.
	CARGO=$CARGO bench/ab.sh "$1" . "$PERF_BENCH" "${PERF_ROUNDS:-6}" | tee /dev/stderr |
		awk -v max="${PERF_MAX:-1.20}" 'NR > 3 && $NF + 0 > max && $4 / $2 > max { print "slower than " max "x (min and median): " $1; bad = 1 } END { exit bad }'
}

perf() {
	local a=${1:-}
	if [ -z "$a" ]; then
		if [ -z "$(git status --porcelain --untracked-files=all -- . 2>/dev/null)" ]; then a=HEAD~1; else a=HEAD; fi
	fi
	if [ "$a" != go ] && ! git cat-file -e "$a:rust" 2>/dev/null; then
		say "perf: $a has no rust/ to compare with (commit the port first, or name a revision: ./test.sh perf <rev>; ./test.sh perf go compares with the Go reference)"
		failed=1
		return
	fi
	say "perf: $a vs working tree (B/A > ${PERF_MAX:-1.20} fails; speed is host-dependent)"
	check perf perf_gate "$a"
}

# The suite under ThreadSanitizer: a nightly toolchain, its rust-src (the standard library is rebuilt
# instrumented) and -Zsanitizer=thread. TSAN_BIN is the instrumented suite, built for the host triple named
# explicitly, so cargo keeps it under the target directory's <triple>/, apart from the plain build.
TSAN_BIN=
tsan_ready() {
	"$CARGO" "+$NIGHTLY" --version >/dev/null 2>&1 &&
		rustup component list --toolchain "$NIGHTLY" --installed 2>/dev/null | grep -q '^rust-src'
}
build_tsan() {
	local target
	target=$(rustc "+$NIGHTLY" -vV | sed -n 's/^host: //p')
	TSAN_BIN=$(RUSTFLAGS='-Zsanitizer=thread' RUSTDOCFLAGS='-Zsanitizer=thread' \
		executable "+$NIGHTLY" test --lib --no-run -Zbuild-std --target "$target") &&
		[ -x "$TSAN_BIN" ]
}
run_tsan() { # run_tsan <env...> -- <libtest args...>: the instrumented suite, ASLR off where setarch exists
	local envs=()
	while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
	shift
	local pre=()
	if command -v setarch >/dev/null; then pre=(setarch "$(uname -m)" -R); fi
	# allocator_may_return_null: TestConfigUnaddressable asks for a table no allocator can give, and new
	# returns an Error when the allocation returns null; TSan's allocator aborts instead by default.
	env "${envs[@]}" TSAN_OPTIONS="halt_on_error=1 allocator_may_return_null=1 ${TSAN_OPTIONS:-}" "${pre[@]}" "$TSAN_BIN" --test-threads=1 "$@"
}

stress() {
	check build build_suite
	[ -x "$BIN" ] || return
	local quiet=(TF_STRESS= TF_FUZZ= TF_FUZZ_CASE= TF_CFUZZ= TF_CFUZZ_CASE=)
	local soak=(--nocapture --exact "$(names '^TestStressSoak$')")
	local tsan=0
	if ! tsan_ready; then
		skip race "no $NIGHTLY toolchain with rust-src: ThreadSanitizer needs -Zsanitizer=thread and -Zbuild-std"
	else
		check build-tsan build_tsan
		[ -x "$TSAN_BIN" ] && tsan=1
	fi
	if [ $tsan = 1 ]; then
		# The suite under the race detector, without the soak and the fuzzer (their own checks follow).
		check race-full run_tsan "${quiet[@]}" -- --nocapture
		check race-constant-time run_tsan "${quiet[@]}" TF_CT=1 -- --nocapture
		check race-background run_tsan "${quiet[@]}" TF_BG=1 -- --nocapture
		if [ $x86 = 1 ]; then check race-no-cas2 run_tsan "${quiet[@]}" TF_NOCAS2=1 -- --nocapture; fi
	fi
	check racy-regressions racy_regressions "${STRESS_RUNS:-300}"
	check soak run TF_STRESS="${TF_STRESS:-60s}" -- -run '^TestStressSoak$' -v
	if [ $tsan = 1 ]; then
		check soak-race run_tsan TF_STRESS="${TF_STRESS_RACE:-20s}" -- "${soak[@]}"
	else
		skip soak-race "no ThreadSanitizer build"
	fi
	check soak-background run TF_BG=1 TF_STRESS="${TF_STRESS:-60s}" -- -run '^TestStressSoak$' -v
	check soak-background-tags run TF_BG=1 TF_STRESS="${TF_STRESS:-60s}" TF_STRESS_TAGS=8 -- -run '^TestStressSoak$' -v
	check fuzz run TF_FUZZ="${TF_FUZZ:-1e9}" -- -run '^TestFuzzUpper$' -v
	check fuzz-concurrent run TF_CFUZZ="${TF_CFUZZ:-5m}" -- -run '^TestFuzzConcurrent$' -v
	check fuzz-concurrent-tags run TF_CFUZZ="${TF_CFUZZ:-5m}" TF_CFUZZ_TAGS=8 -- -run '^TestFuzzConcurrent$' -v
}

case $tier in
smoke | sanity | regression | perf | stress) "$tier" "$@" ;;
all)
	sanity
	regression
	stress
	;;
*) sed -n '2,28p' "$0"; exit 2 ;;
esac
if [ $failed != 0 ]; then
	say "SOME CHECKS FAILED ($tier; logs in $OUT)"
elif [ ${#skipped[@]} -gt 0 ]; then
	say "ALL PASS ($tier), but skipped: ${skipped[*]}"
else
	say "ALL PASS ($tier)"
fi
exit $failed
