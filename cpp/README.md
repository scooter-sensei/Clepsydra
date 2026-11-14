# Clepsydra in C++

The C++ port of the Go reference implementation (`go/`). It follows [the format](../docs/specification/format.md), reproduces `spec/vectors/` exactly (every hash, every operation's full `Result`, the end counters), and provides the five test tiers behind one entry point ([conformance](../docs/specification/conformance.md)). Where the specification is silent the Go code is normative, so this port translates it function by function: each `.cpp` file is one `.go` file, each function keeps its Go name, and every claim tag in a comment (`[A29]`, `[D29]`) points at the same code in both.

C++20; no dependencies beyond the standard library and the platform's threads. Run with Clang 18 (libc++) on x86-64 Linux, on arm64 Linux (musl, under qemu) and natively on x86-64 Windows (mingw-w64); written for GCC 11+ and MSVC 19.29+ too, which CI builds (see [Portability](#portability)).

## Build and test

```
cd cpp
cmake -S . -B build -G Ninja && cmake --build build
./build/clepsydra_test                  # every test; -run REGEX, -v
./test.sh smoke                         # also: sanity, regression, perf, stress, all
```

`test.sh` takes `CXX`, `CMAKE`, `GENERATOR`, `BUILD` (build root, default `./build`) and `OUT` (reports, default `./verify-report/<tier>`) from the environment. The suite reads the same switches as the Go suite: `TF_CT=1` (every filter ConstantTime), `TF_BG=1` (every filter Background), `TF_NOCAS2=1` (realign, stamp, unpoint and demotion hold word1 instead of using CMPXCHG16B).

On a Linux host without a system C++ compiler, zig's bundled Clang works: a wrapper `zcxx` running `zig c++ -target x86_64-linux-gnu "$@"`, then `CXX=zcxx ./test.sh sanity`. ThreadSanitizer needs ASLR off on kernels with high mmap entropy ("unexpected memory mapping"); the stress tier runs its binaries under `setarch -R`.

Undefined behaviour and memory errors, which Go rules out and C++ doesn't, have their own build: `-DCLEPSYDRA_SANITIZE=address,undefined` (CI's `sanitize` job) adds the sanitizers and the standard library's bounds checks; with zig's Clang, which ships no UBSan runtime, `-DCLEPSYDRA_SANITIZE=undefined -DCLEPSYDRA_SANITIZE_TRAP=ON` traps instead of reporting.

## Test tiers

| Tier | What it runs here | Status |
|---|---|---|
| **smoke** | configure; build with `-Werror` (the Go tier's vet); `TestSmoke` (every public operation once, three configs), `TestSmokeOptional` (Merge, a snapshot round trip, a Resizable transition), SipHash reference vectors | passes |
| **sanity** | smoke, the suite with `-short` (as Go's tier), the suite under `TF_CT=1`, `TF_BG=1` and, on x86-64, `TF_NOCAS2=1`, and the conformance vectors | passes |
| **regression** | conformance vectors, `TestRegress*`, and every deterministic output of the Go regression tier against `go/verify/expected/` byte for byte: `exact` (`tfbench -exact`), `placement`, `register-experiment`, `velocity-bound`, `velocity-information`, `attacks`, `rates`, `sketch-experiments`, `flood-sizing`, `answer-digest` | passes: all ten reproduce byte for byte on x86-64 and on arm64 |
| **perf** | the twelve benchmarks of `go/test.sh`'s `PERF_BENCH` (`bench/bench.cpp`, run as `testing.B` runs them and printed as `go test -bench` prints them), revision A against the working tree, alternated (`bench/ab.sh`); fails above `PERF_MAX` (1.20) | runs; `./test.sh perf go` compares with the Go reference (below). The C++/C++ gate needs a committed `cpp/` to compare with |
| **stress** | ThreadSanitizer on the suite, plain and in each mode; the four racy regression tests 300 times each, each run its own process (as `go/verify/replay.sh stress`); `TestStressSoak` (plain, under TSan, Background, Background with tags shared 8 ways); `TestFuzzUpper`; `TestFuzzConcurrent` (plain and with tags shared 8 ways) | passed at its defaults (soaks 60 s, under TSan 20 s; fuzzer 1e9 operations; concurrent fuzzer 5 min each) before `TestGhostEpochRace`'s stops and the Windows timer; since then, TSan over the `-short` suite in each mode |

### The suite

152 tests. 139 are Go's (of the reference's 150), under their Go names, with Go's constants, windows, rounds and skip conditions: every test of the library's files, the race tests and their hooks (the overflow table's two interleaving tests among them, [below](#the-overflow-tables-race-tests)), the white-box unit tests, the experiments the regression tier diffs (`TF_EXP=1`), the timing-leak measurement (`TF_TIMING=1`), the batch hash's (`go/internal/siphash`) and the register simulation's (`go/internal/registersim`). 13 are the port's own: the format's optional operations (`TestSmokeOptional`, `TestSnapshotRoundTrip`, `TestSnapshotRefusals`, `TestMergeSums`, `TestTouchIfUnderNeverOverLimit`, `TestHotKeepsBurstsExact`, `TestResizeKeepsCounts`), configuration edges (`TestConfigDefaults`, `TestMaxTick`, `TestTicksNegative`, `TestExactBoundaryContains`), `TestConformanceScenarios`, and `TestGoRand` (the port of `math/rand/v2` reproduces Go's streams).

Not ported, 11 of Go's 150: the host experiments behind `TF_EXP` and `TF_CTEXP` that the regression tier doesn't diff (`TestThroughput`, `TestHostFloor`, `TestHostGaps`, `TestMemProbe`, `TestInsertTail`, `TestPlacerInterference`, `TestSweepLatency`, `TestCTPads`, `TestCTParallel`: they measure the host and the Go runtime), and `go/internal/ecm`'s two (`TestHistogramError`, `TestSketchBounds`: the comparison sketch of `cmd/h2h`, not the filter). `go/cmd/tfbench` is ported for `-exact` (`tools/tfbench.cpp`); its speed scorecard is the perf tier's job. Of `go/bench_test.go`, the twelve `PERF_BENCH` benchmarks.

`clepsydra_test [-run REGEX] [-v] [-short] [-count N]` runs as `go test` does: `-count N` runs every repeat whatever the others did. The harness (`tests/check.hpp`) prints Go's formats: `%v` of a float as strconv's shortest `'g'`, NaN and infinities as Go's, a duration as `time.Duration` prints.

### The overflow table's race tests

An add to the overflow table decodes each entry's 2-bit slice against the epoch it loaded. If another add moves the epoch on and writes an entry for the new slice before the first add scans, that entry decodes as four slices old, dead, and the first add would overwrite a live history; a floor raise could lower a floor the same way. Every write therefore commits only while the epoch is the one it loaded (changes 28, the defect 42cc598 fixed). The bug takes two orderings at once: the first add's epoch load before the move, the mover's write before the first add's scan.

The reference's `TestGhostEpochRace` once waited for those orderings: 8 goroutines on 2 Ps (`GOMAXPROCS(2)`), each reading back its last three histories after every add. Go preempts a goroutine only after 10 ms on a P, so the orderings came mostly from the two Ps overlapping. Both implementations now make them happen (changes 101, claims A47). Two hooks, `testHookGhostAdding` and `testHookGhostFloor`, run where a ghost write has loaded the epoch and not yet what it decodes. With them:

- `TestGhostAddAcrossEpoch` and `TestGhostFloorAcrossEpoch` force the orderings once, single-threaded, and check every history and floor exactly (as `TestGhostReadAcrossEpoch` does for a read).
- `TestGhostEpochRace` keeps its workload, constants and check, runs on every core, and on one add pass in 16 (one floor raise in 4) holds the worker in its window, yielding until other adds have moved the epoch one to three slices. This is delay injection at a known conflict point, the idea behind TSVD (Li et al., SOSP 2019), aimed at both of the bug's orderings rather than at random priority-change points as in PCT (Burckhardt et al., ASPLOS 2010). It fails if no stop ever saw the epoch move.

The design was measured, before either implementation took it, with mutants of `ghost.cpp` and `ghost.go`, each guard dropped in turn, each arrangement run 20 times with `-short`, each run its own process, on the Ryzen host (6 cores / 12 threads, WSL, a TLA+ model check using about 1.7 cores alongside); `verify/ghost-race/` reproduces it (`CXX=… ./setup.sh && ./run.sh`). "Rejections" counts how often a guard found the epoch moved (the interleaving reached a commit) in the unmutated code; for a mutant, runs failed and live histories read low per run:

| | Go, `GOMAXPROCS(2)` (the reference's test before changes 101) | Go, all cores | C++, 8 threads on all cores (Go's arrangement ported) | C++, 8 threads on 2 CPUs | C++, stops in the window |
|---|---|---|---|---|---|
| free-entry rejections per run | 33 | 118 | 498 | 162 | 17,767 |
| floor rejections per run | 0.1 | 0 | 0.1 | 0.1 | 362 |
| unmutated code: runs failed | 0/20 | 0/20 | 0/20 | 0/20 | 0/20 |
| epoch read once, no guards (42cc598's defect) | 20/20, 4 low reads | 20/20, 13 | 20/20, 17 | 20/20, 8 | 20/20, 2,551 |
| the free-entry write's guard dropped | **14/20**, 1.4 | 19/20, 2.9 | 19/20, 9.9 | 18/20, 3.5 | 20/20, 2,256 |
| `raiseFloor`'s guard dropped | **0/20** | 0/20 | 0/20 | 0/20 | 14/20, 1.8 |
| the victim path's guards dropped | 0/20 | 0/20 | 0/20 | 0/20 | 0/20 |
| seconds per run | 0.16 | – | 0.13 | 0.13 | 1.0 |

At full length (40 rounds, 10 runs each) the free-entry rejections per run are 265 (Go, `GOMAXPROCS(2)`), 728 (Go, all cores), 2,391 (the ported arrangement) and 88,675 (stops); every arrangement then catches the free-entry guard dropped (10/10), and `raiseFloor`'s guard dropped is caught 0/10 by all but the stops, 9/10; a run takes 0.65 s ported and 5 s with the stops.

The two deterministic tests fail on every run of the mutants they cover (the free-entry guard and the epoch read once: `TestGhostAddAcrossEpoch`; `raiseFloor`'s guard and the epoch read once: `TestGhostFloorAcrossEpoch`) and pass on the code, in microseconds. Confining the C++ threads to 2 CPUs, as `GOMAXPROCS(2)` confined the goroutines, made the race rarer (a third of the rejections), so both run on every core. The reference with the stops reaches a guard with a stale epoch about 18,000 times a short run and catches `raiseFloor`'s guard dropped 61 runs in 80 (claims A47). Every history in the race test has 5 events, so a floor lowered by a stale raise still reads 5; only a stop of two or three slices, where a floor's slice goes wrong, shows it there, and the deterministic test shows it always. No arrangement catches the victim path's guards dropped. A floor keeps its slice mod 4, so a victim decoded against a stale epoch is floored with the same two bits: those guards protect nothing a reader can observe here, and the mutant is very likely equivalent.

## Architecture

`include/clepsydra/clepsydra.hpp` is the public API: `Config`, `Result`, `Filter`, `Resizable`, `MaxTick`, `AutoPad`, `Error`, `TickError`. Names are the Go reference's exported names (`TouchIfUnder`, `CheckAndTouchBatch`, `Config::Stash`), so [the paper](../docs/paper/04-operations.md) and the specification read the same for both.

`src/core.hpp` holds `detail::Core`: the Go `Filter` struct, field for field, with every method the reference has. Go's package-private visibility maps to this internal header: every member is public to the library and to the white-box tests, as Go's unexported identifiers are to `go/*_test.go`. `Filter` owns a `Core` (one indirection per call) so the public header carries none of it.

| Go | C++ | What it holds |
|---|---|---|
| `filter.go` | `src/filter.cpp`, `src/api.cpp` | construction and defaults, the clock, hashing, `ContainsAt`, counters; the public `Filter`'s methods |
| `slots.go` | `src/slots.cpp` | slot words, lookups, fingerprint, tag, line arithmetic |
| `touch.go` | `src/touch.cpp` | `touch`, `count`, `read`, `lookupStable`, the overflow read, realign and its 16-byte CAS |
| `cuckoo.go` | `src/cuckoo.cpp` | `miss`, `claim`, `claimAt`, eviction, install, relocation, abandon |
| `stash.go` | `src/stash.cpp` | the stash and the wait room, the Background placer |
| `ct.go` | `src/ct.cpp` | ConstantTime's pads and the placement steps |
| `batch.go` | `src/batch.cpp` | the batches and their prefetching |
| `decay.go` | `src/decay.cpp` | the decay sweep |
| `register.go` | `src/register.cpp` | the velocity register, `bump`, `unpoint`, `strayed`, `mass`, `estimate` |
| `hot.go` | `src/hot.cpp` | hot entries |
| `ghost.go` | `src/ghost.cpp` | the overflow table |
| `overflow.go` | `src/overflow.cpp` | `spill`, the overflow table's allocation, and the count-min sketch the ghost table replaced (behind `Core::cms`, for the experiments and the answer digest) |
| `buzz.go`, `touchif.go` | `src/buzz.cpp` | conditional touches and buzz entries |
| `merge.go` | `src/merge.cpp` | `Merge` |
| `snapshot.go` | `src/snapshot.cpp` | `WriteTo`, `ReadFrom` (version 3, reads version 2) |
| `resize.go` | `src/resize.cpp` | `Resizable` |
| `internal/cpu` | `src/cpu.hpp` | `CAS2` (LOCK CMPXCHG16B, CPUID probe for CX16), `PrefetchLines` |
| `internal/siphash` | `src/siphash.hpp` | SipHash-2-4 |
| `hash/crc32` (Castagnoli) | `src/crc32c.hpp` | the snapshot's check words |
| `math/bits` | `src/bits.hpp` | `Mul64`, `Div64` (`unsigned __int128`, or MSVC's intrinsics) |
| `*_test.go` | `tests/*_test.cpp` | tests under their Go names; `tests/check.hpp` is a minimal harness (Go's `testing`), `tests/json.hpp` reads the vectors, `tests/gorand.hpp` is Go's `math/rand/v2` over PCG bit for bit (so a seeded case replays as in Go), `tests/helpers.hpp` holds `whereIs`, `floorLive` and the tag amplifier |

## Type mapping

| Go | C++ | Note |
|---|---|---|
| `uint64`, `int64`, `uint32` | `std::uint64_t`, `std::int64_t`, `std::uint32_t` | |
| `int` (counts, `Config` sizes, `Result.Count`) | `std::int64_t` | Go's `int` is 64-bit on every platform the reference runs on; a hot key's count passes 2^31 |
| `int` (a word index, -1 for none) | `std::int64_t` (`i64`) | |
| `float64` | `double` | built with `-ffp-contract=off`: Go on amd64 doesn't fuse multiply-add, and a fused estimate differs in the last bit |
| `float64` → `int` | `static_cast<std::int64_t>` | truncation toward zero, as Go |
| `uint64` → `int64` | `static_cast<std::int64_t>` | modular since C++20, as Go |
| `time.Duration` | `clepsydra::Duration` = `std::chrono::nanoseconds` | |
| `time.Now`, `time.Since` | `std::chrono::steady_clock` | the built-in clock; `system_clock` for the snapshot's wall time |
| `func() uint64` (`Config.Clock`) | `std::function<std::uint64_t()>` | |
| `*[16]byte` (`Config.Key`) | `std::optional<std::array<std::uint8_t, 16>>` | |
| `[]uint64` (outputs of batches) | `std::span<T>` | a span shorter than its input throws `std::out_of_range` (Go: an index panic) |
| `string` (keys) | `std::string_view` | |
| `(T, bool)`, `(bool, int)` | `std::pair` | |
| `error` returned | `throw clepsydra::Error` | the reference's messages, word for word |
| `panic(errTick)` | `throw clepsydra::TickError` | before any lock, as the reference |
| `sync/atomic` on `[]uint64` | `std::atomic<std::uint64_t>` arrays | Go's atomics are sequentially consistent: every operation here is `seq_cst` (the default); a plain Go access to a word others access atomically is an atomic access too |
| `atomic.CompareAndSwap(p, old, new)` | `detail::cas(a, old, new)` | takes `old` by value (`compare_exchange` writes it back) |
| `atomic.Pointer[T]` | `std::atomic<T*>` plus an owning `unique_ptr` | Go's garbage collector frees a replaced table; here the filter owns every table until it is destroyed |
| `cpu.CAS2` | inline `lock cmpxchg16b` / `_InterlockedCompareExchange128` | over two `std::atomic<std::uint64_t>`, as the reference over two words; mixed-size atomics are outside the C++ model, inside every x86-64 CPU's |
| `sync.Mutex`, `TryLock` | `std::mutex`, `try_lock` | `paddedMutex` is `alignas(64)` |
| `defer` | `std::unique_lock`, `detail::Defer` at function scope | Go runs a defer at function return, never at a block's end: a guard is never declared inside a block a Go defer sat in |
| goroutine (CoarseClock) | `std::thread`, stopped by `Close` or the destructor | |
| goroutine (Background placer) | one `std::thread`, started on the first wait, parked on a condition variable while the goroutine would have exited | the destructor stops and joins it |
| `runtime.Gosched` | `std::this_thread::yield` | |
| `bits.TrailingZeros64`, `RotateLeft64` | `std::countr_zero`, `std::rotl` | |
| `unsafe` aligned tables (`alignedWords`) | `detail::AlignedBuf<T>` | 64-byte aligned, zeroed, so allocation takes the pages' faults (the reference's `prefault`) |
| package variables (test hooks, `forceCT`, `condMu`) | `inline` variables in `detail`; `condMu()` a function-local static | `condMu` is shared across filters, as in Go |

## Porting rules

These are the rules the translation keeps; a change to either implementation should keep them too.

1. **Operator precedence.** Go binds `&`, `&^`, `<<` and `>>` tighter than `+` and `-`, and `&` tighter than `==`; C++ does neither. Every mixed expression here is fully parenthesized (`h>>36&(1<<tagBits-1)` is `(h >> 36) & ((1 << tagBits) - 1)`).
2. **Shifts.** Go defines a shift by 64 or more as 0; C++ does not. No shift count here can reach 64.
3. **Name shadowing.** Go's field `f.hot` and function `hot(w1)` live apart; in a C++ member function the member wins, so the function is called as `detail::hot`. Go's local `ct` in `touch` is `inside` here.
4. **Control flow.** Go's `continue outer` is a flag and a `continue`; `break` inside an `if` in a `for` leaves the `for` (it does in both languages: checked where a `switch` precedes it).
5. **Same order of floating-point operations** as the reference, term for term, so `Count` agrees to the bit.
6. **Same allocation sizes** as the reference: `sizeof(StashEntry) == 24`, `sizeof(HotEntry) == 64`, `sizeof(BuzzBlock) == 512`, so `Bytes()` reports what the reference reports.

## Speed

`./test.sh perf go` (`bench/ab.sh go .`): the twelve `PERF_BENCH` benchmarks, Go 1.24.7 against this port (RelWithDebInfo, Clang 18), alternated six rounds on the Ryzen host (6 cores / 12 threads, WSL) with a TLA+ model check using about 1.8 cores alongside throughout. Medians, ns/op:

| benchmark | Go | C++ | C++/Go |
|---|---:|---:|---:|
| CheckAndTouchHit | 231.6 | 227.2 | 0.98 |
| CheckAndTouchHitHot | 61.7 | 63.3 | 1.03 |
| CheckAndTouchMissInsert | 2478.0 | 1885.2 | 0.76 |
| CheckAndTouchBatch (per key) | 106.3 | 86.9 | 0.82 |
| CheckAndTouchParallel | 27.5 | 27.6 | 1.00 |
| TouchAndCount | 254.2 | 244.0 | 0.96 |
| VelocityCount | 225.7 | 209.7 | 0.93 |
| VelocityCountHot | 97.1 | 66.2 | 0.68 |
| ContainsAbsent | 207.8 | 188.5 | 0.91 |
| InsertSteady | 605.4 | 489.9 | 0.81 |
| InsertNearFull | 6169.5 | 5128.0 | 0.83 |
| InsertSaturated | 1394.5 | 1163.3 | 0.83 |

What one response time tells about a key is compiled code's business, so the port has its own reading of it: `TF_TIMING=1 clepsydra_test -run '^TestTimingLeak$' -v` (2M keys at 85% load, 40,000 probes, the TLA+ check alongside). Kolmogorov-Smirnov distance between seen and unseen latencies, without ConstantTime: 0.178 (Count), 0.542 (Touch); with a 3 µs pad: 0.009 and 0.015, at the noise floor of about 0.014 at 20,000 samples per side, as the reference's [S41].

## Defects found while porting

Each was found by a test or a review of the port, fixed, and is covered since:

| Where | Defect | Found by |
|---|---|---|
| `roomGet` (`src/stash.cpp`) | after waiting out a busy wait-room entry it returned found with `retry` still set (Go returns `true, false`): `recall` answered "retry" for an entry already taken out of the room, the key's history was dropped, its stripe's move count stayed unbalanced and every later lookup on the stripe spun. Background mode only | the port of `TestBackgroundConcurrentCounts` |
| `ReadFrom` | into a CoarseClock filter that had been `Close`d it didn't move the cached tick (Go moves it whenever CoarseClock is on, `Close` or not) | review against `snapshot.go` |
| `tests/helpers.hpp` `Hooks` | reset all the test hooks at scope end, writing hooks a test never set while a thread it started could still read them (Go's defers reset only the hooks a test sets) | ThreadSanitizer |
| `buzz.cpp`, `cuckoo.cpp`, `hot.cpp` | three locals named as members of their class (`res`, `room`, `c`): MSVC's C4458 at `/W4`, an error under the library's `/WX` | Clang's `-Wshadow-all` standing in for MSVC |
| `Core` (`src/filter.cpp`) | CoarseClock's thread refreshed the cached tick on a condition variable's timed wait, which Windows times on the system timer (15.6 ms apart unless some process asks for finer): a 1 ms CoarseClock could lag 15 ms, and `TestCoarseClock` failed about 1 run in 100 on Windows. Go's runtime waits on a high-resolution timer there; the port now does too | the first native Windows run |
| `tests/main.cpp` | `-count N` stopped a test's repeats at its first failure; `go test -count` runs them all | review against `go test` |
| `tests/main.cpp` | `envOn` read `TF_CT=0` as off; Go's `os.Getenv(...) != ""` reads it as on | review against the Go suite |

## Deviations from the reference

| What | Reference | Here | Why |
|---|---|---|---|
| Hashing without `Config::Key` | `hash/maphash` under a random seed | SipHash-2-4 under a random 128-bit key from `std::random_device` | the format asks for a secret per-filter random seed; C++ has no maphash. SipHash costs more per key than maphash did in Go [S26]; a faster seeded hash is a possible change |
| `HashUint64s` | 8 keys at once with AVX-512F | one key at a time | same results; the batch hash is a later speed step |
| The table's pages | faulted on first touch | faulted at construction (zeroed) | construction time only |

## Portability

x86-64 uses CMPXCHG16B after a CPUID check, as the reference does; other architectures take the hold path (`HasCAS2` false), as the reference does on arm64. GCC and Clang get `unsigned __int128` for `Mul64` and `Div64`, MSVC `_umul128`, `__umulh` and `_udiv128`.

| Platform | Compiler, library | What has run |
|---|---|---|
| x86-64 Linux | Clang 18, libc++ (zig 0.13) | every tier; ThreadSanitizer over the suite in each mode; UBSan (trap mode) with libc++'s extensive bounds checks over the suite in each mode |
| arm64 Linux | Clang 18, musl, static (zig), under `qemu-aarch64` | conformance, the suite in plain, `TF_CT=1` and `TF_BG=1`, the ten regression outputs byte for byte (no 16-byte CAS: every slot change takes the hold path) |
| x86-64 Windows | Clang 18, libc++, mingw-w64 (zig, cross-built), run natively | the suite in each mode (built with `-Wall -Wextra`: no warning) |
| x86-64 Linux | GCC, libstdc++; Clang, libstdc++; Clang with ASan and UBSan | CI (`.github/workflows/cpp.yml`) |
| x86-64 Windows | MSVC, `/W4 /WX` for the library | CI |

The library keeps MSVC's `/W4` clean by construction: no local declaration hides a member (C4458), no unary minus on an unsigned operand (C4146), no 32-bit shift widened to 64 (C4334). The tests use only the standard library; the library's one platform-specific piece is CoarseClock's timer on Windows (a high-resolution waitable timer, as Go's runtime uses, with the condition variable as the fallback before Windows 10 1803). On Windows `steady_clock` usually steps 100 ns (QueryPerformanceCounter at 10 MHz), close to ConstantTime's 128 ns dither; the reference's clock there steps 100 ns as well.
