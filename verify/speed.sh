#!/usr/bin/env bash
# Re-measures every speed claim, one comparison at a time, and prints a
# Markdown report (verify/speed.md was produced this way). Host-dependent:
# compare the B/A ratios, not absolute numbers. Takes about an hour and
# ~2 GB of RAM (the DRAM benchmarks build 600 MB tables).
#
#   verify/speed.sh > verify/speed.md
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
X=verify/experiments
ab() { # ab <claim> <A> <B> <regex> [rounds]
	echo "### $1"
	echo
	echo '```'
	echo "verify/ab.sh $2 $3 '$4' ${5:-8}"
	verify/ab.sh "$2" "$3" "$4" "${5:-8}" 2>&1 | sed '1,2d'
	echo '```'
	echo
}
echo "# Speed measurements"
echo
echo "Produced by \`verify/speed.sh\` on $(date -u +%Y-%m-%d). Each block is \`verify/ab.sh\`: both builds run alternately, ns/op min and median over the rounds, B/A = ratio of medians. Benchmarks with the same name are compared as each commit defines them."
echo
echo '```'
echo "$(go version); $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ //') x $(nproc); L3 $(cat /sys/devices/system/cpu/cpu0/cache/index3/size 2>/dev/null)"
echo '```'
echo
echo "## Scorecard speed columns at HEAD (cmd/tfbench, best of 3 per column, 3 runs)"
echo
echo '```'
grep -m1 '^| when' results/history.md
for i in 1 2 3; do go run ./cmd/tfbench 2>/dev/null | tail -1; done
echo '```'
echo
echo "## Comparisons"
echo
ab "Clock read: built-in monotonic vs CoarseClock (same build)" HEAD HEAD 'Clock'
ab "Batch APIs vs single calls, L3-resident 1M-key table (same build)" HEAD HEAD 'CheckAndTouchHit$|CheckAndTouchBatch$|TouchAndCount$|TouchAndCountBatch|VelocityCount$|VelocityCountBatch'
ab "DRAM-sized 32M-key table: hit, batch, absent, velocity (same build)" HEAD HEAD 'Huge' 3
ab "Velocity-aware eviction (2a526a8): insert unchanged" a5292e3 2a526a8 'CheckAndTouchMissInsert$'
ab "Striped first-touch lock (a8a2848): insert cost" bc2e12d a8a2848 'CheckAndTouchMissInsert$|CheckAndTouchHit$'
ab "TrailingZeros increment (c8dbb54): hot-key hit" a8a2848 c8dbb54 'CheckAndTouchHitHot$'
ab "Displaced bit (6bf61f0): no speed cost" c8dbb54 6bf61f0 'CheckAndTouchHit$|CheckAndTouchHitHot$|CheckAndTouchMissInsert$|VelocityCount$'
ab "Register published with key (e42327a): saturated inserts" f07b04e e42327a 'InsertSaturated$|InsertSteady$'
ab "Estimate without re-bucketing (24f133b): VelocityCount" d5648a8 24f133b 'VelocityCount$'
ab "Depth 4 / budget 1000 (9f2529f) vs depth 3 / budget 500 (acea1cf): insert cost" 9f2529f acea1cf 'InsertSaturated$|InsertNearFull$|CheckAndTouchMissInsert$'
ab "Branchless find (reverted)" 1e0ef14 "1e0ef14+$X/branchless-find.patch" 'CheckAndTouchHit$|CheckAndTouchHitHot$|VelocityCount$'
ab "One CMPXCHG16B vs two CASes (reverted): microbenchmark" "1e0ef14+$X/cas2-cmpxchg16b.patch" "1e0ef14+$X/cas2-cmpxchg16b.patch" 'ZZTwoCAS|ZZCAS2'
ab "One CMPXCHG16B on the hit path (reverted)" 1e0ef14 "1e0ef14+$X/cas2-cmpxchg16b.patch" 'CheckAndTouchHit$|CheckAndTouchHitHot$'
ab "Per-line spill counts (reverted)" 1e0ef14 "1e0ef14+$X/spill-counts.patch" 'ContainsAbsent$|InsertSteady$|InsertNearFull$|CheckAndTouchHit$'
ab "Homing (reverted)" 1e0ef14 "1e0ef14+$X/homing.patch" 'HitChurned$|InsertSteady$|CheckAndTouchHit$'
ab "Relocation prefetch (reverted)" 1e0ef14 "1e0ef14+$X/reloc-prefetch.patch" 'InsertSteady$|InsertSaturated$'
ab "Reciprocal clock division (reverted)" 1e0ef14 "1e0ef14+$X/reciprocal-clock.patch" 'CheckAndTouchString$'
ab "Alternate-line prefetch (40f9223, reverted in bc2e12d)" 7ca297c 40f9223 'CheckAndTouchHit$|CheckAndTouchMissInsert$'
ab "8-slot buckets (reverted), incl. DRAM table" 1e0ef14 "1e0ef14+$X/eight-slot.patch" 'HugeHit$|HugeAbsent$|InsertNearFull$|InsertSaturated$|InsertSteady$' 3
ab "THP madvise (reverted), DRAM table" 1e0ef14 "1e0ef14+$X/thp-madvise.patch" 'HugeHit$|HugeAbsent$|HugeBatch$' 3
ab "Hot-key contention (benchmark only)" "1e0ef14+$X/contention-bench.patch" "1e0ef14+$X/contention-bench.patch" 'Contended|Uncontended' 3
