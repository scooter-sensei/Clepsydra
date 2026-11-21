#!/bin/bash
# lab/sw W11b's readings (SW-RESULTS.md): outputs in lab/w11b/ (cleared). The head is W11b's commit; its library is
# 2393104's (W12's reading's), which this script checks, and differs from 8a15270's (W4e's, with W10b's hook) by W11's
# testHookRestoreAdd alone, which it applies, as a patch, to 23f03b6 (W4d's library) for the sight, as lab/w11.sh did.
#  1 sight: the head's go/drip_hotfresh_test.go on a detached worktree of 23f03b6 with the hook, tfdrip:
#    TestDripHotRestoreFresh, and TestDripHotFreshRace for ROUNDS rounds;
#  2 the head, tfdrip: TestDripHotRestoreFresh plain and under -race; TestDripHotFreshRace for ROUNDS rounds, and
#    RACE_ROUNDS under -race; W11's TestDripHotRestoreLegs and W4e's TestDripHotRestorePushOnce, plain;
#  3 the head, the default build: TestDripHotRestoreFresh;
#  4 lab/w11b.py on lab/w11b/ (clauses.txt).
# The reading judges the tests' lines, not exit codes (on 23f03b6 the legs' test fails where W4d's library pushes
# twice). A run counts only with its "W11b" lines (step 2's run of W11's tests, with its "W11" lines). Exits 1 if a
# run lacks them, 2 if the script cannot start; the joiner's exit is logged, not folded in. W11B_LAB, W11B_FRESH,
# W11B_RACE, W11B_W11 (the clone; go test -run patterns), W11B_ROUNDS and W11B_RACE_ROUNDS are for trying the script
# on a scratch clone with stand-in tests only.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
unset TF_CT TF_BG TF_NOCAS2 TF_EXP TF_DRIP_RACE_ROUNDS TF_DRIP_FRESH_ROUNDS
L=${W11B_LAB:-$HOME/lab-sw}
O=$L/lab/w11b
P=8a15270 # W4e's library with W10b's hook: W11's hook is the head's go/hot.go against it
Q=2393104 # W12's reading: the library W11b reads
FRESH=${W11B_FRESH:-'^TestDripHotRestoreFresh$'} RACE=${W11B_RACE:-'^TestDripHotFreshRace$'}
W11=${W11B_W11:-'^TestDripHotRestore(Legs|PushOnce)$'}
ROUNDS=${W11B_ROUNDS:-200000} RACE_ROUNDS=${W11B_RACE_ROUNDS:-20000}
git -C $L ls-files --error-unmatch lab/w11b.sh lab/w11b.py go/drip_hotfresh_test.go >/dev/null 2>&1 &&
	[ -z "$(git -C $L status --porcelain -- go lab/w11b.sh lab/w11b.py SW-RESULTS.md)" ] ||
	{ echo "the tree, lab/w11b.sh or lab/w11b.py is not committed as it is: commit it first"; exit 2; }
rm -rf $O && mkdir -p $O
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
h=$(git -C $L rev-parse --short HEAD)
log "W11b: head $h; $(go version | cut -d' ' -f3)"
libQ=$(git -C $L diff --name-only $Q HEAD -- go ':!go/*_test.go' ':!go/verify' | tr '\n' ' ')
log "0: library files that differ from $Q's: [$libQ]"
[ -z "$libQ" ] || { log "0: the library is not $Q's: stop"; exit 2; }
git -C $L diff $P HEAD -- go/hot.go >$O/hook.patch
lib=$(git -C $L diff --name-only $P HEAD -- go ':!go/*_test.go' ':!go/verify' | tr '\n' ' ')
log "0: library files that differ from $P's: [$lib]; lines removed from go/hot.go: $(grep -c '^-[^-]' $O/hook.patch), added: $(grep -c '^+[^+]' $O/hook.patch)"
[ "$lib" = "go/hot.go " ] && [ "$(grep -c '^-[^-]' $O/hook.patch)" = 0 ] || { log "0: the library differs from $P's by more than the hook: stop"; exit 2; }
W=$(mktemp -d)
trap 'for t in $W/*; do git -C $L worktree remove --force $t >/dev/null 2>&1; done; git -C $L worktree prune; rm -rf $W' EXIT
bad=0

# lines <file> <name> [tag]: a run's lines, and whether it has any with its tag (default "W11b ")
lines() {
	grep -E 'W11b? |=== RUN|--- (PASS|FAIL|SKIP)|^(ok|FAIL|panic)|WARNING: DATA RACE|drip_hot(fresh|legs)?_test.go:[0-9]+: ' $1 | sed 's/^ *//' | cut -c1-900 >>$O/progress.txt
	grep -q "${3:-W11b }" $1 || { log "$2: NO ${3:-W11b }LINES" && bad=1; }
}

log "1: sight, the head's go/drip_hotfresh_test.go on 23f03b6 (W4d's library) with the hook"
if git -C $L worktree add --detach -q $W/w4d 23f03b6 && (cd $W/w4d && git apply $O/hook.patch) && cp $L/go/drip_hotfresh_test.go $W/w4d/go/; then
	d=$(git -C $W/w4d diff --name-only -- go ':!go/*_test.go' | tr '\n' ' ')
	same=$(diff <(git -C $W/w4d diff -U0 -- go/hot.go | grep -E '^[+-][^+-]') <(grep -E '^[+-][^+-]' $O/hook.patch) >/dev/null && echo yes || echo NO)
	log "1: 23f03b6's library files changed [$d], the changes the hook's: $same"
	(cd $W/w4d/go && go test -tags tfdrip -count=1 -run "$FRESH" -v -timeout 2m . >$O/fresh-w4d.txt 2>&1)
	log "1 legs on 23f03b6: go test exit $?"
	lines $O/fresh-w4d.txt "1 legs on 23f03b6"
	(cd $W/w4d/go && TF_DRIP_FRESH_ROUNDS=$ROUNDS go test -tags tfdrip -count=1 -run "$RACE" -v -timeout 30m . >$O/race-w4d.txt 2>&1)
	log "1 race on 23f03b6, $ROUNDS rounds: go test exit $?"
	lines $O/race-w4d.txt "1 race on 23f03b6"
else
	log "1: no worktree of 23f03b6 with the hook" && bad=1
fi

log "2: the head, tfdrip"
cd $L/go || exit 2
go test -tags tfdrip -count=1 -run "$FRESH" -v -timeout 2m . >$O/fresh-head.txt 2>&1
log "2 legs: go test exit $?"
lines $O/fresh-head.txt "2 legs"
race go test -race -tags tfdrip -count=1 -run "$FRESH" -v -timeout 2m . >$O/fresh-head-race.txt 2>&1
log "2 legs under -race: go test exit $?"
lines $O/fresh-head-race.txt "2 legs under -race"
TF_DRIP_FRESH_ROUNDS=$ROUNDS go test -tags tfdrip -count=1 -run "$RACE" -v -timeout 30m . >$O/race-head.txt 2>&1
log "2 race, $ROUNDS rounds: go test exit $?"
lines $O/race-head.txt "2 race"
race env TF_DRIP_FRESH_ROUNDS=$RACE_ROUNDS go test -race -tags tfdrip -count=1 -run "$RACE" -v -timeout 30m . >$O/race-head-race.txt 2>&1
log "2 race under -race, $RACE_ROUNDS rounds: go test exit $?"
lines $O/race-head-race.txt "2 race under -race"
go test -tags tfdrip -count=1 -run "$W11" -v -timeout 2m . >$O/w11-head.txt 2>&1
log "2 W11's legs and W4e's test: go test exit $?"
lines $O/w11-head.txt "2 W11's legs and W4e's test" "W11 "

log "3: the head, the default build"
go test -count=1 -run "$FRESH" -v -timeout 2m . >$O/fresh-default.txt 2>&1
log "3 legs: go test exit $?"
lines $O/fresh-default.txt "3 legs"

log "4: lab/w11b.py"
python3 -I $L/lab/w11b.py $O >$O/clauses.txt 2>&1
log "4: lab/w11b.py exit $? (clauses.txt)"
log "W11b readings finished (exit $bad)"
exit $bad
