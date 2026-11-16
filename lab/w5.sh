#!/bin/bash
# lab/sw W5a's and W5b's readings (SW-RESULTS.md). Usage: w5.sh a|b
# W4c's tree is ~/lab-sw (lab/sw-2026-11), W5a's ~/lab-sw-w5, W5b's ~/lab-sw-w5b.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
arm=$1
O=$HOME/lab-sw-out/w5$arm
mkdir -p $O
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
digest() { TF_EXP=1 go test "$@" -count=1 -run '^TestAnswerDigest$' -v . | grep '^cell '; }
fails() { grep -E '^--- FAIL' "$1" | awk '{print $3}' | sort | tr '\n' ' '; }
W4C=$HOME/lab-sw/go
if [ $arm = a ]; then T=$HOME/lab-sw-w5/go; else T=$HOME/lab-sw-w5b/go; fi

# W4c's tree: the tfdrip build's references (computed once)
if [ ! -f $HOME/lab-sw-out/w4c-ref/digest-drip.txt ]; then
	mkdir -p $HOME/lab-sw-out/w4c-ref
	cd $W4C && go run -tags tfdrip ./cmd/tfbench -exact >$HOME/lab-sw-out/w4c-ref/exact-drip.txt 2>&1
	digest -tags tfdrip >$HOME/lab-sw-out/w4c-ref/digest-drip.txt
	for m in plain TF_CT=1 TF_BG=1 TF_NOCAS2=1; do
		e=""; [ $m = plain ] || e=$m
		env $e go test -tags tfdrip -short -count=1 ./... >$HOME/lab-sw-out/w4c-ref/unit-drip-$m.txt 2>&1
	done
fi
R=$HOME/lab-sw-out/w4c-ref
cd $T || exit 1

log "1: default build"
go run ./cmd/tfbench -exact >$O/exact-default.txt 2>&1
cmp -s $O/exact-default.txt verify/expected/exact.txt && log "1 exact: IDENTICAL" || log "1 exact: DIFFER"
digest >$O/digest-default.txt
cmp -s $O/digest-default.txt verify/expected/answer-digest.txt && log "1 digest: IDENTICAL" || log "1 digest: DIFFER"
if [ $arm = a ]; then
	./test.sh regression >$O/regression.txt 2>&1 && log "1 regression tier: PASS" || log "1 regression tier: FAIL ($(grep -iE 'fail' $O/regression.txt | head -3 | tr '\n' ' '))"
fi

log "2: tfdrip against W4c's"
go run -tags tfdrip ./cmd/tfbench -exact >$O/exact-drip.txt 2>&1
cmp -s $O/exact-drip.txt $R/exact-drip.txt && log "2 exact: IDENTICAL" || log "2 exact: DIFFER"
digest -tags tfdrip >$O/digest-drip.txt
cmp -s $O/digest-drip.txt $R/digest-drip.txt && log "2 digest: IDENTICAL" || log "2 digest: DIFFER"

if [ $arm = b ]; then
	log "3: translation"
	for K in 566935680000 1013612280000 1460288880000; do
		TF_TICK_OFFSET=$K digest -tags tfdrip >$O/digest-drip-K$K.txt
		cmp -s $O/digest-drip-K$K.txt $O/digest-drip.txt && log "3 K=$K: IDENTICAL" || { log "3 K=$K: DIFFER"; diff $O/digest-drip.txt $O/digest-drip-K$K.txt | head -6 >>$O/progress.txt; }
	done
	log "4: TestWrapAlias: $(go test -tags tfdrip -count=1 -run '^TestWrapAlias$' . 2>&1 | tail -1)"
fi

log "5: unit suites"
if [ $arm = a ]; then
	go test -short -count=1 ./... >$O/unit-default.txt 2>&1
	log "5 default: failing: $(fails $O/unit-default.txt)"
fi
for m in plain TF_CT=1 TF_BG=1 TF_NOCAS2=1; do
	e=""; [ $m = plain ] || e=$m
	env $e go test -tags tfdrip -short -count=1 ./... >$O/unit-drip-$m.txt 2>&1
	log "5 tfdrip $m: failing: $(fails $O/unit-drip-$m.txt)| W4c's: $(fails $R/unit-drip-$m.txt)"
done

if [ $arm = b ]; then
	log "6: soak and concurrent fuzzer under -race, 5 minutes each"
	race env TF_STRESS=5m go test -race -tags tfdrip -count=1 -run '^TestStressSoak$' -v -timeout 0 . >$O/soak-race.txt 2>&1
	log "6 soak: $(tail -1 $O/soak-race.txt) $(grep -o '[0-9]* ops, [0-9]* counts checked[^;]*' $O/soak-race.txt | head -1)"
	race env TF_CFUZZ=5m go test -race -tags tfdrip -count=1 -run '^TestFuzzConcurrent$' -v -timeout 0 . >$O/cfuzz-race.txt 2>&1
	log "6 cfuzz: $(tail -1 $O/cfuzz-race.txt)"
	log "7: speed (reported): default, W4c tfdrip, W5b tfdrip; 3 interleaved rounds"
	go test -c -o $O/def.test . && go test -tags tfdrip -c -o $O/w5b.test . && (cd $W4C && go test -tags tfdrip -c -o $O/w4c.test .)
	B='^Benchmark(CheckAndTouchHit|CheckAndTouchHitHot|CheckAndTouchMissInsert|TouchAndCount|VelocityCount|VelocityCountHot|ContainsAbsent|CheckAndTouchBatch)$'
	for r in 1 2 3; do
		for b in def w4c w5b; do
			$O/$b.test -test.run '^$' -test.bench "$B" -test.benchtime 1s >>$O/bench-$b.txt 2>&1
		done
	done
fi
log "W5$arm readings finished"
