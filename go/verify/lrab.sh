#!/usr/bin/env bash
# Placement-randomized A/B of the package's Go benchmarks (claims S54).
#
#   verify/lrab.sh <bench-regex> <layouts> <rounds> <arm>...
#
# An arm is a revision spec as in verify/ab.sh (HEAD, ., 4103013,
# HEAD+verify/experiments/<name>.patch). verify/ab.sh times each version at
# the one code layout its build happens to get, and on the Ryzen host (docs/README.md, Hosts)
# placement moves a benchmark by a few percent (S54, S55). Here every arm is
# built at <layouts> layouts: never-run pad functions of 0 to 24 statements
# (~0-450 bytes), sized independently by a hash of the layout's seed and the
# pad's place, at the top of filter.go, hot.go and register.go and inside
# filter.go before touch, hit and locate, kept in the binary by an init
# behind an environment variable no run sets; layout k has the same pads in
# every arm. Rounds alternate layouts and arms (in reverse order on even
# rounds). Per benchmark it prints the first arm's median and its spread
# over the layouts (max/min of its per-layout medians), how much of the
# readings' variation is the layouts' own (sd layout, the code's placement)
# and how much is run to run at one layout (sd run, the host), as standard
# deviations of log readings; then each other arm's ratio to it, paired at
# each layout: the geometric mean over layouts and a 95% t-interval.
# METRIC=cycles/op reads the benchmarks' cycle counts (Linux), which the
# host's clock frequency doesn't move (S56). NULL=1 adds an arm that runs
# the first arm's binaries again: its interval is what the host alone gives
# a change of nothing. SEED0=n uses layouts n+1..n+<layouts>. Speed is
# host-dependent: compare ratios on one host.
set -euo pipefail
cd "$(dirname "$0")/.."
. verify/lib/lib.sh
[ $# -ge 5 ] || { sed -n '2,26p' "$0"; exit 2; }
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

padn() { # padn <seed> <point>: a pad of 0 to 24 statements, independent across points (a 32-bit integer hash)
	local x=$((($1 * 0x9E3779B1 + $2 * 0x85EBCA77 + 0x165667B1) & 0xFFFFFFFF))
	x=$(((((x >> 16) ^ x) * 0x45D9F3B) & 0xFFFFFFFF))
	x=$(((((x >> 16) ^ x) * 0x45D9F3B) & 0xFFFFFFFF))
	echo $((((x >> 16) ^ x) % 25 * ($1 > 0)))
}

pad() { # pad <dir> <seed>: the layout's pad functions
	local d=$1 seed=$2 i=0 p f fn calls=""
	# The top of three files, and inside filter.go before three hot functions
	# (with their doc comments), so the hot code moves within files too.
	for p in filter.go: hot.go: register.go: filter.go:touch filter.go:hit filter.go:locate; do
		i=$((i + 1)) f=${p%%:*} fn=${p#*:}
		[ -f "$d/$f" ] || continue
		awk -v n="$(padn "$seed" $i)" -v name="layoutPad$i" -v fn="$fn" '
			function padf() {
				printf "\n// %s (verify/lrab.sh): %d statements, never run.\n//\n//go:noinline\nfunc %s(x uint64) uint64 {\n", name, n, name
				for (j = 0; j < n; j++) printf "\tx = x*0x9e3779b97f4a7c15 ^ x>>%d\n", 7 + (j * 5) % 50
				print "\treturn x\n}\n"
			}
			fn == "" { print }
			fn == "" && $0 == "import (" { imp = 1 }
			fn == "" && imp && $0 == ")" { imp = 0; padf() }
			fn != "" && /^\/\// { buf = buf $0 "\n"; next }
			fn != "" && !done && $0 ~ "^func \\(f \\*Filter\\) " fn "\\(" { padf(); done = 1 }
			fn != "" { printf "%s", buf; buf = ""; print }
			END { printf "%s", buf }' "$d/$f" >"$d/$f.lr" && mv "$d/$f.lr" "$d/$f"
		if grep -q "^func layoutPad$i(" "$d/$f"; then calls="$calls\t\t_ = layoutPad$i(1)\n"; fi
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
if [ "${NULL:-0}" = 1 ]; then # the first arm again, binary for binary: what the host alone does to a ratio
	for k in $(seq $((${SEED0:-0} + 1)) $((${SEED0:-0} + K))); do cp "$tmp/a0-$k.test" "$tmp/null-$k.test"; done
	names+=(null)
	echo "null = a0's binaries, run as another arm"
fi
for r in $(seq "$ROUNDS"); do
	order=("${names[@]}")
	if [ $((r % 2)) = 0 ]; then # even rounds run the arms in reverse: no arm always runs first
		order=()
		for ((i = ${#names[@]} - 1; i >= 0; i--)); do order+=("${names[i]}"); done
	fi
	for k in $(seq $((${SEED0:-0} + 1)) $((${SEED0:-0} + K))); do
		for n in "${order[@]}"; do
			"$tmp/$n-$k.test" -test.run '^$' -test.bench "$RE" -test.count 1 2>/dev/null |
				awk -v s="$n" -v k="$k" -v m="${METRIC:-ns/op}" '$1 ~ /^Benchmark/ { for (i = 3; i < NF; i++) if ($(i+1) == m) print s, k, $1, $i }' >>"$tmp/results"
		done
	done
done
awk -v arms="${names[*]}" -v unit="${METRIC:-ns/op}" '
function med(arr, n,   i, j, t) { for (i = 1; i <= n; i++) for (j = i + 1; j <= n; j++) if (arr[j] < arr[i]) { t = arr[i]; arr[i] = arr[j]; arr[j] = t } return n % 2 ? arr[(n + 1) / 2] : (arr[n / 2] + arr[n / 2 + 1]) / 2 }
# sd(b): one-way random-effects components of the first arm`s log readings, layouts as groups:
# sl, the layouts` own spread (code placement), and se, run to run at one layout (the host).
function sd(b,   k, i, m, l, s, sw, dfw, sb, nk, nall, gm, msw, msb) {
	sw = dfw = sb = nk = nall = gm = 0
	for (k in ks) {
		m = n[A[1], k, b]; if (!m) continue
		s = 0; for (i = 1; i <= m; i++) s += log(v[A[1], k, b, i])
		mu[k] = s / m; cnt[k] = m; nk++; nall += m; gm += s
		for (i = 1; i <= m; i++) { l = log(v[A[1], k, b, i]) - mu[k]; sw += l * l }
		dfw += m - 1
	}
	gm /= nall
	for (k in mu) sb += cnt[k] * (mu[k] - gm) ^ 2
	msw = dfw ? sw / dfw : 0; msb = nk > 1 ? sb / (nk - 1) : 0
	se = sqrt(msw); sl = msb > msw ? sqrt((msb - msw) / (nall / nk)) : 0
	delete mu; delete cnt
}
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
		sd(b)
		line = sprintf("%-34s %s %7.1f %s (layouts %.3f; sd layout %.3f, run %.3f) |", b, A[1], med(all, nk), unit, hi / lo, sl, se)
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
