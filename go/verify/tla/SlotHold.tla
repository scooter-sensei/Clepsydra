------------------------------ MODULE SlotHold ------------------------------
(***************************************************************************)
(* A hold of an occupied slot (go/filter.go: sweepLine, putOver, install)  *)
(* against the occupant's touches. Slot 1 holds key K, last touched long   *)
(* enough ago that the holder sees it expired; slot 2 is free. The holder  *)
(* is the decay sweep (it clears the slot) or another key L's insert (it   *)
(* takes the slot): word1 busy, then word0, then word1 (or, if word0       *)
(* changed, word1 put back), the hold counted as a move of K's stripe.     *)
(*                                                                         *)
(* Lagging: a touch of K at a clock where K is live, which found slot 1 a  *)
(* moment before, moves word0 on lock-free (a touch in the register's      *)
(* unit), then counts in word1; finding word1 not K's, it looks K up again *)
(* (lookupStable: a match, or a miss no move overlapped, else look again)  *)
(* and, on a miss, inserts K fresh in a free slot. Reinsert: K's next      *)
(* first touch at a clock where slot 1 is expired: a hit resets the        *)
(* register; on a miss it takes a free or expired slot, installing a       *)
(* register bit-identical to the old one (one event, the same tag and      *)
(* epoch bits: claims A26's case). Both look again and insert under K's    *)
(* claim stripe, one at a time, as first touches do.                       *)
(*                                                                         *)
(* Variant: "fixed" is the protocol as committed (claims A26, A27).        *)
(* "oldSweep" is the sweep before 2408705 (word0 cleared, then word1 CASed *)
(* from the register read to 0; no hold). "noCount": the hold not counted  *)
(* as a move (the sweep without its count; inserts before 0779e1e).        *)
(* "noPutBack": word1 left busy when word0 changed. Invariants: NoDup (K   *)
(* matched in at most one slot), NoZombie (no slot with K's fingerprint    *)
(* and an empty register), Kept (once all are done, K's slot holds every   *)
(* live event counted), NoStuck (once all are done, no word1 is left       *)
(* busy). Reachable (each must be violated): NeverTaken (the holder        *)
(* completes), NeverPutBack, NeverWaited (a lookup waits out a hold).      *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS Variant, Holder, Lagging, Reinsert

S == 1..2
None == [k |-> "none", n |-> 0]
Busy == [k |-> "busy", n |-> 0]
KVals == {"K0", "K1", "K2", "K3"}  \* K's word0: old (expired to the holder), moved by the lagging touch, inserted by it, by the re-insert
Min(X) == CHOOSE x \in X : \A y \in X : x <= y
Counted == Variant \notin {"noCount", "oldSweep"}

(* --algorithm SlotHold
variables
  w0 = [j \in S |-> IF j = 1 THEN "K0" ELSE "free"],
  w1 = [j \in S |-> IF j = 1 THEN [k |-> "K", n |-> 1] ELSE None],
  begun = 0, done = 0,                 \* K's stripe's move counts
  stripe = "none",                     \* K's claim stripe: first touches look again and insert under it
  required = 0,                        \* K's live events whose touches counted
  taken = FALSE, putBack = FALSE, waited = FALSE;

define
  IsK(j) == w0[j] \in KVals /\ w1[j].k = "K"
  KSlots == {j \in S : IsK(j)}
  NoDup == Cardinality(KSlots) <= 1
  NoZombie == \A j \in S : w0[j] \in KVals => w1[j].k /= "none"
  AllDone == \A p \in DOMAIN pc : pc[p] = "Done"
  Kept == AllDone => (required = 0 \/ (KSlots /= {} /\ w1[Min(KSlots)].n >= required))
  NoStuck == AllDone => \A j \in S : w1[j].k /= "busy"
  NeverTaken == ~taken
  NeverPutBack == ~putBack
  NeverWaited == ~waited
end define;

fair process holder = "h"
variables rw0 = "free", rw1 = None;
begin
h_read:
  rw0 := w0[1];
  rw1 := w1[1];
  if rw0 /= "K0" \/ rw1.k = "busy" then goto h_out; end if;  \* nothing expired to take, or held
h_mode:
  if Variant = "oldSweep" then goto o_cas0; end if;
h_begin:
  if Counted then begun := begun + 1; end if;
h_cas1:
  if w1[1] = rw1 then w1[1] := Busy; else goto h_done; end if;
h_cas0:
  if w0[1] = rw0 then
    w0[1] := IF Holder = "sweep" THEN "free" ELSE "L";
  else
    if Variant /= "noPutBack" then w1[1] := rw1; end if;
    putBack := TRUE;
    goto h_done;
  end if;
h_store:
  w1[1] := IF Holder = "sweep" THEN None ELSE [k |-> "L", n |-> 1];
  taken := TRUE;
h_done:
  if Counted then done := done + 1; end if;
  goto h_out;
o_cas0:                                   \* the sweep before 2408705
  if w0[1] = rw0 then w0[1] := "free"; else goto h_out; end if;
o_cas1:
  if w1[1] = rw1 then w1[1] := None; end if;
  taken := TRUE;
h_out:
  skip;
end process;

fair process lag \in (IF Lagging THEN {"t"} ELSE {})
begin
t_cas0:                                   \* found slot 1 a moment before: word0 moves on, lock-free
  if w0[1] = "K0" then w0[1] := "K1"; else goto t_look; end if;
t_bump:
  if w1[1].k = "K" then
    w1[1].n := w1[1].n + 1;
    required := required + 1;
    goto t_out;
  end if;
t_look:                                   \* look again, lock-free (locate)
  if KSlots /= {} then
    w1[Min(KSlots)].n := w1[Min(KSlots)].n + 1;
    required := required + 1;
    goto t_out;
  end if;
t_lock:                                   \* a miss: K's claim stripe
  await stripe = "none";
  stripe := "t";
t_stable:                                 \* lookupStable, under the stripe
  if KSlots /= {} then
    w1[Min(KSlots)].n := w1[Min(KSlots)].n + 1;
    required := required + 1;
    stripe := "none";
    goto t_out;
  elsif begun /= done then
    waited := TRUE;
    goto t_stable;
  end if;
t_ins:                                    \* claim: K fresh in a free slot, or look again
  if \E i \in S : w0[i] = "free" /\ w1[i].k /= "busy" then
    with i \in {Min({i2 \in S : w0[i2] = "free" /\ w1[i2].k /= "busy"})} do
      w0[i] := "K2" || w1[i] := [k |-> "K", n |-> 1];
    end with;
    required := required + 1;
    stripe := "none";
  else
    stripe := "none";
    goto t_look;
  end if;
t_out:
  skip;
end process;

fair process re \in (IF Reinsert THEN {"r"} ELSE {})
begin
r_look:                                   \* lock-free: a hit, where an expired last touch resets the register
  if KSlots /= {} then
    with i \in {Min(KSlots)} do
      if w0[i] = "K0" then
        w0[i] := "K3" || w1[i].n := 1;
      else
        w1[i].n := w1[i].n + 1;
      end if;
    end with;
    required := required + 1;
    goto r_out;
  end if;
r_lock:
  await stripe = "none";
  stripe := "r";
r_stable:
  if KSlots /= {} then
    with i \in {Min(KSlots)} do
      if w0[i] = "K0" then
        w0[i] := "K3" || w1[i].n := 1;
      else
        w1[i].n := w1[i].n + 1;
      end if;
    end with;
    required := required + 1;
    stripe := "none";
    goto r_out;
  elsif begun /= done then
    waited := TRUE;
    goto r_stable;
  end if;
r_ins:                                    \* claim: the first slot free or expired, word1 not busy
  if \E i \in S : w0[i] \in {"free", "K0"} /\ w1[i].k /= "busy" then
    with i \in {Min({i2 \in S : w0[i2] \in {"free", "K0"} /\ w1[i2].k /= "busy"})} do
      w0[i] := "K3" || w1[i] := [k |-> "K", n |-> 1];  \* bit-identical to the old register
    end with;
    required := required + 1;
    stripe := "none";
  else
    stripe := "none";
    goto r_look;
  end if;
r_out:
  skip;
end process;

end algorithm; *)
=============================================================================
