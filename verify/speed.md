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
