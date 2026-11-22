#!/bin/bash
# lab/sw W13's readings (SW-RESULTS.md): outputs in lab/w13/ (cleared). The head is W13's commit; its library is
# c8eab03's (W11b's reading's) with W13's push count: notes in go/touch.go, go/cuckoo.go and go/filter.go, and
# go/push.go, go/push_on.go and go/push_off.go, which this script checks. 23f03b6 (W4d's library) is read with W11's
# hook (lab/w11b/hook.patch), the notes ported by hand (lab/w13-w4d.patch: its miss has no missPlaced), the head's
# push files and tests (go/push*.go, go/drip_hotfresh_test.go), and its own fuzzer's wiring ported by hand
# (lab/w13-w4d-fuzz.patch). Every test run is tfdrip,tfpush unless named.
#  0 the library: its files that differ from c8eab03's; the port's changed lines against the head's;
#  1 the library's answers: the default build's tfbench -exact and answer digest against go/verify/expected/; tfdrip's
#    and tfdrip,tfpush's tfbench -exact against lab/w4e/exact-drip.txt; TestConformance under tfdrip and under
#    tfdrip,tfpush; go vet in the four tag sets;
#  2 sight: on 23f03b6's port, W11b's legs with TestPushPaths, and TestDripHotFreshRace for ROUNDS rounds; on the head,
#    the same plain and under -race (RACE_ROUNDS rounds), and W11's TestDripHotRestoreLegs and W4e's
#    TestDripHotRestorePushOnce;
#  3 W7's scenario (TestDripConcurrentCounts, TF_EXP=1), Background, spread, W7_ROUNDS rounds, on the head;
#  4 the fuzzer (TestFuzzConcurrent) for FUZZ each: on the head as it is, with TF_CFUZZ_STRAGGLE=1, with
#    TF_CFUZZ_TAGS=8; on 23f03b6's port with TF_CFUZZ_STRAGGLE=1 (the power leg);
#  5 lab/w13.py on lab/w13/ (clauses.txt).
# The reading judges the tests' lines, not exit codes (on 23f03b6 W11b's legs and race fail where W4d's library pushes
# twice). A run counts only with its "W13" lines or its summary line. Exits 1 if a run lacks them, 2 if the script
# cannot start; the joiner's exit is logged, not folded in. W13_LAB, W13_LEGS, W13_RACE, W13_W11, W13_CONF, W13_W7,
# W13_FUZZER (the clone; go test -run patterns), W13_ROUNDS, W13_RACE_ROUNDS, W13_W7_ROUNDS, W13_FUZZ and
# W13_NOEXACT=1 (step 1 without tfbench and the digest) are for trying the script on a scratch clone with stand-in
# tests only.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
unset TF_CT TF_BG TF_NOCAS2 TF_EXP TF_DRIP_RACE_ROUNDS TF_DRIP_FRESH_ROUNDS TF_DRIP_ROUNDS TF_DRIP_MODES TF_DRIP_SHAPES
unset TF_STRESS TF_CFUZZ TF_CFUZZ_TAGS TF_STRESS_TAGS TF_CFUZZ_STRAGGLE TF_CFUZZ_SEED TF_CFUZZ_CASE TF_CFUZZ_RUNS
L=${W13_LAB:-$HOME/lab-sw}
O=$L/lab/w13
C=c8eab03 # W11b's reading: the library W13 adds its notes to
T=tfdrip,tfpush
LEGS=${W13_LEGS:-'^(TestDripHotRestoreFresh|TestPushPaths|TestPushNames)$'} RACE=${W13_RACE:-'^TestDripHotFreshRace$'}
W11=${W13_W11:-'^TestDripHotRestore(Legs|PushOnce)$'} CONF=${W13_CONF:-'^TestConformance$'}
W7=${W13_W7:-'^TestDripConcurrentCounts$'} FUZZER=${W13_FUZZER:-'^TestFuzzConcurrent$'}
ROUNDS=${W13_ROUNDS:-200000} RACE_ROUNDS=${W13_RACE_ROUNDS:-20000} W7_ROUNDS=${W13_W7_ROUNDS:-20000} FUZZ=${W13_FUZZ:-60m}
NOTES='go/cuckoo.go go/filter.go go/push.go go/push_off.go go/push_on.go go/touch.go '
git -C $L ls-files --error-unmatch lab/w13.sh lab/w13.py lab/w13-w4d.patch lab/w13-w4d-fuzz.patch go/push.go go/push_test.go >/dev/null 2>&1 &&
	[ -z "$(git -C $L status --porcelain -- go lab/w13.sh lab/w13.py lab/w13-w4d.patch lab/w13-w4d-fuzz.patch SW-RESULTS.md)" ] ||
	{ echo "the tree, lab/w13.sh, lab/w13.py or the ports are not committed as they are: commit them first"; exit 2; }
rm -rf $O && mkdir -p $O
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
h=$(git -C $L rev-parse --short HEAD)
log "W13: head $h; $(go version | cut -d' ' -f3)"
lib=$(git -C $L diff --name-only $C HEAD -- go ':!go/*_test.go' ':!go/verify' | tr '\n' ' ')
log "0: library files that differ from $C's: [$lib]"
[ "$lib" = "$NOTES" ] || { log "0: the library differs from $C's by more than the notes' files: stop"; exit 2; }
git -C $L diff $C HEAD -- go/touch.go go/cuckoo.go go/filter.go >$O/notes.patch
headOnly=$(diff <(grep -E '^[+-][^+-]' $O/notes.patch | sort) <(grep -E '^[+-][^+-]' $L/lab/w13-w4d.patch | sort) | grep -c '^<')
portOnly=$(diff <(grep -E '^[+-][^+-]' $O/notes.patch | sort) <(grep -E '^[+-][^+-]' $L/lab/w13-w4d.patch | sort) | grep -c '^>')
log "0: the notes' changed lines: head $(grep -cE '^[+-][^+-]' $O/notes.patch), the port $(grep -cE '^[+-][^+-]' $L/lab/w13-w4d.patch); the head's alone $headOnly, the port's alone $portOnly"
diff <(grep -E '^[+-][^+-]' $O/notes.patch | sort) <(grep -E '^[+-][^+-]' $L/lab/w13-w4d.patch | sort) | grep '^[<>]' | sed 's/^/    /' >>$O/progress.txt
W=$(mktemp -d)
trap 'for t in $W/*; do git -C $L worktree remove --force $t >/dev/null 2>&1; done; git -C $L worktree prune; rm -rf $W' EXIT
bad=0

# lines <file> <name> <pattern>: a run's lines in progress.txt, and whether it has any matching pattern
lines() {
	grep -E 'W1[13]b? |=== RUN|--- (PASS|FAIL|SKIP)|^(ok|FAIL|panic)|WARNING: DATA RACE|_test.go:[0-9]+: ' $1 | grep -v '=== RUN.*/' | sed 's/^ *//' | cut -c1-700 >>$O/progress.txt
	grep -qE "$3" $1 || { log "$2: NO LINES ($3)" && bad=1; }
}

log "1: the library's answers"
cd $L/go || exit 2
if [ "${W13_NOEXACT:-}" = 1 ]; then
	log "1: tfbench -exact and the digest skipped (W13_NOEXACT, a trial)"
else
	go run ./cmd/tfbench -exact >$O/exact-default.txt 2>&1
	cmp -s $O/exact-default.txt verify/expected/exact.txt && log "1a default exact: IDENTICAL" || log "1a default exact: DIFFER"
	TF_EXP=1 go test -count=1 -run '^TestAnswerDigest$' -v . | grep '^cell ' >$O/digest-default.txt
	cmp -s $O/digest-default.txt verify/expected/answer-digest.txt && log "1b default digest: IDENTICAL" || log "1b default digest: DIFFER"
	go run -tags tfdrip ./cmd/tfbench -exact >$O/exact-drip.txt 2>&1
	cmp -s $O/exact-drip.txt ../lab/w4e/exact-drip.txt && log "1c tfdrip exact: IDENTICAL to W4e's" || log "1c tfdrip exact: DIFFER from W4e's"
	go run -tags $T ./cmd/tfbench -exact >$O/exact-push.txt 2>&1
	cmp -s $O/exact-push.txt ../lab/w4e/exact-drip.txt && log "1d $T exact: IDENTICAL to W4e's" || log "1d $T exact: DIFFER from W4e's"
fi
go test -tags tfdrip -count=1 -run "$CONF" -v . >$O/conf-drip.txt 2>&1
log "1e TestConformance, tfdrip: go test exit $?"
go test -tags $T -count=1 -run "$CONF" -v . >$O/conf-push.txt 2>&1
log "1e TestConformance, $T: go test exit $?"
for tags in "" tfdrip tfpush $T; do
	go vet -tags "$tags" ./... >$O/vet.txt 2>&1
	log "1f go vet, tags [$tags]: exit $? ($(wc -l <$O/vet.txt) lines)"
	sed 's/^/    /' $O/vet.txt >>$O/progress.txt
done
rm -f $O/vet.txt

log "2: sight"
if git -C $L worktree add --detach -q $W/w4d 23f03b6 && (cd $W/w4d && git apply $L/lab/w11b/hook.patch && git apply $L/lab/w13-w4d.patch && git apply $L/lab/w13-w4d-fuzz.patch) &&
	cp $L/go/push.go $L/go/push_on.go $L/go/push_off.go $L/go/push_test.go $L/go/drip_hotfresh_test.go $W/w4d/go/; then
	d=$(git -C $W/w4d status --porcelain -- go | tr '\n' ' ')
	log "2: 23f03b6's port: [$d]"
	(cd $W/w4d/go && go test -tags $T -count=1 -run "$LEGS" -v -timeout 2m . >$O/legs-w4d.txt 2>&1)
	log "2 legs and paths on 23f03b6: go test exit $?"
	lines $O/legs-w4d.txt "2 legs and paths on 23f03b6" 'W13 (leg|path) '
	(cd $W/w4d/go && TF_DRIP_FRESH_ROUNDS=$ROUNDS go test -tags $T -count=1 -run "$RACE" -v -timeout 30m . >$O/race-w4d.txt 2>&1)
	log "2 race on 23f03b6, $ROUNDS rounds: go test exit $?"
	lines $O/race-w4d.txt "2 race on 23f03b6" 'W13 race: '
else
	log "2: no worktree of 23f03b6 with the hook and the ports" && bad=1
fi
go test -tags $T -count=1 -run "$LEGS" -v -timeout 2m . >$O/legs-head.txt 2>&1
log "2 legs and paths: go test exit $?"
lines $O/legs-head.txt "2 legs and paths" 'W13 (leg|path) '
race go test -race -tags $T -count=1 -run "$LEGS" -v -timeout 2m . >$O/legs-head-race.txt 2>&1
log "2 legs and paths under -race: go test exit $?"
lines $O/legs-head-race.txt "2 legs and paths under -race" 'W13 (leg|path) '
TF_DRIP_FRESH_ROUNDS=$ROUNDS go test -tags $T -count=1 -run "$RACE" -v -timeout 30m . >$O/race-head.txt 2>&1
log "2 race, $ROUNDS rounds: go test exit $?"
lines $O/race-head.txt "2 race" 'W13 race: '
race env TF_DRIP_FRESH_ROUNDS=$RACE_ROUNDS go test -race -tags $T -count=1 -run "$RACE" -v -timeout 30m . >$O/race-head-race.txt 2>&1
log "2 race under -race, $RACE_ROUNDS rounds: go test exit $?"
lines $O/race-head-race.txt "2 race under -race" 'W13 race: '
go test -tags $T -count=1 -run "$W11" -v -timeout 2m . >$O/w11-head.txt 2>&1
log "2 W11's legs and W4e's test: go test exit $?"
lines $O/w11-head.txt "2 W11's legs and W4e's test" 'W11 '

log "3: W7's scenario, Background, spread, $W7_ROUNDS rounds"
TF_EXP=1 TF_DRIP_MODES=bg TF_DRIP_SHAPES=spread TF_DRIP_ROUNDS=$W7_ROUNDS go test -tags $T -count=1 -run "$W7" -v -timeout 0 . >$O/w7-head.txt 2>&1
log "3 W7: go test exit $?"
lines $O/w7-head.txt "3 W7" 'W13 .*push counts: '

# fuzz <name> <dir> [env...]: the fuzzer for FUZZ in dir
fuzz() {
	local n=$1 d=$2
	shift 2
	(cd $d && env TF_CFUZZ=$FUZZ "$@" go test -tags $T -count=1 -run "$FUZZER" -v -timeout 0 . >$O/$n.txt 2>&1)
	log "4 $n: go test exit $?"
	grep -E 'cases from seed|^(ok|FAIL|panic)|WARNING: DATA RACE|--- (PASS|FAIL|SKIP)' $O/$n.txt | sed 's/^ *//' | cut -c1-300 >>$O/progress.txt
	grep -q 'W13: push counts: ' $O/$n.txt || { log "4 $n: NO SUMMARY WITH PUSH COUNTS" && bad=1; }
}
log "4: the fuzzer, $FUZZ each"
fuzz cfuzz-head $L/go
fuzz cfuzz-head-straggle $L/go TF_CFUZZ_STRAGGLE=1
fuzz cfuzz-head-tags $L/go TF_CFUZZ_TAGS=8
if [ -d $W/w4d/go ]; then
	fuzz cfuzz-w4d-straggle $W/w4d/go TF_CFUZZ_STRAGGLE=1
else
	log "4: no worktree of 23f03b6 for the power leg" && bad=1
fi

log "5: lab/w13.py"
python3 -I $L/lab/w13.py $O >$O/clauses.txt 2>&1
log "5: lab/w13.py exit $? (clauses.txt)"
log "W13 readings finished (exit $bad)"
exit $bad
