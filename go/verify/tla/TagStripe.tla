------------------------------ MODULE TagStripe ------------------------------
(***************************************************************************)
(* Which stripe a hold counts on (go/filter.go: putOver, sweepLine, the    *)
(* eviction in claimAt, relocate). Slot 1 is free (Start "free") or holds  *)
(* key J, expired to everyone (Start "J"); slot 2 is free. A holder (the   *)
(* decay sweep, or another key L's insert) reads slot 1's word0, then, in  *)
(* a step of its own, word1, then counts its hold as a move of a stripe,   *)
(* takes word1 busy by a CAS from the value it read, then word0 by a CAS   *)
(* (or puts word1 back), then publishes. K's first touch inserts K in the  *)
(* first slot whose word0 is free or expired (its install fails, and it    *)
(* looks again, if that slot's word1 is busy); its second touch counts in  *)
(* K's slot, or, missing it, looks again under K's claim stripe            *)
(* (lookupStable: a match, or a miss no move of K's stripe overlapped) and *)
(* inserts K. A reader that begins after K's insert reads K the same way.  *)
(*                                                                         *)
(* SameTag: J's tag is K's, so K's fresh register (one event) is           *)
(* bit-identical to J's. Variant "fp": the hold counts on the stripe of    *)
(* the key whose word0 it read (none over a free word0), as before;        *)
(* "reread": the same, but the holder reads word0 again after word1 and    *)
(* gives up if it moved (a consistent snapshot); "tag": on the stripe of   *)
(* word1's tag (none over a free word1).                                   *)
(* Invariants: NoDup (K visible in at most one slot), ReadNotLow (the      *)
(* reader never answers absent), Kept (once all are done, K's slot holds   *)
(* both events), NoStuck (no word1 left busy).                             *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS Variant, Holder, Start, SameTag, Reader

S == 1..2
None == [t |-> "none", n |-> 0]
Busy == [t |-> "busy", n |-> 0]
TagK == "T"
TagJ == IF SameTag THEN "T" ELSE "U"
Stripes == {"fJ", "fK", "T", "U", "none"}
FpStripe(v) == CASE v = "J" -> "fJ" [] v = "K" -> "fK" [] OTHER -> "none"
KStripe == IF Variant = "tag" THEN TagK ELSE "fK"
HoldStripe(r0, r1) == IF Variant = "tag"
                      THEN (IF r1.t \in {"none", "busy"} THEN "none" ELSE r1.t)
                      ELSE FpStripe(r0)
Min(X) == CHOOSE x \in X : \A y \in X : x <= y

(* --algorithm TagStripe
variables
  w0 = [j \in S |-> IF j = 1 /\ Start = "J" THEN "J" ELSE "free"],
  w1 = [j \in S |-> IF j = 1 /\ Start = "J" THEN [t |-> TagJ, n |-> 1] ELSE None],
  begun = [s \in Stripes |-> 0], done = [s \in Stripes |-> 0],
  kIn = FALSE, absent = FALSE, required = 0;

define
  IsK(j) == w0[j] = "K" /\ w1[j].t = TagK
  KSlots == {j \in S : IsK(j)}
  NoDup == Cardinality(KSlots) <= 1
  ReadNotLow == ~absent
  AllDone == \A p \in DOMAIN pc : pc[p] = "Done"
  Kept == AllDone => (KSlots /= {} /\ w1[Min(KSlots)].n >= required)
  NoStuck == AllDone => \A j \in S : w1[j].t /= "busy"
  Open == {i \in S : w0[i] \in {"free", "J"}}   \* free or expired by word0
end define;

fair process holder = "h"
variables rw0 = "free", rw1 = None, st = "none";
begin
h_r0:
  rw0 := w0[1];
  if rw0 \notin (IF Holder = "sweep" THEN {"J"} ELSE {"free", "J"}) then goto h_out; end if;
h_r1:
  rw1 := w1[1];
  if rw1.t = "busy" then goto h_out; else st := HoldStripe(rw0, w1[1]); end if;
h_re:
  if Variant = "reread" /\ w0[1] /= rw0 then goto h_out; end if;
h_begin:
  begun[st] := begun[st] + 1;
h_cas1:
  if w1[1] = rw1 then w1[1] := Busy; else goto h_done; end if;
h_cas0:
  if w0[1] = rw0 then
    w0[1] := IF Holder = "sweep" THEN "free" ELSE "L";
  else
    w1[1] := rw1;
    goto h_done;
  end if;
h_store:
  w1[1] := IF Holder = "sweep" THEN None ELSE [t |-> "V", n |-> 1];
h_done:
  done[st] := done[st] + 1;
h_out:
  skip;
end process;

fair process k = "k"
variables b = 0, d = 0;
begin
k_ins:                                    \* K's first touch: K is nowhere yet
  if w1[Min(Open)].t = "busy" then
    goto k_ins;
  else
    w0[Min(Open)] := "K" || w1[Min(Open)] := [t |-> TagK, n |-> 1];
    required := 1;
    kIn := TRUE;
  end if;
k_t2:                                     \* the second touch, lock-free
  if KSlots /= {} then
    w1[Min(KSlots)].n := w1[Min(KSlots)].n + 1;
    required := 2;
    goto k_out;
  end if;
k_b:                                      \* a miss: lookupStable under K's claim stripe
  b := begun[KStripe];
  d := done[KStripe];
k_scan:
  if KSlots /= {} then
    w1[Min(KSlots)].n := w1[Min(KSlots)].n + 1;
    required := 2;
    goto k_out;
  elsif ~(b = d /\ begun[KStripe] = b) then
    goto k_b;
  end if;
k_ins2:                                   \* insert K: the first open slot, or look again
  if Open = {} \/ w1[Min(Open)].t = "busy" then
    goto k_t2;
  else
    w0[Min(Open)] := "K" || w1[Min(Open)] := [t |-> TagK, n |-> 1];
    required := 2;
  end if;
k_out:
  skip;
end process;

fair process r \in (IF Reader THEN {"r"} ELSE {})
variables rb = 0, rd = 0;
begin
r_wait:
  await kIn;
r_b:
  rb := begun[KStripe];
  rd := done[KStripe];
r_scan:
  if KSlots /= {} then
    goto r_out;
  elsif rb = rd /\ begun[KStripe] = rb then
    absent := TRUE;
  else
    goto r_b;
  end if;
r_out:
  skip;
end process;

end algorithm; *)
=============================================================================
