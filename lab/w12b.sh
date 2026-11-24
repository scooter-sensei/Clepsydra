#!/bin/bash
# lab/sw W12b's reading (SW-RESULTS.md, W12b): outputs in lab/w12b/ (cleared). W12b changes no library file:
# the head's library equals 4a586d8's, and the test files it reuses equal 073d7da's; this script checks both.
# Tests: go/swsplit_test.go (TF_EXP=1). The per-query files go to $TF_W12B_Q (cleared), never under lab/.
#  1 sight: TestShortWindowLagSplitSight, the default build and tfdrip (sight-<build>.txt);
#  2 runs: TestShortWindowLagSplit, the 45 runs a build (nine delays, five seeds), default then tfdrip
#    (runs-<build>.txt); it needs TF_W12B_Q and TF_W12B_HEAD, both set here;
#  3 join: TestShortWindowLagSplitJoin, default build, over the files in $TF_W12B_Q (join.txt);
#  4 lab/w12b.py: the tables and clauses, from the lines above and lab/w12's (tables.txt).
# The reading judges the lines, not exit codes. A step counts only with its done line: "W12b sight done:",
# "W12b done:", "W12b join done:". Exits 1 if a step lacks it, 2 if the script cannot start. The timeouts
# (10m, 60m a build, 30m) are provisional, until the dry runs before the rule size them. W12B_LAB, W12B_Q,
# W12B_RUN, W12B_SIGHT, W12B_JOIN (the clone, the query directory, go test -run patterns), W12B_SEEDS and
# W12B_DELAYS are for trying the script on a scratch clone only.
set -u
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
unset TF_CT TF_BG TF_NOCAS2 TF_EXP
for v in $(compgen -e | grep -E '^TF_W12B?_'); do unset "$v"; done
# A scratch clone (W12B_LAB) needs its own query directory (W12B_Q): the default one is the real run's, and it is
# cleared. Both are made absolute before anything uses them, and Q may not be /, $HOME, a parent of $HOME or of the
# clone, or the clone itself or anything under it, since it is cleared with rm -rf.
if [ -n "${W12B_LAB:-}" ] && [ -z "${W12B_Q:-}" ]; then
	echo "W12B_LAB is set without W12B_Q: the default query directory is the real run's and would be cleared: stop" >&2
	exit 2
fi
L=$(realpath -m "${W12B_LAB:-$HOME/lab-sw}") && Q=$(realpath -m "${W12B_Q:-$HOME/lab-sw-out/w12b-q}") &&
	H=$(realpath -m "$HOME") || { echo "cannot resolve the clone or the query directory: stop" >&2; exit 2; }
bad_q=
case "$Q/" in "$L/"*) bad_q=1 ;; esac # the clone or inside it
case "$L/" in "${Q%/}/"*) bad_q=1 ;; esac # the clone or a parent of it (/ too)
case "$H/" in "${Q%/}/"*) bad_q=1 ;; esac # $HOME or a parent of it (/ too)
[ -z "$bad_q" ] || { echo "the query directory $Q is /, \$HOME or a parent of it or of the clone $L, or the clone or inside it: stop" >&2; exit 2; }
O=$L/lab/w12b
P=4a586d8 # the library's base
R=073d7da # the reused test files' base
RUN=${W12B_RUN:-'^TestShortWindowLagSplit$'} SIGHT=${W12B_SIGHT:-'^TestShortWindowLagSplitSight$'}
JOIN=${W12B_JOIN:-'^TestShortWindowLagSplitJoin$'}
git -C "$L" ls-files --error-unmatch lab/w12b.sh lab/w12b.py go/swsplit_test.go >/dev/null 2>&1 &&
	[ -z "$(git -C "$L" status --porcelain -- go lab/w12b.sh lab/w12b.py lab/w12/a-default.txt lab/w12/a-drip.txt SW-RESULTS.md)" ] ||
	{ echo "the tree or lab/w12b.sh is not committed as it is: commit it first"; exit 2; }
rm -rf "$O" && mkdir -p "$O"
log() { echo "[$(date +%T)] $*" | tee -a "$O/progress.txt"; }
# a trial says so in its progress.txt
for v in $(compgen -e | grep -E '^W12B_'); do log "0: override $v=${!v}"; done
h=$(git -C "$L" rev-parse HEAD) || { log "W12b: git rev-parse HEAD failed: stop"; exit 2; }
log "W12b: head ${h:0:7}; $(go version | cut -d' ' -f3)"
for r in $P $R; do
	git -C "$L" cat-file -e "$r^{commit}" 2>/dev/null || { log "0: $r is not a commit here: stop"; exit 2; }
done
lib=$(git -C "$L" diff --name-only $P HEAD -- go ':!go/*_test.go') ||
	{ log "0: git diff against $P failed: stop"; exit 2; }
lib=$(echo "$lib" | tr '\n' ' ' | sed 's/ *$//')
log "0: library files that differ from $P's: [$lib]"
reu=$(git -C "$L" diff --name-only $R HEAD -- go/swlag_test.go go/swinfo_test.go go/velinfo_test.go) ||
	{ log "0: git diff against $R failed: stop"; exit 2; }
reu=$(echo "$reu" | tr '\n' ' ' | sed 's/ *$//')
log "0: reused test files that differ from $R's: [$reu]"
[ -z "$lib" ] || { log "0: the library differs from $P's: stop"; exit 2; }
[ -z "$reu" ] || { log "0: the reused test files differ from $R's: stop"; exit 2; }
rm -rf "$Q" && mkdir -p "$Q" || { log "0: cannot make $Q: stop"; exit 2; }
cd "$L/go" || exit 2
export TF_EXP=1 TF_W12B_Q="$Q" TF_W12B_HEAD="$h"
[ -n "${W12B_SEEDS:-}" ] && export TF_W12B_SEEDS="$W12B_SEEDS"
[ -n "${W12B_DELAYS:-}" ] && export TF_W12B_DELAYS="$W12B_DELAYS"
bad=0

# lines <file> <name> <pattern>: the run's short status lines into progress.txt (not its W12 run:, W12b run:
# or W12b split: lines, which are long), and whether it has its pattern
lines() {
	grep -E 'W12b (sight|done|join done)|--- (PASS|FAIL)|^(ok|FAIL|panic)|WARNING: DATA RACE|swsplit_test.go:[0-9]+: [a-zA-Z]' "$1" |
		grep -vE 'W12 run:|W12b run:|W12b split:' | sed 's/^ *//' | cut -c1-400 >>"$O/progress.txt"
	grep -q "$3" "$1" || { log "$2: NO W12b LINES" && bad=1; }
}

log "1: sight"
for b in default drip; do
	tags=; [ $b = drip ] && tags="-tags tfdrip"
	go test $tags -count=1 -run "$SIGHT" -v -timeout 10m . >"$O/sight-$b.txt" 2>&1
	log "1 $b: go test exit $?"
	lines "$O/sight-$b.txt" "1 $b" 'W12b sight done:'
done

log "2: runs"
for b in default drip; do
	tags=; [ $b = drip ] && tags="-tags tfdrip"
	go test $tags -count=1 -run "$RUN" -v -timeout 60m . >"$O/runs-$b.txt" 2>&1
	log "2 $b: go test exit $?; $(grep -c 'W12b run:' "$O/runs-$b.txt") runs"
	lines "$O/runs-$b.txt" "2 $b" 'W12b done:'
done

log "3: join"
go test -count=1 -run "$JOIN" -v -timeout 30m . >"$O/join.txt" 2>&1
log "3: go test exit $?; $(grep -c 'W12b split:' "$O/join.txt") split lines"
lines "$O/join.txt" "3" 'W12b join done:'

log "4: lab/w12b.py"
python3 -I "$L/lab/w12b.py" "$O" "$L/lab/w12" >"$O/tables.txt" 2>&1
log "4: exit $?"
grep -E '^W12b clause|MISSING|NO W12b' "$O/tables.txt" | tee -a "$O/progress.txt"
log "W12b reading finished (exit $bad)"
exit $bad
