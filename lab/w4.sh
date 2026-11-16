#!/bin/bash
# lab/sw W4's readings (SW-RESULTS.md): run from anywhere; outputs in ~/lab-sw-out/w4/.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
O=$HOME/lab-sw-out/w4
mkdir -p $O
cd $HOME/lab-sw/go || exit 1
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
step=${1:-all}

if [ $step = all ] || [ $step = 1 ]; then
	log "1: default build unchanged"
	go run ./cmd/tfbench -exact >$O/exact-default.txt 2>&1
	if cmp -s $O/exact-default.txt verify/expected/exact.txt; then log "1a exact: IDENTICAL"; else log "1a exact: DIFFER"; fi
	TF_EXP=1 go test -count=1 -run '^TestAnswerDigest$' -v . | grep '^cell ' >$O/digest-default.txt
	if cmp -s $O/digest-default.txt verify/expected/answer-digest.txt; then log "1b digest: IDENTICAL"; else log "1b digest: DIFFER"; fi
fi

if [ $step = all ] || [ $step = 2 ]; then
	log "2: tfdrip velocity"
	go run -tags tfdrip ./cmd/tfbench -exact >$O/exact-drip.txt 2>&1
	diff $O/exact-default.txt $O/exact-drip.txt >$O/exact-diff.txt
	log "2a tfbench -exact lines that differ: $(grep -c '^<' $O/exact-diff.txt)"
	TF_EXP=1 TF_SW_FILTER='B12 rice0 r=u/16' go test -tags tfdrip -count=1 -run '^TestShortWindowEncodings$' -v . >$O/w2-drip.txt 2>&1
	TF_EXP=1 go test -tags tfdrip -count=1 -run '^TestShortWindowRobust$' -v . >$O/w3-drip.txt 2>&1
	log "2b done"
fi

if [ $step = all ] || [ $step = 3 ]; then
	log "3: digest without Count"
	rm -rf $O/nc && mkdir -p $O/nc && cp -r $HOME/lab-sw/go $O/nc/go
	cd $O/nc/go
	sed -i 's#x.Seen, x.Count, x.Upper#x.Seen, 0, x.Upper#; s#put("tcbatch", fmt.Sprint(out, counts))#put("tcbatch", fmt.Sprint(out))#' answers_test.go
	grep -q 'x.Seen, 0, x.Upper' answers_test.go && grep -q 'fmt.Sprint(out))' answers_test.go || log "3: sed failed"
	TF_EXP=1 go test -count=1 -run '^TestAnswerDigest$' -v . | grep '^cell ' >$O/nc-default.txt
	TF_EXP=1 go test -tags tfdrip -count=1 -run '^TestAnswerDigest$' -v . | grep '^cell ' >$O/nc-drip.txt
	if cmp -s $O/nc-default.txt $O/nc-drip.txt; then log "3: IDENTICAL ($(wc -l <$O/nc-default.txt) cells)"; else log "3: DIFFER"; diff $O/nc-default.txt $O/nc-drip.txt | head -20 >>$O/progress.txt; fi
	cd $HOME/lab-sw/go
fi

if [ $step = all ] || [ $step = 4 ]; then
	log "4: Upper never below the truth"
	go test -tags tfdrip -count=1 -run '^(TestAttackHeavyHitter|TestUpperBoundRandom)$' -v . >$O/upper-drip.txt 2>&1
	log "4: $(grep -E '^(--- |ok|FAIL)' $O/upper-drip.txt | tr '\n' ' ')"
fi

if [ $step = all ] || [ $step = 5 ]; then
	log "5: suites with tfdrip"
	for m in plain TF_CT=1 TF_BG=1 TF_NOCAS2=1; do
		e=""; [ $m = plain ] || e=$m
		env $e go test -tags tfdrip -short -count=1 ./... >$O/unit-$m.txt 2>&1
		log "5 unit $m: $(grep -cE '^--- FAIL' $O/unit-$m.txt) failing: $(grep -E '^--- FAIL' $O/unit-$m.txt | awk '{print $3}' | tr '\n' ' ')"
	done
	go test -tags tfdrip -count=1 -run '^TestSlotUnitRaces$' -v . >$O/slotunit.txt 2>&1
	log "5 slotunit: $(tail -1 $O/slotunit.txt)"
	race go test -race -tags tfdrip -count=1 -run '^TestSlotUnitRaces$' . >$O/slotunit-race.txt 2>&1
	log "5 slotunit race: $(tail -1 $O/slotunit-race.txt)"
	race env TF_STRESS=5m go test -race -tags tfdrip -count=1 -run '^TestStressSoak$' -v -timeout 0 . >$O/soak-race.txt 2>&1
	log "5 soak race: $(tail -1 $O/soak-race.txt)"
	race env TF_CFUZZ=5m go test -race -tags tfdrip -count=1 -run '^TestFuzzConcurrent$' -v -timeout 0 . >$O/cfuzz-race.txt 2>&1
	log "5 cfuzz race: $(tail -1 $O/cfuzz-race.txt)"
fi

if [ $step = all ] || [ $step = 6 ]; then
	log "6: speed (3 interleaved rounds)"
	go test -c -o $O/def.test . && go test -tags tfdrip -c -o $O/drip.test .
	B='^Benchmark(CheckAndTouchHit|CheckAndTouchHitHot|CheckAndTouchMissInsert|TouchAndCount|VelocityCount|VelocityCountHot)$'
	for r in 1 2 3; do
		for b in def drip; do
			$O/$b.test -test.run '^$' -test.bench "$B" -test.benchtime 1s >>$O/bench-$b.txt 2>&1
		done
	done
	log "6 done"
fi
log "W4 readings finished ($step)"
