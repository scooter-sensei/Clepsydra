#!/bin/bash
# lab/sw W12's readings (SW-RESULTS.md, Phase 12): outputs in lab/w12/ (cleared). W12 changes no library file:
# the head's library equals 828524f's, which this script checks. Tests: go/swlag_test.go (TF_EXP=1).
#  1 sight: TestShortWindowLagClassify, the default build and tfdrip;
#  2 leg A: TestShortWindowLag, TF_W12_LEG=a (five seeds, every delay), both builds;
#  3 leg B: TestShortWindowLag, TF_W12_LEG=b ((9,9), every delay, plain, Background, ConstantTime; owned keys
#    once, shared keys three times), both builds; and under -race, tfdrip, (9,9), jitter:1000, plain, the shared
#    keys once;
#  4 lab/w12.py: the tables and clauses 2 to 5, from both builds' lines.
# The reading judges the lines, not exit codes. A run counts only with its "W12 done:" line (the classify test:
# its "W12 classify" lines). Exits 1 if a run lacks them, 2 if the script cannot start. W12_LAB, W12_RUN,
# W12_CLS (the clone; go test -run patterns), W12_SEEDS, W12_DELAYS, W12_MODES and W12_REPEATS are for trying
# the script on a scratch clone only.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
unset TF_CT TF_BG TF_NOCAS2 TF_EXP TF_W12_LEG TF_W12_SEEDS TF_W12_DELAYS TF_W12_MODES TF_W12_REPEATS
L=${W12_LAB:-$HOME/lab-sw}
O=$L/lab/w12
P=828524f # W12's parent
RUN=${W12_RUN:-'^TestShortWindowLag$'} CLS=${W12_CLS:-'^TestShortWindowLagClassify$'}
git -C $L ls-files --error-unmatch lab/w12.sh lab/w12.py go/swlag_test.go >/dev/null 2>&1 &&
	[ -z "$(git -C $L status --porcelain -- go lab/w12.sh lab/w12.py SW-RESULTS.md)" ] ||
	{ echo "the tree or lab/w12.sh is not committed as it is: commit it first"; exit 2; }
rm -rf $O && mkdir -p $O
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
h=$(git -C $L rev-parse --short HEAD)
log "W12: head $h; $(go version | cut -d' ' -f3)"
lib=$(git -C $L diff --name-only $P HEAD -- go ':!go/*_test.go' | tr '\n' ' ')
log "0: library files that differ from $P's: [$lib]"
[ -z "$lib" ] || { log "0: the library differs from $P's: stop"; exit 2; }
cd $L/go || exit 2
export TF_EXP=1
for v in SEEDS DELAYS MODES REPEATS; do
	eval "x=\${W12_$v:-}"
	[ -n "$x" ] && export TF_W12_$v="$x"
done
bad=0

# lines <file> <name> <pattern>: the run's W12 lines into progress.txt, and whether it has its pattern
lines() {
	grep -E 'W12 (classify|done)|--- (PASS|FAIL)|^(ok|FAIL|panic)|WARNING: DATA RACE|swlag_test.go:[0-9]+: [a-zA-Z]' $1 |
		grep -v 'W12 run:' | sed 's/^ *//' | cut -c1-400 >>$O/progress.txt
	grep -q "$3" $1 || { log "$2: NO W12 LINES" && bad=1; }
}

log "1: TestShortWindowLagClassify"
for b in default drip; do
	tags=; [ $b = drip ] && tags="-tags tfdrip"
	go test $tags -count=1 -run "$CLS" -v -timeout 2m . >$O/classify-$b.txt 2>&1
	log "1 $b: go test exit $?"
	lines $O/classify-$b.txt "1 $b" 'W12 classify'
done

log "2: leg A"
for b in default drip; do
	tags=; [ $b = drip ] && tags="-tags tfdrip"
	TF_W12_LEG=a go test $tags -count=1 -run "$RUN" -v -timeout 60m . >$O/a-$b.txt 2>&1
	log "2 $b: go test exit $?; $(grep -c 'W12 run:' $O/a-$b.txt) runs"
	lines $O/a-$b.txt "2 $b" 'W12 done:'
done

log "3: leg B"
for b in default drip; do
	tags=; [ $b = drip ] && tags="-tags tfdrip"
	TF_W12_LEG=b go test $tags -count=1 -run "$RUN" -v -timeout 90m . >$O/b-$b.txt 2>&1
	log "3 $b: go test exit $?; $(grep -c 'W12 run:' $O/b-$b.txt) runs"
	lines $O/b-$b.txt "3 $b" 'W12 done:'
done
TF_W12_LEG=b TF_W12_DELAYS=jitter:1000 TF_W12_MODES=plain TF_W12_REPEATS=1 race go test -race -tags tfdrip -count=1 -run "$RUN" -v -timeout 60m . >$O/b-race.txt 2>&1
log "3 under -race: go test exit $?; $(grep -c 'W12 run:' $O/b-race.txt) runs; data races reported: $(grep -c 'WARNING: DATA RACE' $O/b-race.txt)"
lines $O/b-race.txt "3 under -race" 'W12 done:'

log "4: lab/w12.py"
python3 -I $L/lab/w12.py $O $L/go/verify/expected/exact.txt $L/lab/w4e/exact-drip.txt >$O/tables.txt 2>&1
log "4: exit $?"
grep -E '^W12 clause|MISSING|NO W12' $O/tables.txt | tee -a $O/progress.txt
log "W12 readings finished (exit $bad)"
exit $bad
