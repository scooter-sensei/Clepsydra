# 5. Concurrency

Hits are lock-free: a touch is a CAS on each of the slot's two words. A key's first touch takes one of 256 striped locks (the claim stripes, by fingerprint), so racing writers can't create two slots [R2]. Everything else that changes a slot (an insert, a move, an eviction, a realign without a 16-byte CAS, the decay sweep) **holds** it: word1 is set to a reserved "busy" tag before word0 changes, and the hold counts as a move of a stripe so that a lookup that misses meanwhile looks again. The bugs this protocol closed, one by one, are listed in [record/defects.md](../record/defects.md); this section explains the protocol and the analysis behind it.

## 5.1 Counts under concurrency

[R2, D14] A slot changes hands with word1 set to a reserved "busy" tag before word0 changes, and a touch reads word1, then word0, so its check passes only if the slot was its key's when word0 was read. Word1 alone does not identify a key, though: two keys with the same 12-bit tag have bit-identical first registers in one unit (tag, the 4 epoch bits, one event). Every CAS or hold from a value read earlier is built around that:

- **A touch's count** is a CAS of word1 from the register it read, and can land in a same-tag key handed the slot after the check. The touch reads word0 again after its CAS; if the slot is no longer its key's, it waits out any hold of the slot and reads word1's tag: its own tag means the hand-over came before the count, which went to the other key (it reads one high), and the touch counts afresh; another tag means it came after, and the count went with the key's history [A29, X34]. Two coincidences are left: a hand-over to a same-tag key just after the count counts the event twice (about 6.6e-10 of counts, high), and a same-tag key handed on to another tag within the touch's window leaves the key one low (~6e-17 of counts: not observable).
- **Every hold** of a slot that may have an occupant (word1 busy: a move, an eviction, realign without CMPXCHG16B, the decay sweep, an insert taking a free or expired slot) counts as a move of the stripe of the tag in the word1 its CAS takes, so a lookup of whoever the slot held then, which misses it meanwhile, looks again instead of inserting the key a second time. The holder's word0 can belong to someone else: a key installed between its two loads, or a same-tag occupant [A26, A27, A28].
- A relocation's copy stays hidden until the hand-off has taken the source unchanged, so a key never has two live slots; a move whose source changed meanwhile is abandoned, and its copy, which counted nothing, cleared [A23]. A hot key's slot past the horizon resets under realign's hold [A30]. Realign's 16-byte CAS needs no hold: no lookup waits on it, and a touch whose two loads straddle it fails its own CAS from what it read; it compares word0 too, since a same-tag key handed the slot can hold the register bit for bit [A33].
- The concurrent fuzzer reads keys while other goroutines touch them; with every key's tag one of 8 values (`TF_CFUZZ_TAGS=8`), such coincidences come 512 times as often. Every open kind it had found traced to these races; after the fixes of 43e707b, 200e5ea and 9464187 it finds none, amplified or not [A22].

## 5.2 The race family: a CAS from a value read earlier

Every failure kind the concurrent fuzzer and the soak had left open (cases 12598, 3974, the Background soak's low, 4291, 3490, 1220) came from one family of race, in three mechanisms: **a CAS from a value read earlier succeeds on a slot whose occupant has changed, because word1 doesn't identify the occupant.** Two keys with the same 12-bit tag have bit-identical first registers in one unit (tag, the 4 epoch bits, one event), and a holder reads word0 and word1 in two loads. Each mechanism has a test that fails before its fix and passes after, a TLA+ model, and a hookless reproduction.

| Commit | Mechanism | Cases | Fix |
|---|---|---|---|
| 43e707b (A28) | A hold (insert over a free or expired slot, the sweep, an eviction, a relocation's hand-off) counted as a move of the stripe of the key whose **word0** it read (none over a word0 of 0), but its CAS takes word1 from whoever has it by then: a key installed between the holder's two loads (torn), or one whose word1 is bit-identical to the old occupant's (same tag). That key was hidden (word1 busy) with no move counted on its stripe: its touch inserted it a second time, its read answered absent | 3974 (two slots); 1220 (Count 0, and `whereIs` found no slot: the hold was still on); 1089's ConstantTime kind | Move counts striped by word1's tag (`moveOf`): a hold counts on the stripe of whoever has the value its CAS takes. `putOver` counts whenever word1 is nonzero and not busy. Busy slots are skipped as eviction victims explicitly (the old stripe skipped them implicitly) |
| 200e5ea (A29) | A touch checks its key's slot (word1, then word0), then counts by a CAS of word1 alone. If the slot is handed meanwhile to a key with the same tag and a bit-identical register, the CAS succeeds and counts in the newcomer; the key's stashed or spilled history is one event short. `bump`'s comment said "a newcomer with the same tag can't be mistaken for it": true when word0 is read, not at the CAS | 12598 (its evict record precedes the touch record; the evictor's clock is in the unit of the key's last touch); the soak's low | `bump` reads word0 again after its CAS. If the slot is no longer its key's, `strayed` waits out any hold of the slot and decides on that load of word1: the key's tag means the hand-over came before the count, which went to that key, and the touch counts afresh (that key reads one high); another tag means it came after, and the count went with the key. `promote`, which also CASes word1 from the register read, takes a counted hold and checks word0 under it |
| 9464187 (A30) | A hot key's slot past the horizon skipped `realign`: touch A moved word0, then reset word1 by a CAS from what it read. Touches after A's word0 found the key live and added to the hot entry (word1 unchanged: A's CAS succeeds) or demoted it (word1 their register: A resets it). Their events were gone | 4291 (4 of 5 lost); 3490 (1 lost, a hot entry) | A stale hot slot resets under `realign`'s hold, as a stale register already did |

### Deciding a stray count

A first version of the count fix counted afresh whenever word0, read again after the count's CAS, wasn't the key's. That is never low, but a hand-over just *after* the count, which takes the key's history with the event in it, also changes word0: the event was counted twice. The racy regression tests (D14), which check exact counts, caught it: 2, 11, 92 and 6 of 300 runs failed (plain; 2, 9, 89 and 14 with `TF_BG=1`), every failure one high. 7903faa, and 7903faa with the A28 and A30 fixes alone, read 0 of 300 on all four, plain and Background: the highs were that version's. The two cases differ in the slot's occupant: a hand-over before the count gave the slot to a key whose register was bit-identical to the one read, so of the key's tag; one after it gives the slot to a key of another tag but one time in 4096. `strayed` reads word1 once any hold of the slot is over (a holder may put back the register it took: realign, the sweep, an install that lost word0) and counts afresh only on the key's tag.

| | before the count (to a same-tag key) | after the count, newcomer of another tag | after the count, same-tag newcomer | same-tag key, then handed on to another tag, within the touch |
|---|---|---|---|---|
| no re-read (7903faa) | **low** | exact | exact | **low** |
| re-read, count afresh on any change (first version) | exact (newcomer high) | **high** | **high** | exact (newcomer high) |
| re-read, then the tag (`strayed`) | exact (newcomer high) | exact | **high** (residual) | **low** (residual) |

The model (`BumpABA.tla`: the eviction before or after the count, a hold that puts word1 back, a second hand-over; invariants KKept, never low, and KExact, exactly the key's events) checks every cell: 15 runs as wanted. Its first version decided on a second load of word1 after the wait and found a low (a hold beginning between the two loads, its busy word1 read as another tag); the code decides on the load it waited on, and so does the model now. Without the wait (`noWait`) a put-back hold over a same-tag key makes the key read low.

### The residuals' rates

From bump's counters (`bumpcount7.py`, not committed) over the concurrent fuzzer, 10 minutes each, seed 9000:

| Tags | Count CASes | word0 foreign after the CAS | counted afresh (`strayed`) | waited out a hold | failing cases |
|---|---|---|---|---|---|
| real (1 in 4096) | 1.067e9 | 2,870 | 0 | 187 | 0 of 1,803 |
| shared 8 ways | 1.054e9 | 5,924 | 706 | 237 | 0 of 1,733 |

Shared 8 ways, 706 of 5,924 is 0.119, the 1/8 at which a hand-over just after the count goes to a key of the counting key's tag: nearly every verdict to count afresh there is the high residual, and the true catches (a count that landed in another key) are a few dozen at most, as the arm without the count fix suggested (13 failing cases in 10 minutes, each a count lost that way). So the high residual runs at 6.7e-7 per count with tags shared 8 ways and about 2,870 / 4096 / 1.07e9 ≈ 6.6e-10 per count with real tags (a key reads one high; `Upper` stays a bound). The low residual needs both coincidences in one touch: a count that landed in a same-tag key (≈ 1e-8 per count shared 8 ways, from the 13 failures; 512 times rarer with real tags) and a second hand-over of that slot before the touch reads word1 (≈ 5.6e-6, the rate of the first), about 7e-14 per count shared 8 ways and 6e-17 with real tags: not observable. (An earlier build counted 604 foreign re-reads in a comparable unamplified run; such rates vary from run to run with the races.)

Where the high residual shows: `TestAbandonedMoveKeepsCounts` (exact totals of ~77,000 touches per round at 99% load) fails about once in 2,000 to 2,800 runs with real tags, one round one count high; with every cold key on a hot key's tag (`TF_SHARED_TAGS=1`) 6 of 1,300 runs at 9464187 against 0 of 1,300 for experiment E21, which removes the residual (one-sided Fisher p = 0.015) [A29, E21]. Details are in [record/defects.md](../record/defects.md#the-residual-high-count).

### How each mechanism was established

- **Hook tests** (`tagstripe_test.go`; plain, `TF_BG=1`, `TF_CT=1`; each fails with only its hooks added, passes with its fix), 5 runs each: A28's four tests (torn reads of an insert's scan and of the sweep, a same-tag sweep hold) fail 5 of 5 with the hooks alone and pass 5 of 5 at 43e707b; A29's `TestCountAcrossSameTagHandOver` and `TestCountIntoHeldSameTagSlotLooksAgain` fail 5 of 5 and pass at 200e5ea, and its two guards of the other side (`TestCountThenEvictionCountsOnce`, `TestCountThenMoveCountsOnce`: a hand-over just after the count) pass with the hooks alone and failed on the first version (Count 3 for 2); A30's `TestStaleHotResetKeepsTouches` fails 5 of 5 and passes at 9464187. Every cell in all three modes. Without `strayed`'s wait (a mutation), the held-slot test fails (Count 1 for 2).
- **TLA+**: `TagStripe.tla` (two keys, word0 and word1 read in separate steps, the hold's stripe as the variant): counting on word0's key violates NoDup and ReadNotLow (insert over a free word0, sweep, insert over an expired key); re-reading word0 after word1 (a consistent snapshot) fixes the torn reads but still violates both with a same-tag key; counting on word1's tag holds in all five settings (616–766 distinct states, exhaustive). `BumpABA.tla`: above. `SlotHold.tla` had passed because it reads both words in one step and its holder can only count on the one key it models.
- **Hookless, by amplification**: the concurrent fuzzer with every key's tag one of 8 values and its fingerprint unique in the case (`TF_CFUZZ_TAGS=8`; keys keep their lines; capacity ≤ 4096), so two keys that meet in a slot share a tag one time in 8 instead of one in 4096 (10 minutes per arm, seed 1): 7903faa 21 failing of 1,925 cases (20 one event short, 1 ConstantTime key a lookup under its stripe missed); the A28 fix only, 13 (all H1); the A29 fix only, 1 (3974's signature); both, 0 of 1,858. The committed series: 30 minutes, seed 3000, 0 failing of 5,775; 10 minutes, seed 9000, 0 of 1,733 (and 0 of 1,803 with real tags). Background soaks with tags shared 8 ways (`TF_STRESS_TAGS=8`, 60 s): 7903faa read low in 8 of 8, the series in 0 of 8.
- **12598's trace, re-read**: the evict record precedes the touch record, and both are written after their CASes succeed. The evictor's clock is in the unit of the key's last touch, so any key with tag 0x36c installed then has a register bit-identical to the key's. It had first been read as "both CASes succeeded from the same value": they did, the second from the newcomer's identical value.
- **Hookless H3** (a stress test, not committed: 12 goroutines released at once on a hot key whose slot just went stale), 200,000 rounds: 7903faa 200 rounds low (250 events lost), 9464187 0.

## 5.3 Solutions considered

**H1** (the count's CAS across a same-tag hand-over):

| Option | Exact? | Cost | Verdict |
|---|---|---|---|
| Re-read word0 after the CAS; if the slot isn't the key's, wait out a hold and count afresh only on the key's tag (`strayed`) | the newcomer reads one high; residuals: a same-tag hand-over just after the count (high), a same-tag key handed on to another tag within the touch's window (low) | one load of a line the CAS just took; the wait only on the rare path | **adopted** (A29) |
| Re-read word0 after the CAS; count afresh on any change (the first version) | never low; but a hand-over just after the count takes the event with the key's history, and the touch counts it again | the same | rejected: the racy tests read one high in 2–92 of 300 runs |
| Hold for every count (CAS word1 busy, check word0, store), as realign does | exact | two writes per touch; a descheduled holder blocks its key's touches and reads; lookups must wait out busy slots whose word0 is theirs | rejected: makes hits lock-like |
| Double-width CAS over both words (CMPXCHG16B, CASP) | exact; one CAS per touch instead of two | assembly per architecture; Java and Python ports can't; E3 measured a 16-byte CAS slower on the Xeon host | not now (measured later as E21, §5.5) |
| Validate with the tag stripe's move counts (every hand-over counts there after A28) | the residual low remains: the second hand-over of K2's slot counts on the same stripe as K's would | a load per touch of a line every hold writes | no gain over `strayed` |
| Undo the stray count (CAS the newcomer's register back) | restores the newcomer when untouched since | if the hand-over came after the count and the newcomer's register equals the counted one, takes an event from the newcomer: a low | rejected |
| Installs never publish a register equal to the one they replace | no: the stale touch's register can be an older value of the old key's (a touch counted between), equal to the newcomer's while the replaced one is not | a victim or path refused | incomplete |
| More identity bits in word1 | rarer, not gone | counter bits (the 48-bit counter decision, [record/decisions.md](../record/decisions.md#decided)) | rejected |

**H2** (holds counted on the wrong stripe):

| Option | Exact? | Cost | Verdict |
|---|---|---|---|
| Stripe move counts by word1's tag | yes, torn and same-tag alike (TagStripe.tla) | none: the index comes from a value already loaded | **adopted** (A28) |
| Re-read word0 after word1 (a consistent snapshot) | torn reads only; same-tag ABA remains (TagStripe.tla `reread`) | one load | insufficient |
| Count on both stripes | yes | twice the counting | no gain over the tag stripe |
| Lookups treat their fingerprint over a busy word1 as "look again" | yes | every lookup's rule changes; a key waits on another's hold when fingerprints collide | larger change |

Claim stripes (the first-touch mutexes) stay by fingerprint; only the move counts move to the tag.

**H3** (stale hot reset): reset under realign's hold (**adopted**, A30; costs nothing outside that case); or close the entry under its lock and fold events newer than the reset's decision into the fresh register (more code, same effect). Making the reset conditional on word1 still being the pointer doesn't help: adds to the entry leave word1 unchanged.

**Audit of the family** (every CAS or hold from a value read earlier): bump's count (H1), promote (H1), realign (validates word0 by CAS; its hold H2), install for evictions, relocations and inserts (H2), the sweep (H2), relocate's snapshot (re-reads word0; hand-off H2), a touch's word0 CAS (compares the whole word0, with its timestamp), markSat (word0, whole), the hot entry's demotion (under the entry's lock, owner checked). No other site found.

**Prior art.** MemC3's optimistic cuckoo hashing (Fan, Andersen, Kaminsky, NSDI 2013) validates lock-free reads with version counters striped by key, but runs one writer under a global lock, so the displacing writer always knows which key it moves. With lock-free, multi-writer hand-overs, the only reliable identity of the key a hold takes is the value its CAS took: hence the stripe by word1's tag (A28).

## 5.4 Hot entries leaving the stash

A touch of a hot key moves word0, then adds to the key's entry by the key's identity, not through the slot (one CAS of bucket 0's word, or the entry's lock). If the key is evicted between the two, its history (word0 with the touch's timestamp, word1 the entry's pointer) goes to the stash, and the add still finds the entry the key's: it counts there, and the event goes wherever the history goes. Two ways out of the stash left the entry the key's while nothing pointed to it any more, and the event was gone [A32]:

| Exit | Before | After (0becd6e) |
|---|---|---|
| The stash spills the history (to the overflow table; with it off, `Lost`) | `spill` read the entry and left it the key's | `spill` closes the entry under its lock (`hotClose`) and spills that last state; the late add fails, and the touch looks again and inserts the key afresh with the event |
| A full set's put takes the lightest way, which weighs nothing in the horizon | overwritten in place: a hot history whose touch has moved word0 but not yet added weighs nothing when its entry's events are past the horizon | the lightest way always leaves through `spill`, which closes the entry; a history with no events in the horizon is then neither spilled nor counted lost (its touch counts afresh) |

A register's count can't do this: it is a CAS of the slot's word1, which the eviction took, so it fails and the touch looks again. The model's `spill` writer (`HotEntry.tla`) stands for either exit: the history leaves, then the writer closes the entry and spills its state; without the close (`noSpillClose`) KExact fails.

**Every exit of a hot history**, audited: an eviction moves the pointer to the stash; a relocation moves it with the key; a restore takes it back from the stash; the spill and the full set's lightest way close the entry (A32); a demotion and a reclaim are the entry's own writers, under its lock (A31). The rest drop a history whose last touch is past the horizon at the dropper's clock (the stash's and the wait room's expired ways, the sweep, an insert over an expired occupant, a stale hot slot's reset, A30), whose events no clock up to the newest may count. Closing the entry there would be wrong as well as unneeded: an expired stash way can share its entry with the key's live slot, once the entry is reclaimed and given to the same key again.

## 5.5 Sixteen-byte CAS

### Realign

[A33, S52] A touch 8 or more units past its register's unit (every touch of a key touched less often than every 8 units: 20 s at a 60 s horizon), or of a hot key past the horizon, used to move word0 and word1 under a hold: word1 busy, counted as a move of the tag's stripe (two atomic adds on a counter line shared by 2–3 stripes and written by every core's realigns), a CAS of word0, then a store. On amd64 it now moves both by one CMPXCHG16B from the two words read, with no busy word1, so no lookup looks again and no move is counted; a promotion takes its entry first and publishes it by the same CAS, and closes it if the CAS fails. The CAS compares word0 because word1 alone doesn't name the key (a same-tag newcomer can hold the register bit for bit), and a touch whose two loads straddle it fails its own CAS from what it read. The count's CAS stays a CAS of word1 alone. `realign2` lives in `filter.go`, the CAS and a CPUID probe for CX16 in `internal/cpu/cas2_amd64.s` (GOAMD64=v1 doesn't promise the instruction). Other architectures keep the hold, and `TF_NOCAS2=1` keeps it on amd64 for the tests (`./test.sh sanity` runs the suite that way too). Sparse touches from every core run 14–15% faster in a program importing the library (one core 1.5–3%).

### The count as one CAS (experiment E21, not adopted)

[E21, S51] The count as one CMPXCHG16B over both words, from word0 and the register as read (`verify/experiments/cas2-count.patch`): a count can't land in a same-tag newcomer, so bump needs no re-read of word0 and no `strayed`, and has no residuals (`BumpABA.tla` `cas2`). Every check was clean, but the test binaries read TouchAndCount and CheckAndTouchBatch 1.07 against a bar of 1.03 written down before the readings (in a program importing the library alone, within its layout control). Placement-randomized over 10 layouts, Batch reads 1.077 [1.058, 1.096]: that cost is E21's own [S54]. Not adopted. Earlier, a single CMPXCHG16B for both slot words on the hit path had measured 19 vs 15 ns on the Xeon host [E3, S15].

## 5.6 Model checks

`go/verify/tla.sh` model-checks the slot protocol's concurrent parts with TLC (TLA+ tools 1.7.4, Java 11 or later), in `go/verify/tla/`. Each model runs the protocol as committed, exhaustively at small sizes, and then each of its guards undone, which must break an invariant.

| Model | What it checks | Claims |
|---|---|---|
| `WaitRoom.tla` | Background's wait room against touches and reads | A20 |
| `RegisterEpoch.tla` | a register's 4-bit epoch against its last touch; realign both ways as committed (its hold, and its 16-byte CAS on amd64); experiment E21's 16-byte count | A21, A33, E21 |
| `Relocate.tla` | a relocation's hidden copy against the key's touches, reads and eviction | A23 |
| `RoomWays.tla` | a wait-room set shared by a key being placed and one waiting beside it | A24 |
| `SlotHold.tla` | a hold of an occupied slot (the decay sweep, an insert over an expired entry) against the occupant's lagging touch and its re-insert | A26, A27 |
| `TagStripe.tla` | the stripe a hold counts its move on when word0 and word1 are read in two loads, with a key installed between them or one of the occupant's tag | A28 |
| `BumpABA.tla` | a touch's count across a hand-over of its slot, before or after the count, to a key of its tag or another; E21's 16-byte count | A29, E21 |
| `HotEntry.tla` | a hot entry's lock-free add against a writer that advances the entry, demotes it, reclaims it for another key or spills its history to the overflow table | A31, A32 |
| `experiments/HotCells.tla` | per-core cells for a hot entry (experiment E23, not adopted) | S53 |

How to run them, and the state counts, are in [artifact/reproducing.md](../artifact/reproducing.md#model-checks).
