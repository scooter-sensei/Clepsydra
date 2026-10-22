#!/usr/bin/env bash
# Model checks of the slot protocol (verify/tla/*.tla) with TLC.
#
#   verify/tla.sh [model...]   WaitRoom, RegisterEpoch, Relocate, RoomWays
#                              (default: all): the protocol as committed at
#                              a few sizes, then each guard undone (each
#                              must break an invariant)
#
# Needs java (11 or later) and tla2tools.jar (TLA+ tools 1.7.4, from
# github.com/tlaplus/tlaplus/releases), at $TLA2TOOLS or
# ~/tools/tla/tla2tools.jar. Each run translates the PlusCal (pcal.trans) in
# a scratch directory, so the committed .tla stays as written. Prints one
# line per run and exits non-zero if a run's outcome isn't the one wanted.
# TLA_BIG=1 adds the large sizes the claims cite (WaitRoom 2 touchers x 2
# rounds, 72 M states; RegisterEpoch 3 touchers, 52 M states: tens of
# minutes each). TLC_WORKERS (default 4) sets TLC's threads.
set -uo pipefail
cd "$(dirname "$0")/tla"
J=${TLA2TOOLS:-$HOME/tools/tla/tla2tools.jar}
[ -f "$J" ] || { echo "tla2tools.jar not found (set TLA2TOOLS)"; exit 2; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
bad=0
runs=0

# run <want: ok|violation> <model> <label> <invariants> <constant definitions...>
run() {
	local want=$1 m=$2 label=$3 inv=$4 got=ok states why c d
	shift 4
	runs=$((runs + 1))
	d="$tmp/$m-$runs"
	mkdir -p "$d" && tr -d '\r' <"$m.tla" >"$d/$m.tla"
	(cd "$d" && java -cp "$J" pcal.trans -nocfg "$m.tla" >pcal.log 2>&1) || { cat "$d/pcal.log"; bad=1; return; }
	{
		echo "SPECIFICATION Spec"
		echo "CONSTANTS"
		for c in "$@"; do echo "  $c"; done
		echo "INVARIANTS $inv"
	} >"$d/$m.cfg"
	(cd "$d" && java -XX:+UseParallelGC -cp "$J" tlc2.TLC -workers "${TLC_WORKERS:-4}" -cleanup "$m" >tlc.log 2>&1)
	grep -q 'is violated\|Deadlock reached\|^Error:' "$d/tlc.log" && got=violation
	states=$(grep -o '[0-9,]* distinct states found' "$d/tlc.log" | tail -1)
	why=$(grep -m1 -o 'Invariant [A-Za-z]* is violated\|Deadlock reached\|unexpected exception\|Parsing or semantic analysis failed' "$d/tlc.log")
	printf '%-13s %-40s %-9s (want %s) %s%s\n' "$m" "$label" "$got" "$want" "$states" "${why:+; $why}"
	[ "$got" = "$want" ] || bad=1
}

# procs <prefix> <n>: a TLA+ set of n process names.
procs() {
	local s="" i
	for i in $(seq "$2"); do s="$s${s:+, }\"$1$i\""; done
	echo "{$s}"
}

# WaitRoom (claims A20): Background's wait room against touches and reads.
waitroom() {
	local inv="NoDup ReadNotLow NoFresh Conserved" sz v
	local sizes="1 1,2 1,1 2"
	[ "${TLA_BIG:-}" = 1 ] && sizes="$sizes,2 2"
	IFS=, read -ra szs <<<"$sizes"
	for sz in "${szs[@]}"; do
		set -- $sz
		run ok WaitRoom "fixed, $1 toucher(s), $2 round(s)" "$inv" "Touchers = $(procs t "$1")" "Rounds = $2" 'Variant = "fixed"'
	done
	for v in noTouchMove lateBegin earlyDone noPlacerMove noBusyWait; do
		run violation WaitRoom "$v" "$inv" "Touchers = $(procs t 1)" "Rounds = 1" "Variant = \"$v\""
	done
}

# RegisterEpoch (claims A21): a register's 4-bit epoch against its last touch.
registerepoch() {
	local inv="NotAged Kept ReaderRight" c='Clocks = {101, 107, 108, 116, 126}' v i
	run ok RegisterEpoch "fixed, 2 touchers" "$inv" "Touchers = $(procs t 2)" "$c" 'Variant = "fixed"' "H = 24"
	[ "${TLA_BIG:-}" = 1 ] && run ok RegisterEpoch "fixed, 3 touchers" "$inv" "Touchers = $(procs t 3)" "$c" 'Variant = "fixed"' "H = 24"
	for i in NotAged Kept ReaderRight; do
		run violation RegisterEpoch "old (before 5e21d7e): $i" "$i" "Touchers = $(procs t 2)" "$c" 'Variant = "old"' "H = 24"
	done
	for v in noHold ownRef noWait noReread; do
		run violation RegisterEpoch "$v" "$inv" "Touchers = $(procs t 2)" "$c" "Variant = \"$v\"" "H = 24"
	done
}

# Relocate (claims A23): a relocation's copy, hidden until its hand-off,
# against the key's touches, a read and an eviction.
relocate() {
	local inv="ReadNotLow TouchNotLow OneHome Conserved" e cf r v
	local k='Touches = 1' rd='Reads = 1' ev='Evict = TRUE' first='CopyFirst = TRUE'
	for e in FALSE TRUE; do
		for cf in TRUE FALSE; do
			run ok Relocate "hidden, evict $e, copy first $cf" "$inv" "$k" "$rd" "Evict = $e" "CopyFirst = $cf" 'Variant = "hidden"'
		done
	done
	if [ "${TLA_BIG:-}" = 1 ]; then
		run ok Relocate "hidden, 2 touches, evict, copy first" "$inv" 'Touches = 2' "$rd" "$ev" "$first" 'Variant = "hidden"'
	fi
	for r in NotMoved NotAbandoned NotEvicted; do
		run violation Relocate "hidden, reachable: $r" "$r" "$k" "$rd" "$ev" "$first" 'Variant = "hidden"'
	done
	run violation Relocate "visible (before A23): a read" "ReadNotLow" "$k" "$rd" 'Evict = FALSE' "$first" 'Variant = "visible"'
	run violation Relocate "visible: a touch" "TouchNotLow" 'Touches = 2' 'Reads = 0' 'Evict = FALSE' "$first" 'Variant = "visible"'
	run violation Relocate "visible: an event lost" "Conserved" 'Touches = 2' 'Reads = 0' "$ev" "$first" 'Variant = "visible"'
	for v in noMoveCount noHandoffCheck; do
		run violation Relocate "$v" "$inv" "$k" "$rd" "$ev" "$first" "Variant = \"$v\""
	done
	run ok Relocate "hiddenNoEvictCheck" "$inv" "$k" "$rd" "$ev" "$first" 'Variant = "hiddenNoEvictCheck"'
}

# RoomWays (claims A24): one wait-room set shared by a key the placer
# commits and a key that waits beside it.
roomways() {
	local inv="KKept KOwn LOnce" w i
	for w in 1 2; do
		run ok RoomWays "fixed, $w way(s)" "$inv" 'Variant = "fixed"' "Ways = $w"
	done
	for i in KKept KOwn; do
		run violation RoomWays "old (before A24): $i" "$i" 'Variant = "old"' "Ways = 2"
	done
}

models=("$@")
[ ${#models[@]} -gt 0 ] || models=(WaitRoom RegisterEpoch Relocate RoomWays)
for m in "${models[@]}"; do
	case $m in
	WaitRoom) waitroom ;;
	RegisterEpoch) registerepoch ;;
	Relocate) relocate ;;
	RoomWays) roomways ;;
	*) echo "unknown model $m"; bad=1 ;;
	esac
done
exit $bad
