#!/bin/bash
# W7 clause 1 diagnosis (not a reading): burst shape, split by Saturated; with and without equal hot entries.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
S=$HOME/lab-sw/lab/w7/diag
L=$HOME/lab-sw; O=$L/lab/w7/diag; mkdir -p $O
W=$HOME/lab-sw-out/w7-diag; rm -rf $W; git -C $L worktree add --detach -q $W HEAD
cp $S/dripdiag_test.go $W/go/ && cp $S/dripdiag_test.go $O/
cd $W/go
gofmt -l dripdiag_test.go
for tags in "" tfdrip; do
	for hot in 0 1; do
		TF_EXP=1 TF_DIAG_HOT=$hot TF_DRIP_ROUNDS=2000 TF_DRIP_SHAPES=burst go test -tags "$tags" -count=1 -timeout 0 -run '^TestDripConcurrentCountsDiag$' -v . >$O/burst-${tags:-default}-hot$hot.txt 2>&1
		echo "== ${tags:-default} hot-equal=$hot"
		sed 's/^ *dripdiag_test.go:[0-9]*: //' $O/burst-${tags:-default}-hot$hot.txt | grep -E '^DIAG|wrong:|UNSATURATED' | head -12
	done
done
cd $L && git worktree remove --force $W && git worktree prune
