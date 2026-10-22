# Testing

Every implementation (Go now; Python, C++ and Java later) has the same five test tiers behind one entry point in its folder (`go/test.sh <tier>`). A tier passes only if every check in it passes. Each check writes a log, and the run ends with a summary.

| Tier | Question it answers | When to run | Go: what it runs | Time (Go) |
|---|---|---|---|---|
| **smoke** | Does it build and do the basic operations work at all? | every save or commit | build, vet, gofmt, `TestSmoke` (every public operation once, under three configs), SipHash reference vectors | ~2 s |
| **sanity** | Is the whole unit suite green? | every push | smoke, plus the unit suite (`-short`), the suite again in ConstantTime mode, and the conformance vectors | ~20 s |
| **regression** | Has anything that was once fixed or measured changed? | every push to main | conformance vectors, bug-fix tests (`TestRegress*`), and every deterministic result compared byte for byte with `go/verify/expected/`: accuracy scorecard, placement limits, register experiment, attacks, sustained rates, overflow experiments, flood sizing. `--full` also replays each recorded bug on the commit that had it and the commit that fixed it, every scorecard row, and old harnesses (`go/verify/run.sh`) | ~1 min (`--full` ~25 min) |
| **perf** | Is this change slower? | before merging anything on a hot path | the 12 core benchmarks of a base revision and of the working tree, built once and run alternately on the same host; fails when both the minimum and the median are more than `PERF_MAX` (1.20) times the base | ~5 min |
| **stress** | Does it hold under concurrency and overload? | before a release, nightly | race detector on the whole suite, racy regression tests 300 times each, and a soak (`TestStressSoak`): every operation from 2 goroutines per core for `TF_STRESS` (60 s), while the key pool cycles from 0.5x to 4x capacity. The soak checks the fail-closed contract on every count, then runs again under the race detector. Then a fuzzer (`TestFuzzUpper`): `TF_FUZZ` (1e9) operations over random configurations (table, stash, overflow and hot sizes, horizons, ConstantTime), load from idle to 64x capacity per horizon, and callers' clocks lagging by up to two units, checking `Upper` against an exact model on every count and touch. Each case is one goroutine from a seed, so a failure replays exactly (`TF_FUZZ_CASE=<seed>`, and `TF_FUZZ_TRACE=<key>` for that key's operations) | ~15 min |

`go/test.sh all` runs smoke, sanity, regression and stress. Perf is left out because its results depend on the host, so it runs only when asked.

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

A port passes when it reproduces every `want`, every hash and `end` exactly. The vectors describe the default (non-ConstantTime) insert policy: in ConstantTime mode a first touch that finds both lines full waits in the stash and is placed later, so which keys get evicted differs, and the Go suite skips the vectors with evictions under `TF_CT=1`. Go regenerates the files with `go test -run TestConformance -update` (in `go/`). Do that only for a deliberate change in behaviour, and say so in the commit, because every port must then follow.

## Layout rule for a new language

```
<lang>/test.<ext or sh>   the tier entry point: smoke | sanity | regression | perf | stress | all
<lang>/...                its own build, tests and benchmarks
```

Its regression tier must include the conformance vectors. Its smoke tier must call every public operation once.
