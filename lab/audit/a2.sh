#!/bin/bash
# gap audit: does TestWrapAlias/stash under TF_BG=1 fail before W5d too (4e4966e = W5b/W5c's code on W4d; d4d0187 = W5c as read)?
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
O=$HOME/lab-sw-out/audit; mkdir -p $O
cd $HOME/lab-sw
for c in 4e4966e d4d0187; do
  git cat-file -e $c^{commit} 2>/dev/null || { echo "$c: not in the clone"; continue; }
  rm -rf $O/wt-$c; git worktree add --detach -q $O/wt-$c $c || continue
  (cd $O/wt-$c/go && go test -tags tfdrip -c -o $O/$c.test .) || { echo "$c: build failed"; continue; }
  for m in TF_BG=1; do
    n=0; for i in $(seq 1 40); do env $m $O/$c.test -test.run '^TestWrapAlias$' -test.count=1 >$O/wa.out 2>&1 || { n=$((n+1)); cp $O/wa.out $O/wa-fail-$c.out; }; done
    echo "$c TestWrapAlias $m: $n of 40 fail"
  done
  echo "$c TestTickRange: $($O/$c.test -test.run '^TestTickRange$' -test.count=1 2>&1 | tail -1)"
  git worktree remove --force $O/wt-$c
done
git worktree prune
