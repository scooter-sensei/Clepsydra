#!/bin/bash
# W7 diagnosis (not a reading): the non-saturated differences, Background, burst, the shadow's hot entries as the filter's.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
S=$HOME/lab-sw/lab/w7/diag
L=$HOME/lab-sw; O=$L/lab/w7/diag; mkdir -p $O
W=$HOME/lab-sw-out/w7-diag; rm -rf $W; git -C $L worktree add --detach -q $W HEAD
cp $S/dripdiag_test.go $W/go/ && cp $S/dripdiag_test.go $O/
cd $W/go
go vet -tags tfdrip . || exit 1
for tags in tfdrip ""; do
	TF_EXP=1 TF_DIAG_HOT=1 TF_DRIP_ROUNDS=10000 TF_DRIP_MODES=bg TF_DRIP_SHAPES=burst go test -tags "$tags" -count=1 -timeout 0 -run '^TestDripConcurrentCountsDiag$' -v . >$O/unsat-${tags:-default}.txt 2>&1
	echo "== ${tags:-default}"
	sed 's/^ *dripdiag_test.go:[0-9]*: //' $O/unsat-${tags:-default}.txt | grep -E '^DIAG|UNSATURATED' | head -14
done
cd $L && git worktree remove --force $W && git worktree prune
