#!/bin/bash
# gap audit: is W5d's TestWrapAlias/stash failure under TF_BG=1 deterministic? and TestTickRange
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
O=$HOME/lab-sw-out/audit; mkdir -p $O
cd $HOME/lab-sw-w5b/go
git log --oneline -1
go test -tags tfdrip -c -o $O/w5d.test . || exit 1
for m in plain TF_BG=1 TF_CT=1; do
  e=""; [ $m = plain ] || e=$m
  n=0; for i in $(seq 1 40); do env $e $O/w5d.test -test.run '^TestWrapAlias$' -test.count=1 >$O/wa.out 2>&1 || { n=$((n+1)); cp $O/wa.out $O/wa-fail-$m.out; }; done
  echo "TestWrapAlias $m: $n of 40 fail"
done
for m in plain TF_BG=1; do e=""; [ $m = plain ] || e=$m; echo "TestTickRange $m:"; env $e $O/w5d.test -test.run '^TestTickRange$' -test.count=1 2>&1 | tail -4; done
grep -h -A3 -- '--- FAIL' $O/wa-fail-TF_BG=1.out 2>/dev/null | head -8
