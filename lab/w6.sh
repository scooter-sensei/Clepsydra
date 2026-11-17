#!/bin/bash
# lab/sw W6's reading (SW-RESULTS.md): TestShortWindowFreshSeeds in both builds; outputs in lab/w6/.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
O=$HOME/lab-sw/lab/w6; mkdir -p $O
cd $HOME/lab-sw/go || exit 1
git log --oneline -1 >$O/tree.txt
TF_EXP=1 go test -count=1 -timeout 0 -run '^TestShortWindowFreshSeeds$' -v . >$O/default.txt 2>&1
TF_EXP=1 go test -tags tfdrip -count=1 -timeout 0 -run '^TestShortWindowFreshSeeds$' -v . >$O/drip.txt 2>&1
for f in default drip; do echo "== $f"; sed 's/^ *swseeds_test.go:[0-9]*: //' $O/$f.txt; done
