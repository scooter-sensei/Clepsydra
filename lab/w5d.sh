#!/bin/bash
# lab/sw W5d's readings (SW-RESULTS.md). W4d's tree is ~/lab-sw, W5d's ~/lab-sw-w5b.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
O=$HOME/lab-sw-out/w5d; R=$HOME/lab-sw-out/w4d-ref
mkdir -p $O $R
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
digest() { TF_EXP=1 go test "$@" -count=1 -run '^TestAnswerDigest$' -v . | grep '^cell '; }
fails() { grep -E '^--- FAIL' "$1" | awk '{print $3}' | sort | tr '\n' ' '; }
cd $HOME/lab-sw/go
go run -tags tfdrip ./cmd/tfbench -exact >$R/exact-drip.txt 2>&1
digest -tags tfdrip >$R/digest-drip.txt
for m in plain TF_CT=1 TF_BG=1 TF_NOCAS2=1; do e=""; [ $m = plain ] || e=$m; env $e go test -tags tfdrip -short -count=1 ./... >$R/unit-drip-$m.txt 2>&1; done
cd $HOME/lab-sw-w5b/go
log "1: default build"
go run ./cmd/tfbench -exact >$O/exact-default.txt 2>&1
cmp -s $O/exact-default.txt verify/expected/exact.txt && log "1 exact: IDENTICAL" || log "1 exact: DIFFER"
digest >$O/digest-default.txt
cmp -s $O/digest-default.txt verify/expected/answer-digest.txt && log "1 digest: IDENTICAL" || log "1 digest: DIFFER"
log "2: tfdrip against W4d's"
go run -tags tfdrip ./cmd/tfbench -exact >$O/exact-drip.txt 2>&1
diff $R/exact-drip.txt $O/exact-drip.txt >$O/exact-diff.txt; log "2 exact: $(grep -c '^<' $O/exact-diff.txt) lines differ"
digest -tags tfdrip >$O/digest-drip.txt
cmp -s $O/digest-drip.txt $R/digest-drip.txt && log "2 digest: IDENTICAL" || log "2 digest: DIFFER"
log "3: translation"
TF_TICK_OFFSET=1200000 digest -tags tfdrip >$O/digest-ref.txt
for K in 566935680000 1013612280000 1460288880000; do
	TF_TICK_OFFSET=$K digest -tags tfdrip >$O/digest-K$K.txt
	cmp -s $O/digest-K$K.txt $O/digest-ref.txt && log "3 K=$K: IDENTICAL" || log "3 K=$K: DIFFER"
done
log "4: lockstep with a fast clock"
for K in 17175600000 34355520000 51535320000 85895040000 120254880000; do
	TF_EXP=1 TF_TICK_SPEED=30 TF_TICK_BASE=1200000 TF_TICK_OFFSET=$K go test -tags tfdrip -count=1 -run '^TestWrapTranslateDiverge$' -v . >$O/lockstep-$K.txt 2>&1
	log "4 K=$K: $(grep -E 'wrap_test' $O/lockstep-$K.txt | sed 's/^ *//' | head -1) $(tail -1 $O/lockstep-$K.txt)"
done
log "5: $(go test -tags tfdrip -count=1 -run '^(TestWrapAlias|TestWrapDeadlineEntries)$' . 2>&1 | tail -1)"
log "6: suites"
for m in plain TF_CT=1 TF_BG=1 TF_NOCAS2=1; do
	e=""; [ $m = plain ] || e=$m
	env $e go test -tags tfdrip -short -count=1 ./... >$O/unit-drip-$m.txt 2>&1
	log "6 $m: $(fails $O/unit-drip-$m.txt)| W4d's: $(fails $R/unit-drip-$m.txt)"
done
log "7: soak and fuzzer across a wrap, under -race"
race env TF_STRESS=5m TF_STRESS_CLOCK_BASE=$(( (1<<34) - 10000000 )) go test -race -tags tfdrip -count=1 -run '^TestStressSoak$' -v -timeout 0 . >$O/soak-wrap.txt 2>&1
log "7 soak: $(tail -1 $O/soak-wrap.txt) $(grep -o '[0-9]* ops, [0-9]* counts checked[^;]*' $O/soak-wrap.txt | head -1)"
race env TF_CFUZZ=5m TF_CFUZZ_CLOCK_BASE=$(( (1<<34) - (1<<29) )) TF_CFUZZ_JUMP=1 go test -race -tags tfdrip -count=1 -run '^TestFuzzConcurrent$' -v -timeout 0 . >$O/cfuzz-wrap.txt 2>&1
log "7 cfuzz: $(tail -1 $O/cfuzz-wrap.txt) $(grep -o '[0-9]* cases from[^;]*' $O/cfuzz-wrap.txt | head -1)"
log "8: the catch-up stall (reported)"
TF_EXP=1 go test -tags tfdrip -count=1 -run '^TestWrapCatchUpStall$' -v -timeout 0 . >$O/stall.txt 2>&1
log "8: $(grep -E 'first call' $O/stall.txt | sed 's/^ *wrap_test.go:[0-9]*: //' | tr '\n' ';')"
log "9: speed (reported)"
go test -c -o $O/def.test . && go test -tags tfdrip -c -o $O/w5d.test . && (cd $HOME/lab-sw/go && go test -tags tfdrip -c -o $O/w4d.test .)
B='^Benchmark(CheckAndTouchHit|CheckAndTouchHitHot|CheckAndTouchMissInsert|TouchAndCount|VelocityCount|VelocityCountHot|ContainsAbsent|CheckAndTouchBatch)$'
for r in 1 2 3; do for b in def w4d w5d; do $O/$b.test -test.run '^$' -test.bench "$B" -test.benchtime 1s >>$O/bench-$b.txt 2>&1; done; done
log "W5d readings finished"
