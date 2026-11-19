#!/bin/bash
# lab/sw W10b's readings (SW-RESULTS.md): outputs in lab/w10b/ (cleared). The head is W10b's commit; its
# library differs from 01e49a6's (W4e's) by testHookSketchPart alone, which this script checks and applies, as
# a patch, to the older trees it runs the head's tests on.
#  1 sight: TestPartCells on the head; lab/w10b_test.go (TestW10bSight, TestW10bSightOverflow) with the head's
#    test files, tfdrip, on detached worktrees of 23f03b6 (W4d's library, with the hook) and of the head;
#  2 case 865: N single runs (TF_CFUZZ_CASE, TF_CFUZZ_RUNS=1, one process each) on the head and, the control, on
#    01e49a6 (W10's check), tfdrip and the default build;
#  3 sight under concurrency: TestDripConcurrentCounts, Background, spread, 20,000 rounds, tfdrip, on 0837f9f
#    (W4c's tree, with the hook and the head's test files) and on the head;
#  4 the library: the default build's tfbench -exact and answer digest against verify/expected/, tfdrip's
#    tfbench -exact against lab/w4e/exact-drip.txt;
#  5 W10's runs on the head: the soak (TF_STRESS) and the fuzzer (TF_CFUZZ), 5 minutes each, tfdrip plain and
#    under -race and the default build, and the fuzzer with a straggler (tfdrip);
#  6 reported: the fuzzer on 23f03b6 (with the hook and the head's test files), tfdrip, 5 minutes.
# A run counts only with its summary line (the soak, the fuzzer and TestDripConcurrentCounts skip, and pass,
# with their variables unset); the reading judges the lines, not exit codes. Exits 1 if a judged run lacks its
# summary, 2 if the script cannot start. W10B_LAB, W10B_SIGHT, W10B_FUZZ, W10B_SOAK, W10B_W7 (the clone; go
# test -run patterns), W10B_CASE, W10B_N, W10B_ROUNDS, W10B_DUR and W10B_NOEXACT are for trying the script on a
# scratch clone with stand-in tests only.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
unset TF_CT TF_BG TF_NOCAS2 TF_STRESS TF_CFUZZ TF_CFUZZ_TAGS TF_STRESS_TAGS TF_CFUZZ_STRAGGLE TF_CFUZZ_SEED TF_CFUZZ_CASE TF_CFUZZ_RUNS TF_EXP TF_DRIP_ROUNDS TF_DRIP_MODES TF_DRIP_SHAPES
L=${W10B_LAB:-$HOME/lab-sw}
O=$L/lab/w10b
P=01e49a6 # W10b's parent: W10's check, W4e's library
SIGHT=${W10B_SIGHT:-'^TestW10bSight(Overflow)?$'} FUZZ=${W10B_FUZZ:-'^TestFuzzConcurrent$'} SOAK=${W10B_SOAK:-'^TestStressSoak$'}
W7=${W10B_W7:-'^TestDripConcurrentCounts$'} CASE=${W10B_CASE:-865} N=${W10B_N:-200} ROUNDS=${W10B_ROUNDS:-20000} D=${W10B_DUR:-5m}
TESTS="countcheck_test.go soak_test.go fuzzconc_test.go fuzz_test.go dripcount_test.go"
git -C $L ls-files --error-unmatch lab/w10b_test.go lab/w10b.sh go/countcheck_test.go >/dev/null 2>&1 &&
	[ -z "$(git -C $L status --porcelain -- go lab/w10b.sh lab/w10b_test.go SW-RESULTS.md)" ] ||
	{ echo "the tree, lab/w10b.sh or lab/w10b_test.go is not committed as it is: commit it first"; exit 2; }
rm -rf $O && mkdir -p $O/bin
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
h=$(git -C $L rev-parse --short HEAD)
log "W10b: head $h; $(go version | cut -d' ' -f3)"
# the library: the head's differs from $P's in go/touch.go alone, by added lines only (the hook)
git -C $L diff $P HEAD -- go/touch.go >$O/hook.patch
lib=$(git -C $L diff --name-only $P HEAD -- go ':!go/*_test.go' ':!go/verify' | tr '\n' ' ')
log "0: library files that differ from $P's: [$lib]; lines removed from go/touch.go: $(grep -c '^-[^-]' $O/hook.patch), added: $(grep -c '^+[^+]' $O/hook.patch)"
[ "$lib" = "go/touch.go " ] && [ "$(grep -c '^-[^-]' $O/hook.patch)" = 0 ] || { log "0: the library differs from $P's by more than the hook: stop"; exit 2; }
W=$(mktemp -d)
trap 'for t in $W/*; do git -C $L worktree remove --force $t >/dev/null 2>&1; done; git -C $L worktree prune; rm -rf $W' EXIT

# tree <name> <commit> [hook]: a detached worktree of commit; with "hook", the hook patch applied and the head's
# test files copied in, and checked to differ from the commit by the hook alone in the library
tree() {
	local n=$1 c=$2
	git -C $L worktree add --detach -q $W/$n $c || return 1
	[ "${3:-}" = hook ] || return 0
	(cd $W/$n && git apply $O/hook.patch) || { log "$n: the hook does not apply to $c"; return 1; }
	for x in $TESTS; do cp $L/go/$x $W/$n/go/$x || return 1; done
	local d=$(git -C $W/$n diff --name-only -- go ':!go/*_test.go' | tr '\n' ' ')
	local same=$(diff <(git -C $W/$n diff -U0 -- go/touch.go | grep -E '^[+-][^+-]') <(grep -E '^[+-][^+-]' $O/hook.patch) >/dev/null && echo yes || echo NO)
	log "$n ($c): library files changed [$d], the changes the hook's: $same"
	[ "$d" = "go/touch.go " ] && [ $same = yes ]
}

log "1: sight"
(cd $L/go && go test -count=1 -run '^TestPartCells$' -v . >$O/partcells.txt 2>&1)
log "1 TestPartCells on the head: exit $?; $(grep -cE -- '--- PASS: TestPartCells' $O/partcells.txt) PASS lines"
tree s-head HEAD && cp $L/lab/w10b_test.go $W/s-head/go/w10b_test.go
tree s-w4d 23f03b6 hook && cp $L/lab/w10b_test.go $W/s-w4d/go/w10b_test.go
for n in s-w4d s-head; do
	if [ -f $W/$n/go/w10b_test.go ]; then
		(cd $W/$n/go && go test -tags tfdrip -count=1 -run "$SIGHT" -v . >$O/sight-$n.txt 2>&1)
		log "1 $n: go test exit $?"
		grep -E 'W10b sight|w10b_test|--- (PASS|FAIL)' $O/sight-$n.txt | sed 's/^ *//' >>$O/progress.txt
	else
		log "1 $n: no worktree"
	fi
done

# rep <name> <test binary> <summary regex>: N single runs of case CASE; a run passes only with its summary
rep() {
	local n=$1 b=$2 re=$3 fails=0 ok=0 none=0 i
	mkdir -p $O/$n
	for i in $(seq 1 $N); do
		TF_CFUZZ_CASE=$CASE TF_CFUZZ_RUNS=1 $b -test.run "$FUZZ" -test.v >$O/$n/run.txt 2>&1
		if grep -q -- '--- FAIL' $O/$n/run.txt; then
			fails=$((fails + 1)) && cp $O/$n/run.txt $O/$n/fail-$i.txt
		elif grep -qE "$re" $O/$n/run.txt && grep -q -- '--- PASS' $O/$n/run.txt; then
			ok=$((ok + 1))
		else
			none=$((none + 1)) && cp $O/$n/run.txt $O/$n/none-$i.txt
		fi
	done
	rm -f $O/$n/run.txt
	log "2 $n: $N single runs of case $CASE: $fails fail, $ok pass with their summary, $none neither"
	cat $O/$n/fail-*.txt 2>/dev/null | grep -oE '(case [0-9]+: [0-9]+ [a-zA-Z ]+ \(first: |(Count|Touch): Upper [0-9]+ \(the overflow table.s [0-9]+\) > [0-9]+ events)' |
		sed -E 's/case [0-9]+: //; s/ \(first: //' | sort | uniq -c | sed "s/^/  2 $n:/" >>$O/progress.txt
}
log "2: case $CASE, $N single runs each"
(cd $L/go && go test -tags tfdrip -c -o $O/bin/head-drip.test . && go test -c -o $O/bin/head-default.test .) || log "2: the head's binaries do not build"
tree c-w10 $P && (cd $W/c-w10/go && go test -tags tfdrip -c -o $O/bin/w10-drip.test . && go test -c -o $O/bin/w10-default.test .) || log "2: $P's binaries do not build"
for b in drip default; do
	[ -x $O/bin/w10-$b.test ] && rep control-$b $O/bin/w10-$b.test "case $CASE: .*W10: "
	[ -x $O/bin/head-$b.test ] && rep head-$b $O/bin/head-$b.test "case $CASE: .*W10b: "
done
rm -rf $O/bin

# w7 <name> <dir>: TestDripConcurrentCounts, Background, spread, ROUNDS rounds, tfdrip, in dir
w7() {
	(cd $2 && TF_EXP=1 TF_DRIP_MODES=bg TF_DRIP_SHAPES=spread TF_DRIP_ROUNDS=$ROUNDS go test -tags tfdrip -count=1 -run "$W7" -v -timeout 0 . >$O/$1.txt 2>&1)
	log "3 $1: go test exit $?"
	grep -E 'W10b at rest|shape spread: [0-9]+ rounds|  (log over|Upper over|part over|Count high)' $O/$1.txt | sed 's/^ *//' | cut -c1-600 >>$O/progress.txt
	grep -q 'W10b at rest' $O/$1.txt || log "3 $1: NO SUMMARY"
}
log "3: sight under concurrency, TestDripConcurrentCounts, bg, spread, $ROUNDS rounds"
tree w4c 0837f9f hook && w7 conc-w4c $W/w4c/go || log "3: no tree for 0837f9f"
w7 conc-head $L/go

log "4: the library"
cd $L/go || exit 2
if [ -z "${W10B_NOEXACT:-}" ]; then
	go run ./cmd/tfbench -exact >$O/exact-default.txt 2>&1
	cmp -s $O/exact-default.txt verify/expected/exact.txt && log "4a default exact: IDENTICAL" || log "4a default exact: DIFFER"
	TF_EXP=1 go test -count=1 -run '^TestAnswerDigest$' -v . | grep '^cell ' >$O/digest-default.txt
	cmp -s $O/digest-default.txt verify/expected/answer-digest.txt && log "4b default digest: IDENTICAL" || log "4b default digest: DIFFER"
	go run -tags tfdrip ./cmd/tfbench -exact >$O/exact-drip.txt 2>&1
	cmp -s $O/exact-drip.txt ../lab/w4e/exact-drip.txt && log "4c tfdrip exact: IDENTICAL to W4e's" || log "4c tfdrip exact: DIFFER from W4e's"
else
	log "4: skipped (W10B_NOEXACT)"
fi

# summary <file>: the run's summary lines, and whether they are there
summary() {
	grep -E 'W10b: |cases from seed|workers, .*ops|^(--- |ok|FAIL|panic)|WARNING: DATA RACE' $1 | sed 's/^ *//' | head -12 >>$O/progress.txt
	grep -q 'W10b: ' $1 && grep -qE -- '--- (PASS|FAIL)' $1
}
bad=0
# run5 <name> <cmd...>: a step-5 run in $L/go
run5() {
	local n=$1
	shift
	"$@" >$O/$n.txt 2>&1
	local rc=$?
	if summary $O/$n.txt; then log "5 $n: exit $rc"; else log "5 $n: exit $rc, NO SUMMARY" && bad=1; fi
	grep -oE 'case [0-9]+: [0-9]+ [a-zA-Z ]+ \(first: [^;]*' $O/$n.txt | cut -c1-300 | head -5 >>$O/progress.txt
}
log "5: the head's soak and fuzzer, $D each"
run5 soak-drip env TF_STRESS=$D go test -tags tfdrip -count=1 -run "$SOAK" -v -timeout 0 .
run5 soak-drip-race race env TF_STRESS=$D go test -race -tags tfdrip -count=1 -run "$SOAK" -v -timeout 0 .
run5 soak-default env TF_STRESS=$D go test -count=1 -run "$SOAK" -v -timeout 0 .
run5 cfuzz-drip env TF_CFUZZ=$D go test -tags tfdrip -count=1 -run "$FUZZ" -v -timeout 0 .
run5 cfuzz-drip-race race env TF_CFUZZ=$D go test -race -tags tfdrip -count=1 -run "$FUZZ" -v -timeout 0 .
run5 cfuzz-drip-straggle env TF_CFUZZ=$D TF_CFUZZ_STRAGGLE=1 go test -tags tfdrip -count=1 -run "$FUZZ" -v -timeout 0 .
run5 cfuzz-default env TF_CFUZZ=$D go test -count=1 -run "$FUZZ" -v -timeout 0 .

log "6: reported, the fuzzer on 23f03b6 (W4d's library, with the hook), tfdrip, $D"
if tree w4d 23f03b6 hook; then
	(cd $W/w4d/go && TF_CFUZZ=$D go test -tags tfdrip -count=1 -run "$FUZZ" -v -timeout 0 . >$O/w4d-cfuzz.txt 2>&1)
	log "6: exit $?; failing cases $(grep -cE 'fuzzconc_test.go:[0-9]+: case [0-9]+:' $O/w4d-cfuzz.txt)"
	summary $O/w4d-cfuzz.txt || log "6: no summary"
	grep -oE 'case [0-9]+: [0-9]+ [a-zA-Z ]+ \(first: [^;]*' $O/w4d-cfuzz.txt | cut -c1-300 | head -5 >>$O/progress.txt
else
	log "6: no tree for 23f03b6"
fi
log "W10b readings finished (exit $bad)"
exit $bad
