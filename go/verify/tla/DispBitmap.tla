----------------------------- MODULE DispBitmap -----------------------------
(***************************************************************************)
(* The displacement bitmap (claims A43; go/slots.go: mayDisp,              *)
(* setDisp, sweepDisp; go/cuckoo.go: install). One table line A, two       *)
(* slots, and its disp bit: "A may hold a displaced entry". Every entry    *)
(* modelled lives in A displaced (its primary line is elsewhere), so a     *)
(* lookup of it that misses its primary line reads A only if mayDisp says  *)
(* so.                                                                     *)
(*                                                                         *)
(* Writers place an entry (K, or another key X) in a free slot of A: word1 *)
(* busy and word0 taken in one step (the entry is hidden), then the bit    *)
(* (setDisp: read it, set it if clear), then either publish word1 or       *)
(* abandon the hidden copy (a relocation whose source changed). A          *)
(* published entry is later taken out (a relocation's hand-off, an         *)
(* eviction, the sweep's expiry): word1 busy, then word0 cleared. A        *)
(* sweeper reads the bit and, if set, scans A slot by slot; finding no     *)
(* displaced word0 it clears: begun + 1, clear the bit, scan A again, set  *)
(* the bit back if a displaced word0 is there, done + 1. A reader looks K  *)
(* up: the bit, and if it is clear, done, the bit again, begun; it reads A *)
(* (slot by slot) unless the bit is clear and begun = done.                *)
(*                                                                         *)
(* Variant: "fixed" is the protocol as in the patch. "noSetBefore": the    *)
(* writer publishes before it sets the bit. "noSeq": the reader trusts a   *)
(* clear bit without the clear counts. "noRescan": the sweeper does not    *)
(* look at A again after clearing. Invariants: NoMiss (a lookup that       *)
(* starts while K's entry is published and ends before it is taken out     *)
(* finds it), Covered (a published entry in A has the bit set or a clear   *)
(* under way). Reachable (each must be violated): NeverCleared (a sweep    *)
(* clears the bit), NeverSkipped (a lookup skips A), NeverRaced (a lookup  *)
(* reads A only because a clear overlapped its read of the bit).           *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS Variant, Writers, Sweepers, Rounds, Sweeps, Lookups

S == 1..2

(* --algorithm DispBitmap
variables
  w0 = [j \in S |-> "free"],   \* word0: "free", or the key whose displaced word0 is there
  pub = [j \in S |-> FALSE],   \* word1 published (not busy): a lookup can match the slot
  bit = FALSE,                 \* A's disp bit
  begun = 0, done = 0,         \* clears of the bits of A's stripe, begun and done
  removals = 0,                \* K's published entries taken out (ghost)
  missed = FALSE, cleared = FALSE, skipped = FALSE, raced = FALSE;

define
  KVisible == \E j \in S : w0[j] = "K" /\ pub[j]
  NoMiss == ~missed
  Covered == (\E j \in S : w0[j] /= "free" /\ pub[j]) => (bit \/ begun /= done)
  NeverCleared == ~cleared
  NeverSkipped == ~skipped
  NeverRaced == ~raced
end define;

fair process writer \in Writers
variables r = 0, slot = 1;
begin
w_top:
  if r >= Rounds then goto w_end; end if;
w_take:                         \* install: word1 busy, word0 CAS (hidden)
  with s \in {x \in S : w0[x] = "free"} do
    w0[s] := self;
    slot := s;
  end with;
  if Variant = "noSetBefore" then goto n_pub; end if;
w_load:                         \* setDisp
  if bit then goto w_pub; end if;
w_or:
  bit := TRUE;
w_pub:
  either
    pub[slot] := TRUE;
    goto w_hold;
  or
    w0[slot] := "free";            \* abandon the hidden copy
    goto w_next;
  end either;
n_pub:                          \* noSetBefore: publish, then the bit
  either
    pub[slot] := TRUE;
  or
    w0[slot] := "free";
    goto w_next;
  end either;
n_load:
  if bit then goto w_hold; end if;
n_or:
  bit := TRUE;
w_hold:                         \* taken out: word1 busy...
  pub[slot] := FALSE;
  if self = "K" then removals := removals + 1; end if;
w_free:                         \* ...then word0 cleared or replaced
  w0[slot] := "free";
w_next:
  r := r + 1;
  goto w_top;
w_end:
  skip;
end process;

fair process sweeper \in Sweepers
variables n = 0, seen = FALSE;
begin
s_top:
  if n >= Sweeps then goto s_end; end if;
s_bit:
  seen := FALSE;
  if ~bit then goto s_next; end if;
s_pre1:                         \* holdsDisp, a slot at a time
  if w0[1] /= "free" then goto s_next; end if;
s_pre2:
  if w0[2] /= "free" then goto s_next; end if;
s_begun:
  begun := begun + 1;
s_and:
  bit := FALSE;
  cleared := TRUE;
  if Variant = "noRescan" then goto s_done; end if;
s_re1:
  if w0[1] /= "free" then seen := TRUE; end if;
s_re2:
  if w0[2] /= "free" then seen := TRUE; end if;
s_or:
  if seen then bit := TRUE; end if;
s_done:
  done := done + 1;
s_next:
  n := n + 1;
  seen := FALSE;
  goto s_top;
s_end:
  skip;
end process;

fair process reader = "reader"
variables k = 0, vis = FALSE, rem = 0, d = 0, b = FALSE, g = 0, found = FALSE;
begin
r_top:
  if k >= Lookups then goto r_end; end if;
r_start:                        \* K's primary line missed
  vis := KVisible;
  rem := removals;
  found := FALSE;
r_fast:                         \* mayDisp: the bit (set: read A), then done, the bit again, begun
  if bit then goto r_s1; end if;
r_done:
  d := done;
r_bit:
  b := bit;
r_begun:
  g := begun;
  if ~b /\ (Variant = "noSeq" \/ g = d) then
    skipped := TRUE;
    goto r_check;
  end if;
r_race:
  if ~b then raced := TRUE; end if;
r_s1:                           \* find, a slot at a time
  if w0[1] = "K" /\ pub[1] then found := TRUE; end if;
r_s2:
  if w0[2] = "K" /\ pub[2] then found := TRUE; end if;
r_check:
  if vis /\ removals = rem /\ ~found then missed := TRUE; end if;
  k := k + 1;
  vis := FALSE || rem := 0 || d := 0 || b := FALSE || g := 0 || found := FALSE;  \* forget: fewer states
  goto r_top;
r_end:
  skip;
end process;
end algorithm; *)
=============================================================================
