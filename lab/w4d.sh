#!/bin/bash
# lab/sw W4d's readings (SW-RESULTS.md): outputs in ~/lab-sw-out/w4c/.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
export O=$HOME/lab-sw-out/w4d
mkdir -p $O
cd $HOME/lab-sw/go || exit 1
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
bash ../lab/w4.sh 1; bash ../lab/w4.sh 2; bash ../lab/w4.sh 3; bash ../lab/w4.sh 4
log "2c: the 11-bit model on both seeds"
TF_EXP=1 TF_SW_FILTER='B11 rice0 r=u/16' go test -tags tfdrip -count=1 -run '^TestShortWindowEncodings$' -v . >$O/w2-b11.txt 2>&1
TF_EXP=1 go test -tags tfdrip -count=1 -run '^TestShortWindowInformation$' -v . 2>&1 | grep -E 'filter  ' >$O/filter-drip-seeds.txt
log "5b: TestBackgroundConcurrentCounts, 200 runs with tfdrip, 20 under -race"
go test -tags tfdrip -c -o $O/bgc.test . && fails=0
for i in $(seq 1 200); do $O/bgc.test -test.run '^TestBackgroundConcurrentCounts$' -test.count=1 >$O/bgc.out 2>&1 || { fails=$((fails+1)); cp $O/bgc.out $O/bgc-fail-$i.txt; }; done
log "5b: $fails of 200 fail"
race go test -race -tags tfdrip -c -o $O/bgc-race.test . && fails=0
for i in $(seq 1 20); do $O/bgc-race.test -test.run '^TestBackgroundConcurrentCounts$' -test.count=1 >$O/bgcr.out 2>&1 || { fails=$((fails+1)); cp $O/bgcr.out $O/bgcr-fail-$i.txt; }; done
log "5b race: $fails of 20 fail"
log "5c: TestDripPushOnce, 5000 rounds, and 500 under -race"
TF_DRIP_ROUNDS=5000 go test -tags tfdrip -count=1 -run '^TestDripPushOnce$' -v . >$O/probe.txt 2>&1
log "5c: $(grep -E 'logs hold' $O/probe.txt)"
race env TF_DRIP_ROUNDS=500 go test -race -tags tfdrip -count=1 -run '^TestDripPushOnce$' -v . >$O/probe-race.txt 2>&1
log "5c race: $(grep -E 'logs hold' $O/probe-race.txt) $(tail -1 $O/probe-race.txt)"
bash ../lab/w4.sh 5; bash ../lab/w4.sh 7; bash ../lab/w4.sh 6; bash ../lab/w4.sh 9
log "W4d readings finished"
