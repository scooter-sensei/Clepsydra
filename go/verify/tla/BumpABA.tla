------------------------------ MODULE BumpABA ------------------------------
(***************************************************************************)
(* bump's count CAS across hand-overs (go/register.go: bump, strayed).    *)
(* Slot 1 holds K with one event; slot 2 is free. K's touch reads word1,   *)
(* then word0, checks that they are K's, and CASes word1 to its count. An  *)
(* insert of K2 evicts K, before the CAS or after it: it reads the        *)
(* victim's words, takes K's claim stripe, word1 busy, word0 K2's, word1   *)
(* K2's fresh register (one event), then stashes K's history and lets the  *)
(* stripe go. SameTag: K2's tag is K's, so its fresh register is           *)
(* bit-identical to K's. A touch that finds K in no slot looks again under *)
(* K's claim stripe, once no hold of slot 1 is under way (a hold counts as *)
(* a move: TagStripe): K's history, from the stash, goes to slot 2 with    *)
(* the event counted. Hold: a hold of slot 1 that puts its word1 back      *)
(* (realign, the sweep, an install that lost word0). Second: an insert of  *)
(* K3, of a third tag, evicts K2 from slot 1.                              *)
(*                                                                         *)
(* Variant "old": the CAS is the count. "post": after the CAS the touch    *)
(* reads word0 again and, if it isn't K's, looks again (fix B as first     *)
(* written). "tag": if it isn't K's, the touch waits out a busy word1 and  *)
(* looks again only if slot 1 then holds K's tag; otherwise the event went *)
(* with K's history. It decides on the load of word1 it waited on: a hold  *)
(* may begin after that load (deciding on a second load read the hold's    *)
(* busy word1 as another tag). "noWait": "tag" reading a busy word1 as     *)
(* another tag. "cas2" (experiment E21): the count is one CAS of both      *)
(* words, from those read; no re-read.                                     *)
(* Invariants: KKept (once all are done, K's events in its slot and the    *)
(* stash are at least its two touches: never low), KExact (exactly two:    *)
(* never high either), K2Kept (K2's event is in its slot or the stash).    *)
(* Reachable: NeverAgain (the post-check sends the touch to look again).   *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS Variant, SameTag, Hold, Second

None == [t |-> "none", n |-> 0]
Busy == [t |-> "busy", n |-> 0]
TagK2 == IF SameTag THEN "T" ELSE "U"

(* --algorithm BumpABA
variables
  w0 = <<"K", "free">>,
  w1 = <<[t |-> "T", n |-> 1], None>>,
  stash = 0, stash2 = 0, stripeK = FALSE, again = FALSE, k2In = FALSE;

define
  KSlots == {j \in 1..2 : w0[j] = "K" /\ w1[j].t = "T"}
  KEvents == stash + (IF KSlots = {} THEN 0 ELSE w1[CHOOSE j \in KSlots : TRUE].n)
  AllDone == \A p \in DOMAIN pc : pc[p] = "Done"
  KKept == AllDone => KEvents >= 2
  KExact == AllDone => KEvents = 2
  K2Kept == AllDone /\ k2In => stash2 >= 1 \/ \E j \in 1..2 : w0[j] = "K2" /\ w1[j].n >= 1
  NeverAgain == ~again
end define;

fair process touch = "t"
variables rv1 = None, rv0 = "free";
begin
t_r1:
  rv1 := w1[1];
t_r0:
  rv0 := w0[1];
  if rv0 /= "K" \/ rv1.t /= "T" then goto t_miss; end if;
t_cas:
  if w1[1] = rv1 /\ (Variant /= "cas2" \/ w0[1] = rv0) then w1[1] := [t |-> "T", n |-> rv1.n + 1];
  else goto t_r1;
  end if;
t_post:
  if Variant \in {"old", "cas2"} \/ w0[1] = "K" then goto t_out;
  elsif Variant = "post" then again := TRUE; goto t_miss;
  end if;
t_wait:                                   \* one load of word1: wait on it, decide on it
  if Variant = "tag" then await w1[1].t /= "busy"; end if;
  if w1[1].t = "T" then again := TRUE; goto t_miss; else goto t_out; end if;
t_miss:                                   \* look again under K's claim stripe
  await ~stripeK /\ w1[1].t /= "busy";
  if KSlots /= {} then
    w1[CHOOSE j \in KSlots : TRUE].n := w1[CHOOSE j \in KSlots : TRUE].n + 1;
  else
    w0[2] := "K" || w1[2] := [t |-> "T", n |-> stash + 1];
    stash := 0;
  end if;
t_out:
  skip;
end process;

fair process evict = "e"
variables vw0 = "free", vw1 = None;
begin
e_scan:
  vw0 := w0[1];
  vw1 := w1[1];
  if vw0 /= "K" \/ vw1.t = "busy" then goto e_out; end if;
e_lock:
  await ~stripeK;
  stripeK := TRUE;
e_cas1:
  if w1[1] = vw1 then w1[1] := Busy; else stripeK := FALSE; goto e_out; end if;
e_cas0:
  if w0[1] = vw0 then w0[1] := "K2"; else w1[1] := vw1; stripeK := FALSE; goto e_out; end if;
e_store:
  w1[1] := [t |-> TagK2, n |-> 1];
  k2In := TRUE;
e_stash:
  stash := vw1.n;
  stripeK := FALSE;
e_out:
  skip;
end process;

fair process hold = "h"
variables h1 = None;
begin
h_read:
  if ~Hold then goto h_out; end if;
h_r:
  h1 := w1[1];
  if h1.t = "busy" \/ h1.t = "none" then goto h_out; end if;
h_cas:
  if w1[1] = h1 then w1[1] := Busy; else goto h_out; end if;
h_back:
  w1[1] := h1;
h_out:
  skip;
end process;

fair process evict2 = "e2"
variables v20 = "free", v21 = None;
begin
f_scan:
  if ~Second then goto f_out; end if;
f_r:
  v20 := w0[1];
  v21 := w1[1];
  if v20 /= "K2" \/ v21.t = "busy" then goto f_out; end if;
f_cas1:
  if w1[1] = v21 then w1[1] := Busy; else goto f_out; end if;
f_cas0:
  if w0[1] = v20 then w0[1] := "K3"; else w1[1] := v21; goto f_out; end if;
f_store:
  w1[1] := [t |-> "V", n |-> 1];
f_stash:
  stash2 := v21.n;
f_out:
  skip;
end process;

end algorithm; *)
=============================================================================
