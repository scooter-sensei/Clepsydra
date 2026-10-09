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
ab "Adaptive relocation budget (378bc37): overfull-table insert" "b3e9aa5+$X/insert-saturated-bench-b3e9aa5.patch" 378bc37 'InsertSaturated$' 4
ab "Velocity-aware eviction (eeb5521): insert cost" 564d865 eeb5521 'CheckAndTouchMissInsert$'
ab "Striped first-touch lock (593bd1b): insert cost" 70546b2 593bd1b 'CheckAndTouchMissInsert$|CheckAndTouchHit$'
ab "Striped first-touch lock (593bd1b): steady insert (benchmark from HEAD, added as a patch)" "70546b2+$X/insert-steady-bench-70546b2.patch" "593bd1b+$X/insert-steady-bench-70546b2.patch" 'InsertSteady$|CheckAndTouchMissInsert$'
ab "TrailingZeros increment (68eee6c): hot-key hit" 593bd1b 68eee6c 'CheckAndTouchHitHot$'
ab "Displaced bit (99c054a): speed cost" 68eee6c 99c054a 'CheckAndTouchHit$|CheckAndTouchHitHot$|CheckAndTouchMissInsert$|VelocityCount$'
ab "Register published with key (f31512c): saturated inserts" d1db914 f31512c 'InsertSaturated$|InsertSteady$'
ab "Count-race fixes (4a037a5, d7fb2b5): busy hand-over, word0 re-read on every hit" 07e2d35 d7fb2b5 'CheckAndTouchHit$|CheckAndTouchHitHot$|TouchAndCount$|CheckAndTouchMissInsert$|InsertSteady$|InsertSaturated$'
ab "Estimate without re-bucketing (a477d31): VelocityCount" 0a00063 a477d31 'VelocityCount$'
ab "Depth 4 / budget 1000 (6aa1c1b) vs depth 3 / budget 500 (7fbc51a): insert cost" 6aa1c1b 7fbc51a 'InsertSaturated$|InsertNearFull$|CheckAndTouchMissInsert$'
ab "Branchless find (reverted)" fbe6968 "fbe6968+$X/branchless-find.patch" 'CheckAndTouchHit$|CheckAndTouchHitHot$|VelocityCount$'
ab "One CMPXCHG16B vs two CASes (reverted): microbenchmark" "fbe6968+$X/cas2-cmpxchg16b.patch" "fbe6968+$X/cas2-cmpxchg16b.patch" 'ZZTwoCAS|ZZCAS2'
ab "One CMPXCHG16B on the hit path (reverted)" fbe6968 "fbe6968+$X/cas2-cmpxchg16b.patch" 'CheckAndTouchHit$|CheckAndTouchHitHot$'
ab "Per-line spill counts (reverted)" fbe6968 "fbe6968+$X/spill-counts.patch" 'ContainsAbsent$|InsertSteady$|InsertNearFull$|CheckAndTouchHit$'
ab "Homing (reverted)" fbe6968 "fbe6968+$X/homing.patch" 'HitChurned$|InsertSteady$|CheckAndTouchHit$'
ab "Relocation prefetch (reverted)" fbe6968 "fbe6968+$X/reloc-prefetch.patch" 'InsertSteady$|InsertSaturated$'
ab "Reciprocal clock division (reverted)" fbe6968 "fbe6968+$X/reciprocal-clock.patch" 'CheckAndTouchString$'
ab "Alternate-line prefetch (6c1f5dd, reverted in 70546b2)" bed5c07 6c1f5dd 'CheckAndTouchHit$|CheckAndTouchMissInsert$'
ab "8-slot buckets (reverted), incl. DRAM table" fbe6968 "fbe6968+$X/eight-slot.patch" 'HugeHit$|HugeAbsent$|InsertNearFull$|InsertSaturated$|InsertSteady$' 3
ab "THP madvise (reverted), DRAM table" fbe6968 "fbe6968+$X/thp-madvise.patch" 'HugeHit$|HugeAbsent$|HugeBatch$' 6
ab "Hot-key contention (benchmark only)" "fbe6968+$X/contention-bench.patch" "fbe6968+$X/contention-bench.patch" 'Contended|Uncontended' 3
