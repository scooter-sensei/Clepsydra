#!/usr/bin/env bash
# lab/sw W8's readings (SW-RESULTS.md): TLC on go/verify/tla/experiments/DripPush.tla.
#   lab/w8.sh [small|large|two|all]   small: one toucher, one touch (clause 1's one-toucher sight, the
#                                     reported runs, clause 2's one-touch configurations); large: one
#                                     toucher, two touches (clause 2's two-touch configurations, NotRepushed's
#                                     two-touch run); two: two touchers, one touch each (clause 1's
#                                     two-toucher sight, clause 3, NotRepushed's two-toucher run);
#                                     all (default): the three, in that order
# Outputs in lab/w8/small, lab/w8/large and lab/w8/two, each cleared when its phase starts: results.txt,
# and every run's tlc.log and cfg (runN.log, runN.cfg), numbered from 1 within the phase.
# A run is "ok" only if TLC exits 0 and prints its completion line with no error; "violation" only if
# TLC reports the invariant or property the run names; "timeout" (TLC_TIMEOUT, default 1800 s: Relocate.tla
# at 2 touches and 0 reads, the base, takes 15 s) is reported with its states and not judged; anything else
# (a deadlock, a translation failure, a TLC error or crash) is "error", never as wanted. TLC's scratch is on
# disk (TMPDIR_DISK, default ~/tlc-tmp). Exits 1 if a judged outcome is not as wanted, 2 if it cannot start.
set -uo pipefail
what=${1:-all}
case $what in small | large | two | all) ;; *) echo "usage: lab/w8.sh [small|large|two|all]"; exit 2 ;; esac
L=$HOME/lab-sw M=go/verify/tla/experiments/DripPush.tla
J=${TLA2TOOLS:-$HOME/tools/tla/tla2tools.jar}
[ -f "$J" ] || { echo "tla2tools.jar not found (set TLA2TOOLS)"; exit 2; }
command -v java >/dev/null || { echo "java not found"; exit 2; }
[ -z "$(git -C $L status --porcelain -- $M lab/w8.sh)" ] || { echo "$M or lab/w8.sh differs from the commit: commit it first"; exit 2; }
export TMPDIR=${TMPDIR_DISK:-$HOME/tlc-tmp}
mkdir -p "$TMPDIR"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
bad=0 pbad=0 timeouts=0 O="" runs=0 got="" why=""

# phase <name>: outputs to lab/w8/<name>, cleared, runs numbered from 1
phase() {
	O=$L/lab/w8/$1
	rm -rf "$O" && mkdir -p "$O"
	runs=0 pbad=0 timeouts=0
	{ git -C $L log --oneline -1; echo "lab/w8.sh $1"; } >"$O/results.txt"
}

# phase_end: the phase's verdict line
phase_end() {
	local msg="every judged outcome as wanted"
	[ $pbad = 0 ] || { msg="a judged outcome not as wanted"; bad=1; }
	[ $timeouts = 0 ] || msg="$msg; $timeouts judged run(s) or pair(s) timed out, not judged: their clause is judged on fewer runs"
	echo "W8 readings (${O##*/}) finished: $msg" | tee -a "$O/results.txt"
}

# classify <tlc.log> <exit code> <names>: sets got (ok, violation, timeout, error) and why
classify() {
	local log=$1 rc=$2 n
	why=$(grep -m1 -o 'Invariant [A-Za-z0-9]* is violated\|Temporal properties were violated\|Deadlock reached\|unexpected exception\|Parsing or semantic analysis failed\|OutOfMemoryError' "$log")
	got=error
	if [ "$rc" = 124 ]; then
		got=timeout
		return
	fi
	for n in $3; do
		if [ "$n" = Termination ]; then
			grep -q '^Error: Temporal properties were violated' "$log" && got=violation
		elif grep -q "^Error: Invariant $n is violated" "$log"; then
			got=violation
		fi
	done
	[ "$got" = violation ] && return
	if [ "$rc" = 0 ] && grep -q '^Model checking completed. No error has been found.' "$log" && ! grep -q '^Error:' "$log"; then
		got=ok
	elif [ -z "$why" ]; then # an error TLC names otherwise, or none (exit code)
		why=$(grep -m1 -i 'error\|exception\|not found' "$log" | cut -c1-90)
		why=${why:-"exit $rc"}
	fi
}

# run <want: ok|violation|report> <label> <checks: "INVARIANTS a b ..." or "PROPERTY Termination"> <constant definitions...>
# sets got; a judged outcome other than want, or an error in any run, sets bad; a timeout is counted, not judged
run() {
	local want=$1 label=$2 checks=$3 m=DripPush states="" rc d c
	shift 3
	runs=$((runs + 1))
	got=error why=""
	d="$tmp/${O##*/}-$runs"
	mkdir -p "$d" && tr -d '\r' <"$L/$M" >"$d/$m.tla"
	if (cd "$d" && java -cp "$J" pcal.trans -nocfg "$m.tla" >pcal.log 2>&1); then
		{
			echo "SPECIFICATION Spec"
			echo "CONSTANTS"
			for c in "$@"; do echo "  $c"; done
			echo "$checks"
		} >"$d/$m.cfg"
		(cd "$d" && timeout "${TLC_TIMEOUT:-1800}" java -XX:+UseParallelGC -cp "$J" tlc2.TLC -workers "${TLC_WORKERS:-4}" -cleanup "$m" >tlc.log 2>&1)
		rc=$?
		classify "$d/tlc.log" "$rc" "${checks#* }"
		states=$(grep -o '[0-9,]* distinct states found' "$d/tlc.log" | tail -1)
		cp "$d/tlc.log" "$O/run$runs.log"
		cp "$d/$m.cfg" "$O/run$runs.cfg"
	else
		why="translation failed"
		cp "$d/pcal.log" "$O/run$runs.pcal.log"
	fi
	printf '%-3s %-66s %-9s (want %s) %s%s\n' "$runs" "$label" "$got" "$want" "$states" "${why:+; $why}" | tee -a "$O/results.txt"
	case $got in
	timeout) [ "$want" = report ] || timeouts=$((timeouts + 1)) ;;
	error) pbad=1 ;;
	*) [ "$want" = report ] || [ "$got" = "$want" ] || pbad=1 ;;
	esac
}

# either <got1> <got2> <label> [report]: a sight pair, judged as one (reported only, with "report"):
# seen if either run violated; not judged if neither violated and both timed out; else NOT seen
either() {
	local msg judged=1
	[ "${4:-}" = report ] && judged=0
	if [ "$1" = violation ] || [ "$2" = violation ]; then
		msg="seen"
	elif [ "$1" = timeout ] && [ "$2" = timeout ]; then
		msg="not judged (both runs timed out)"
		[ $judged = 1 ] && timeouts=$((timeouts + 1))
	else
		msg="NOT seen"
		{ [ "$1" = timeout ] || [ "$2" = timeout ]; } && msg="$msg (on one run: the other timed out)"
		[ $judged = 1 ] && pbad=1
	fi
	echo "    $3: $msg" | tee -a "$O/results.txt"
}

# pair <judged|report> <what a violation shows> <label> <checks> <constant definitions but CopyFirst...>:
# a sight pair, the two scan orders, judged (or reported) as one by either
pair() {
	local how=$1 shows=$2 label=$3 checks=$4 g1 g2
	shift 4
	run report "$label, copy first TRUE" "$checks" "$@" 'CopyFirst = TRUE'; g1=$got
	run report "$label, copy first FALSE" "$checks" "$@" 'CopyFirst = FALSE'; g2=$got
	if [ "$how" = judged ]; then either "$g1" "$g2" "$shows (${checks#* })"; else either "$g1" "$g2" "$shows (${checks#* })" report; fi
}

# configs <clause> <touchers> <touches>: W4d in the 8 configurations, each with the invariants and with Termination
configs() {
	local bu e cf c n="$2 toucher(s) x $3 touch(es)"
	echo "== clause $1: w4d, $n" | tee -a "$O/results.txt"
	for bu in FALSE TRUE; do
		for e in FALSE TRUE; do
			for cf in TRUE FALSE; do
				c=("Touchers = $2" "Touches = $3" "Burst = $bu" "$r0" "Evict = $e" "CopyFirst = $cf" 'Variant = "w4d"')
				run ok "w4d, $n, burst $bu, evict $e, copy first $cf" "INVARIANTS $inv" "${c[@]}"
				run ok "w4d, $n, burst $bu, evict $e, copy first $cf: Termination" "PROPERTY Termination" "${c[@]}"
			done
		done
	done
}

# --- the readings ---
inv="PushOnce CountLogged LogComplete TouchNotLow OneHome Conserved" # ReadNotLow cannot fail at Reads = 0
r0='Reads = 0'
one=('Touchers = 1' 'Touches = 1' "$r0")
if [ "$what" = small ] || [ "$what" = all ]; then
	phase small
	echo "== clause 1: sight, one toucher" | tee -a "$O/results.txt"
	pair judged "W4b's double push" "w4b, 1 touch, relocation only" "INVARIANTS PushOnce" "${one[@]}" 'Burst = FALSE' 'Evict = FALSE' 'Variant = "w4b"'
	pair judged "W4c's double push through miss" "w4c, 1 touch, evict" "INVARIANTS PushOnce" "${one[@]}" 'Burst = FALSE' 'Evict = TRUE' 'Variant = "w4c"'
	pair judged "a lost push" "tickOnly, 1 touch, burst, relocation only" "INVARIANTS LogComplete" "${one[@]}" 'Burst = TRUE' 'Evict = FALSE' 'Variant = "tickOnly"'
	for r in NotMoved NotEvicted; do
		run violation "w4d, reachable: $r" "INVARIANTS $r" "${one[@]}" 'Burst = FALSE' 'Evict = TRUE' 'CopyFirst = TRUE' 'Variant = "w4d"'
	done
	echo "== reported" | tee -a "$O/results.txt"
	run report "w4c, 1 touch, relocation only, copy first TRUE" "INVARIANTS $inv" "${one[@]}" 'Burst = FALSE' 'Evict = FALSE' 'CopyFirst = TRUE' 'Variant = "w4c"'
	run report "w4d, 1 touch, evict, reachable: NotRepushed" "INVARIANTS NotRepushed" "${one[@]}" 'Burst = FALSE' 'Evict = TRUE' 'CopyFirst = TRUE' 'Variant = "w4d"'
	pair report "a push again into a history from before it" "noHandoffCheck, 1 touch, relocation only" "INVARIANTS NotRepushed" "${one[@]}" 'Burst = FALSE' 'Evict = FALSE' 'Variant = "noHandoffCheck"'
	pair report "a count into a copy from before the push, at the touch's tick, the push skipped" "noHandoffCheck, 1 touch, burst, relocation only" "INVARIANTS CountLogged" "${one[@]}" 'Burst = TRUE' 'Evict = FALSE' 'Variant = "noHandoffCheck"'
	for cf in TRUE FALSE; do # one toucher, spread: a copy from before the push is behind the touch's tick
		run report "noHandoffCheck, 1 touch, relocation only, copy first $cf: CountLogged" "INVARIANTS CountLogged" "${one[@]}" 'Burst = FALSE' 'Evict = FALSE' "CopyFirst = $cf" 'Variant = "noHandoffCheck"'
	done
	configs 2 1 1
	phase_end
fi
if [ "$what" = large ] || [ "$what" = all ]; then
	phase large
	echo "== reported" | tee -a "$O/results.txt"
	run report "w4d, 2 touches, evict, reachable: NotRepushed" "INVARIANTS NotRepushed" 'Touchers = 1' 'Touches = 2' "$r0" 'Burst = FALSE' 'Evict = TRUE' 'CopyFirst = TRUE' 'Variant = "w4d"'
	configs 2 1 2
	phase_end
fi
if [ "$what" = two ] || [ "$what" = all ]; then
	phase two
	two=('Touchers = 2' 'Touches = 1' "$r0")
	echo "== clause 1: sight, two touchers" | tee -a "$O/results.txt"
	pair judged "a count into a copy from before the push, moved to the touch's tick by the other toucher's push" "noHandoffCheck, 2 touchers x 1 touch, relocation only" "INVARIANTS CountLogged" "${two[@]}" 'Burst = FALSE' 'Evict = FALSE' 'Variant = "noHandoffCheck"'
	echo "== reported" | tee -a "$O/results.txt"
	run report "w4d, 2 touchers x 1 touch, evict, reachable: NotRepushed" "INVARIANTS NotRepushed" "${two[@]}" 'Burst = FALSE' 'Evict = TRUE' 'CopyFirst = TRUE' 'Variant = "w4d"'
	configs 3 2 1
	phase_end
fi
exit $bad
