# The checks behind verify/run.sh and test.sh, sourced from go/. Results go
# to $OUT (default ./verify-report): summary.txt plus one log per check.
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

attacks() {
	go test -count=1 -run '^(TestAttackForgedIdentity|TestAttackStripeFlood|TestAttackEvictionReset|TestAttackFloodCollateral|TestAttackHeavyHitter|TestUpperBoundRandom)$' -v . |
		grep -vE '^(=== |--- |PASS|ok |FAIL)' | sed -E 's/^ *[a-z_]+\.go:[0-9]+: //'
}

rates() {
	go test -count=1 -run '^TestSustainedRate$' -v . | grep -vE '^(=== |--- |PASS|ok |FAIL)' | sed -E 's/^ *[a-z_]+\.go:[0-9]+: //'
}

flood_sizing() {
	TF_EXP=1 go test -count=1 -run '^TestFloodSizing$' -v . | grep -vE '^(=== |--- |PASS|ok |FAIL)' | sed -E 's/^ *[a-z_]+\.go:[0-9]+: //'
}

sketch_experiments() {
	TF_EXP=1 go test -count=1 -run '^(TestSketchShapes|TestSpillThreshold)$' -v . |
		grep -vE '^(=== |--- |PASS|ok |FAIL)' | sed -E 's/^ *[a-z_]+\.go:[0-9]+: //'
}

timing() {
	TF_TIMING=1 go test -count=1 -run '^TestTimingLeak$' -v . | grep -E 'ConstantTime' | sed -E 's/^ *[a-z_]+\.go:[0-9]+: //'
}

ct_pads() {
	TF_CTEXP=1 go test -count=1 -run '^(TestCTPads|TestCTParallel)$' -v . | grep -vE '^(=== |--- |PASS|ok |FAIL)' | sed -E 's/^ *[a-z_]+\.go:[0-9]+: //'
}

speed() {
	for i in 1 2 3; do go run ./cmd/tfbench 2>/dev/null | tail -1; done
}
