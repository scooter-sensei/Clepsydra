#!/usr/bin/env bash
# Re-runs the deterministic effect of each reverted experiment. The patches
# in verify/experiments/ are diffs against fbe6968 (the last commit of the
# improvement loop; its library code matches HEAD's apart from comments).
# Each one is applied to a fresh export of that commit, then today's accuracy
# harness (cmd/tfbench -exact) and the patch's own tests run on it.
# Speed effects are host-dependent: verify/ab.sh fbe6968 fbe6968+verify/experiments/<name>.patch <bench>.
# verify/experiments/README.md describes each patch.
set -uo pipefail
cd "$(dirname "$0")/.."
. verify/lib.sh
WORK=${WORK:-$(mktemp -d)}
BASE=fbe6968
root=$PWD

prep() { # prep <name> [patch]: tree at BASE plus the patch and today's harness
	local d="$WORK/exp-$1"
	export_tree "$BASE" "$d"
	[ -n "${2:-}" ] && (cd "$d" && git apply "$root/$2")
	rm -rf "$d/cmd/tfbench" && mkdir -p "$d/cmd" && cp -r cmd/tfbench "$d/cmd/" && cp -r internal "$d/"
	echo "$d"
}

exact() { # exact <dir>: the -exact report, minus nothing
	(cd "$1" && go run -tags accuracyonly ./cmd/tfbench -exact)
}

testline() { # testline <dir> <regex>: PASS/FAIL plus the test's log lines (file:line prefixes dropped)
	(cd "$1" && go test -count=1 -run "$2" -v . 2>&1) |
		grep -E '^(--- (PASS|FAIL)|    [a-z_]+\.go:[0-9]+: )' | sed -E 's/^ +[a-z_]+\.go:[0-9]+: /    /; s/ \([0-9.]+s\)$//'
}

echo "== baseline $BASE (no patch)"
d=$(prep baseline)
exact "$d"
echo "== oldest-first-eviction: spray test must fail"
d=$(prep oldest verify/experiments/oldest-first-eviction.patch)
testline "$d" '^TestSprayCannotEvictRepeatOffender$'
exact "$d" | grep -E '^(spray|churn)'
for p in register-46 register-44 piecewise-linear dyadic-h32 eight-slot spill-counts; do
	echo "== $p"
	d=$(prep "$p" "verify/experiments/$p.patch")
	exact "$d"
done
echo "== spill-counts: concurrent audit"
testline "$WORK/exp-spill-counts" '^TestSpillCountsNeverUnderCount$' | grep -E '^--- '
echo "== homing: share of keys in their alternate line (baseline = test file only)"
d=$(prep homing-base) && (cd "$d" && git apply --include=alt_fraction_test.go "$root/verify/experiments/homing.patch")
testline "$d" 'ZZAltFraction' | sed 's/^/baseline /'
d=$(prep homing verify/experiments/homing.patch)
testline "$d" 'ZZAltFraction' | sed 's/^/patch    /'
echo "== branchless-find: SETEQ instructions in find (codegen)"
for v in base branchless; do
	if [ $v = base ]; then d=$(prep find-base); else d=$(prep find-branchless verify/experiments/branchless-find.patch); fi
	n=$(cd "$d" && go build -gcflags=-S . 2>&1 | awk '/^github.com\/scooter-sensei\/temporalfilter\.\(\*Filter\)\.find STEXT/ {f = 1; next} /STEXT/ {f = 0} f && /SETEQ/' | wc -l)
	echo "$v: $n"
done
