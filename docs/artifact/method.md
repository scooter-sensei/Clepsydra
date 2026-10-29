# Method

How the evidence in this artifact was gathered, and what the work learned about gathering it. Each rule below came from a case where its absence led to a wrong or weak conclusion; the case is named.

## Decisions

- **Write the rule before the reading.** Every speed or power reading that decided a change had its rule (the arms, the statistic, the bar) written down before it ran. When a rule failed as written it was not overridden: a replication under a corrected rule decided (M7 → M7b, S60).
- **A control condition must hold in the arm it controls for.** M7's "own/none ≤ 1.02" measured the writer thread taking an SMT sibling, which both contrasted arms share; the rule failed as written and a replication with a corrected rule decided.
- **Measure a mechanism's ceiling before building it.** Per-core cells for hot entries could give at most 1.23x at 12 goroutines on the Ryzen host (S53): measured with the add removed (counting nothing), under a bar written before, so the cells were not built into the library.

## Correctness

- **Check a fix against an exact invariant as well as the contract's bound.** The fuzzer and the soak check only `Upper ≥ truth`, so the first count fix's double count passed them; the racy tests, which count exactly, caught it (A29). Likewise the fuzzer now checks both directions (A36).
- **Amplify an improbable coincidence instead of waiting for it.** Forcing keys to share tags (with unique fingerprints, so no two keys share an identity) turned one failure per 30–60 minutes into two a minute, and made each fix's effect measurable in ten minutes. The same amplifier on the racy regression test (cold keys on the hot keys' tags) measures the high residual (A28, A29).
- **Two records that each say their CAS succeeded from the same value mean the value came back, not that a CAS failed**: look for a writer that installs an equal value, including another key's (case 12598).
- **Model the code's loads one for one.** A model that reads two words in one step, or whose holder can only count on the key it models, can't see torn reads or wrong-stripe counts (`SlotHold.tla` passed where `TagStripe.tla` failed). One that waits on one read and decides on another can find a race the code doesn't have (`BumpABA.tla`'s first version).
- **Rerun at the same power on a whole-tree export before calling a fix done.** A fix that passed 9 batches of reruns was incomplete: two mechanisms shared the case, and the reruns had run on an export missing `spec/vectors`, so the suites around them were partial too (A32).
- **List every path that removes an identity before reading the absence of a record as the absence of an event.** A trace that records an overwrite only where it names the history leaving (the spill of `out`) misses the overwrite that names no one (A32's in-place overwrite at no weight).
- **When a trace's cost hides a race, the trace's last good failure plus a hypothesis can be faster**: a hook test of the hypothesis reproduced it in one run (A32).
- **Keep each test's logs apart.** A shared work directory overwrote a failing run's log twice before `verify/replay.sh stress` gave each test its own directory; the sign of the failure (high or low) was what mattered (D14).
- **Give a check power before trusting it.** Undo a fix, plant a bug, or restore the old protocol, and confirm the check fails ([reproducing.md](reproducing.md#the-power-of-the-checks)).

## Speed

- **A spread of medians across layouts holds the host's run-to-run noise as well as placement.** Give every placement reading a null arm (the same binaries run again) and separate the two components; `verify/lrab.sh NULL=1` does both (X35).
- **Read cycles, not nanoseconds, when the question is the code**: on a laptop host the clock frequency is part of every ns/op (S56).
- **A pad generator's sizes need independence checked, not assumed**: two primes mod 25 gave a line, not a cube (X37).
- **The control for a change's cost on a path it doesn't run is the change's own bytes at its own place, never called.** A 32-byte pad underestimates placement: the same ~700 bytes in three places moved TouchAndCount from 0.999 to 1.147 in the test binaries (S52). A function nothing calls is dropped by the linker and controls nothing (X33).
- **Judge the geometric mean over layouts against the bar**, report the interval, and size the number of layouts to the benchmark's placement spread ([measuring-speed.md](measuring-speed.md#rules-for-readings)).
- **A benchmark's comment is a claim about the path it times**: check it against a profile when the code under it changes (X36).
- **Speed runs keep the host to themselves**: a speed queue waits out a model check, and checks that shared the host say so in their records.
- **Compare ratios on one host**, alternate the builds, and treat ratios inside a same-build pair's spread as noise.
