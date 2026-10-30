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
LICENSE        Apache License 2.0; NOTICE: the attribution it asks redistributors to keep
docs/          the documentation (paper, specification, artifact, record)
spec/vectors/  conformance vectors every implementation must reproduce
bench/h2h/     the head-to-head's industry arms (Rust), docs/artifact/head-to-head.md
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

## License

Copyright 2026 Priyasis Chakraborty. Licensed under the [Apache License, Version 2.0](LICENSE): you may use, modify and redistribute this work, including commercially, provided you keep the license and the [NOTICE](NOTICE) file with it and mark the files you change. The license grants a patent license from every contributor for their contributions (Section 3) and comes with no warranty (Section 7).

The license covers the whole repository: the library, its tests and tools, the conformance vectors in `spec/vectors/`, and the documentation. A port to another language in this repository is under the same license. Contributions submitted for inclusion are licensed under it too, unless their author states otherwise (Section 5).

`bench/h2h` builds against third-party crates (`pingora-limits`, `dashmap`, `sha2` and their dependencies) that Cargo fetches at build time under their own licenses; none of their code is in this repository.
