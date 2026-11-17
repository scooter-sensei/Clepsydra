#!/usr/bin/env bash
# lab/sw W8's readings (SW-RESULTS.md): TLC on go/verify/tla/experiments/DripPush.tla; outputs in lab/w8/.
#   lab/w8.sh [small|large|all]   small: clause 1, the reported runs and clause 2's one-touch runs;
#                                 large: clause 2's two-touch runs; all (default): both, small first
# tla.sh's run(), with a time cap per run (TLC_TIMEOUT, default 1800 s: Relocate.tla at 2 touches and
# 0 reads, the base, takes 15 s), TLC's scratch on disk, every run's tlc.log and cfg kept in lab/w8/.
set -uo pipefail
what=${1:-all}
L=$HOME/lab-sw; O=$L/lab/w8; mkdir -p $O
cd $L/go/verify/tla/experiments || exit 2
J=${TLA2TOOLS:-$HOME/tools/tla/tla2tools.jar}
[ -f "$J" ] || { echo "tla2tools.jar not found (set TLA2TOOLS)"; exit 2; }
export TMPDIR=${TMPDIR_DISK:-$HOME/tlc-tmp}; mkdir -p "$TMPDIR"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
[ "$what" = large ] || : >$O/results.txt
git -C $L log --oneline -1 >>$O/results.txt
bad=0 runs=$(ls $O/run*.log 2>/dev/null | wc -l) got=""

# run <want: ok|violation|report> <label> <invariants> <constant definitions...>; sets got
run() {
	local want=$1 label=$2 inv=$3 m=DripPush states why c d
	shift 3
	got=ok
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
	(cd "$d" && timeout "${TLC_TIMEOUT:-1800}" java -XX:+UseParallelGC -cp "$J" tlc2.TLC -workers "${TLC_WORKERS:-4}" -cleanup "$m" >tlc.log 2>&1)
	[ $? = 124 ] && got=timeout
	grep -q 'is violated\|Deadlock reached\|^Error:' "$d/tlc.log" && got=violation
	states=$(grep -o '[0-9,]* distinct states found' "$d/tlc.log" | tail -1)
	why=$(grep -m1 -o 'Invariant [A-Za-z0-9]* is violated\|Deadlock reached\|unexpected exception\|Parsing or semantic analysis failed' "$d/tlc.log")
	cp "$d/tlc.log" "$O/run$runs.log"; cp "$d/$m.cfg" "$O/run$runs.cfg"
	printf '%-3s %-60s %-9s (want %s) %s%s\n' "$runs" "$label" "$got" "$want" "$states" "${why:+; $why}" | tee -a $O/results.txt
	[ "$want" = report ] || [ "$got" = "$want" ] || bad=1
}

# either <label>: a sight pair, judged as one: at least one of its two runs violated
either() {
	if [ "$1" = violation ] || [ "$2" = violation ]; then echo "    $3: seen" | tee -a $O/results.txt
	else echo "    $3: NOT seen" | tee -a $O/results.txt; bad=1; fi
}

inv="PushOnce LogComplete TouchNotLow ReadNotLow OneHome Conserved"
r0='Reads = 0'
if [ "$what" != large ]; then
	echo "== clause 1: sight" | tee -a $O/results.txt
	run report "w4b, 1 touch, relocation only, copy first TRUE" PushOnce 'Touches = 1' 'Burst = FALSE' "$r0" 'Evict = FALSE' 'CopyFirst = TRUE' 'Variant = "w4b"'; g1=$got
	run report "w4b, 1 touch, relocation only, copy first FALSE" PushOnce 'Touches = 1' 'Burst = FALSE' "$r0" 'Evict = FALSE' 'CopyFirst = FALSE' 'Variant = "w4b"'; g2=$got
	either $g1 $g2 "W4b's double push (PushOnce)"
	run report "w4c, 1 touch, evict, copy first TRUE" PushOnce 'Touches = 1' 'Burst = FALSE' "$r0" 'Evict = TRUE' 'CopyFirst = TRUE' 'Variant = "w4c"'; g1=$got
	run report "w4c, 1 touch, evict, copy first FALSE" PushOnce 'Touches = 1' 'Burst = FALSE' "$r0" 'Evict = TRUE' 'CopyFirst = FALSE' 'Variant = "w4c"'; g2=$got
	either $g1 $g2 "W4c's double push through miss (PushOnce)"
	run report "tickOnly, 1 touch, burst, relocation only, copy first TRUE" LogComplete 'Touches = 1' 'Burst = TRUE' "$r0" 'Evict = FALSE' 'CopyFirst = TRUE' 'Variant = "tickOnly"'; g1=$got
	run report "tickOnly, 1 touch, burst, relocation only, copy first FALSE" LogComplete 'Touches = 1' 'Burst = TRUE' "$r0" 'Evict = FALSE' 'CopyFirst = FALSE' 'Variant = "tickOnly"'; g2=$got
	either $g1 $g2 "a lost push (LogComplete)"
	for r in NotMoved NotEvicted; do
		run violation "w4d, reachable: $r" "$r" 'Touches = 1' 'Burst = FALSE' "$r0" 'Evict = TRUE' 'CopyFirst = TRUE' 'Variant = "w4d"'
	done
	echo "== reported" | tee -a $O/results.txt
	run report "w4c, 1 touch, relocation only, copy first TRUE" "$inv" 'Touches = 1' 'Burst = FALSE' "$r0" 'Evict = FALSE' 'CopyFirst = TRUE' 'Variant = "w4c"'
	for k in 1 2; do
		run report "w4d, $k touch(es), evict, reachable: NotRepushed" NotRepushed "Touches = $k" 'Burst = FALSE' "$r0" 'Evict = TRUE' 'CopyFirst = TRUE' 'Variant = "w4d"'
	done
fi
echo "== clause 2: w4d ($what)" | tee -a $O/results.txt
ks="1 2"; [ "$what" = small ] && ks=1; [ "$what" = large ] && ks=2
for k in $ks; do
	for bu in FALSE TRUE; do
		for e in FALSE TRUE; do
			for cf in TRUE FALSE; do
				run ok "w4d, $k touch(es), burst $bu, evict $e, copy first $cf" "$inv" "Touches = $k" "Burst = $bu" "$r0" "Evict = $e" "CopyFirst = $cf" 'Variant = "w4d"'
			done
		done
	done
done
echo "W8 readings ($what) finished: $([ $bad = 0 ] && echo 'every judged outcome as wanted' || echo 'a judged outcome not as wanted')" | tee -a $O/results.txt
