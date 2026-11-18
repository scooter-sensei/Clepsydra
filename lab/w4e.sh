#!/bin/bash
# lab/sw W4e's readings (SW-RESULTS.md): outputs in lab/w4e/ (cleared). The head is W4e's commit; its parent
# has W4d's library (b7a33a7's). Clause 1 runs the head's go/drip_hot_test.go on a detached worktree of the
# parent; the rest run in ~/lab-sw/go, as lab/w4d.sh did (lab/w4.sh's steps 1 and 5, W4d's 5b and 5c).
# W4E_LAB, W4E_RUN and W4E_STOP (the clone, clauses 1-2's -run pattern, stop after clause 2) are for trying
# the script on a scratch clone only.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
unset TF_CT TF_BG TF_NOCAS2
L=${W4E_LAB:-$HOME/lab-sw}
export O=$L/lab/w4e
[ -z "$(git -C $L status --porcelain -- go lab/w4e.sh lab/w4.sh SW-RESULTS.md)" ] || { echo "the tree differs from the commit: commit it first"; exit 2; }
git -C $L ls-files --error-unmatch go/drip_hot_test.go lab/w4e.sh >/dev/null 2>&1 || { echo "go/drip_hot_test.go or lab/w4e.sh is not committed"; exit 2; }
rm -rf $O && mkdir -p $O
cd $L/go || exit 2
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
T=${W4E_RUN:-'^TestDripHotRestorePushOnce$'}
h=$(git -C $L rev-parse --short HEAD) p=$(git -C $L rev-parse --short HEAD^)
lib=$(git -C $L diff --quiet b7a33a7 $p -- go ':!go/*_test.go' ':!go/verify' && echo "b7a33a7's (W4d)" || echo "NOT b7a33a7's")
log "W4e: head $h, parent $p with library $lib; $(go version | cut -d' ' -f3)"

log "1: sight, the head's TestDripHotRestorePushOnce on the parent (W4d's library)"
W=$(mktemp -d) rc="no run"
if git -C $L worktree add --detach -q $W/t $p && cp drip_hot_test.go $W/t/go/drip_hot_test.go; then
	(cd $W/t/go && go test -tags tfdrip -count=1 -run "$T" -v . >$O/sight.txt 2>&1)
	rc=$?
fi
log "1: go test exit $rc (want 1, the test failing on W4d's library); $(grep -cE -- '--- FAIL' $O/sight.txt 2>/dev/null) FAIL lines"
grep -E 'drip_hot_test|--- (PASS|FAIL)' $O/sight.txt | sed 's/^ *//' >>$O/progress.txt
git -C $L worktree remove --force $W/t; git -C $L worktree prune; rm -rf $W

log "2: the mend, TestDripHotRestorePushOnce on the head"
go test -tags tfdrip -count=1 -run "$T" -v . >$O/mend.txt 2>&1
log "2: exit $? (want 0)"
grep -E 'drip_hot_test|--- (PASS|FAIL)' $O/mend.txt | sed 's/^ *//' >>$O/progress.txt
[ -z "${W4E_STOP:-}" ] || { log "stopped after clause 2 (W4E_STOP)"; exit 0; }

bash ../lab/w4.sh 1 # 3: default build unchanged (exact, digest)

log "4: tfdrip tfbench -exact against W4d's reading's"
go run -tags tfdrip ./cmd/tfbench -exact >$O/exact-drip.txt 2>&1
if cmp -s $O/exact-drip.txt ../lab/w4d/exact-drip.txt; then log "4: IDENTICAL"; else log "4: DIFFER"; diff ../lab/w4d/exact-drip.txt $O/exact-drip.txt | head -20 >>$O/progress.txt; fi

log "5b: TestBackgroundConcurrentCounts, 200 runs with tfdrip, 20 under -race"
go test -tags tfdrip -c -o $O/bgc.test . && fails=0
for i in $(seq 1 200); do $O/bgc.test -test.run '^TestBackgroundConcurrentCounts$' -test.count=1 >$O/bgc.out 2>&1 || { fails=$((fails+1)); cp $O/bgc.out $O/bgc-fail-$i.txt; }; done
log "5b: $fails of 200 fail"
race go test -race -tags tfdrip -c -o $O/bgc-race.test . && fails=0
for i in $(seq 1 20); do $O/bgc-race.test -test.run '^TestBackgroundConcurrentCounts$' -test.count=1 >$O/bgcr.out 2>&1 || { fails=$((fails+1)); cp $O/bgcr.out $O/bgcr-fail-$i.txt; }; done
log "5b race: $fails of 20 fail"
rm -f $O/bgc.test $O/bgc-race.test $O/bgc.out $O/bgcr.out
log "5c: TestDripPushOnce, 5000 rounds, and 500 under -race"
TF_DRIP_ROUNDS=5000 go test -tags tfdrip -count=1 -run '^TestDripPushOnce$' -v . >$O/probe.txt 2>&1
log "5c: $(grep -E 'logs hold' $O/probe.txt)"
race env TF_DRIP_ROUNDS=500 go test -race -tags tfdrip -count=1 -run '^TestDripPushOnce$' -v . >$O/probe-race.txt 2>&1
log "5c race: $(grep -E 'logs hold' $O/probe-race.txt) $(tail -1 $O/probe-race.txt)"
bash ../lab/w4.sh 5 # 5: the suites in four modes, TestSlotUnitRaces, soak and concurrent fuzzer under -race
for m in plain TF_CT=1 TF_BG=1 TF_NOCAS2=1; do
	grep -A4 -E -- '--- FAIL: TestMerge' $O/unit-$m.txt | head -6 | sed "s/^/  unit-$m: /" >>$O/progress.txt
done
log "W4e readings finished"
