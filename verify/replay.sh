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
		grep -h -E 'slots used|counted [0-9]+ of|Ticks\(|panic:' "$WORK/fix-$name-$old/run.log" 2>/dev/null | sed 's/^ *//' | sort | uniq -c | sort -rn | head -5 | sed 's/^/    before: /'
	done <<-LIST
	duplicate-slots|a8a2848|dup_test.go|^TestConcurrentFirstInsertNoDuplicates$|
	lost-counts-relocation|e42327a|reloc_race_test.go|^TestTouchRacingRelocationKeepsCounts$|20
	negative-ticks|f9c9127|regress_test.go|^TestRegressNegativeTicks$|3
	empty-batch-panic|f07b04e|regress_test.go|^TestRegressEmptyBatch$|3
	LIST
}

# cols <header> <row> <names...>: the named columns of a scorecard row, "-" if
# the header has no such column.
cols() {
	awk -F'|' -v h="$1" -v r="$2" -v names="$3" 'BEGIN {
		nh = split(h, H, "|"); split(r, R, "|"); nn = split(names, N, ",")
		for (j = 1; j <= nn; j++) {
			v = "-"
			for (i = 1; i <= nh; i++) { x = H[i]; gsub(/^ +| +$/, "", x); if (x == N[j]) { v = R[i]; gsub(/ /, "", v) } }
			printf "%s ", v
		}
	}'
}

history() {
	# Old harnesses named the FN columns by load point; map by position for
	# those, by name for the rest.
	local hh
	hh=$(grep -m1 '^| when' results/history.md)
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
		local out oh orow fnw fng want got
		out=$(cd "$d" && go run ./cmd/tfbench -quick 2>/dev/null)
		oh=$(grep -m1 '^| when' <<<"$out")
		orow=$(grep -m1 -E '^\| [0-9]{2}-' <<<"$out")
		fnw=$(awk -F'|' '{for (i = 4; i <= 9; i++) {gsub(/ /, "", $i); printf "%s ", $i}}' <<<"$row")
		fng=$(awk -F'|' '{for (i = 4; i <= 9; i++) {gsub(/ /, "", $i); printf "%s ", $i}}' <<<"$orow")
		want="$fnw$(cols "$hh" "$row" "B/key,vel MAE")"
		got="$fng$(cols "$oh" "$orow" "B/key,vel MAE")"
		# A column the old harness didn't print can't be checked here.
		local wa ga i n=0 m=0 note=""
		read -ra wa <<<"$want"
		read -ra ga <<<"$got"
		for i in "${!wa[@]}"; do
			if [ "${ga[$i]:-}" = - ]; then
				[ "${wa[$i]}" != - ] && note=" (${wa[$i]} not printed by that harness)"
				continue
			fi
			n=$((n + 1))
			[ "${wa[$i]}" = "${ga[$i]:-}" ] && m=$((m + 1))
		done
		if [ $n -gt 0 ] && [ $m = $n ]; then
			printf 'history %s %-8s tree %s: %s=> MATCH %d/%d columns%s\n' "$when" "$rev" "$c" "$got" "$m" "$n" "$note"
		else
			printf 'history %s %-8s tree %s: recorded %s replayed %s=> MISMATCH %d/%d columns\n' "$when" "$rev" "$c" "$want" "$got" "$m" "$n"
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
