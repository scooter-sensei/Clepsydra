------------------------------ MODULE Relocate ------------------------------
(***************************************************************************)
(* A relocation of key K (go/filter.go: relocate, abandon, install,        *)
(* claimAt's eviction, touch, read). K's entry is in slot "src"; a         *)
(* relocation copies it to the free slot "dst" in K's other line, then     *)
(* hands src to a new key N (install: word1 busy, word0, word1, each a     *)
(* CAS on the words it read). Lookups scan the two lines in Order; a slot  *)
(* matches K when word0's key and word1's tag are K's (a busy word1 has    *)
(* no key's tag). A register is its event count.                           *)
(*                                                                         *)
(* The owner touches K Touches times, one at a time (the fuzzer's own      *)
(* keys): a lookup, then word0's CAS (if its clock is later; either), then *)
(* bump (word1, word0, CAS word1). A lookup that finds K nowhere trusts    *)
(* the miss only if no move of K's stripe began or ended around it         *)
(* (lookupStable); then, under K's claim stripe, the history is in the     *)
(* stash (a restore: here K's touches and reads go on in the stash). A     *)
(* reader reads K Reads times. An evictor (Evict) takes K's slot for a key *)
(* M under K's stripe and stashes K's words; evictions and moves of the    *)
(* stripe exclude each other with counts (moveCount).                      *)
(*                                                                         *)
(* Variant:                                                                *)
(*   "visible"   the protocol until claims A23: the copy is published by   *)
(*               its install; an abandoned move retires the copy and adds  *)
(*               what it counted beyond the source's words back to the     *)
(*               entry (fold)                                              *)
(*   "hidden"    as committed (A23): the copy goes in with word1 busy and  *)
(*               is published after the hand-off; an abandoned copy is     *)
(*               cleared                                                   *)
(*   "noMoveCount"         hidden, but a miss is trusted at once           *)
(*   "noHandoffCheck"      hidden, but the hand-off takes src whatever it  *)
(*                         holds                                           *)
(*   "hiddenNoEvictCheck"  hidden, without the eviction/move exclusion     *)
(*                         (it must still hold: the exclusion now saves    *)
(*                         wasted moves, not histories)                    *)
(* Invariants: ReadNotLow (a read sees every touch that returned before it *)
(* began), TouchNotLow (a touch's answer counts it and every earlier one), *)
(* OneHome (K matches at most one slot), Conserved (once every process is *)
(* done, K is in one place holding every event). NotMoved, NotAbandoned   *)
(* and NotEvicted check that a move, an abandoned move and an eviction    *)
(* are reachable: each must be violated.                                   *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, Sequences, TLC

CONSTANTS Touches, Reads, Evict, CopyFirst, Variant

Slots == {"src", "dst"}
Order == IF CopyFirst THEN <<"dst", "src">> ELSE <<"src", "dst">>
Hidden == Variant \in {"hidden", "noMoveCount", "noHandoffCheck", "hiddenNoEvictCheck"}
EvictCheck == Variant /= "hiddenNoEvictCheck"
Free0 == [k |-> "free", t |-> 0]
Free1 == [k |-> "free", c |-> 0]
Busy == [k |-> "busy", c |-> 0]

(* --algorithm Relocate
variables
  w0 = [s \in Slots |-> IF s = "src" THEN [k |-> "K", t |-> 0] ELSE Free0],
  w1 = [s \in Slots |-> IF s = "src" THEN [k |-> "K", c |-> 1] ELSE Free1],
  stash = [k |-> "none", c |-> 0],
  begun = 0, done = 0, evicting = 0,
  lock = "none",                   \* K's claim stripe
  truth = 1,                       \* ghost: K's events recorded (one before the run)
  readOK = TRUE, touchOK = TRUE,   \* ghost
  seen = {};                       \* ghost: "moved", "abandoned", "evicted" (reachability)

define
  Match(s) == w0[s].k = "K" /\ w1[s].k = "K"
  Where == {s \in Slots : Match(s)}
  ReadNotLow == readOK
  TouchNotLow == touchOK
  OneHome == Cardinality(Where) <= 1
  Final == \A p \in {"owner", "reader", "mover", "evictor"} : pc[p] = "Done"
  Total == IF Where = {} THEN (IF stash.k = "K" THEN stash.c ELSE 0)
           ELSE w1[CHOOSE s \in Where : TRUE].c
  NotMoved == "moved" \notin seen
  NotAbandoned == "abandoned" \notin seen
  NotEvicted == "evicted" \notin seen
  Conserved == Final => Cardinality(Where) + (IF stash.k = "K" THEN 1 ELSE 0) = 1 /\ Total = truth
end define;

fair process owner = "owner"
variables i = 0, os = "none", last = Free0, old = Free1, start = 0, b = 0, d = 0;
begin
o_start:
  if i = Touches then goto o_end; else start := truth; end if;
o_s1:
  if Match(Order[1]) then os := Order[1]; goto o_w0; end if;
o_s2:
  if Match(Order[2]) then os := Order[2]; goto o_w0; end if;
o_m0:                                         \* the miss path: rescan between move counts
  b := begun || d := done;
o_m1:
  if Match(Order[1]) then os := Order[1]; goto o_w0; end if;
o_m2:
  if Match(Order[2]) then os := Order[2]; goto o_w0; end if;
o_m3:
  if Variant /= "noMoveCount" /\ (b /= d \/ begun /= b) then goto o_s1; end if;
o_lock:
  await lock = "none";
  lock := "owner";
o_st:                                         \* restore, or (none) a fresh history
  if stash.k = "K" then
    touchOK := touchOK /\ stash.c + 1 >= start + 1;
    stash.c := stash.c + 1;
  else
    touchOK := touchOK /\ 1 >= start + 1;
    stash := [k |-> "K", c |-> 1];
  end if;
  truth := truth + 1;
  lock := "none";
  i := i + 1;
  os := "none" || last := Free0 || old := Free1 || b := 0 || d := 0;
  goto o_start;
o_w0:
  if w0[os].k /= "K" then goto o_s1; else last := w0[os]; end if;
o_cas0:
  either
    skip;                                     \* the same tick: word0 stays
  or
    if w0[os] = last then w0[os].t := last.t + 1; else goto o_s1; end if;
  end either;
o_b1:
  old := w1[os];
o_b2:
  if old.k /= "K" \/ w0[os].k /= "K" then goto o_s1; end if;
o_b3:
  if w1[os] = old then
    w1[os].c := old.c + 1;
    touchOK := touchOK /\ old.c + 1 >= start + 1;
    truth := truth + 1;
    i := i + 1;
    os := "none" || last := Free0 || old := Free1 || b := 0 || d := 0;
    goto o_start;
  else
    goto o_b1;
  end if;
o_end:
  skip;
end process;

fair process reader = "reader"
variables j = 0, rs = 0, rb = 0, rd = 0, res = 0;
begin
r_start:
  if j = Reads then goto r_end; else rs := truth; end if;
r_s1:
  if Match(Order[1]) then res := w1[Order[1]].c; goto r_chk; end if;
r_s2:
  if Match(Order[2]) then res := w1[Order[2]].c; goto r_chk; end if;
r_m0:
  rb := begun || rd := done;
r_m1:
  if Match(Order[1]) then res := w1[Order[1]].c; goto r_chk; end if;
r_m2:
  if Match(Order[2]) then res := w1[Order[2]].c; goto r_chk; end if;
r_m3:
  if Variant /= "noMoveCount" /\ (rb /= rd \/ begun /= rb) then goto r_s1; end if;
r_lock:
  await lock = "none";
  lock := "reader";
r_st:
  res := IF stash.k = "K" THEN stash.c ELSE 0;
  lock := "none";
r_chk:
  readOK := readOK /\ res >= rs;
  j := j + 1;
  res := 0 || rb := 0 || rd := 0;
  goto r_start;
r_end:
  skip;
end process;

fair process mover = "mover"
variables m0 = Free0, m1 = Free1, c1 = Free1, n = 0, fv = Free1;
begin
v_r0:
  m0 := w0["src"];
  if m0.k /= "K" then goto v_end; end if;
v_r1:
  m1 := w1["src"];
v_pre:
  if m1.k /= "K" \/ w0["src"] /= m0 then goto v_end; end if;
v_beg:
  begun := begun + 1;
v_ev:
  if EvictCheck /\ evicting /= 0 then goto v_dn; end if;
v_i1:                                         \* install the copy: word1 busy, word0, word1
  if w1["dst"] /= Free1 then goto v_dn; else w1["dst"] := Busy; end if;
v_i0:
  if w0["dst"] /= Free0 then w1["dst"] := Free1; goto v_dn; else w0["dst"] := m0; end if;
v_i2:
  w1["dst"] := IF Hidden THEN Busy ELSE m1;
v_h1:                                         \* the hand-off: src to N, if it still holds m0/m1
  if Variant = "noHandoffCheck" then
    w1["src"] := Busy;
  elsif w1["src"] = m1 then
    w1["src"] := Busy;
  else
    goto v_ab;
  end if;
v_h0:
  if Variant = "noHandoffCheck" \/ w0["src"] = m0 then
    w0["src"] := [k |-> "N", t |-> 0];
  else
    w1["src"] := m1;
    goto v_ab;
  end if;
v_h2:
  w1["src"] := [k |-> "N", c |-> 0];
v_pub:
  if Hidden then w1["dst"] := m1; end if;
  seen := seen \cup {"moved"};
  goto v_dn;
v_ab:
  seen := seen \cup {"abandoned"};
  if Hidden then
    w0["dst"] := Free0;
    goto v_ab1;
  end if;
v_ab0:                                        \* visible: retire the copy, word1 first
  if w1["dst"].k /= "K" then
    goto v_dn;                                \* the copy was evicted
  else
    c1 := w1["dst"] || w1["dst"] := Busy;
  end if;
v_ab2:
  w0["dst"] := Free0;
v_ab1:
  w1["dst"] := Free1;
  if Hidden \/ c1.c <= m1.c then goto v_dn; else n := c1.c - m1.c; end if;
v_f1:                                         \* fold n into the entry: src, else dst
  if w1["src"].k = "busy" then
    goto v_f1;
  elsif w1["src"].k = "K" /\ w0["src"].k = "K" then
    fv := w1["src"] || w1["src"] := Busy;
  else
    goto v_f3;
  end if;
v_f2:
  w1["src"] := [k |-> "K", c |-> fv.c + n];
  goto v_dn;
v_f3:
  if w1["dst"].k = "busy" then
    goto v_f3;
  elsif w1["dst"].k = "K" /\ w0["dst"].k = "K" then
    fv := w1["dst"] || w1["dst"] := Busy;
  else
    goto v_dn;                                \* nowhere: the events are lost
  end if;
v_f4:
  w1["dst"] := [k |-> "K", c |-> fv.c + n];
v_dn:
  done := done + 1;
  m0 := Free0 || m1 := Free1 || c1 := Free1 || n := 0 || fv := Free1;
v_end:
  skip;
end process;

fair process evictor = "evictor"
variables vs = "none", e0 = Free0, e1 = Free1;
begin
e_scan:
  if ~Evict \/ Where = {} then
    goto e_end;
  else
    with x \in Where do vs := x; e0 := w0[x]; e1 := w1[x]; end with;
  end if;
e_mv:                                         \* a key whose move is under way is left alone
  if EvictCheck /\ begun /= done then goto e_end; end if;
e_lock:                                       \* TryLock K's stripe
  if lock /= "none" then goto e_end; else lock := "evictor"; end if;
e_ev:
  evicting := evicting + 1;
e_chk:
  if EvictCheck /\ begun /= done then
    evicting := evicting - 1;
    lock := "none";
    goto e_end;
  end if;
e_beg:
  begun := begun + 1;
e_i1:
  if w1[vs] /= e1 then goto e_fail; else w1[vs] := Busy; end if;
e_i0:
  if w0[vs] /= e0 then w1[vs] := e1; goto e_fail; else w0[vs] := [k |-> "M", t |-> 0]; end if;
e_i2:
  w1[vs] := [k |-> "M", c |-> 0];
e_st:
  stash := [k |-> "K", c |-> e1.c];
  seen := seen \cup {"evicted"};
e_fail:
  done := done + 1;
  evicting := evicting - 1;
  lock := "none";
  vs := "none" || e0 := Free0 || e1 := Free1;
e_end:
  skip;
end process;

end algorithm; *)
=============================================================================
