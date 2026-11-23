#!/bin/bash
# lab/sw W14's reading (SW-RESULTS.md, W14): outputs in lab/w14/ (cleared). W14 changes no library file:
# the head's library equals 495f1aa's, which this script checks. Tests: go/swload_test.go (TF_EXP=1).
#  1 sight: TestShortWindowLoadWhere and TestShortWindowLoadSource, the default build and tfdrip;
#  2 check: TestShortWindowLoad, TF_W14_LEG=check ((9,9): the churn's prefix; the watcher off; ConstantTime's
#    replay), both builds;
#  3 leg 1: TF_W14_LEG=1 (five seeds; no churn, 1.00, 1.15; plain, Background five times, ConstantTime), both builds;
#  4 leg 2: TF_W14_LEG=2 (five seeds; Capacity 1,739, 1,000, 500; the same modes), both builds;
#  5 under -race, tfdrip, (9,9), leg 2 at Capacity 500 in Background, once;
#  6 lab/w14.py: the tables and clauses 1 to 5, from both builds' lines.
# The reading judges the lines, not exit codes. A run counts only with its "W14 done:" line (the sight: its
# "W14 where" lines). Exits 1 if a run lacks them, 2 if the script cannot start. W14_LAB, W14_RUN, W14_SIGHT
# (the clone; go test -run patterns), W14_SEEDS, W14_LOADS, W14_CAPS, W14_MODES and W14_REPEATS are for trying
# the script on a scratch clone only.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
unset TF_CT TF_BG TF_NOCAS2 TF_EXP TF_W14_LEG TF_W14_SEEDS TF_W14_LOADS TF_W14_CAPS TF_W14_MODES TF_W14_REPEATS TF_W14_QUIET
L=${W14_LAB:-$HOME/lab-sw}
O=$L/lab/w14
P=495f1aa # W14's parent
RUN=${W14_RUN:-'^TestShortWindowLoad$'} SIGHT=${W14_SIGHT:-'^TestShortWindowLoad(Where|Source)$'}
git -C $L ls-files --error-unmatch lab/w14.sh lab/w14.py go/swload_test.go >/dev/null 2>&1 &&
	[ -z "$(git -C $L status --porcelain -- go lab/w14.sh lab/w14.py lab/w4e/exact-drip.txt SW-RESULTS.md)" ] ||
	{ echo "the tree or lab/w14.sh is not committed as it is: commit it first"; exit 2; }
rm -rf $O && mkdir -p $O
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
h=$(git -C $L rev-parse --short HEAD)
log "W14: head $h; $(go version | cut -d' ' -f3)"
lib=$(git -C $L diff --name-only $P HEAD -- go ':!go/*_test.go' | tr '\n' ' ')
log "0: library files that differ from $P's: [$lib]"
[ -z "$lib" ] || { log "0: the library differs from $P's: stop"; exit 2; }
cd $L/go || exit 2
export TF_EXP=1
for v in SEEDS LOADS CAPS MODES REPEATS; do
	eval "x=\${W14_$v:-}"
	[ -n "$x" ] && export TF_W14_$v="$x"
done
bad=0

# lines <file> <name> <pattern>: the run's W14 lines into progress.txt, and whether it has its pattern
lines() {
	grep -E 'W14 (where|check|done)|--- (PASS|FAIL)|^(ok|FAIL|panic)|WARNING: DATA RACE|swload_test.go:[0-9]+: [a-zA-Z]' $1 |
		grep -v 'W14 run:' | sed 's/^ *//' | cut -c1-400 >>$O/progress.txt
	grep -q "$3" $1 || { log "$2: NO W14 LINES" && bad=1; }
}

log "1: sight"
for b in default drip; do
	tags=; [ $b = drip ] && tags="-tags tfdrip"
	go test $tags -count=1 -run "$SIGHT" -v -timeout 5m . >$O/where-$b.txt 2>&1
	log "1 $b: go test exit $?"
	lines $O/where-$b.txt "1 $b" 'W14 where'
done

log "2: check"
for b in default drip; do
	tags=; [ $b = drip ] && tags="-tags tfdrip"
	TF_W14_LEG=check go test $tags -count=1 -run "$RUN" -v -timeout 30m . >$O/check-$b.txt 2>&1
	log "2 $b: go test exit $?"
	lines $O/check-$b.txt "2 $b" 'W14 done:'
done

for n in 1 2; do
	log "$((n + 2)): leg $n"
	for b in default drip; do
		tags=; [ $b = drip ] && tags="-tags tfdrip"
		TF_W14_LEG=$n go test $tags -count=1 -run "$RUN" -v -timeout 60m . >$O/l$n-$b.txt 2>&1
		log "$((n + 2)) $b: go test exit $?; $(grep -c 'W14 run:' $O/l$n-$b.txt) runs"
		lines $O/l$n-$b.txt "$((n + 2)) $b" 'W14 done:'
	done
done

log "5: under -race"
TF_W14_LEG=2 TF_W14_SEEDS=9,9 TF_W14_CAPS=500 TF_W14_MODES=bg TF_W14_REPEATS=1 \
	race go test -race -tags tfdrip -count=1 -run "$RUN" -v -timeout 60m . >$O/race.txt 2>&1
log "5: go test exit $?; $(grep -c 'W14 run:' $O/race.txt) runs; data races reported: $(grep -c 'WARNING: DATA RACE' $O/race.txt)"
lines $O/race.txt "5" 'W14 done:'

log "6: lab/w14.py"
python3 -I $L/lab/w14.py $O $L/go/verify/expected/exact.txt $L/lab/w4e/exact-drip.txt >$O/tables.txt 2>&1
log "6: exit $?"
grep -E '^W14 clause|MISSING|NO W14' $O/tables.txt | tee -a $O/progress.txt
log "W14 reading finished (exit $bad)"
exit $bad
