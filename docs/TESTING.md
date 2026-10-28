# Testing

Every implementation (Go now; Python, C++ and Java later) has the same five test tiers behind one entry point in its folder (`go/test.sh <tier>`). A tier passes only if every check in it passes. Each check writes a log, and the run ends with a summary.

| Tier | Question it answers | When to run | Go: what it runs | Time (Go) |
|---|---|---|---|---|
| **smoke** | Does it build and do the basic operations work at all? | every save or commit | build, vet, gofmt, `TestSmoke` (every public operation once, under three configs), SipHash reference vectors | ~2 s |
| **sanity** | Is the whole unit suite green? | every push | smoke, plus the unit suite (`-short`), the suite again in ConstantTime mode (`TF_CT=1`), in Background mode (`TF_BG=1`) and, on amd64, with realign's hold instead of its 16-byte CAS (`TF_NOCAS2=1`: the path of other architectures), and the conformance vectors | ~40 s |
| **regression** | Has anything that was once fixed or measured changed? | every push to main | conformance vectors, bug-fix tests (`TestRegress*`), and every deterministic result compared byte for byte with `go/verify/expected/`: accuracy scorecard, placement limits, register experiment, velocity bound, attacks, sustained rates, overflow experiments, flood sizing. `--full` also replays each recorded bug on the commit that had it and the commit that fixed it, every scorecard row, and old harnesses (`go/verify/run.sh`) | ~1 min (`--full` ~25 min) |
| **perf** | Is this change slower? | before merging anything on a hot path | the 12 core benchmarks of a base revision and of the working tree, built once and run alternately on the same host; fails when both the minimum and the median are more than `PERF_MAX` (1.20) times the base | ~5 min |
| **stress** | Does it hold under concurrency and overload? | before a release, nightly | race detector on the whole suite, racy regression tests 300 times each, and a soak (`TestStressSoak`): every operation from 2 goroutines per core for `TF_STRESS` (60 s), while the key pool cycles from 0.5x to 4x capacity. The soak checks the fail-closed contract on every count, then runs again under the race detector and in Background mode. Then a fuzzer (`TestFuzzUpper`): `TF_FUZZ` (1e9) operations over random configurations (table, stash, overflow and hot sizes, horizons, ConstantTime, Background with the placer run at seeded points), load from idle to 64x capacity per horizon, and callers' clocks lagging by up to two units, checking `Upper` against an exact model on every count and touch. Each case is one goroutine from a seed, so a failure replays exactly (`TF_FUZZ_CASE=<seed>`, and `TF_FUZZ_TRACE=<key>` for that key's operations). Last, the concurrent fuzzer (below) for `TF_CFUZZ` (5 min), plain and with tags shared 8 ways, and a Background soak with tags shared 8 ways | ~30 min |

`go/test.sh all` runs smoke, sanity, regression and stress. Perf is left out because its results depend on the host, so it runs only when asked.

## Concurrent fuzzer

The soak and `TestFuzzUpper` give each key one goroutine, so a key is never read while another goroutine touches it. `TestFuzzConcurrent` (`TF_CFUZZ=<duration>`, e.g. `10m`) runs the fuzzer's configurations with 2 to 2×GOMAXPROCS goroutines on one filter, a fifth of its cases in ConstantTime and a fifth with Background (the placer a goroutine). Each goroutine owns its keys and checks `Upper` on every Touch and Count, as `TestFuzzUpper` does. One operation in eight is on a few keys every goroutine shares, and a Count of a shared key must cover the touches that returned before it began. The clock is shared, and callers read it with a lag of up to two units; with `TF_CFUZZ_STRAGGLE=1` one goroutine of every case is a straggler, its calls half a horizon to a horizon behind (a caller descheduled that long, or one passing its own timestamps). The straggler found nothing in 10 minutes at c1cbace, but it has no practical power over the slot holds of claims A26 and A27, which need its touch within a few nanoseconds of a hold; their hook tests are their evidence. Some races need two keys of the same 12-bit tag to meet in one slot, since their first registers in one unit are bit-identical (claims A28, A29): with real tags one hand-over in 4,096, and the fuzzer found those kinds once per 30 to 60 minutes. `TF_CFUZZ_TAGS=n` gives every key a tag from n values and a fingerprint unique in the case (capacity at most 4,096, so fewer than 65,536 keys), so two keys share a tag one time in n: at n = 8 those races come 512 times as often. `TF_STRESS_TAGS=n` does the same in the soak. A case's configuration and streams come from its seed (`TF_CFUZZ_SEED`, default 1), but its run depends on its races, so a failure doesn't replay exactly: `TF_CFUZZ_CASE=<seed>` runs that case `TF_CFUZZ_RUNS` times (default 20).

## Model checks

`go/verify/tla.sh` model-checks the slot protocol's concurrent parts with TLC (TLA+ tools 1.7.4, Java 11 or later): the wait room (`WaitRoom.tla`), a register's 4-bit epoch against its last touch (`RegisterEpoch.tla`), a relocation's hidden copy against the key's touches, reads and eviction (`Relocate.tla`), a wait-room set shared by a key being placed and one waiting beside it (`RoomWays.tla`), and a hold of an occupied slot (the decay sweep, an insert over an expired entry) against the occupant's lagging touch and its re-insert (`SlotHold.tla`), the stripe a hold counts its move on when word0 and word1 are read in two loads, with a key installed between them or one of the occupant's tag (`TagStripe.tla`), a touch's count across a hand-over of its slot, before or after the count, to a key of its tag or another (`BumpABA.tla`), and a hot entry's lock-free add against a writer that advances the entry, demotes it, reclaims it for another key or spills its history to the overflow table (`HotEntry.tla`), in `go/verify/tla/`. Each model runs the protocol as committed, exhaustively at small sizes, and then each of its guards undone, which must break an invariant; `RegisterEpoch` runs realign both ways as committed, its hold and its 16-byte CAS on amd64 (claims A33), and `BumpABA` and `RegisterEpoch` also run experiment E21's 16-byte count, which must hold. `TLA_BIG=1` adds the large sizes the claims cite. The models are not in a test tier: they change only with the protocol, and some sizes take tens of minutes.

## Conformance vectors

`spec/vectors/*.json` pin the filter's observable behaviour bit for bit, so a port can show it is the same filter. Each file is one scenario:

- `key_hex`: the SipHash-2-4 key, the same in every file (bytes 00..0f).
- `config`: what the filter is built with. Defaults apply to fields that are absent: load 0.85, stash 1 entry per 16 slots, overflow 5 units per 9 slots (two candidate buckets, three floors per bucket), spill_min 4, hot entries 1 per 128 slots.
- `hashes`: `Hash(string)` and `HashUint64(uint64)` under the key, as 16 hex digits.
- `ops`: calls in order on one fresh filter. The clock reads `now_ms` during the call, at a resolution of `resolution_ms`. `op` is `touch` (record an event, then answer) or `count` (answer only), and `want` is the full Result. A missing flag means false.
- `end`: the counters and occupancy after the last op.

| Scenario | What it exercises |
|---|---|
| `basic` | hashing, Seen and Count at assorted windows, events exactly one window old |
| `burst` | rates above the register's capacity (`Saturated`), then decay |
| `churn` | a 4x-capacity flood of one-off keys against 8 repeat keys (velocity-aware eviction) |
| `overflow` | a small stash: evicted histories come back from the stash (`Restored`) or the overflow table (`Degraded`) |

A port passes when it reproduces every `want`, every hash and `end` exactly. The vectors describe the default (non-ConstantTime) insert policy: in ConstantTime and Background modes a first touch that finds both lines full waits and is placed later, so which keys get evicted differs, and the Go suite skips the vectors with evictions under `TF_CT=1` and `TF_BG=1`. Go regenerates the files with `go test -run TestConformance -update` (in `go/`). Do that only for a deliberate change in behaviour, and say so in the commit, because every port must then follow.

## Layout rule for a new language

```
<lang>/test.<ext or sh>   the tier entry point: smoke | sanity | regression | perf | stress | all
<lang>/...                its own build, tests and benchmarks
```

Its regression tier must include the conformance vectors. Its smoke tier must call every public operation once.
