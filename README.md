# Clepsydra

A fixed-memory temporal filter: **"seen key X in the last W?"** and **"how many times in the last W?"**, for any window `W` up to a configured horizon, chosen per query. Each key's exact timestamp and event counter share one cache line, so a hit costs one memory access.

Status: research prototype. The Go implementation is the reference; Python, C++ and Java ports are planned.

## Documentation

All documentation is in [docs/](docs/README.md), organized the way a new data structure is documented:

- [**paper/**](docs/paper/01-introduction.md): what the structure is, why it works, what it guarantees and how well it does.
- [**specification/**](docs/specification/format.md): the language-neutral format and the conformance rules every implementation must meet.
- [**artifact/**](docs/artifact/claims.md): every claim with the command that checks it, and how to reproduce the results.
- [**record/**](docs/record/changes.md): the changes, defects, decisions and scorecard history behind it.

## Layout

```
docs/          the documentation (paper, specification, artifact, record)
spec/vectors/  conformance vectors every implementation must reproduce
go/            Go reference implementation
  *.go           the library and its white-box tests (one Go package)
  test.sh        test tiers; bench.sh: a scorecard row
  cmd/tfbench/   accuracy and speed scorecard
  internal/      cpu (assembly primitives), siphash, workload, registersim
  verify/        tools that check every claim; isolated.sh re-runs them offline
```

Every file is listed by category in [docs/artifact/source-map.md](docs/artifact/source-map.md).

Each port goes in its own top-level folder (`python/`, `cpp/`, `java/`) with its own build. A port follows [docs/specification/format.md](docs/specification/format.md), must reproduce `spec/vectors/` exactly and must provide the same five test tiers ([docs/specification/conformance.md](docs/specification/conformance.md)).

## Go

```
cd go
./test.sh smoke         # also: sanity, regression, perf, stress, all
./bench.sh "label"      # a scorecard row in docs/record/scorecard-history.md
verify/run.sh           # every claim check (docs/artifact/claims.md)
```
