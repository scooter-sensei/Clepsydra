#!/usr/bin/env bash
# Placement-randomized A/B of the package's Go benchmarks (claims S54).
#
#   verify/lrab.sh <bench-regex> <layouts> <rounds> <arm>...
#
# An arm is a revision spec as in verify/ab.sh (HEAD, ., 4103013,
# HEAD+verify/experiments/<name>.patch). verify/ab.sh times each version at
# the one code layout its build happens to get, and on the 2026-10-21 host
# placement alone moves a benchmark by up to 19% (S54): more than most
# changes measured. Here every arm is built at <layouts> layouts: a pad
# function of a seeded size (0 to 24 statements, ~0-450 bytes), never run,
# at the top of filter.go, hot.go and register.go, kept in the binary by an
# init behind an environment variable no run sets; layout k has the same pads
# in every arm. Rounds alternate layouts and arms (in reverse order on even
# rounds). Per benchmark it prints
# the first arm's median ns/op and its spread over the layouts (max/min of
# its per-layout medians: what placement alone moves), then each other arm's
# ratio to it, paired at each layout: the geometric mean over layouts and a
# 95% t-interval. A change that runs no new instruction still reads its own
# shift, which the pads randomize only between files: on that host a 32-byte
# shift inside filter.go read up to 1.023 (S54). SEED0=n uses layouts
# n+1..n+<layouts>. Speed is host-dependent: compare ratios on one host.
set -euo pipefail
cd "$(dirname "$0")/.."
. verify/lib.sh
[ $# -ge 5 ] || { sed -n '2,22p' "$0"; exit 2; }
RE=$1 K=$2 ROUNDS=$3
shift 3
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

src() { # src <spec> <dir>: the version's Go module in <dir>
	local rev=${1%%+*} root=$PWD p patches
	if [ "$rev" = . ]; then
		mkdir -p "$2" && git ls-files -co --exclude-standard -z . | xargs -0 tar -c | tar -x -C "$2"
	else
		export_tree "$rev" "$2"
	fi
	IFS=+ read -ra patches <<<"${1#"$rev"}"
	for p in "${patches[@]}"; do
		[ -n "$p" ] && (cd "$2" && git apply "$root/$p")
	done
}

pad() { # pad <dir> <seed>: the layout's pad functions
	local d=$1 seed=$2 i=0 f calls=""
	for f in filter.go hot.go register.go; do
		i=$((i + 1))
		[ -f "$d/$f" ] || continue
		calls="$calls\t\t_ = layoutPad$i(1)\n"
		awk -v n=$(((seed * 7919 + i * 104729) % 25 * (seed > 0))) -v name="layoutPad$i" '
			{ print }
			$0 == "import (" { imp = 1 }
			imp && $0 == ")" {
				imp = 0
				printf "\n// %s (verify/lrab.sh): %d statements, never run.\n//\n//go:noinline\nfunc %s(x uint64) uint64 {\n", name, n, name
				for (j = 0; j < n; j++) printf "\tx = x*0x9e3779b97f4a7c15 ^ x>>%d\n", 7 + (j * 5) % 50
				print "\treturn x\n}"
			}' "$d/$f" >"$d/$f.lr" && mv "$d/$f.lr" "$d/$f"
	done
	printf "package %s\n\nimport \"os\"\n\nfunc init() {\n\tif os.Getenv(\"LRAB_NEVER\") == \"never\" {\n$calls\t}\n}\n" \
		"$(awk '/^package /{print $2; exit}' "$d/filter.go")" >"$d/zzlayout.go"
}

names=()
for a in "$@"; do
	n=a${#names[@]}
	names+=("$n")
	echo "$n = $a"
	src "$a" "$tmp/src-$n"
	for k in $(seq $((${SEED0:-0} + 1)) $((${SEED0:-0} + K))); do
		cp -r "$tmp/src-$n" "$tmp/t"
		pad "$tmp/t" "$k"
		(cd "$tmp/t" && go test -c -o "$tmp/$n-$k.test" .)
		rm -rf "$tmp/t"
	done
done
for r in $(seq "$ROUNDS"); do
	order=("${names[@]}")
	if [ $((r % 2)) = 0 ]; then # even rounds run the arms in reverse: no arm always runs first
		order=()
		for ((i = ${#names[@]} - 1; i >= 0; i--)); do order+=("${names[i]}"); done
	fi
	for k in $(seq $((${SEED0:-0} + 1)) $((${SEED0:-0} + K))); do
		for n in "${order[@]}"; do
			"$tmp/$n-$k.test" -test.run '^$' -test.bench "$RE" -test.count 1 2>/dev/null |
				awk -v s="$n" -v k="$k" '$1 ~ /^Benchmark/ { for (i = 3; i < NF; i++) if ($(i+1) == "ns/op") print s, k, $1, $i }' >>"$tmp/results"
		done
	done
done
awk -v arms="${names[*]}" '
function med(arr, n,   i, j, t) { for (i = 1; i <= n; i++) for (j = i + 1; j <= n; j++) if (arr[j] < arr[i]) { t = arr[i]; arr[i] = arr[j]; arr[j] = t } return n % 2 ? arr[(n + 1) / 2] : (arr[n / 2] + arr[n / 2 + 1]) / 2 }
function t95(df) { return df >= 30 ? 2.04 : T[df] }
BEGIN {
	split("12.71 4.30 3.18 2.78 2.57 2.45 2.36 2.31 2.26 2.23 2.20 2.18 2.16 2.14 2.13 2.12 2.11 2.10 2.09 2.09 2.08 2.07 2.07 2.06 2.06 2.06 2.05 2.05 2.05", T, " ")
	na = split(arms, A, " ")
}
{ c = ++n[$1, $2, $3]; v[$1, $2, $3, c] = $4; ks[$2] = 1; bs[$3] = 1 }
END {
	for (b in bs) {
		nk = 0; lo = 1e300; hi = 0
		for (k in ks) {
			m = n[A[1], k, b]; if (!m) continue
			for (i = 1; i <= m; i++) x[i] = v[A[1], k, b, i]
			base[k] = med(x, m); nk++
			if (base[k] < lo) lo = base[k]; if (base[k] > hi) hi = base[k]
			all[nk] = base[k]
		}
		line = sprintf("%-34s %s %7.1f ns (layouts %.3f) |", b, A[1], med(all, nk), hi / lo)
		for (a = 2; a <= na; a++) {
			s = 0; ss = 0; c = 0
			for (k in ks) {
				m = n[A[a], k, b]; if (!m || !(k in base)) continue
				for (i = 1; i <= m; i++) x[i] = v[A[a], k, b, i]
				r = log(med(x, m) / base[k]); s += r; ss += r * r; c++
			}
			mean = s / c; sd2 = c > 1 ? (ss - c * mean * mean) / (c - 1) : 0
			h = sd2 > 0 ? t95(c - 1) * sqrt(sd2 / c) : 0
			line = line sprintf("  %s %.3f [%.3f, %.3f]", A[a], exp(mean), exp(mean - h), exp(mean + h))
		}
		print line
		delete base
	}
}' "$tmp/results"
