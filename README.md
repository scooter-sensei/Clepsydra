# Clepsydra

A fixed-memory temporal filter: **"seen key X in the last W?"** and **"how many times in the last W?"**, for any window `W` up to a configured horizon, chosen per query. Each key's exact timestamp and event counter share one cache line, so a hit costs one memory access.

Status: research prototype. The Go implementation is the reference; Python, C++ and Java ports are planned.

## Layout

```
docs/       language-neutral: algorithm and layout (DESIGN), threat model (SECURITY), prior art (RESEARCH), test tiers (TESTING)
spec/       conformance vectors every implementation must reproduce
go/         Go reference implementation
  *.go        library and tests
  cmd/        tfbench: accuracy and speed scorecard
  results/    measured history, backlog of ideas, round reports
  verify/     every claim with its check; isolated.sh re-runs them offline
```

Each port goes in its own top-level folder (`python/`, `cpp/`, `java/`) with its own build. A port follows `docs/DESIGN.md` and must reproduce `spec/vectors/` exactly and provide the same five test tiers (`docs/TESTING.md`).

## Go

```
cd go
./test.sh smoke         # also: sanity, regression, perf, stress, all
./bench.sh "label"      # scorecard row in results/history.md
verify/run.sh           # every claim check (verify/claims.md)
```
