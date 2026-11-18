#!/usr/bin/env bash
# lab/sw W9's reading (SW-RESULTS.md): lab/w9_test.go, the hot restore's push, on the lab's head (W4d's
# library), built with tfdrip and without, plain mode. The probe is copied into a detached worktree of the
# head and is never in the tree. Outputs in lab/w9 (cleared): results.txt, tfdrip.log, default.log.
# Exits 1 if a probe check fails (a test fails), 2 if it cannot start. W9_LAB and W9_RUN (the clone and
# go test's -run pattern) are for trying the script on a scratch clone only.
set -uo pipefail
export PATH=$HOME/sdk/go1.24.7/bin:$PATH GOTOOLCHAIN=local CGO_ENABLED=0
unset TF_CT TF_BG
L=${W9_LAB:-$HOME/lab-sw}
O=$L/lab/w9
command -v go >/dev/null || { echo "go not found"; exit 2; }
git -C $L ls-files --error-unmatch lab/w9_test.go lab/w9.sh >/dev/null 2>&1 && [ -z "$(git -C $L status --porcelain -- lab/w9_test.go lab/w9.sh)" ] ||
	{ echo "lab/w9_test.go or lab/w9.sh is not committed as it is: commit it first"; exit 2; }
c=$(git -C $L rev-parse --short HEAD)
W=$(mktemp -d)
trap 'git -C $L worktree remove --force "$W/t" >/dev/null 2>&1; git -C $L worktree prune; rm -rf "$W"' EXIT
git -C $L worktree add --detach -q "$W/t" "$c" || exit 2
cp "$L/lab/w9_test.go" "$W/t/go/w9_test.go" || exit 2
rm -rf "$O" && mkdir -p "$O"
lib=$(git -C $L diff --quiet b7a33a7 "$c" -- go ':!go/*_test.go' ':!go/verify' && echo "b7a33a7's (W4d)" || echo "NOT b7a33a7's")
echo "$c: lab/w9.sh; library $lib; $(go version | cut -d' ' -f3)" >"$O/results.txt"
bad=0
for b in tfdrip default; do
	tags=()
	[ $b = tfdrip ] && tags=(-tags tfdrip)
	(cd "$W/t/go" && go test "${tags[@]}" -count=1 -timeout 300s -run "${W9_RUN:-^TestW9}" -v . >"$O/$b.log" 2>&1) || bad=1
	echo "== $b" >>"$O/results.txt"
	grep -E 'W9 |--- (PASS|FAIL|SKIP)|probe:|^(ok|FAIL|panic)' "$O/$b.log" | sed 's/^ *//' >>"$O/results.txt"
	grep -qE -- '--- (PASS|FAIL)' "$O/$b.log" || { echo "no test ran" >>"$O/results.txt"; bad=1; }
done
echo "W9 (exit $bad: $([ $bad = 0 ] && echo "every probe check held" || echo "a probe check failed or the run did not finish"))" >>"$O/results.txt"
cat "$O/results.txt"
exit $bad
