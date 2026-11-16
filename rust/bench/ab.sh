#!/usr/bin/env bash
# A/B speed comparison of two versions of the Rust port's benchmarks (go/verify/ab.sh's counterpart).
#
#   bench/ab.sh <A> <B> <bench-regex> [rounds]
#
# A and B are git revisions whose tree has rust/, or . (the working tree, uncommitted and untracked files
# included). Each version's bench binary is built once (cargo bench --no-run, the release profile; CARGO
# as test.sh takes it); the binaries then run alternately (A, B, A, B, ...) `rounds` times (default 8), so
# drift and noisy neighbours hit both sides. Prints min and median ns/op per benchmark and the B/A ratio of
# medians. Speed is host-dependent: compare ratios, not absolute numbers.
#
# A=go compares the Go reference's benchmarks of the same names (the working tree's go/, built with the go
# on PATH) with B: the port against the reference on this host.
set -euo pipefail
cd "$(dirname "$0")/.."
[ $# -ge 3 ] || { sed -n '2,14p' "$0"; exit 2; }
A=$1 B=$2 RE=$3 ROUNDS=${4:-8}
here=$(pwd)
top=$(cd .. && pwd)
CARGO=${CARGO:-cargo}
case $CARGO in /*) ;; */*) CARGO=$here/$CARGO ;; esac
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

build() { # build <spec> <name>: $tmp/<name>.run runs that version's benchmarks matching $RE
	local d="$tmp/$2" exe
	if [ "$1" = go ]; then
		(cd "$top/go" && go test -c -o "$d.test" .)
		printf '#!/bin/sh\nexec "%s" -test.run "^$" -test.bench "$1" -test.count 1\n' "$d.test" >"$d.run"
	else
		mkdir -p "$d/src"
		if [ "$1" = . ]; then
			# the working tree, without build outputs (git ls-files where git can read the checkout)
			if git ls-files -co --exclude-standard -z . >"$d.files" 2>/dev/null && [ -s "$d.files" ]; then
				xargs -0 tar -c <"$d.files" | tar -x -C "$d/src"
			else
				tar -c --exclude=./target --exclude=./verify-report . | tar -x -C "$d/src"
			fi
		else
			git -C "$top" cat-file -e "$1:rust" 2>/dev/null || { echo "ab.sh: $1 has no rust/" >&2; exit 2; }
			git -C "$top" archive "$1" rust | tar -x -C "$d" && rmdir "$d/src" && mv "$d/rust" "$d/src"
		fi
		# The bench's executable, by name: the build lists tfbench's too (cargo builds a package's binaries
		# for its benches), before or after the bench's as the two finish compiling.
		exe=$(CARGO_TARGET_DIR="$d/target" "$CARGO" bench --manifest-path "$d/src/Cargo.toml" --bench bench --no-run \
			--message-format=json-render-diagnostics 2>"$d.log" | grep -o '"executable":"[^"]*/bench-[0-9a-f]*"' | tail -1 | cut -d'"' -f4)
		[ -x "$exe" ] || { cat "$d.log" >&2; exit 1; }
		printf '#!/bin/sh\nexec "%s" -bench "$1" -count 1\n' "$exe" >"$d.run"
	fi
	chmod +x "$d.run"
}
build "$A" a
build "$B" b
for i in $(seq "$ROUNDS"); do
	for s in a b; do
		"$tmp/$s.run" "$RE" 2>/dev/null |
			awk -v s="$s" -v m="ns/op" '$1 ~ /^Benchmark/ { for (i = 3; i < NF; i++) if ($(i+1) == m) print s, $1, $i }' >>"$tmp/results"
	done
done
awk -v A="$A" -v B="$B" -v unit="ns/op" '
function med(arr, n,   i, j, t) { for (i = 1; i <= n; i++) for (j = i + 1; j <= n; j++) if (arr[j] < arr[i]) { t = arr[i]; arr[i] = arr[j]; arr[j] = t } return n % 2 ? arr[(n + 1) / 2] : (arr[n / 2] + arr[n / 2 + 1]) / 2 }
{ k = $1 SUBSEP $2; n[k]++; v[k, n[k]] = $3; names[$2] = 1 }
END {
	printf "A = %s\nB = %s\n%-34s %10s %10s %10s %10s %7s\n", A, B, "benchmark (" unit ")", "A min", "A median", "B min", "B median", "B/A"
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
