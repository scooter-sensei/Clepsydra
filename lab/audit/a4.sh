#!/bin/bash
# gap audit: TestAuditTickPoison with a caller far ahead within MaxTick (W5d's tree, both builds)
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
S=$HOME/lab-sw/lab/audit
O=$HOME/lab-sw-out/audit; mkdir -p $O
cd $HOME/lab-sw
c=9db48f9
rm -rf $O/wt-$c; git worktree add --detach -q $O/wt-$c $c || exit 1
cp $S/audit_test.go $O/wt-$c/go/audit_test.go
cd $O/wt-$c/go
echo "=== $c tfdrip: poison and ahead"
go test -tags tfdrip -count=1 -run '^TestAuditTickPoison$' -v . 2>&1 | grep -E 'audit_test|^(--- |ok|FAIL)' | sed 's/^ *//'
echo "=== $c default build: poison and ahead"
go test -count=1 -run '^TestAuditTickPoison$' -v . 2>&1 | grep -E 'audit_test|^(--- |ok|FAIL)' | sed 's/^ *//'
cd $HOME/lab-sw
git worktree remove --force $O/wt-$c
git worktree prune
