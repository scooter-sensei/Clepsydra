#!/usr/bin/env bash
# Replays recorded results against the commits that produced them. Run from
# the repository root (a git checkout with full history).
#
#   verify/replay.sh fixes [N]   each bug-fix test: fails on the commit before the fix, passes on the fix
#   verify/replay.sh history     each results/history.md row: re-run that commit's own tfbench -quick,
#                                compare the deterministic columns with the recorded row
#   verify/replay.sh harness     today's accuracy harness (tfbench -exact) on every commit that
#                                changed the library, plus the one older-harness case the backlog cites
#
# Each subcommand prints one result line per case and exits non-zero if a
# case did not reproduce.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
WORK=${WORK:-$(mktemp -d)}
HEAD_REV=$(git rev-parse --short HEAD)
bad=0

tree() { # tree <rev> <dir>: export a commit's files (no .git)
	rm -rf "$2" && mkdir -p "$2" && git archive "$1" | tar -x -C "$2"
}

fixes() {
	local n=${1:-10}
	# name | fix commit | test files copied from HEAD | test regex | runs
	while IFS='|' read -r name fix files re runs; do
		[ -z "$name" ] && continue
		runs=${runs:-$n}
		local old res=()
		old=$(git rev-parse --short "$fix^")
		for rev in "$old" "$fix"; do
			local d="$WORK/fix-$name-$rev"
			tree "$rev" "$d"
			for f in $files; do cp "$f" "$d/"; done
			if ! (cd "$d" && go test -c -o fix.test . >"$d/build.log" 2>&1); then
				res+=("build-failed")
				continue
			fi
			local fails=0
			for _ in $(seq "$runs"); do
				(cd "$d" && ./fix.test -test.run "$re" -test.count 1 >>"$d/run.log" 2>&1) || fails=$((fails + 1))
			done
			res+=("$fails/$runs")
		done
		local verdict=REPRODUCED
		if [ "${res[0]%%/*}" = 0 ] || [ "${res[0]}" = build-failed ] || [ "${res[1]%%/*}" != 0 ]; then
			verdict="NOT REPRODUCED"
			bad=1
		fi
		printf 'fix %-22s before %s failed %-7s | fix %s failed %-7s => %s\n' "$name" "$old" "${res[0]}" "$fix" "${res[1]}" "$verdict"
		grep -h -m1 -E 'duplicate slots|counted [0-9]+ of|Ticks\(|panic:' "$WORK/fix-$name-$old/run.log" 2>/dev/null | sed 's/^/    before: /'
	done <<-LIST
	duplicate-slots|a8a2848|dup_test.go|^TestConcurrentFirstInsertNoDuplicates$|
	lost-counts-relocation|e42327a|reloc_race_test.go|^TestTouchRacingRelocationKeepsCounts$|20
	negative-ticks|f9c9127|regress_test.go|^TestRegressNegativeTicks$|3
	empty-batch-panic|f07b04e|regress_test.go|^TestRegressEmptyBatch$|3
	LIST
}

# cols <row>: the deterministic columns of a scorecard row (FN a/b/c, FP churn,
# static FPR, spray, B/key, vel MAE).
cols() { awk -F'|' '{for (i = 4; i <= 11; i++) {gsub(/ /, "", $i); printf "%s ", $i}}' <<<"$1"; }

history() {
	while IFS= read -r row; do
		local when rev c
		when=$(awk -F'|' '{gsub(/^ +| +$/, "", $2); print $2}' <<<"$row")
		rev=$(awk -F'|' '{gsub(/ /, "", $3); print $3}' <<<"$row")
		c=${rev%+}
		if [ "$rev" != "$c" ]; then # measured on an uncommitted tree: it became the next commit
			c=$(git rev-list --reverse --ancestry-path "$c..HEAD" | head -1 | cut -c1-7)
		fi
		local d="$WORK/hist-$c"
		[ -d "$d" ] || tree "$c" "$d"
		local out want got
		out=$(cd "$d" && go run ./cmd/tfbench -quick 2>/dev/null | grep -E '^\| [0-9]{2}-')
		want=$(cols "$row")
		got=$(cols "$out")
		if [ "$want" = "$got" ]; then
			printf 'history %s %-8s tree %s: %s=> MATCH\n' "$when" "$rev" "$c" "$got"
		else
			printf 'history %s %-8s tree %s: recorded %s replayed %s=> MISMATCH\n' "$when" "$rev" "$c" "$want" "$got"
			bad=1
		fi
	done < <(grep -E '^\| [0-9]{2}-[0-9]{2} ' results/history.md)
}

harness() {
	# Today's accuracy harness against each library version.
	local h="$WORK/harness-src"
	tree HEAD "$h"
	for c in $(git rev-list --reverse HEAD -- filter.go register.go | cut -c1-7); do
		local d="$WORK/harness-$c"
		tree "$c" "$d"
		rm -rf "$d/cmd/tfbench" "$d/internal" && mkdir -p "$d/cmd"
		cp -r "$h/cmd/tfbench" "$d/cmd/" && cp -r "$h/internal" "$d/"
		printf 'harness %s %s\n' "$c" "$(git log -1 --format=%s "$c" | cut -c1-70)"
		(cd "$d" && go run -tags accuracyonly ./cmd/tfbench -exact 2>&1) | sed 's/^/    /'
	done
	# Backlog item 1 ("missed keys at 2.5x load: 5.0% -> 0%") was measured
	# with the 597ee63 harness (2.5x load point) on the 8381fd2 library.
	local d="$WORK/harness-597ee63-on-8381fd2"
	tree 8381fd2 "$d"
	rm -rf "$d/cmd/tfbench" && mkdir -p "$d/cmd" && git archive 597ee63 cmd/tfbench | tar -x -C "$d"
	printf 'harness 597ee63-on-8381fd2 (tfbench -quick row)\n'
	(cd "$d" && go run ./cmd/tfbench -quick 2>/dev/null | grep -E '^\| [0-9]{2}-') | sed 's/^/    /'
}

case "${1:-}" in
fixes) fixes "${2:-10}" ;;
history) history ;;
harness) harness ;;
*) sed -n '2,12p' "$0"; exit 2 ;;
esac
exit $bad
