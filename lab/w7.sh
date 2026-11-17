#!/bin/bash
# lab/sw W7's readings (SW-RESULTS.md): outputs in lab/w7/.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
L=$HOME/lab-sw; O=$L/lab/w7; mkdir -p $O
log() { echo "[$(date +%T)] $*" | tee -a $O/progress.txt; }
race() { CGO_ENABLED=1 CC=$HOME/bin/zcc GOFLAGS=-ldflags=-linkmode=external "$@"; }
sum() { sed 's/^ *dripcount_test.go:[0-9]*: //' "$1" | grep -E 'build, mode|^ +(Count|log|another)|^(FAIL|ok)' | tr '\n' ';'; }
run() { TF_EXP=1 go test -count=1 -timeout 0 -run '^TestDripConcurrentCounts$' -v "$@" .; }
cd $L/go || exit 1
git log --oneline -1 >$O/tree.txt
log "1: control, the default build, 20,000 rounds per mode and shape"
TF_DRIP_ROUNDS=20000 run >$O/c1-default.txt 2>&1
log "1: $(sum $O/c1-default.txt)"
log "2: sight, W4b's and W4c's trees with tfdrip, Background, spread"
for c in 06940e8:3000 0837f9f:20000; do
	id=${c%%:*}; n=${c##*:}; W=$HOME/lab-sw-out/w7-$id
	rm -rf $W; git -C $L worktree add --detach -q $W $id
	cp $L/go/dripcount_test.go $W/go/
	(cd $W/go && TF_DRIP_ROUNDS=$n TF_DRIP_MODES=bg TF_DRIP_SHAPES=spread run -tags tfdrip >$O/c2-$id.txt 2>&1)
	log "2 $id ($n rounds): $(sum $O/c2-$id.txt)"
	git -C $L worktree remove --force $W
done
git -C $L worktree prune
log "3: W4d's library with tfdrip, 20,000 rounds per mode and shape"
TF_DRIP_ROUNDS=20000 run -tags tfdrip >$O/c3-drip.txt 2>&1
log "3: $(sum $O/c3-drip.txt)"
log "3 race: 1,000 rounds per mode and shape under -race"
TF_DRIP_ROUNDS=1000 race run -race -tags tfdrip >$O/c3-race.txt 2>&1
log "3 race: $(sum $O/c3-race.txt)"
log "W7 readings finished"
