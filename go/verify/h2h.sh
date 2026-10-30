#!/bin/bash
# Head-to-head, part 1 (docs/artifact/head-to-head.md): Clepsydra against the
# industry structures on this host, under the same floods. Builds both
# runners (go/cmd/h2h, bench/h2h), writes the traces once, runs every pass of
# every arm for ROUNDS rounds with the arms' order rotating, re-runs a void
# pass (recorded in void.jsonl), then the count-min sizing runs and the
# ECM-sketch supplement, and prints the report. Part 2: go run ./cmd/h2h ecm.
#
#   verify/h2h.sh [workdir]
#   env: N=262144 ROUNDS=5 THREADS=12 PASSES="acc t1 t12 lat1 lat12" SIZING="8 64"
#        PARTS="main sizing ecm" (a workdir with traces and sizing.txt can run one part)
#        CARGO_TARGET_DIR (default ~/h2h-target); Rust's linker: CARGO_TARGET_*_LINKER
set -euo pipefail
cd "$(dirname "$0")/.."
WORK=${1:-$HOME/h2h-work}
N=${N:-262144}
ROUNDS=${ROUNDS:-5}
THREADS=${THREADS:-12}
PASSES=${PASSES:-acc t1 t12 lat1 lat12}
SIZING=${SIZING:-8 64}
PARTS=" ${PARTS:-main sizing ecm} "
export CARGO_TARGET_DIR=${CARGO_TARGET_DIR:-$HOME/h2h-target}
mkdir -p "$WORK"
go build -o "$WORK/h2h-go" ./cmd/h2h
(cd ../bench/h2h && cargo build --release -q 2>/dev/null)
RUST=$CARGO_TARGET_DIR/release/h2h
FLOODS="heavy1x heavy2x heavy4x heavy8x heavy16x light16x"
[ -f "$WORK/light16x.trace" ] || "$WORK/h2h-go" gen -dir "$WORK" -n "$N" | tee "$WORK/traces.txt"

# pingora-limits gets Clepsydra's peak bytes (after the largest flood):
# 2 estimators x 4 rows x 8 B per slot.
if [ -z "${SLOTS:-}" ] && [ -s "$WORK/sizing.txt" ]; then
	peak=$(sed -n 's/^Clepsydra peak bytes \([0-9]*\).*/\1/p' "$WORK/sizing.txt")
	SLOTS=$((peak / 64))
elif [ -z "${SLOTS:-}" ]; then
	peak=$("$WORK/h2h-go" run -trace "$WORK/heavy16x.trace" -pass acc | sed 's/.*"bytes":\([0-9]*\).*/\1/')
	SLOTS=$((peak / 64))
	echo "Clepsydra peak bytes $peak: pingora-limits 2 x 4 x $SLOTS slots ($((SLOTS * 64)) B)" | tee "$WORK/sizing.txt"
fi

# one ARM LABEL SLOTS FLOOD PASS ROUND: one pass, re-run while void.
one() {
	local arm=$1 label=$2 slots=$3 fl=$4 p=$5 r=$6 ans="" out
	[ "$p" = acc ] && ans="$WORK/ans-$label-$fl-r$r.bin"
	for _ in 1 2 3; do
		case $arm in
		clepsydra) out=$("$WORK/h2h-go" run -trace "$WORK/$fl.trace" -pass "$p" -round "$r" -threads "$THREADS" ${ans:+-answers "$ans"}) ;;
		*) out=$("$RUST" --arm "$arm" --label "$label" --slots "$slots" --trace "$WORK/$fl.trace" --pass "$p" --round "$r" --threads "$THREADS" ${ans:+--answers "$ans"}) || true ;;
		esac
		if [[ $out == *'"note":"void'* ]]; then
			echo "$out" >>"$WORK/void.jsonl"
			continue
		fi
		echo "$out"
		return
	done
	echo "h2h.sh: $label $fl $p round $r void three times" >&2
	exit 1
}

arms=(clepsydra pingora dashmap)
[[ $PARTS == *" main "* ]] && for r in $(seq 1 "$ROUNDS"); do
	for fl in $FLOODS; do
		for p in $PASSES; do
			for i in 0 1 2; do
				a=${arms[$(((i + r - 1) % 3))]}
				one "$a" "$a" "$SLOTS" "$fl" "$p" "$r"
			done
		done
	done
done | tee -a "$WORK/results.jsonl"

# Sizing: the count-min estimator with 8x and 64x Clepsydra's bytes, accuracy only.
[[ $PARTS == *" sizing "* ]] && for m in $SIZING; do
	for r in $(seq 1 "$ROUNDS"); do
		for fl in $FLOODS; do
			one pingora "pingora-x$m" $((SLOTS * m)) "$fl" acc "$r"
		done
	done
done | tee -a "$WORK/results.jsonl"

"$WORK/h2h-go" report -dir "$WORK" | tee "$WORK/report.md"
