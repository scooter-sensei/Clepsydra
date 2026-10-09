# Speed measurements

Produced by `verify/speed.sh` on 2026-10-08. Each block is `verify/ab.sh`: both builds run alternately, ns/op min and median over the rounds, B/A = ratio of medians. Benchmarks with the same name are compared as each commit defines them.

```
go version go1.24.7 linux/amd64; Intel(R) Xeon(R) Processor @ 2.10GHz x 4; L3 266240K
```

## Scorecard speed columns at HEAD (cmd/tfbench, best of 3 per column, 3 runs)

```
| when (UTC) | rev | FN a | FN b | FN c | FP churn | static FPR | spray | B/key | vel MAE | hit cold | batch | hit hot | insert | insert sat | velocity | parallel | str | str coarse | change | hit DRAM |
| 10-08 17:47 | 64c01d8 | 0.000% | 0.000% | 0.051% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 62 | 38 | 24 | 158 | 375 | 68 | 18.2 | 119 | 73 | | 165 |
| 10-08 17:48 | 64c01d8 | 0.000% | 0.000% | 0.051% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 66 | 38 | 25 | 159 | 368 | 67 | 17.9 | 115 | 75 | | 154 |
| 10-08 17:49 | 64c01d8 | 0.000% | 0.000% | 0.051% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 66 | 38 | 25 | 161 | 381 | 70 | 18.2 | 121 | 73 | | 158 |
```

## Comparisons

### Clock read: built-in monotonic vs CoarseClock (same build)

```
verify/ab.sh HEAD HEAD 'Clock' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkClockCoarse-4                    1.4        1.4        1.4        1.5    1.03
BenchmarkClockBuiltin-4                  30.3       31.6       30.5       31.8    1.01
```

### Batch APIs vs single calls, L3-resident 1M-key table (same build)

```
verify/ab.sh HEAD HEAD 'CheckAndTouchHit$|CheckAndTouchBatch$|TouchAndCount$|TouchAndCountBatch|VelocityCount$|VelocityCountBatch' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkTouchAndCount-4                 93.7       97.9       94.9       96.8    0.99
BenchmarkVelocityCountBatch-4            50.0       51.3       50.4       51.7    1.01
BenchmarkCheckAndTouchHit-4              62.7       65.3       62.6       65.7    1.01
BenchmarkTouchAndCountBatch-4            60.7       62.9       60.2       62.2    0.99
BenchmarkVelocityCount-4                 69.3       72.3       69.2       70.4    0.97
BenchmarkCheckAndTouchBatch-4            41.9       42.6       39.9       42.0    0.99
```

### DRAM-sized 32M-key table: hit, batch, absent, velocity (same build)

```
verify/ab.sh HEAD HEAD 'Huge' 3
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkHugeAbsent-4                    88.9       91.3       85.4       89.8    0.98
BenchmarkHugeBatch-4                     76.2       78.1       76.1       78.5    1.00
BenchmarkHugeVelocity-4                 140.7      142.3      131.7      140.0    0.98
BenchmarkHugeHit-4                      167.0      174.4      158.8      174.3    1.00
```

### Adaptive relocation budget (126bd87): overfull-table insert

```
verify/ab.sh 597ee63+verify/experiments/insert-saturated-bench-597ee63.patch 126bd87 'InsertSaturated$' 4
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkInsertSaturated-4             3608.0     3728.5      298.4      304.2    0.08
```

### Velocity-aware eviction (2a526a8): insert cost

```
verify/ab.sh a5292e3 2a526a8 'CheckAndTouchMissInsert$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkCheckAndTouchMissInsert-4       81.0       83.0      106.4      109.5    1.32
```

### Striped first-touch lock (a8a2848): insert cost

```
verify/ab.sh bc2e12d a8a2848 'CheckAndTouchMissInsert$|CheckAndTouchHit$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkCheckAndTouchMissInsert-4      465.9      488.8      493.0      509.6    1.04
BenchmarkCheckAndTouchHit-4              61.6       63.7       63.4       64.4    1.01
```

### Striped first-touch lock (a8a2848): steady insert (benchmark from HEAD, added as a patch)

```
verify/ab.sh bc2e12d+verify/experiments/insert-steady-bench-bc2e12d.patch a8a2848+verify/experiments/insert-steady-bench-bc2e12d.patch 'InsertSteady$|CheckAndTouchMissInsert$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkCheckAndTouchMissInsert-4      501.8      518.1      529.2      541.2    1.04
BenchmarkInsertSteady-4                 143.7      154.9      166.5      174.1    1.12
```

### TrailingZeros increment (c8dbb54): hot-key hit

```
verify/ab.sh a8a2848 c8dbb54 'CheckAndTouchHitHot$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkCheckAndTouchHitHot-4           33.0       34.0       25.6       26.1    0.77
```

### Displaced bit (6bf61f0): speed cost

```
verify/ab.sh c8dbb54 6bf61f0 'CheckAndTouchHit$|CheckAndTouchHitHot$|CheckAndTouchMissInsert$|VelocityCount$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkCheckAndTouchMissInsert-4      495.8      504.0      495.3      515.9    1.02
BenchmarkCheckAndTouchHit-4              60.3       61.3       60.4       62.7    1.02
BenchmarkVelocityCount-4                 67.3       68.4       66.8       72.3    1.06
BenchmarkCheckAndTouchHitHot-4           24.5       25.6       24.1       25.5    1.00
```

### Register published with key (e42327a): saturated inserts

```
verify/ab.sh f07b04e e42327a 'InsertSaturated$|InsertSteady$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkInsertSteady-4                 170.6      173.4      161.1      168.1    0.97
BenchmarkInsertSaturated-4              373.3      386.8      375.8      394.2    1.02
```

### Count-race fixes (0e073a5, 75db206): busy hand-over, word0 re-read on every hit

```
verify/ab.sh 2187079 75db206 'CheckAndTouchHit$|CheckAndTouchHitHot$|TouchAndCount$|CheckAndTouchMissInsert$|InsertSteady$|InsertSaturated$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkTouchAndCount-4                 96.5       98.1       94.9       98.1    1.00
BenchmarkCheckAndTouchMissInsert-4      508.0      531.2      521.7      542.3    1.02
BenchmarkInsertSteady-4                 159.5      164.6      160.3      167.6    1.02
BenchmarkCheckAndTouchHit-4              63.1       64.0       61.9       65.3    1.02
BenchmarkInsertSaturated-4              352.0      375.8      367.5      385.9    1.03
BenchmarkCheckAndTouchHitHot-4           26.1       27.5       26.0       28.0    1.02
```

### Estimate without re-bucketing (24f133b): VelocityCount

```
verify/ab.sh d5648a8 24f133b 'VelocityCount$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkVelocityCount-4                 84.8       88.2       68.6       71.1    0.81
```

### Depth 4 / budget 1000 (9f2529f) vs depth 3 / budget 500 (acea1cf): insert cost

```
verify/ab.sh 9f2529f acea1cf 'InsertSaturated$|InsertNearFull$|CheckAndTouchMissInsert$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkCheckAndTouchMissInsert-4      508.9      529.0      446.8      464.1    0.88
BenchmarkInsertNearFull-4              3303.0     3344.5     2142.0     2256.0    0.67
BenchmarkInsertSaturated-4              342.3      350.4      310.8      326.9    0.93
```

### Branchless find (reverted)

```
verify/ab.sh 1e0ef14 1e0ef14+verify/experiments/branchless-find.patch 'CheckAndTouchHit$|CheckAndTouchHitHot$|VelocityCount$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkCheckAndTouchHit-4              61.5       64.4       54.6       55.7    0.87
BenchmarkVelocityCount-4                 69.2       71.0       82.2       84.1    1.18
BenchmarkCheckAndTouchHitHot-4           26.9       27.2       21.0       21.6    0.80
```

### One CMPXCHG16B vs two CASes (reverted): microbenchmark

```
verify/ab.sh 1e0ef14+verify/experiments/cas2-cmpxchg16b.patch 1e0ef14+verify/experiments/cas2-cmpxchg16b.patch 'ZZTwoCAS|ZZCAS2' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkZZCAS2-4                        13.8       14.4       13.2       14.1    0.98
BenchmarkZZTwoCAS-4                      11.3       11.9       11.4       11.8    1.00
```

### One CMPXCHG16B on the hit path (reverted)

```
verify/ab.sh 1e0ef14 1e0ef14+verify/experiments/cas2-cmpxchg16b.patch 'CheckAndTouchHit$|CheckAndTouchHitHot$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkCheckAndTouchHit-4              62.1       64.2       64.5       66.6    1.04
BenchmarkCheckAndTouchHitHot-4           26.5       27.3       29.7       30.4    1.11
```

### Per-line spill counts (reverted)

```
verify/ab.sh 1e0ef14 1e0ef14+verify/experiments/spill-counts.patch 'ContainsAbsent$|InsertSteady$|InsertNearFull$|CheckAndTouchHit$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkInsertSteady-4                 157.1      164.9      179.0      180.3    1.09
BenchmarkCheckAndTouchHit-4              61.8       64.7       61.7       63.6    0.98
BenchmarkContainsAbsent-4                29.2       30.9       26.4       27.7    0.90
BenchmarkInsertNearFull-4              2219.0     2304.5     2433.0     2513.0    1.09
```

### Homing (reverted)

```
verify/ab.sh 1e0ef14 1e0ef14+verify/experiments/homing.patch 'HitChurned$|InsertSteady$|CheckAndTouchHit$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkInsertSteady-4                 158.9      161.8      179.7      184.2    1.14
BenchmarkCheckAndTouchHit-4              62.0       63.8       64.9       67.4    1.06
BenchmarkHitChurned-4                    79.0       81.7       77.8       81.0    0.99
```

### Relocation prefetch (reverted)

```
verify/ab.sh 1e0ef14 1e0ef14+verify/experiments/reloc-prefetch.patch 'InsertSteady$|InsertSaturated$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkInsertSteady-4                 155.8      160.4      159.9      170.1    1.06
BenchmarkInsertSaturated-4              347.8      368.3      354.4      366.2    0.99
```

### Reciprocal clock division (reverted)

```
verify/ab.sh 1e0ef14 1e0ef14+verify/experiments/reciprocal-clock.patch 'CheckAndTouchString$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkCheckAndTouchString-4          113.5      115.9      111.2      116.2    1.00
```

### Alternate-line prefetch (40f9223, reverted in bc2e12d)

```
verify/ab.sh 7ca297c 40f9223 'CheckAndTouchHit$|CheckAndTouchMissInsert$' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkCheckAndTouchMissInsert-4      464.9      490.1      457.5      490.1    1.00
BenchmarkCheckAndTouchHit-4              63.7       64.4       63.0       65.6    1.02
```

### 8-slot buckets (reverted), incl. DRAM table

```
verify/ab.sh 1e0ef14 1e0ef14+verify/experiments/eight-slot.patch 'HugeHit$|HugeAbsent$|InsertNearFull$|InsertSaturated$|InsertSteady$' 3
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkInsertSteady-4                 157.2      161.3      146.7      150.7    0.93
BenchmarkHugeAbsent-4                    86.4       89.1      131.5      134.5    1.51
BenchmarkInsertNearFull-4              2197.0     2218.0      331.5      335.1    0.15
BenchmarkInsertSaturated-4              362.4      380.3      586.2      599.7    1.58
BenchmarkHugeHit-4                      168.8      169.9      172.5      185.1    1.09
```

### THP madvise (reverted), DRAM table

```
verify/ab.sh 1e0ef14 1e0ef14+verify/experiments/thp-madvise.patch 'HugeHit$|HugeAbsent$|HugeBatch$' 3
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkHugeAbsent-4                    83.6       86.7       74.2       75.1    0.87
BenchmarkHugeBatch-4                     73.2       73.6       68.4       68.5    0.93
BenchmarkHugeHit-4                      168.5      170.3      137.2      138.8    0.82
```

Same comparison with 6 rounds (THP mode `madvise` on this host):

```
verify/ab.sh 1e0ef14 1e0ef14+verify/experiments/thp-madvise.patch 'HugeHit$|HugeAbsent$|HugeBatch$' 6
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkHugeAbsent-4                    84.2       86.3       74.0       76.6    0.89
BenchmarkHugeBatch-4                     76.3       77.7       66.8       68.7    0.89
BenchmarkHugeHit-4                      171.2      174.3      138.8      145.9    0.84
```

### Hot-key contention (benchmark only)

```
verify/ab.sh 1e0ef14+verify/experiments/contention-bench.patch 1e0ef14+verify/experiments/contention-bench.patch 'Contended|Uncontended' 3
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkContended16-4                   12.4       12.8       12.3       12.5    0.97
BenchmarkUncontended-4                   18.9       19.2       18.6       19.4    1.01
BenchmarkContended1-4                     6.9        7.3        7.0        7.1    0.98
```


### Security changes: keyed hash, stash, saturated bit, Result (S27)

```
verify/ab.sh 5c57619 afdd877 'CheckAndTouchHit$|CheckAndTouchHitHot|TouchAndCount$|VelocityCount$|VelocityCountHot|InsertSteady|InsertSaturated|InsertNearFull|ContainsAbsent$|CheckAndTouchString|CheckAndTouchParallel' 8
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkTouchAndCount-4                118.5      126.6      123.9      128.2    1.01
BenchmarkInsertSteady-4                 208.3      220.9      221.8      233.4    1.06
BenchmarkCheckAndTouchHit-4              78.8       86.9       78.3       90.1    1.04
BenchmarkCheckAndTouchString-4          166.2      170.7      167.3      185.7    1.09
BenchmarkContainsAbsent-4                37.3       40.0       36.3       40.8    1.02
BenchmarkInsertNearFull-4              2838.0     3026.0     2975.0     3189.0    1.05
BenchmarkVelocityCount-4                 89.9       95.1       90.5       94.5    0.99
BenchmarkInsertSaturated-4              516.7      525.2      734.2      748.1    1.42
BenchmarkCheckAndTouchHitHot-4           36.5       37.3       36.7       39.4    1.06
BenchmarkVelocityCountHot-4              49.0       50.6       54.5       56.8    1.12
BenchmarkCheckAndTouchParallel-4         23.8       24.6       24.4       26.0    1.06
```

### Overflow sketch and ConstantTime (S29)

This host was ~2x slower than for S27 on the same day's runs; compare ratios. B is the commit that adds `overflow.go` (code identical to the A/B build).

```
verify/ab.sh 64a4b30 55c82ed 'CheckAndTouchHit$|CheckAndTouchHitHot|TouchAndCount$|VelocityCount$|VelocityCountHot|InsertSteady|InsertSaturated|InsertNearFull|ContainsAbsent$|CheckAndTouchParallel|HugeHit$|HugeAbsent$' 6
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkTouchAndCount-4                183.6      220.3      222.8      225.1    1.02
BenchmarkInsertSteady-4                 337.1      368.0      381.9      404.7    1.10
BenchmarkHugeAbsent-4                   139.4      147.5      154.2      172.8    1.17
BenchmarkCheckAndTouchHit-4             166.9      183.2      165.8      187.3    1.02
BenchmarkContainsAbsent-4                80.0       93.1       83.1       95.0    1.02
BenchmarkInsertNearFull-4              2936.0     3328.0     3091.0     3569.0    1.07
BenchmarkVelocityCount-4                124.3      163.7      165.5      181.8    1.11
BenchmarkInsertSaturated-4              872.0      960.0     1377.0     1582.5    1.65
BenchmarkCheckAndTouchHitHot-4           34.7       35.4       37.8       38.8    1.09
BenchmarkVelocityCountHot-4              52.0       54.7       55.5       58.0    1.06
BenchmarkHugeHit-4                      257.1      273.6      262.4      298.2    1.09
BenchmarkCheckAndTouchParallel-4         71.0       78.7       53.6       68.0    0.86
```

Every filter in ConstantTime mode, no pad (`TF_CT=1`; A ignores it):

```
TF_CT=1 verify/ab.sh 64a4b30 55c82ed 'CheckAndTouchHit$|CheckAndTouchHitHot|VelocityCountHot|TouchAndCount$|InsertSteady|InsertSaturated|ContainsAbsent$|HugeHit$|HugeAbsent$' 3
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkTouchAndCount-4                200.1      213.0      595.1      616.1    2.89
BenchmarkInsertSteady-4                 278.8      326.8      911.2      972.1    2.97
BenchmarkHugeAbsent-4                   135.8      136.9      490.3      535.3    3.91
BenchmarkCheckAndTouchHit-4             126.8      164.5      526.3      551.1    3.35
BenchmarkContainsAbsent-4                87.8       91.0      517.1      544.3    5.98
BenchmarkInsertSaturated-4              821.7      859.1     1162.0     1254.0    1.46
BenchmarkCheckAndTouchHitHot-4           35.5       36.8      226.9      228.1    6.19
BenchmarkVelocityCountHot-4              51.1       54.4      230.8      238.6    4.39
BenchmarkHugeHit-4                      254.4      256.6      503.6      544.0    2.12
```

### Timing leak (S28)

```
TF_TIMING=1 go test -count=1 -run TestTimingLeak -v .   (two runs)
ConstantTime 0s    Count: unseen p50/p99/p99.9  211/  592/  897 ns, seen  235/  578/  775 ns; KS D = 0.272 (best single-timing guess right 63.6%)
ConstantTime 0s    Touch: unseen p50/p99/p99.9  394/ 2163/ 6338 ns, seen  213/  566/  710 ns; KS D = 0.702 (best single-timing guess right 85.1%)
ConstantTime 3µs   Count: unseen p50/p99/p99.9 3094/ 3224/31869 ns, seen 3094/ 3254/30730 ns; KS D = 0.015 (best single-timing guess right 50.7%)
ConstantTime 3µs   Touch: unseen p50/p99/p99.9 3093/ 4505/33771 ns, seen 3093/ 3354/29926 ns; KS D = 0.018 (best single-timing guess right 50.9%)
ConstantTime 0s    Count: unseen p50/p99/p99.9  175/  524/  719 ns, seen  171/  515/  661 ns; KS D = 0.137 (best single-timing guess right 56.9%)
ConstantTime 0s    Touch: unseen p50/p99/p99.9  368/ 2081/22982 ns, seen  240/  557/  696 ns; KS D = 0.788 (best single-timing guess right 89.4%)
ConstantTime 3µs   Count: unseen p50/p99/p99.9 3093/ 3233/32281 ns, seen 3093/ 3233/30668 ns; KS D = 0.011 (best single-timing guess right 50.6%)
ConstantTime 3µs   Touch: unseen p50/p99/p99.9 3093/ 4083/32863 ns, seen 3093/ 3355/33078 ns; KS D = 0.015 (best single-timing guess right 50.8%)
```

### Lazy 8-bit sketch and SpillMin (S32)

```
verify/ab.sh a96dc28 a55178e 'CheckAndTouchHit$|CheckAndTouchHitHot|TouchAndCount$|VelocityCount$|VelocityCountHot|InsertSteady|InsertSaturated|InsertNearFull|ContainsAbsent$|CheckAndTouchParallel|HugeHit$|HugeAbsent$' 6
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkTouchAndCount-4                215.4      223.4      203.6      214.0    0.96
BenchmarkInsertSteady-4                 350.1      383.7      328.7      379.4    0.99
BenchmarkHugeAbsent-4                   157.5      170.9      140.7      158.1    0.93
BenchmarkCheckAndTouchHit-4             149.1      180.9      168.5      179.1    0.99
BenchmarkContainsAbsent-4                79.5       93.7       71.8       94.5    1.01
BenchmarkInsertNearFull-4              3093.0     3263.0     3170.0     3514.0    1.08
BenchmarkVelocityCount-4                157.0      173.6      154.3      164.9    0.95
BenchmarkInsertSaturated-4             1328.0     1446.0      892.7     1004.8    0.69
BenchmarkCheckAndTouchHitHot-4           37.5       38.2       37.9       38.9    1.02
BenchmarkVelocityCountHot-4              57.7       59.5       56.1       56.4    0.95
BenchmarkHugeHit-4                      250.2      279.2      280.6      285.0    1.02
BenchmarkCheckAndTouchParallel-4         62.3       75.8       47.4       71.8    0.95
```

### Ghost table, Count pad, alternate-line prefetch (S35)

```
verify/ab.sh 0651be5 49df095 'CheckAndTouchHit$|CheckAndTouchHitHot|TouchAndCount$|VelocityCount$|VelocityCountHot|InsertSteady|InsertSaturated|InsertNearFull|ContainsAbsent$|CheckAndTouchParallel|HugeHit$|HugeAbsent$' 6
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkTouchAndCount-4                210.0      235.6      229.7      241.2    1.02
BenchmarkInsertSteady-4                 400.9      441.6      429.2      445.6    1.01
BenchmarkHugeAbsent-4                   165.1      177.5      159.6      193.8    1.09
BenchmarkCheckAndTouchHit-4             180.4      199.8      190.3      201.4    1.01
BenchmarkContainsAbsent-4               107.7      116.4      103.5      119.4    1.03
BenchmarkInsertNearFull-4              3541.0     3881.0     3383.0     3881.0    1.00
BenchmarkVelocityCount-4                178.0      193.2      178.4      194.8    1.01
BenchmarkInsertSaturated-4             1095.0     1266.0      941.8     1067.5    0.84
BenchmarkCheckAndTouchHitHot-4           39.5       40.3       39.8       42.8    1.06
BenchmarkVelocityCountHot-4              58.1       62.5       58.0       63.6    1.02
BenchmarkHugeHit-4                      285.5      315.4      298.1      321.1    1.02
BenchmarkCheckAndTouchParallel-4         62.7       72.4       63.7       74.4    1.03
```

```
verify/ab.sh 0651be5 e0a1e21 (same benchmarks, 6 rounds; SpillMin 1)
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkTouchAndCount-4                230.1      249.6      235.5      239.4    0.96
BenchmarkInsertSteady-4                 396.0      438.5      387.5      442.1    1.01
BenchmarkHugeAbsent-4                   140.2      164.4      150.6      172.3    1.05
BenchmarkCheckAndTouchHit-4             198.0      204.3      187.2      201.7    0.99
BenchmarkContainsAbsent-4               103.7      113.9       99.0      110.9    0.97
BenchmarkInsertNearFull-4              3817.0     4227.0     3408.0     3924.0    0.93
BenchmarkVelocityCount-4                178.6      187.6      185.2      195.8    1.04
BenchmarkInsertSaturated-4             1117.0     1355.5     1627.0     1755.5    1.30
BenchmarkCheckAndTouchHitHot-4           37.8       42.1       39.1       40.8    0.97
BenchmarkVelocityCountHot-4              57.2       60.8       55.4       60.0    0.99
BenchmarkHugeHit-4                      274.7      329.8      292.4      314.7    0.95
BenchmarkCheckAndTouchParallel-4         67.5       79.0       66.1       73.8    0.93

[exited with code 0]

verify/ab.sh e0a1e21 e0a1e21+verify/experiments/spillmin4.patch 'InsertSaturated|InsertNearFull' 5
BenchmarkInsertNearFull-4              3508.0     3791.0     3487.0     4174.0    1.10
BenchmarkInsertSaturated-4             1448.0     1659.0     1018.0     1208.0    0.73

verify/ab.sh e0a1e21 e0a1e21+verify/experiments/no-alt-prefetch.patch 'InsertSaturated|InsertNearFull|InsertSteady' 5
BenchmarkInsertSteady-4                 413.8      442.3      411.0      428.2    0.97
BenchmarkInsertNearFull-4              3939.0     4141.0     3999.0     4507.0    1.09
BenchmarkInsertSaturated-4             1624.0     1753.0     1715.0     1930.0    1.10
```

### ConstantTime pads (S33, S34)

```
TF_CTEXP=1 go test -run 'TestCTPads|TestCTParallel' -v
off              | Count D 0.254 (62.7%) p50   222/  246,   346 ns/op | Touch D 0.705 (85.3%) p50   648/  300,   702 ns/op
fixed 1us        | Count D 0.012 (50.6%) p50  1093/ 1094,  1208 ns/op pad 1000ns | Touch D 0.172 (58.6%) p50  1102/ 1098,  1309 ns/op pad 1000ns
fixed 3us        | Count D 0.005 (50.3%) p50  3126/ 3126,  3385 ns/op pad 3000ns | Touch D 0.012 (50.6%) p50  3094/ 3094,  3310 ns/op pad 3000ns
3us, Count auto  | Count D 0.011 (50.5%) p50   933/  935,  1052 ns/op pad  793ns | Touch D 0.026 (51.3%) p50  3103/ 3102,  3383 ns/op pad 3000ns
AutoPad          | Count D 0.012 (50.6%) p50  1294/ 1290,  1467 ns/op pad 1410ns | Touch D 0.010 (50.5%) p50  2990/ 2987,  3192 ns/op pad 3040ns
p50 is unseen/seen; ns/op is the mean wall time per call on one goroutine
fixed 3us         1.06 M ops/s; probe p50 unseen    3.1 us seen    3.1 us; KS D 0.060 (53.0%); Count pad 3000 ns
3us, auto Count   2.19 M ops/s; probe p50 unseen    1.6 us seen    1.6 us; KS D 0.015 (50.7%); Count pad 1041 ns
```
