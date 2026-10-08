# Improvement loop backlog

Scorecard: `bash bench.sh "label"` (race tests + cmd/tfbench, appends to results/history.md).

## Done
1. Partial-key cuckoo relocation (alt line from fp), iterative deepening to depth 2. Missed keys at 2.5x load: 5.0% → 0%.
2. Adaptive relocation budget (failure EWMA). Overfull-table insert: 12µs → ~0.4µs.
3. 28-bit slot identity (4-bit epoch, 12-bit tag). Static FPR 2e-7 → 4e-8.
4. CheckAndTouchBatch (overlapped misses). Cold hit ~100ns → ~45-55ns/key.

## Tried, no gain
- 2 MiB huge-page mmap for the table: THP was granted, latency unchanged (Go heap already benefits or TLB isn't the bottleneck here).

## Next
- FN is 0 at 2.5x: add a 2.8x point or tighten sizing (target ~85% load at Capacity → about half the memory, and faster).
- Velocity accuracy (MAE vs exact) as a scorecard column, then a dyadic-register option for short windows.
- Hot path: skip the word0 CAS when the timestamp is unchanged at a coarser floor; measure contention on hot keys.
- String path: cheaper clock (cached coarse clock option), faster hashing.
- Batch variants for VelocityCount / TouchAndCount.
