------------------------------ MODULE SlotUnit ------------------------------
(***************************************************************************)
(* A slot's two words without the register's unit (go/touch.go,           *)
(* go/register.go, go/hot.go; claims D29). word0 holds the key's last     *)
(* touch, word1 a register or, for a hot key, a pointer to its entry. A    *)
(* register stores no unit: it is read at word0's. So whenever word1 is a  *)
(* register, it must be at word0's unit (Aligned), and a reader that       *)
(* accepts a pair must read it at its own unit (ReaderRight). Time is in   *)
(* units; a write of word0 inside its unit only changes its value (v).     *)
(*                                                                         *)
(* The writers that move word0 to a later unit, or put a register beside   *)
(* a word0 they did not move:                                              *)
(*   realign  a register's touch into a later unit: both words by one      *)
(*            16-byte CAS, or under a hold of word1 (busy)                 *)
(*   stamp    a hot key's touch into a later unit: word0 moves while word1 *)
(*            still points to the entry (one CAS of both, or a hold)       *)
(*   demote   the touch that starts a unit for the entry hands the counts  *)
(*            back as a register at that unit, only while word0's unit is  *)
(*            it (one CAS of both, word0 compared, or a hold, word0 read   *)
(*            under it)                                                    *)
(*   unpoint  a touch whose entry is gone (reclaimed) replaces the pointer *)
(*            by a fresh register at word0's unit, pinned the same way     *)
(*   bump     a touch inside word0's unit (word0 moved inside it, or not)  *)
(*            counts by a CAS of word1 alone, at word0's unit as loaded    *)
(*            after word1                                                  *)
(* The reader loads word0, word1, then word0 again, and looks again if     *)
(* word1 is busy or word0's unit moved (slotWords).                        *)
(*                                                                         *)
(* Start: "reg" (the key's word1 a register at unit 1) or "ptr" (a hot     *)
(* key, its entry at unit 1). Reclaim: the entry is reclaimed meanwhile.   *)
(* Variant: "cas2" (as committed on amd64), "hold" (as committed           *)
(* elsewhere, and with TF_NOCAS2=1), and each guard undone, which must     *)
(* break an invariant: "lockfreeUnit" (a register's touch into a later     *)
(* unit moves word0, then counts by bump, as before claims D29 without the *)
(* stored unit), "plainStamp" (a hot key's touch moves word0 by a CAS of   *)
(* word0 alone), "plainDemote" (a demotion by a CAS of word1 alone),       *)
(* "plainUnpoint" (likewise the reset of a pointer whose entry is gone).   *)
(***************************************************************************)
EXTENDS Integers, TLC

CONSTANTS Touchers, Clocks, Variant, Start, Reclaim

Hold == Variant = "hold"
Max(a, b) == IF a > b THEN a ELSE b
Reg(e, v) == [k |-> "reg", e |-> e, v |-> v]
Busy(v) == [k |-> "busy", e |-> 0, v |-> v]
None == [k |-> "none", e |-> 0, v |-> 0]

(* --algorithm SlotUnit
variables
  w0 = [u |-> 1, v |-> 0],                          \* word0: the last touch's unit, and its writes
  w1 = IF Start = "reg" THEN Reg(1, 0) ELSE [k |-> "ptr", e |-> 0, v |-> 0],
  entry = IF Start = "ptr" THEN "live" ELSE "none", \* "live", "gone" (reclaimed) or "none"
  ue = 1,                                           \* the entry's unit
  elock = FALSE,                                    \* the entry's lock
  readOK = TRUE;                                    \* ghost: no reader accepted a register at another unit

define
  Aligned == w1.k = "reg" => w1.e = w0.u
  ReaderRight == readOK
end define;

fair process toucher \in Touchers
variables c = 0, a0 = [u |-> 0, v |-> 0], a1 = None, r0 = [u |-> 0, v |-> 0], ok = FALSE, moved = FALSE;
begin
t0:
  with x \in Clocks do c := x; end with;
t_load:                                             \* word0
  a0 := w0;
t_route:                                            \* farAhead: word1, if the touch starts a unit
  a1 := w1;
  if c > a0.u then
    if a1.k = "busy" then goto t_load;
    elsif a1.k = "reg" then goto t_realign;
    else goto t_stamp;
    end if;
  end if;
t_cas0:                                             \* inside word0's unit: word0 moves to the touch's tick once, or not
  if w0 /= a0 then
    goto t_load;
  else
    either await ~moved; w0 := [u |-> a0.u, v |-> a0.v + 1] || moved := TRUE; or skip; end either;
    goto t_bump;
  end if;
t_realign:
  if Variant = "lockfreeUnit" then                  \* word0 first, the register by bump after
    if w0 /= a0 then goto t_load; else w0 := [u |-> c, v |-> a0.v + 1] || moved := TRUE; goto t_bump; end if;
  elsif Hold then
    if w1 /= a1 then goto t_load; else w1 := Busy(a1.v + 1); end if;
  else                                              \* realign2: both words by one CAS
    if w0 /= a0 \/ w1 /= a1 then
      goto t_load;
    else
      w0 := [u |-> c, v |-> a0.v + 1] || w1 := Reg(c, a1.v + 1);
      goto Done;
    end if;
  end if;
t_realign0:                                         \* under the hold: word0, or word1 put back
  if w0 /= a0 then
    w1 := a1;
    goto t_load;
  else
    w0 := [u |-> c, v |-> a0.v + 1];
  end if;
t_realign1:                                         \* the register at c
  w1 := Reg(c, a1.v + 2);
  goto Done;
t_stamp:
  if Variant = "plainStamp" then
    if w0 /= a0 then goto t_load; else w0 := [u |-> c, v |-> a0.v + 1] || moved := TRUE; goto t_bump; end if;
  elsif Hold then
    if w1 /= a1 then goto t_load; else w1 := Busy(a1.v + 1); end if;
  else                                              \* word0 by one CAS of both, word1 unchanged
    if w0 /= a0 \/ w1 /= a1 then goto t_load; else w0 := [u |-> c, v |-> a0.v + 1] || moved := TRUE; goto t_bump; end if;
  end if;
t_stamp0:                                           \* under the hold: word0
  ok := w0 = a0;
  if w0 = a0 then w0 := [u |-> c, v |-> a0.v + 1] || moved := TRUE; end if;
t_stamp1:                                           \* word1 back as it was
  w1 := a1;
  if ~ok then goto t_load; end if;
t_bump:                                             \* bump: word1, then word0
  a1 := w1;
t_bump0:
  r0 := w0;
  if a1.k = "busy" then goto t_load;                \* held: not the key's tag; look again
  elsif a1.k = "ptr" then goto t_hot;
  end if;
t_count:                                            \* the count's CAS of word1 alone, at word0's unit
  if w1 /= a1 then
    goto t_bump;
  else
    w1 := Reg(Max(c, r0.u), a1.v + 1);
    goto Done;
  end if;
t_hot:                                              \* the entry, under its lock
  await ~elock;
  elock := TRUE;
t_add:
  if entry /= "live" then
    elock := FALSE;
    goto t_unpoint;
  elsif c > ue then
    ue := c;
    either goto t_demote; or skip; end either;      \* the counts may fit a register
  end if;
t_add1:
  elock := FALSE;
  goto Done;
t_demote:
  if Variant = "plainDemote" then
    if w1 = a1 then w1 := Reg(c, a1.v + 1) || entry := "none"; end if;
    elock := FALSE;
    goto Done;
  elsif Hold then
    if w1 /= a1 then elock := FALSE; goto Done; else w1 := Busy(a1.v + 1); end if;
  else
    r0 := w0;
    goto t_demote2;
  end if;
t_demoteH:                                          \* under the hold: word0's unit read
  r0 := w0;
t_demoteH1:
  if r0.u = c then w1 := Reg(c, a1.v + 2) || entry := "none"; else w1 := a1; end if;
  elock := FALSE;
  goto Done;
t_demote2:                                          \* one CAS of both words, word0 as loaded
  if r0.u = c /\ w0 = r0 /\ w1 = a1 then w1 := Reg(c, a1.v + 1) || entry := "none"; end if;
  elock := FALSE;
  goto Done;
t_unpoint:                                          \* the entry gone: a fresh register at word0's unit
  if w1 /= a1 then goto t_bump; end if;
t_unpoint1:
  if Variant = "plainUnpoint" then
    if w1 = a1 then w1 := Reg(Max(c, r0.u), a1.v + 1); goto Done; else goto t_bump; end if;
  elsif Hold then
    if w1 /= a1 then goto t_bump; else w1 := Busy(a1.v + 1); end if;
  else
    if w0 = r0 /\ w1 = a1 then w1 := Reg(Max(c, r0.u), a1.v + 1); goto Done; else goto t_bump; end if;
  end if;
t_unpointH:                                         \* under the hold: word0 read, the register published
  r0 := w0;
t_unpointH1:
  w1 := Reg(Max(c, r0.u), a1.v + 2);
end process;

fair process reclaimer = "reclaim"
begin
rc:
  if Reclaim then
    await ~elock;
    if entry = "live" then entry := "gone"; end if;
  end if;
end process;

fair process reader = "reader"
variables b0 = [u |-> 0, v |-> 0], b1 = None;
begin
r_w0:
  b0 := w0;
r_w1:
  b1 := w1;
  if b1.k = "busy" then goto r_w0; end if;
r_again:
  if w0.u /= b0.u then goto r_w0; end if;
r_check:
  if b1.k = "reg" then readOK := readOK /\ b1.e = b0.u; end if;
end process;

end algorithm; *)
=============================================================================
