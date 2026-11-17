#!/bin/bash
# gap audit: probes on a detached copy of a tree (never committed)
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
S=$HOME/lab-sw/lab/audit
O=$HOME/lab-sw-out/audit; mkdir -p $O
cd $HOME/lab-sw
for c in 9db48f9 d4d0187; do
  rm -rf $O/wt-$c; git worktree add --detach -q $O/wt-$c $c || continue
  cp $S/audit_test.go $O/wt-$c/go/audit_test.go
  cd $O/wt-$c/go
  echo "=== $c tfdrip: poison"
  go test -tags tfdrip -count=1 -run '^TestAuditTickPoison$' -v . 2>&1 | grep -E 'audit_test|^(--- |ok|FAIL)' | sed 's/^ *//'
  if [ $c = 9db48f9 ]; then
    echo "=== $c default build: poison"
    go test -count=1 -run '^TestAuditTickPoison$' -v . 2>&1 | grep -E 'audit_test|^(--- |ok|FAIL)' | sed 's/^ *//'
  fi
  go test -tags tfdrip -c -o $O/aud-$c.test . || exit 1
  n=0; for i in $(seq 1 40); do TF_BG=1 $O/aud-$c.test -test.run '^TestAuditAliasSettled$' -test.count=1 >$O/as.out 2>&1 || { n=$((n+1)); cp $O/as.out $O/as-fail-$c.out; }; done
  echo "$c TestAuditAliasSettled TF_BG=1 (placer drained first): $n of 40 fail"
  cd $HOME/lab-sw
  git worktree remove --force $O/wt-$c
done
git worktree prune
