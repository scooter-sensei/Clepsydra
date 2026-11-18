#!/bin/bash
# lab/sw W10's readings (SW-RESULTS.md): outputs in lab/w10/ (cleared).
#  1 sight: lab/w10_test.go with the head's go/countcheck_test.go, tfdrip, on detached worktrees of 23f03b6
#    (W4d's library) and of the head;
#  2 reported: the head's soak and fuzzer test files on a detached worktree of 0837f9f (W4c's tree), tfdrip,
#    the fuzzer for TF_CFUZZ (5 minutes);
#  3 the head, in ~/lab-sw/go: the soak (TF_STRESS) and the fuzzer (TF_CFUZZ), 5 minutes each, with tfdrip
#    plain and under -race and in the default build, and the fuzzer with a straggler (tfdrip).
# A run counts only if its summary line is there (both tests skip, and pass, with their variable unset).
# Exits 1 if a step-3 run fails or lacks its summary, 2 if it cannot start. W10_LAB, W10_SIGHT, W10_SOAK,
# W10_FUZZ (the clone; go test -run patterns for steps 1 and 2-3) and W10_DUR are for trying the script on a
# scratch clone only.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
unset TF_CT TF_BG TF_NOCAS2 TF_STRESS TF_CFUZZ TF_CFUZZ_TAGS TF_STRESS_TAGS TF_CFUZZ_STRAGGLE TF_CFUZZ_SEED TF_CFUZZ_CASE
L=${W10_LAB:-$HOME/lab-sw}
O=$L/lab/w10
S=${W10_SIGHT:-'^TestW10Sight$'} SOAK=${W10_SOAK:-'^TestStressSoak$'} FUZZ=${W10_FUZZ:-'^TestFuzzConcurrent$'} D=${W10_DUR:-5m}
git -C $L ls-files --error-unmatch lab/w10_test.go lab/w10.sh go/countcheck_test.go >/dev/null 2>&1 &&
	[ -z "$(git -C $L status --porcelain -- go lab/w10.sh lab/w10_test.go SW-RESULTS.md)" ] ||
	{ echo "the tree, lab/w10.sh or lab/w10_test.go is not committed as it is: commit it first"; exit 2; }
rm -rf $O && mkdir -p $O
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
h=$(git -C $L rev-parse --short HEAD)
log "W10: head $h; $(go version | cut -d' ' -f3)"
W=$(mktemp -d)
trap 'for t in $W/*; do git -C $L worktree remove --force $t >/dev/null 2>&1; done; git -C $L worktree prune; rm -rf $W' EXIT

# tree <name> <commit> <files from the head's go/ ...>: a detached worktree of commit with those files copied in
tree() {
	local n=$1 c=$2
	shift 2
	git -C $L worktree add --detach -q $W/$n $c || return 1
	for x in "$@"; do cp $L/go/$x $W/$n/go/$x || return 1; done
}

log "1: sight, lab/w10_test.go on 23f03b6 (W4d's library) and on the head"
for c in 23f03b6 $h; do
	if tree s-$c $c countcheck_test.go && cp $L/lab/w10_test.go $W/s-$c/go/w10_test.go; then
		(cd $W/s-$c/go && go test -tags tfdrip -count=1 -run "$S" -v . >$O/sight-$c.txt 2>&1)
		log "1 $c: go test exit $?"
	else
		log "1 $c: no worktree"
	fi
	grep -E 'W10 sight|w10_test|--- (PASS|FAIL)' $O/sight-$c.txt 2>/dev/null | sed 's/^ *//' >>$O/progress.txt
done

# summary <file> <what>: the run's summary lines, and whether it is there
summary() {
	local s
	s=$(grep -E 'W10: |cases from seed|workers, .*ops|^(--- |ok|FAIL|panic)|WARNING: DATA RACE' $1 | sed 's/^ *//' | head -12)
	echo "$s" >>$O/progress.txt
	grep -q 'W10: ' $1 && grep -qE -- '--- (PASS|FAIL)' $1
}

log "2: reported, the head's soak and fuzzer tests on 0837f9f (W4c's tree), tfdrip, the fuzzer for $D"
if tree w4c 0837f9f countcheck_test.go soak_test.go fuzzconc_test.go fuzz_test.go && (cd $W/w4c/go && go vet -tags tfdrip . >$O/w4c-vet.txt 2>&1; go test -tags tfdrip -count=1 -run '^$' . >>$O/w4c-vet.txt 2>&1); then
	(cd $W/w4c/go && TF_CFUZZ=$D go test -tags tfdrip -count=1 -run "$FUZZ" -v -timeout 0 . >$O/w4c-cfuzz.txt 2>&1)
	log "2: exit $?; failing cases $(grep -cE 'fuzzconc_test.go:[0-9]+: case [0-9]+:' $O/w4c-cfuzz.txt)"
	summary $O/w4c-cfuzz.txt || log "2: no summary"
	grep -oE 'case [0-9]+: [^;]*' $O/w4c-cfuzz.txt | cut -c1-200 | head -5 >>$O/progress.txt
else
	log "2: the head's tests do not build on 0837f9f: $(grep -m3 -E 'error|cannot|undefined' $O/w4c-vet.txt 2>/dev/null | tr '\n' ' ' | cut -c1-300)"
fi

log "3: the head's soak and fuzzer, $D each"
cd $L/go || exit 2
bad=0
# run3 <name> <env...> -- <go test args...>
run3() {
	local n=$1
	shift
	"$@" >$O/$n.txt 2>&1
	local rc=$?
	if summary $O/$n.txt; then log "3 $n: exit $rc"; else log "3 $n: exit $rc, NO SUMMARY"; rc=1; fi
	[ $rc = 0 ] || bad=1
}
run3 soak-drip env TF_STRESS=$D go test -tags tfdrip -count=1 -run "$SOAK" -v -timeout 0 .
run3 soak-drip-race race env TF_STRESS=$D go test -race -tags tfdrip -count=1 -run "$SOAK" -v -timeout 0 .
run3 soak-default env TF_STRESS=$D go test -count=1 -run "$SOAK" -v -timeout 0 .
run3 cfuzz-drip env TF_CFUZZ=$D go test -tags tfdrip -count=1 -run "$FUZZ" -v -timeout 0 .
run3 cfuzz-drip-race race env TF_CFUZZ=$D go test -race -tags tfdrip -count=1 -run "$FUZZ" -v -timeout 0 .
run3 cfuzz-drip-straggle env TF_CFUZZ=$D TF_CFUZZ_STRAGGLE=1 go test -tags tfdrip -count=1 -run "$FUZZ" -v -timeout 0 .
run3 cfuzz-default env TF_CFUZZ=$D go test -count=1 -run "$FUZZ" -v -timeout 0 .
log "W10 readings finished (exit $bad)"
exit $bad
