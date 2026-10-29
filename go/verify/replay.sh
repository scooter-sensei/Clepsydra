#!/usr/bin/env bash
# Replays recorded results against the commits that produced them. Needs a
# git checkout with full history.
#
#   verify/replay.sh fixes [N]   each bug-fix test: fails on the commit with the bug, passes on the fix
#   verify/replay.sh stress [N]  the racy regression tests at the working tree, N runs each
#   verify/replay.sh history     each docs/record/scorecard-history.md row: re-run that commit's own tfbench -quick,
#                                compare the deterministic columns with the recorded row
#   verify/replay.sh harness     today's accuracy harness (tfbench -exact) on every commit that
#                                changed the library, plus the one older-harness case docs/record/changes.md cites (item 1)
#
# Each subcommand prints one result line per case and exits non-zero if a
# case did not reproduce.
set -uo pipefail
cd "$(dirname "$0")/.."
. verify/lib/lib.sh
WORK=${WORK:-$(mktemp -d)}
HEAD_REV=$(git rev-parse --short HEAD)
HISTORY=$TOP/docs/record/scorecard-history.md
bad=0

tree() { # tree <rev> <dir>: export a commit's files (no .git)
	rm -rf "$2" && export_tree "$1" "$2"
}

# runmany <dir> <test binary> <regex> <runs>: runs a test binary <runs> times,
# PAR (default 4) at a time, and prints how many failed. Contention between
# runs makes most races show; TestAbandonedMoveKeepsCounts needs its eight
# workers truly parallel, so it runs alone (PAR=1).
runmany() {
	: >"$1/fails"
	seq "$4" | xargs -P "${PAR:-4}" -I{} sh -c '"$1" -test.run "$2" -test.count 1 >"$3/run.$4.log" 2>&1 || echo "$4" >>"$3/fails"' _ "$2" "$3" "$1" {}
	wc -l <"$1/fails" | tr -d ' '
}

fixes() {
	local n=${1:-20}
	# name | commit with the bug | commit with the fix | test files copied from HEAD | test regex | runs | env
	# (verify/testdata/ghost_test.go is the ghost test as of 00a9375: HEAD's
	# calls the later two-bucket newGhost. verify/testdata/restore_test.go is
	# the restore test as of f44e7dd: HEAD's reads forceBG, which 3b8de31
	# added. verify/testdata/mirror_test.go is HEAD's mirror test without
	# its read check: read's slot hint came with d6dbe56.) The split and
	# handshake fixes of
	# 971dc4a and 64ebd85 are proven by deterministic tests on hooks those
	# commits added (TestEvictionDuringMoveKeepsHistory,
	# TestEvictionRacingMoveStart), so they can't run on the code before;
	# so are the fixes of the wait-room transfer (335d75a,
	# testHookTaken), the register epoch (c026b30, testHookStamped), the
	# copy of an evicted key (1683f77, testHookPicked), the wait-room
	# ways (1ea1686, testHookPlacing), the decay sweep's slot hold
	# (2408705, testHookSweeping) and an insert's hold over an expired
	# occupant (0779e1e, testHookInstalling), as are the tag-stripe
	# fixes (dac6542, 2087baa, 326a1f9: tagstripe_test.go) and a hot
	# history's exits from the stash (6b3cb39, testHookHotAdding:
	# hotspill_test.go). The visible copy (1683f77) and the reclaimed hot
	# entry (a519cf1) need no new hook: their tests run on the code before.
	# Duplicate slots: the striped lock (593bd1b) removed most; the install
	# order in f31512c removed the rare rest (a race with relocation), so
	# both cases end at f31512c. Lost counts under relocation: f31512c's
	# install order removed the common cause (38 of 200 runs before); the
	# relocation test still lost one count in about 1 of 1,000-7,000 runs
	# until 4a037a5 (abandoned moves) and d7fb2b5 (same-tag hand-over), so
	# that case ends at d7fb2b5. Racy cases get enough runs that the bug is
	# missed with probability < 1e-4.
	while IFS='|' read -r name old fix files re runs env; do
		[ -z "$name" ] && continue
		runs=${runs:-$n}
		local res=()
		for rev in "$old" "$fix"; do
			local d="$WORK/fix-$name-$rev"
			tree "$rev" "$d"
			for f in $files; do cp "$f" "$d/"; done
			fix_imports "$d"
			if ! (cd "$d" && go test -c -o fix.test . >"$d/build.log" 2>&1); then
				res+=("build-failed")
				continue
			fi
			res+=("$( [ -n "$env" ] && export $env; runmany "$d" "$d/fix.test" "$re" "$runs")/$runs")
		done
		local verdict=REPRODUCED
		if [ "${res[0]%%/*}" = 0 ] || [ "${res[0]}" = build-failed ] || [ "${res[1]%%/*}" != 0 ]; then
			verdict="NOT REPRODUCED"
			bad=1
		fi
		printf 'fix %-29s bug at %s failed %-9s | fixed at %s failed %-9s => %s\n' "$name" "$old" "${res[0]}" "$fix" "${res[1]}" "$verdict"
		cat "$WORK/fix-$name-$old"/run.*.log 2>/dev/null | grep -h -E 'slots used|counted [0-9]+ of|rounds lost counts \(|Ticks\(|panic:' | sed 's/^ *//' | sort | uniq -c | sort -rn | head -3 | sed 's/^/    bug run: /'
	done <<-LIST
	duplicate-slots|70546b2|f31512c|dup_test.go|^TestConcurrentFirstInsertNoDuplicates$|
	duplicate-slots-after-lock|593bd1b|f31512c|dup_test.go|^TestConcurrentFirstInsertNoDuplicates$|400
	lost-counts-relocation|d1db914|d7fb2b5|reloc_race_test.go|^TestTouchRacingRelocationKeepsCounts$|200
	lost-counts-abandoned-move|07e2d35|4a037a5|abandon_test.go|^TestAbandonedMoveKeepsCounts$|20|TF_DISTINCT_TAGS=1 PAR=1
	lost-counts-same-tag-handover|4a037a5|d7fb2b5|abandon_test.go|^TestAbandonedMoveKeepsCounts$|200|PAR=1
	negative-ticks|99c054a|a01e3dc|regress_test.go|^TestRegressNegativeTicks$|3
	empty-batch-panic|2921fd4|d1db914|regress_test.go|^TestRegressEmptyBatch$|3
	torn-lookup|6c051d0|17c55c8|read_race_test.go|^TestReadDuringRelocationFindsKey$|10
	ghost-stale-epoch|17c55c8|42cc598|verify/testdata/ghost_test.go|^TestGhostEpochRace$|10
	ghost-ahead-of-clock|64ebd85|00a9375|verify/testdata/ghost_test.go|^TestGhostAddAheadOfClock$|3
	restore-behind-clock|a82f5c2|f44e7dd|verify/testdata/restore_test.go|^TestRestoreBehindLastTouch$|3
	move-visible-copy|aeaca6d|1683f77|movecopy_test.go|^TestReadDuringMoveSeesEveryEvent$|3
	hot-entry-reclaimed|1ea1686|a519cf1|hot_test.go|^TestRestoreAfterHotEntryReclaimed$|3
	hot-entry-reclaimed-in-claim|1ea1686|a519cf1|hot_test.go|^TestHotEntryReclaimedDuringRestore$|3
	tick-past-range|4f33a45|b866d7f|regress_test.go|^TestRegressTickPastRange$|3
	restore-while-waiting|30cd90d|d6dbe56|restorewait_test.go|^TestEvictedKeyThatWaitsIsRestored$|3
	spill-foreign-hot-entry|30cd90d|d6dbe56|hotspill_test.go|^TestSpillOfForeignHotEntryIsNotLost$|3
	mirror-key-in-found-slot|30cd90d|d6dbe56|verify/testdata/mirror_test.go|^TestMirrorKeyInFoundSlotNotCounted$|3
	LIST
}

# stress [N]: the racy regression tests at the working tree, N runs each
# (default 300), 4 at a time. Any failure is a live bug.
stress() {
	local n=${1:-300} d="$WORK/stress"
	mkdir -p "$d"
	go test -c -o "$d/t.test" .
	for re in '^TestConcurrentFirstInsertNoDuplicates$' '^TestTouchRacingRelocationKeepsCounts$' '^TestAbandonedMoveKeepsCounts$' '^TestStashConcurrentEvictions$'; do
		# Each test its own directory: runmany's logs are run.<n>.log,
		# which the next test's runs would overwrite.
		local f par=4 dd="$d/${re//[^A-Za-z]/}" r
		mkdir -p "$dd"
		[ "$re" = '^TestAbandonedMoveKeepsCounts$' ] && par=1
		f=$(PAR=$par runmany "$dd" "$d/t.test" "$re" "$n")
		printf 'stress %-44s failed %s/%s\n' "$re" "$f" "$n"
		[ "$f" = 0 ] && continue
		bad=1
		for r in $(cat "$dd/fails"); do
			grep -h -E 'rounds lost counts|_test\.go:[0-9]+:|^panic:' "$dd/run.$r.log" | head -3 | sed "s|^ *|    run $r: |"
		done
	done
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
	hh=$(grep -m1 '^| when' "$HISTORY")
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
	done < <(grep -E '^\| [0-9]{2}-[0-9]{2} ' "$HISTORY")
}

harness() {
	# Today's accuracy harness against each library version.
	local h="$WORK/harness-src"
	tree HEAD "$h"
	# The library is every non-test .go file of the module's top directory and of
	# internal/cpu and internal/siphash (filter.go and register.go first; ghost.go,
	# hot.go, overflow.go and sip.go later; sip.go and the assembly wrappers then
	# moved into internal/). Commits that only moved it (into go/) are skipped; only
	# internal/workload is replaced by today's, the harness's own input.
	local c prev="" lib
	for c in $(git rev-list --reverse HEAD -- ':(top,glob)*.go' ':(top,glob)go/*.go' ':(top,glob)go/internal/cpu/*.go' ':(top,glob)go/internal/siphash/*.go' ':(top,exclude,glob)**/*_test.go' | cut -c1-7); do
		lib=$(git -C "$TOP" ls-tree -r "$c" | awk '$4 ~ /^(go\/)?([^\/]*|internal\/(cpu|siphash)\/[^\/]*)\.go$/ && $4 !~ /_test\.go$/ {print $3}')
		[ "$lib" = "$prev" ] && continue
		prev=$lib
		local d="$WORK/harness-$c"
		tree "$c" "$d"
		rm -rf "$d/cmd/tfbench" "$d/internal/workload" && mkdir -p "$d/cmd" "$d/internal"
		cp -r "$h/cmd/tfbench" "$d/cmd/" && cp -r "$h/internal/workload" "$d/internal/" && fix_imports "$d"
		printf 'harness %s %s\n' "$c" "$(git log -1 --format=%s "$c" | cut -c1-70)"
		(cd "$d" && go run -tags accuracyonly ./cmd/tfbench -exact 2>&1) | sed 's/^/    /'
	done
	# Backlog item 1 ("missed keys at 2.5x load: 5.0% -> 0%") was measured
	# with the b3e9aa5 harness (2.5x load point) on the a1a7c69 library.
	local d="$WORK/harness-b3e9aa5-on-a1a7c69"
	tree a1a7c69 "$d"
	rm -rf "$d/cmd/tfbench" && mkdir -p "$d/cmd" && git -C "$TOP" archive b3e9aa5 cmd/tfbench | tar -x -C "$d"
	printf 'harness b3e9aa5-on-a1a7c69 (tfbench -quick row)\n'
	(cd "$d" && go run ./cmd/tfbench -quick 2>/dev/null | grep -E '^\| [0-9]{2}-') |
		sed -E 's/^\| [0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2} \| *\|/|/; s/^/    /' # drop the timestamp and empty rev
}

case "${1:-}" in
fixes) fixes "${2:-20}" ;;
stress) stress "${2:-300}" ;;
history) history ;;
harness) harness ;;
*) sed -n '2,12p' "$0"; exit 2 ;;
esac
exit $bad
