# Clepsydra in Rust

The Rust port of the Go reference implementation (`go/`). It follows [the format](../docs/specification/format.md), reproduces `spec/vectors/` exactly (every hash, every operation's full `Result`, the end counters), and provides the five test tiers behind one entry point ([conformance](../docs/specification/conformance.md)). Where the specification is silent the Go code is normative, so this port translates it function by function, as [the C++ port](../cpp/README.md) does: each module is one `.go` file, each function keeps its Go name in snake_case, and every claim tag in a comment (`[A29]`, `[D29]`) points at the same code in all three.

Rust 2024 edition, declared for Rust 1.85 or later (`rust-version`; see [Portability](#portability)); the standard library only, in the library, its tests and its tools. Run with Rust 1.99.1 on x86-64 Linux and on arm64 Linux (musl, under qemu), under ThreadSanitizer with a nightly toolchain, and built with Rust 1.85; Windows (MSVC) and macOS (arm64) are checked from Linux and run only in CI (see [Portability](#portability)).

## Build and test

```
cd rust
cargo test --lib                        # every test (TF_SHORT=1: the short suite)
cargo test --lib -- TestA TestB --nocapture
cargo run --release --bin tfbench -- -exact
./test.sh smoke                         # also: sanity, regression, perf, stress, all
```

`test.sh` takes `CARGO` (default `cargo`), `NIGHTLY` (the toolchain for ThreadSanitizer, default `nightly`) and `OUT` (reports, default `./verify-report/<tier>`) from the environment. The suite reads the same switches as the Go suite: `TF_CT=1` (every filter ConstantTime), `TF_BG=1` (every filter Background), `TF_NOCAS2=1` (realign, stamp, unpoint and demotion hold word1 instead of using CMPXCHG16B), and `TF_SHORT=1` for `go test -short`. The tests share process-wide hooks, as the reference's package variables, so they run one at a time: `.cargo/config.toml` sets `RUST_TEST_THREADS=1` for `cargo test`, and `test.sh` sets it for the binaries it runs itself.

`test.sh` runs tests by their Go names (`run -- -run REGEX`), and prints the experiments' output as `go test -v` does: with `TF_LOG=<file>` the suite appends `=== RUN`, the log indented by four spaces and `--- PASS`/`--- FAIL` to the file, without the standard harness's lines between them, and the regression tier extracts the outputs from it as `go/verify/lib/checks.sh` extracts the Go tests'.

On a checkout on a Windows drive under WSL, `./cg.sh` is cargo with the target directory on a WSL-native path (builds on the Windows drive are slow) and, without a system C compiler to link with, zig's (a wrapper `zcc` running `zig cc "$@"`, as the C++ port's `zcxx`): `CARGO=./cg.sh ./test.sh sanity`. The stress tier's ThreadSanitizer build needs one thing more of that wrapper: rustc passes its TSan runtime as `-Wl,--whole-archive -Wl,<runtime>.a`, and zig's `cc` takes no archive inside `-Wl,`, so the wrapper passes such an argument's archive as a plain input (name it in `CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER`, which `cg.sh` leaves alone when it is set). The instrumented suite runs with ASLR off (`setarch -R`), as the C++ port's.

## Test tiers

| Tier | What it runs here | Status |
|---|---|---|
| **smoke** | build every target; clippy with warnings denied over `Cargo.toml`'s `[lints]` table (the Go tier's vet; [Porting rules](#porting-rules)); `cargo fmt --check` (its gofmt); rustdoc with its lints denied; the doctests; `TestSmoke` (every public operation once, three configs), `TestSmokeOptional` (Merge, a snapshot round trip, a Resizable transition), SipHash reference vectors | passes |
| **sanity** | smoke, the suite with `TF_SHORT=1` (as Go's tier), the suite under `TF_CT=1`, `TF_BG=1` and, on x86-64, `TF_NOCAS2=1`, and the conformance vectors | passes |
| **regression** | conformance vectors, `TestRegress*`, and every deterministic output of the Go regression tier against `go/verify/expected/` byte for byte: `exact` (`tfbench -exact`), `placement`, `register-experiment`, `velocity-bound`, `velocity-information`, `attacks`, `rates`, `sketch-experiments`, `flood-sizing`, `answer-digest` | passes: all ten reproduce byte for byte |
| **perf** | the twelve benchmarks of `go/test.sh`'s `PERF_BENCH` (`bench/bench.rs`, run as `testing.B` runs them and printed as `go test -bench` prints them), revision A against the working tree, alternated (`bench/ab.sh`); fails above `PERF_MAX` (1.20) | passes: the tree of [Porting rules](#porting-rules) 8 and 9 against the one before it, 0.99 to 1.01 on all twelve; `./test.sh perf go` compares with the Go reference ([below](#speed)) |
| **stress** | ThreadSanitizer on the suite, plain and in each mode (needs a nightly toolchain with `rust-src`; skipped, and the tier says so, without one); the four racy regression tests 300 times each, each run its own process (as `go/verify/replay.sh stress`); `TestStressSoak` (plain, under TSan, Background, Background with tags shared 8 ways); `TestFuzzUpper`; `TestFuzzConcurrent` (plain and with tags shared 8 ways) | passes at its defaults (nightly 1.102 for TSan): ThreadSanitizer over the whole suite, plain and in each mode, no report (21, 11, 12 and 18 min); the racy tests 0 failures in 300 runs each; soaks 60 s each (24 threads, 0.28 to 0.29 billion operations, no unflagged low answer), under TSan 20 s (4.5 million operations); the fuzzer 1.03 billion operations (873 cases); the concurrent fuzzer 3,067 and, tags shared, 2,979 cases. TSan runs with `allocator_may_return_null=1`: its allocator aborts on `TestConfigUnaddressable`'s 2^60-byte table where the system's returns null |

### The suite

157 tests. 139 are Go's (of the reference's 150), under their Go names, with Go's constants, windows, rounds and skip conditions: every test of the library's files, the race tests and their hooks, the white-box unit tests, the experiments the regression tier diffs (`TF_EXP=1`), the soak and the two fuzzers with the tag amplifier, the timing-leak measurement (`TF_TIMING=1`), the batch hash's (`go/internal/siphash`) and the register simulation's (`go/internal/registersim`). 18 are the port's own, the C++ port's and the Rust API's: the format's optional operations (`TestSmokeOptional`, `TestSnapshotRoundTrip`, `TestSnapshotRefusals`, `TestSnapshotRefusesWildIndices`, `TestMergeSums`, `TestTouchIfUnderNeverOverLimit`, `TestHotKeepsBurstsExact`, `TestResizeKeepsCounts`), configuration edges (`TestConfigDefaults`, `TestConfigUnaddressable`, `TestMaxTick`, `TestTicksNegative`, `TestExactBoundaryContains`), the C++ port's exception tests as panics (`TestPanicReleasesStripe`, `TestPanicBalancesMoves`: C++'s `TestThrow*`), every public type's `Debug` (`TestDebug`: never empty, never the key), `TestConformanceScenarios` (the vectors' operation streams are what the reference's generators draw from their seed), and `TestGoRand` (the port of `math/rand/v2` reproduces a million of Go 1.24.7's draws).

Not ported, 11 of Go's 150, as in the C++ port: the host experiments behind `TF_EXP` and `TF_CTEXP` that the regression tier doesn't diff (`TestThroughput`, `TestHostFloor`, `TestHostGaps`, `TestMemProbe`, `TestInsertTail`, `TestPlacerInterference`, `TestSweepLatency`, `TestCTPads`, `TestCTParallel`: they measure the host and the Go runtime), and `go/internal/ecm`'s two (`TestHistogramError`, `TestSketchBounds`: the comparison sketch of `cmd/h2h`, not the filter). `go/cmd/tfbench` is ported for `-exact` (`tools/tfbench.rs`); its speed scorecard is the perf tier's job. Of `go/bench_test.go`, the twelve `PERF_BENCH` benchmarks.

The tests are `#[test]` functions in the crate (`src/tests/`), white-box as Go's are: `Filter` derefs to the reference's struct (`Core`) under `cfg(test)`. `tests/check.rs` is Go's `testing` in miniature (`T`, `errorf!`, `fatalf!`, `logf!`, `skip!`) and prints Go's formats: `%v` of a float as strconv's shortest `'g'`, a duration as `time.Duration` prints, a `Result` as `%+v`. A Go subtest (`t.Run`) is a loop whose failures name the case (`demote/cas2`).

`TestFuzzUpper` drives each case from a seeded stream on one thread, so a case is a pure function of its seed and the code: for seeds 1 to 100 (64K to 4M operations each, ConstantTime, Background, stashless, overflow-off and few-hot-entries configurations among them) every tally the case prints (operations, answers checked, Saturated, floored, evictions, folds, promotions, waits) equals the reference's.

## Architecture

`src/lib.rs` is the public API: `Config`, `Result`, `Filter`, `Resizable`, `MAX_TICK`, `AUTO_PAD`, `ClockFn`, `Error`. Names are the Go reference's exported names in snake_case (`touch_if_under`, `check_and_touch_batch`, `Config::stash`), so [the paper](../docs/paper/04-operations.md) and the specification read the same for all three implementations.

`Core` (`src/filter.rs`) is the Go `Filter` struct, field for field, with every method the reference has; the public `Filter` (`src/api.rs`) owns it behind an `Arc`, which the Background placer's thread shares while it runs. Go's package-private visibility maps to `pub(crate)`: every member is visible to the library and to the white-box tests, as Go's unexported identifiers are to `go/*_test.go`.

The public API keeps [the Rust API Guidelines](https://rust-lang.github.io/api-guidelines/checklist.html) where they don't rename the reference: every public type is `Send` and `Sync` (asserted at compile time in `lib.rs`, C-SEND-SYNC) and implements `Debug` (C-DEBUG; `Filter`'s and `Resizable`'s print their sizes and modes, `Config`'s hides the key); the queries that record nothing (`count`, `contains_at`, the hashes, the counters, the sizes) are `#[must_use]`, as the C++ port's are `[[nodiscard]]`, and the touches, which record, are not; every function that can fail or panic says when under `# Errors` or `# Panics` (C-FAILURE); `write_to` and `read_from` take their writer and reader by value, so `&mut w` and a `&[u8]` both serve (C-RW-VALUE); the examples use `?` (C-QUESTION-MARK).

| Go | Rust | What it holds |
|---|---|---|
| `filter.go` | `src/filter.rs`, `src/api.rs` | construction and defaults, the clock, hashing, `ContainsAt`, counters; the public `Filter`'s methods |
| `slots.go` | `src/slots.rs` | slot words, lookups, fingerprint, tag, line arithmetic; the aligned table, the striped mutexes |
| `touch.go` | `src/touch.rs` | `touch`, `count`, `read`, `lookupStable`, the overflow read, realign and its 16-byte CAS |
| `cuckoo.go` | `src/cuckoo.rs` | `miss`, `claim`, `claimAt`, eviction, install, relocation, abandon |
| `stash.go` | `src/stash.rs` | the stash and the wait room, the Background placer |
| `ct.go` | `src/ct.rs` | ConstantTime's pads and the placement steps |
| `batch.go` | `src/batch.rs` | the batches and their prefetching |
| `decay.go` | `src/decay.rs` | the decay sweep |
| `register.go` | `src/register.rs` | the velocity register, `bump`, `unpoint`, `strayed`, `mass`, `estimate` |
| `hot.go` | `src/hot.rs` | hot entries |
| `ghost.go` | `src/ghost.rs` | the overflow table |
| `overflow.go` | `src/overflow.rs` | `spill`, the overflow table's allocation, and the count-min sketch the ghost table replaced (behind `Core::cms`, for the experiments and the answer digest) |
| `buzz.go`, `touchif.go` | `src/buzz.rs` | conditional touches and buzz entries |
| `merge.go` | `src/merge.rs` | `Merge` |
| `snapshot.go` | `src/snapshot.rs` | `WriteTo`, `ReadFrom` (version 3, reads version 2) |
| `resize.go` | `src/resize.rs` | `Resizable` |
| `internal/cpu`, `math/bits` | `src/cpu.rs` | `CAS2` (CMPXCHG16B after a CPUID check), `PrefetchLines`, `Mul64`, `Div64` (over `u128`) |
| `internal/siphash` | `src/siphash.rs` | SipHash-2-4 |
| `hash/crc32` (Castagnoli) | `src/crc32c.rs` | the snapshot's check words |
| package variables | `src/hooks.rs` | the test hooks (`testHook*`), `forceCT`, `forceBG`, `buzzOff` |
| `*_test.go` | `src/tests/*_test.rs` | tests under their Go names; `check.rs` the harness, `json.rs` reads the vectors, `gorand.rs` is Go's `math/rand/v2` over PCG bit for bit (so a seeded case replays as in Go), `gosort.rs` Go's `sort.Slice` (pdqsort, for tied ticks), `helpers.rs` holds `whereIs`, `floorLive` and the tag amplifier |

## Type mapping

| Go | Rust | Note |
|---|---|---|
| `uint64`, `int64`, `uint32` | `u64`, `i64`, `u32` | |
| `int` (counts, `Config` sizes, `Result.Count`) | `i64` | Go's `int` is 64-bit on every platform the reference runs on |
| `int` (a word index, -1 for none) | `isize` | a negative index panics, as an index out of range does in Go |
| `float64` | `f64` | Rust fuses no multiply-add it isn't asked to (`mul_add`), as Go on amd64; the operations run in the reference's order, term for term |
| `float64` → `int` | `as i64` | truncation toward zero, as Go; out of range Rust saturates where Go's result is the platform's. No value the filter computes from a Config `new` accepts is out of range ([below](#deviations-from-the-reference)) |
| `make`'s panic (a length past the address space), a failed allocation (fatal) | `new` returns an `Error` for the table; a lazily allocated table aborts, as any failed allocation does | `TestConfigUnaddressable` |
| `time.Duration` | `std::time::Duration` in the API, `i64` nanoseconds inside | a `Duration` is never negative: `Ticks` has no negative input to clamp (`TestRegressNegativeTicks`, `TestTicksNegative`); `AUTO_PAD` is `Duration::MAX` (Go: -1) |
| `time.Now`, `time.Since` | `std::time::Instant` | the built-in clock; `SystemTime` for the snapshot's wall time |
| `func() uint64` (`Config.Clock`) | `Option<ClockFn>`, `ClockFn = Arc<dyn Fn() -> u64 + Send + Sync>` | |
| `*[16]byte` (`Config.Key`) | `Option<[u8; 16]>` | |
| `[]uint64` (outputs of batches) | `&mut [T]` | a slice shorter than its input panics before any key is touched (Go: an index panic on the way) |
| `string` (keys) | `&str` | |
| `(T, bool)`, `(bool, int)` | tuples | |
| `error` returned | `std::result::Result<T, clepsydra::Error>` | the reference's messages, word for word |
| `panic(errTick)` | `panic!` with the same text | before any lock, as the reference |
| `sync/atomic` | `AtomicU64` and its kin, `SeqCst` everywhere | Go's atomics are sequentially consistent; a plain Go access to a word others access atomically is an atomic access here too |
| a word a lock guards (stash entries, buzz blocks, the hot table's free list) | atomics, or a `Mutex` the lock is | `StashEntry` and `BuzzEntry` are values read from and written back to their cells under the set's lock |
| `atomic.Pointer[T]`, set once | `OnceLock<T>` | replaces `skMu`; `ReadFrom` writes into the table `get_or_init` gives it (Go stores a new one) |
| `cpu.CAS2` | LOCK CMPXCHG16B in `asm!` | over two `AtomicU64`, as the reference's assembly over two words; mixed-size atomics are outside the Rust model, inside every x86-64 CPU's. Not `core::arch`'s `cmpxchg16b` intrinsic ([Portability](#portability)) |
| `sync.Mutex`, `TryLock` | `std::sync::Mutex<()>`; `PaddedMutex` is 64-byte aligned | a poisoned lock is used as it is, as Go's mutexes have no poison. A tick past MaxTick panics before any of the filter's own locks; what can panic under one (a lazily allocated table sized past what an allocation holds) unwinds through its guard ([Porting rules](#porting-rules) 8). `Resizable`'s mutex is held across a touch that can panic, and its next call recovers it, as Go's deferred unlock releases it |
| `defer` | a guard's `Drop` at function scope | |
| goroutine (CoarseClock) | `std::thread` holding a `Weak` to the filter | it ends at `Close`, or when the filter is dropped. On Windows `thread::sleep` asks for a high-resolution waitable timer, as Go's runtime does (the C++ port's timer defect, below its README's table); run in CI's Windows job. A goroutine always starts; a thread the system refuses is `new`'s `Error` |
| goroutine (Background placer) | `std::thread` holding an `Arc` while it runs | started on the first wait, ends after its idle naps, as the goroutine. A thread the system refuses leaves the keys waiting for the next wait's start, and first touches search inline past one waiting key per 16 wait-room entries, as when the placer falls behind |
| `runtime.Gosched` | `std::thread::yield_now` | |
| `bits.TrailingZeros64`, `RotateLeft64` | `u64::trailing_zeros`, `u64::rotate_left` | |
| `unsafe` aligned tables (`alignedWords`) | `AlignedWords` | 64-byte aligned, zeroed by the allocator; pages fault on first touch, as the reference's |
| `hash/maphash` (unkeyed hashing) | `std::collections::hash_map::RandomState` | a random key per filter, as `maphash.MakeSeed` |
| package variables (test hooks, `forceCT`, `buzzOff`) | `static`s in `hooks.rs` | a hook costs one relaxed load while unset |
| `Result` | `clepsydra::Result` | the reference's name; the standard library's is written `std::result::Result` in this crate |

## Porting rules

These are the rules the translation keeps; a change to any implementation should keep them too.

1. **Wrapping.** Every wrap the reference relies on is spelled `wrapping_*`. The suite runs with overflow checks on (`[profile.dev]`: optimized, overflow and debug assertions on), so an overflow the reference doesn't intend panics in the suite, the soak and the fuzzers instead of wrapping silently.
2. **Operator precedence.** Go binds `<<` and `>>` tighter than `+` and `-`; Rust doesn't. Every mixed expression is fully parenthesized (`h>>36&(1<<tagBits-1)` is `(h >> 36) & ((1 << TAG_BITS) - 1)`), and arithmetic is written as the reference writes it (`(a + b - 1) / b`, `x >= y + 1`: clippy's `manual_div_ceil` and `int_plus_one` are allowed for that).
3. **Shifts.** Go defines a shift by 64 or more as 0; Rust panics in the test profile and masks the count in release. No shift count here can reach 64.
4. **Names.** A method both `Filter` and `Core` have (`touch`, `count`, `hash`) resolves to the public one: the tests call `f.c.touch(...)` for the internal one.
5. **Same order of floating-point operations** as the reference, term for term, so `Count` agrees to the bit.
6. **Same allocation sizes** as the reference: `size_of::<StashCell>() == 24`, `size_of::<HotEntry>() == 64`, `size_of::<BuzzBlock>() == 512` (compile-time assertions), so `Bytes()` reports what the reference reports.
7. **No environment in the library.** The library reads no environment variable, as the reference's and the C++ port's don't: the suite sets `forceCT`, `forceBG` and the 16-byte CAS's switch from `TF_CT`, `TF_BG` and `TF_NOCAS2` (`tests/check.rs`).
8. **Unwinding.** A panic in Go's equivalent places is fatal to the process; here it unwinds out of one call and the filter stays shared by the others. So the state a first touch holds across a call that can panic, its stripe's moves begun and done and its claim stripe, is released by a guard (`Defer`, a `MutexGuard`), dropped where the reference releases it, and as the panic unwinds (`TestPanicReleasesStripe`, `TestPanicBalancesMoves`). Locks have no poison to inherit ([Type mapping](#type-mapping)). Not audited beyond those: a panic in the placer's own thread leaves `placing` set, so no placer starts again, and first touches search inline past one waiting key per 16 wait-room entries.
9. **Lints.** `Cargo.toml`'s `[lints]` table holds what the smoke tier and CI deny on top of rustc's and clippy's defaults, each with its reason: a SAFETY comment on every `unsafe` block and impl, one unsafe operation each, `unsafe_op_in_unsafe_fn`; the API Guidelines' documentation (`missing_docs`, `missing_errors_doc`, `missing_panics_doc`, `missing_debug_implementations`); lossless casts as `From`, pointer casts as `cast`, `cast_mut` and `ptr::from_ref`. Clippy's pedantic group is not on as a whole: most of its lints would have a line stop reading against its Go line (Go's integer conversions, loops, names and identifiers in doc prose). A lint left out at a site says why (`#[expect(..., reason = "...")]`, or `#[allow]` where it fires in some builds only). rustfmt keeps 150 columns (`rustfmt.toml`), one Go statement to a line where it fits: the Style Guide's 100 would re-wrap about 4,200 lines.

## Speed

`./test.sh perf go` (`bench/ab.sh go .`): the twelve `PERF_BENCH` benchmarks, Go 1.24.7 against this port (the release profile, Rust 1.99.1), alternated six rounds on the Ryzen host (6 cores / 12 threads, WSL). Medians, ns/op:

| benchmark | Go | Rust | Rust/Go |
|---|---:|---:|---:|
| CheckAndTouchHit | 184.9 | 178.9 | 0.97 |
| CheckAndTouchHitHot | 59.3 | 55.5 | 0.93 |
| CheckAndTouchMissInsert | 1695.5 | 1477.9 | 0.87 |
| CheckAndTouchBatch (per key) | 89.4 | 69.5 | 0.78 |
| CheckAndTouchParallel | 22.7 | 22.2 | 0.98 |
| TouchAndCount | 198.0 | 195.3 | 0.99 |
| VelocityCount | 184.2 | 168.9 | 0.92 |
| VelocityCountHot | 92.1 | 54.3 | 0.59 |
| ContainsAbsent | 163.9 | 130.5 | 0.80 |
| InsertSteady | 466.9 | 381.2 | 0.82 |
| InsertNearFull | 4976.0 | 3983.6 | 0.80 |
| InsertSaturated | 1145.5 | 968.8 | 0.85 |

The release profile is Cargo's (`debug = true` aside), measured against what [the Rust Performance Book](https://nnethercote.github.io/perf-book/build-configuration.html) suggests trying: each variant A/B against it, six alternated rounds of the twelve on the Ryzen host, B/A of medians. None is taken; a project that links the library sets its own profile (Cargo reads profiles from the workspace root only).

| variant | faster | slower | the rest | bench binary |
|---|---|---|---|---|
| `lto = "fat"`, `codegen-units = 1` | ContainsAbsent 0.85, CheckAndTouchBatch 0.92, CheckAndTouchHitHot 0.92 | VelocityCountHot 1.08, TouchAndCount 1.04 | 0.96-1.00 | 7.6 MB → 4.4 MB |
| `lto = "thin"` | | | 0.97-1.01 | 5.0 MB |
| `codegen-units = 1` | InsertSteady 0.96 | VelocityCountHot 1.11, CheckAndTouchBatch 1.10 | 0.97-1.02 | 7.1 MB |
| `#[inline]` for the 45 `#[inline(always)]` | | | 0.99-1.01 | |
| `#[inline]` on `Filter`'s forwarding methods | | VelocityCountHot 1.04 | 0.98-1.01 | |

The API, lint and defect changes of [Porting rules](#porting-rules) 8 and 9 cost nothing: that tree against the one before them, 0.99-1.01 on all twelve (`./test.sh perf`, whose Rust/Rust gate fails above 1.20).

What one response time tells about a key is compiled code's business, so the port has its own reading of it: `TF_TIMING=1 cargo test --release --lib -- --exact tests::timing_test::TestTimingLeak --nocapture` (2M keys at 85% load, 40,000 probes, the host otherwise idle). Kolmogorov-Smirnov distance between seen and unseen latencies, without ConstantTime: 0.089 (Count), 0.570 (Touch); with a 3 µs pad: 0.008 and 0.011, under the noise floor of about 0.014 at 20,000 samples per side, as the reference's [S41]. The tree of [Porting rules](#porting-rules) 8 and 9 against the one before it, three alternated runs each on another day: padded, Count 0.008-0.011 against 0.008-0.011 and Touch 0.010-0.011 against 0.006-0.012; unpadded the two read alike (Count 0.30-0.41 against 0.31-0.38, Touch 0.65-0.71 against 0.64-0.70), Count's above the 0.089 above in both: the host's state, not the code's.

## Defects found while porting

Each was found by a review of the port against the reference, and fixed:

| Where | Defect | Found by |
|---|---|---|
| `cpu.rs` | the CPU probe read `TF_NOCAS2` from the environment (the library reads none); the suite sets the switch now, and the probe stores its finding only over "not probed yet", so the suite's setting wins | review against `internal/cpu` |
| `snapshot.rs` (`snap_scrub`) | a refused restore left the hot table's `full` (the unit in which an allocation last found no entry) at 0 instead of "none", so promotions in unit 0 skipped their search for a free entry | review against `hot.go` |
| `cpu.rs` (`cas2`) | `core::arch`'s `cmpxchg16b` intrinsic, which Rust 1.85 to 1.98 inline into callers compiled without CMPXCHG16B, became a call of `__atomic_compare_exchange_16` there: at the declared minimum the library didn't link with a system linker (zig's supplied the routine, so this host's 1.85 build linked). The 16-byte CAS is assembly now, as the reference's ([Portability](#portability)) | CI's `msrv` job |
| `filter.rs` (`Core::new`) | a Config with 2^57 lines or more (a `capacity` near 2^62, or a tiny `load`) overflowed the table's word count: the suite's overflow checks panicked in `new`, and a release build wrapped it, so `capacity` 2^62+1024 at `load` 0.5 built a filter of 2^61+512 lines on a table of 512 lines, whose later lines panicked at their first index; a table the allocator refused (`capacity` 2^56) aborted the process; a NaN `load` built 2 lines. `new` returns an `Error` for each (`TestConfigUnaddressable`) | the C++ port's review against its Core Guidelines (its `TestConfigUnaddressable`), checked here in both profiles |
| `cuckoo.rs` (`miss`) | a first touch of a key waiting in Background's wait room takes it out with its stripe's move begun and ends the move once the key is placed; a panic out of the placement (a lazily allocated table sized past what an allocation holds, or the placer thread's start) skipped the end, and every later first touch on the stripe, in every thread, spun forever on the unbalanced count. A guard ends it now, where the reference does or as the panic unwinds (`TestPanicBalancesMoves`: 2 moves begun, 1 done, and a first touch that never returned, before) | the C++ port's `TestThrowBalancesMoves`, as a panic |
| `stash.rs` (`bg_kick`), `filter.rs` | a placer or CoarseClock thread the system refused to start panicked mid-operation (the placer's) or in `new` (the clock's). The placer's leaves the keys waiting now; the clock's is `new`'s `Error` ([Type mapping](#type-mapping)). Neither path is tested: no test makes the system refuse a thread | review against the API Guidelines (C-FAILURE) |
| `bench/ab.sh` | it took the last executable `cargo bench --no-run` listed, and the build lists `tfbench`'s too (cargo builds a package's binaries for its benches), before or after the bench's as the two finish compiling, so a build could run `tfbench` (which prints its usage and exits 2) where the benchmarks belong. It takes the bench's by name now | A/B runs of build profiles with the same selector, one of whose sides ran `tfbench` in every round |

## Deviations from the reference

| What | Reference | Here | Why |
|---|---|---|---|
| Hashing without `Config::key` | `hash/maphash` under a random seed | the standard library's `RandomState` (SipHash-1-3 under a random key) | the format asks for a secret per-filter random seed; Rust has no maphash |
| `HashUint64s` | 8 keys at once with AVX-512F | one key at a time | same results; the batch hash is a later speed step |
| `ReadFrom` of a stream whose check words are right but whose words no quiescent filter writes | accepted; an out-of-range hot-entry index panics at its first use, an odd hot-entry version hangs its readers | refused (`snap_sane`), and a refused restore leaves the filter empty (`snap_scrub`), as in the C++ port | a snapshot is trusted input, but a refusal is a better failure than a panic or a hang later (`TestSnapshotRefusesWildIndices`) |
| `ReadFrom` of a stash section into a filter with `Config::stash < 0` | panics (`newStashTable` takes the address of an empty table's first entry) | refused | as above |
| Negative durations | `Ticks` clamps them to 0 | not representable | `std::time::Duration` |
| A NaN `Config::load` | passes both of `New`'s checks; the line count converts to 2^63 on amd64, and the allocation fails | refused: `Load must be <= 1`, as in the C++ port | invalid input, outside the format's range (at most 1), that the reference doesn't refuse; refused, it can't size a table from nothing |
| A Config whose table has 2^57 lines or more, or more bytes than the allocator gives | wraps the table's word count and fails at `make` or at the first index (on amd64); an allocation that fails is fatal | refused at `new`: `Capacity too large for Load`, or `no memory for the table` | a wrapped count built the filter on a short table in a release build ([Defects](#defects-found-while-porting)); `new` returns a `Result` |

## Portability

x86-64 uses LOCK CMPXCHG16B, in assembly, after a CPUID check, as the reference does; other architectures take the hold path (`has_cas2` false), as the reference does on arm64. `Mul64` and `Div64` are `u128` arithmetic everywhere.

| Platform | Toolchain | What has run |
|---|---|---|
| x86-64 Linux (WSL) | Rust 1.99.1, linked by zig's `cc` | every tier; the suite in each mode |
| x86-64 Linux (WSL), ThreadSanitizer | nightly 1.102 (`-Zsanitizer=thread`, `-Zbuild-std`) | the stress tier's TSan checks ([above](#test-tiers)): the whole suite plain and under `TF_CT=1`, `TF_BG=1` and `TF_NOCAS2=1`, 157 pass in each, no report; the soak |
| arm64 Linux (musl, under qemu) | Rust 1.99.1 (`aarch64-unknown-linux-musl`), linked by zig's `cc` | the suite with `TF_SHORT=1`, plain, `TF_CT=1` and `TF_BG=1`: 155 pass in each (the two CMPXCHG16B tests are x86-64's, as Go's `cas2_amd64_test.go` is amd64's; 5.5, 3.9 and 3.7 min under qemu); the prefetch's `prfm` assembles and runs |
| x86-64 Windows (MSVC); arm64 macOS | Rust 1.99.1, in CI (`.github/workflows/rust.yml`) | the suite with `TF_SHORT=1`, plain and under `TF_CT=1` and `TF_BG=1` (Windows also `TF_NOCAS2=1`), and the conformance vectors, pass (the `windows` and `macos` jobs); clippy over every target with warnings denied, from Linux (`--target x86_64-pc-windows-msvc`, `--target aarch64-apple-darwin`, `--target aarch64-unknown-linux-musl`), in CI's `linux` job too: code under `cfg(target_arch)` compiles only for its target (the arm64 prefetch's pointer cast and an x86-64-only test helper were found there) |
| Rust 1.85, the declared minimum | 1.85.1 | the library, the suite and the tools build (CI's `msrv` job); built here at 1.85, 1.92, 1.95 and 1.99.1, dev and release, no artifact refers to a 16-byte atomic library routine (`nm`) and the release library holds LOCK CMPXCHG16B at its four call sites; clippy's `incompatible_msrv` checks the standard library's APIs against it |

The 16-byte CAS is LOCK CMPXCHG16B in inline assembly, as in the reference and the C++ port, so ThreadSanitizer sees neither it nor the words it writes, and sees the 8-byte atomics on the same words. `core::arch`'s `cmpxchg16b` intrinsic would serve neither the minimum nor TSan:

- Rust 1.85 to 1.98 (each release tested: 1.85, 1.88, 1.92, 1.95, 1.96, 1.97, 1.98) inline the intrinsic into its callers, which are compiled without CMPXCHG16B, and there its 128-bit `cmpxchg` becomes a call of `__atomic_compare_exchange_16`, which toolchains' default libraries don't define (CI's 1.85 build failed to link). 1.92 and 1.95 inline it so even from a never-inlined wrapper compiled with the feature; 1.99 keeps it out of line.
- TSan instruments it, and its runtime (compiler-rt, as the nightly toolchain ships it) runs a 128-bit CAS as plain loads and stores under one global spin lock and, for a non-relaxed order, its address's sync lock. An 8-byte atomic on word1, or a relaxed one on word0, takes neither, so it can land between the load and the store and be overwritten, as it can't be on the hardware.

A probe measured both (one thread swapping a 16-byte-aligned pair from what it read, changing one word; another adding 1 to the other word; 2 million times each, three runs per case). With the intrinsic under TSan, adds to word1 lost 1,283 to 3,934 and relaxed adds to word0 216 to 3,430; SeqCst adds to word0, which share the CAS's sync lock, lost none. With the assembly under TSan, and with either natively, none was lost in any case.
