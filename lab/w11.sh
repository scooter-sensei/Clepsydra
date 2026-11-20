#!/bin/bash
# lab/sw W11's readings (SW-RESULTS.md): outputs in lab/w11/ (cleared). The head is W11's commit; its library
# differs from 8a15270's (W4e's, with W10b's hook) by testHookRestoreAdd alone, which this script checks and
# applies, as a patch, to 23f03b6 (W4d's library) for the sight.
#  1 sight: the head's go/drip_hotlegs_test.go on a detached worktree of 23f03b6 with the hook, tfdrip:
#    TestDripHotRestoreLegs, and TestDripHotRestoreRace for ROUNDS rounds;
#  2 the head, tfdrip: TestDripHotRestoreLegs and W4e's TestDripHotRestorePushOnce, plain and under -race;
#    TestDripHotRestoreRace for ROUNDS rounds, and RACE_ROUNDS under -race;
#  3 the library: the default build's tfbench -exact and answer digest against verify/expected/, tfdrip's
#    tfbench -exact against lab/w4e/exact-drip.txt.
# The reading judges the tests' lines, not exit codes (on 23f03b6 the legs' test fails where W4d's library
# pushes twice). A run counts only with its "W11" lines. Exits 1 if a run lacks them, 2 if the script cannot
# start. W11_LAB, W11_LEGS, W11_RACE (the clone; go test -run patterns), W11_ROUNDS, W11_RACE_ROUNDS and
# W11_NOEXACT are for trying the script on a scratch clone with stand-in tests only.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
unset TF_CT TF_BG TF_NOCAS2 TF_EXP TF_DRIP_RACE_ROUNDS
L=${W11_LAB:-$HOME/lab-sw}
O=$L/lab/w11
P=8a15270 # W11's parent: W4e's library with W10b's hook
LEGS=${W11_LEGS:-'^TestDripHotRestore(Legs|PushOnce)$'} RACE=${W11_RACE:-'^TestDripHotRestoreRace$'}
ROUNDS=${W11_ROUNDS:-200000} RACE_ROUNDS=${W11_RACE_ROUNDS:-20000}
git -C $L ls-files --error-unmatch lab/w11.sh go/drip_hotlegs_test.go >/dev/null 2>&1 &&
	[ -z "$(git -C $L status --porcelain -- go lab/w11.sh SW-RESULTS.md)" ] ||
	{ echo "the tree or lab/w11.sh is not committed as it is: commit it first"; exit 2; }
rm -rf $O && mkdir -p $O
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
h=$(git -C $L rev-parse --short HEAD)
log "W11: head $h; $(go version | cut -d' ' -f3)"
git -C $L diff $P HEAD -- go/hot.go >$O/hook.patch
lib=$(git -C $L diff --name-only $P HEAD -- go ':!go/*_test.go' ':!go/verify' | tr '\n' ' ')
log "0: library files that differ from $P's: [$lib]; lines removed from go/hot.go: $(grep -c '^-[^-]' $O/hook.patch), added: $(grep -c '^+[^+]' $O/hook.patch)"
[ "$lib" = "go/hot.go " ] && [ "$(grep -c '^-[^-]' $O/hook.patch)" = 0 ] || { log "0: the library differs from $P's by more than the hook: stop"; exit 2; }
W=$(mktemp -d)
trap 'for t in $W/*; do git -C $L worktree remove --force $t >/dev/null 2>&1; done; git -C $L worktree prune; rm -rf $W' EXIT
bad=0

# lines <file> <name>: the W11 lines of a run, and whether there are any
lines() {
	grep -E 'W11 |--- (PASS|FAIL)|^(ok|FAIL|panic)|WARNING: DATA RACE|drip_hotlegs_test.go:[0-9]+: (the|K|L)|drip_hot_test.go:[0-9]+: (hook|K|the)' $1 | sed 's/^ *//' | cut -c1-700 >>$O/progress.txt
	grep -q 'W11 ' $1 || { log "$2: NO W11 LINES" && bad=1; }
}

log "1: sight, the head's go/drip_hotlegs_test.go on 23f03b6 (W4d's library) with the hook"
if git -C $L worktree add --detach -q $W/w4d 23f03b6 && (cd $W/w4d && git apply $O/hook.patch) && cp $L/go/drip_hotlegs_test.go $W/w4d/go/; then
	d=$(git -C $W/w4d diff --name-only -- go ':!go/*_test.go' | tr '\n' ' ')
	same=$(diff <(git -C $W/w4d diff -U0 -- go/hot.go | grep -E '^[+-][^+-]') <(grep -E '^[+-][^+-]' $O/hook.patch) >/dev/null && echo yes || echo NO)
	log "1: 23f03b6's library files changed [$d], the changes the hook's: $same"
	(cd $W/w4d/go && go test -tags tfdrip -count=1 -run "$LEGS" -v -timeout 2m . >$O/legs-w4d.txt 2>&1)
	log "1 legs on 23f03b6: go test exit $?"
	lines $O/legs-w4d.txt "1 legs on 23f03b6"
	(cd $W/w4d/go && TF_DRIP_RACE_ROUNDS=$ROUNDS go test -tags tfdrip -count=1 -run "$RACE" -v -timeout 30m . >$O/race-w4d.txt 2>&1)
	log "1 race on 23f03b6, $ROUNDS rounds: go test exit $?"
	lines $O/race-w4d.txt "1 race on 23f03b6"
else
	log "1: no worktree of 23f03b6 with the hook" && bad=1
fi

log "2: the head"
cd $L/go || exit 2
go test -tags tfdrip -count=1 -run "$LEGS" -v -timeout 2m . >$O/legs-head.txt 2>&1
log "2 legs: go test exit $?"
lines $O/legs-head.txt "2 legs"
race go test -race -tags tfdrip -count=1 -run "$LEGS" -v -timeout 2m . >$O/legs-head-race.txt 2>&1
log "2 legs under -race: go test exit $?"
lines $O/legs-head-race.txt "2 legs under -race"
TF_DRIP_RACE_ROUNDS=$ROUNDS go test -tags tfdrip -count=1 -run "$RACE" -v -timeout 30m . >$O/race-head.txt 2>&1
log "2 race, $ROUNDS rounds: go test exit $?"
lines $O/race-head.txt "2 race"
race env TF_DRIP_RACE_ROUNDS=$RACE_ROUNDS go test -race -tags tfdrip -count=1 -run "$RACE" -v -timeout 30m . >$O/race-head-race.txt 2>&1
log "2 race under -race, $RACE_ROUNDS rounds: go test exit $?"
lines $O/race-head-race.txt "2 race under -race"

log "3: the library"
if [ -z "${W11_NOEXACT:-}" ]; then
	go run ./cmd/tfbench -exact >$O/exact-default.txt 2>&1
	cmp -s $O/exact-default.txt verify/expected/exact.txt && log "3a default exact: IDENTICAL" || log "3a default exact: DIFFER"
	TF_EXP=1 go test -count=1 -run '^TestAnswerDigest$' -v . | grep '^cell ' >$O/digest-default.txt
	cmp -s $O/digest-default.txt verify/expected/answer-digest.txt && log "3b default digest: IDENTICAL" || log "3b default digest: DIFFER"
	go run -tags tfdrip ./cmd/tfbench -exact >$O/exact-drip.txt 2>&1
	cmp -s $O/exact-drip.txt ../lab/w4e/exact-drip.txt && log "3c tfdrip exact: IDENTICAL to W4e's" || log "3c tfdrip exact: DIFFER from W4e's"
else
	log "3: skipped (W11_NOEXACT)"
fi
log "W11 readings finished (exit $bad)"
exit $bad
