#!/usr/bin/env bash
# A/B speed comparison of two versions of the package's Go benchmarks.
#
#   verify/ab.sh <A> <B> <bench-regex> [rounds]
#
# A and B are git revisions, optionally with patches: HEAD,
# HEAD+verify/experiments/branchless-find.patch, 983b804. Each version's test
# binary is built once; the binaries then run alternately (A, B, A, B, ...)
# `rounds` times (default 8), so drift and noisy neighbours hit both sides.
# Prints min and median ns/op per benchmark and the B/A ratio of medians.
# Speed is host-dependent: compare ratios, not absolute numbers.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
[ $# -ge 3 ] || { sed -n '2,12p' "$0"; exit 2; }
A=$1 B=$2 RE=$3 ROUNDS=${4:-8}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

build() { # build <spec> <name>
	local rev=${1%%+*} d="$tmp/$2"
	mkdir -p "$d" && git archive "$rev" | tar -x -C "$d"
	local root=$PWD p patches
	IFS=+ read -ra patches <<<"${1#"$rev"}"
	for p in "${patches[@]}"; do
		[ -n "$p" ] && (cd "$d" && git apply "$root/$p")
	done
	(cd "$d" && go test -c -o "$tmp/$2.test" .)
}
build "$A" a
build "$B" b
for i in $(seq "$ROUNDS"); do
	for s in a b; do
		"$tmp/$s.test" -test.run '^$' -test.bench "$RE" -test.count 1 2>/dev/null |
			awk -v s="$s" '$1 ~ /^Benchmark/ { for (i = 3; i < NF; i++) if ($(i+1) == "ns/op") print s, $1, $i }' >>"$tmp/results"
	done
done
awk -v A="$A" -v B="$B" '
function med(arr, n,   i, j, t) { for (i = 1; i <= n; i++) for (j = i + 1; j <= n; j++) if (arr[j] < arr[i]) { t = arr[i]; arr[i] = arr[j]; arr[j] = t } return n % 2 ? arr[(n + 1) / 2] : (arr[n / 2] + arr[n / 2 + 1]) / 2 }
{ k = $1 SUBSEP $2; n[k]++; v[k, n[k]] = $3; names[$2] = 1 }
END {
	printf "A = %s\nB = %s\n%-34s %10s %10s %10s %10s %7s\n", A, B, "benchmark (ns/op)", "A min", "A median", "B min", "B median", "B/A"
	for (b in names) {
		for (s = 0; s < 2; s++) {
			side = s ? "b" : "a"; k = side SUBSEP b; m = n[k]
			for (i = 1; i <= m; i++) x[i] = v[k, i]
			mn[side] = x[1]; for (i = 2; i <= m; i++) if (x[i] < mn[side]) mn[side] = x[i]
			md[side] = med(x, m)
		}
		printf "%-34s %10.1f %10.1f %10.1f %10.1f %7.2f\n", b, mn["a"], md["a"], mn["b"], md["b"], md["b"] / md["a"]
	}
}' "$tmp/results"
