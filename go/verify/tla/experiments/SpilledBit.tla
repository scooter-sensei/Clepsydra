------------------------------ MODULE SpilledBit ------------------------------
(***************************************************************************)
(* A per-key "spilled" bit (NEXT-RESULTS.md N9; go/touch.go, cuckoo.go,    *)
(* stash.go). Key K starts in a slot with one event and the bit clear.     *)
(* While the bit is clear, a touch or read that finds K skips the overflow *)
(* table. An evictor gives K's slot to another key and stashes K's words   *)
(* (claimAt: under K's claim stripe, a move count begun around it); a      *)
(* stasher puts another history into K's stash set, which pushes K's out   *)
(* to the overflow table (stashPutAs: the spill writes the overflow table  *)
(* before the way is overwritten, both under the set's lock). K's toucher  *)
(* touches it Touches times: a hit counts in the slot; a miss is a first   *)
(* touch (K's stripe; recall from the stash under the set's lock; the      *)
(* overflow table read; install with the bit set if recall found a stash   *)
(* history or the overflow table holds anything for K). A reader reads K   *)
(* Reads times (touch.go read): a lock-free scan of the table; a miss is   *)
(* trusted only if no eviction began or ended around it, and if the stash  *)
(* gate is up (an eviction stashed something) the read takes K's stripe    *)
(* and looks in the table and the stash again; then the overflow table     *)
(* unless K was found with the bit clear.                                  *)
(*                                                                         *)
(* Variant:                                                                *)
(*   "fixed"               as the patch                                    *)
(*   "evictNoStripe"       the eviction does not hold K's stripe           *)
(*   "wayFirst"            the stasher frees the way, releases the lock,   *)
(*                         then writes the overflow table                  *)
(*   "checkOutsideStripe"  the first touch reads the overflow table before *)
(*                         taking the stripe and recalling                 *)
(*   "noBit"               the bit is never set                            *)
(* Invariants: ReadNotLow (a read counts every event of K recorded before  *)
(* it began), TouchNotLow (a touch's answer counts it and every earlier    *)
(* event).                                                                 *)
(***************************************************************************)
EXTENDS Integers, TLC

CONSTANTS Touches, Reads, Variant

None == [k |-> "none", c |-> 0, bit |-> FALSE]

(* --algorithm SpilledBit
variables
  slot = [k |-> "K", c |-> 1, bit |-> FALSE],
  stash = None,
  ovf = 0,                         \* K's events in the overflow table
  stripe = "none",                 \* K's claim stripe
  setLock = "none",                \* K's stash set's lock
  gate = FALSE,                    \* the stash gate (stashUntil): raised by a stash write
  begun = 0, done = 0,             \* K's stripe's move count
  truth = 1,                       \* ghost: K's events recorded
  readOK = TRUE, touchOK = TRUE;   \* ghost

define
  ReadNotLow == readOK
  TouchNotLow == touchOK
end define;

process evictor = "evictor"
variables h = None;
begin
E0: if Variant /= "evictNoStripe" then
      await stripe = "none"; stripe := "evictor";
    end if;
    begun := begun + 1;
E1: if slot.k = "K" then
      h := slot; slot := [k |-> "M", c |-> 0, bit |-> FALSE];
    end if;
E2: if h.k = "K" then
      await setLock = "none";
      stash := h; gate := TRUE;
    end if;
E3: done := done + 1;
    if Variant /= "evictNoStripe" then stripe := "none"; end if;
end process;

process stasher = "stasher"
variables x = None;
begin
S0: await setLock = "none"; setLock := "stasher"; x := stash;
S1: if Variant = "wayFirst" then
      stash := [k |-> "X", c |-> 0, bit |-> FALSE]; setLock := "none";
    elsif x.k = "K" then
      ovf := ovf + x.c;
    end if;
S2: if Variant = "wayFirst" then
      if x.k = "K" then ovf := ovf + x.c; end if;
    else
      stash := [k |-> "X", c |-> 0, bit |-> FALSE]; setLock := "none";
    end if;
end process;

process toucher = "toucher"
variables n = 0, o = 0, got = None, tgt = 0, cnt = 0, b = FALSE;
begin
T0: while n < Touches do
T1:   if slot.k = "K" then
        slot.c := slot.c + 1; truth := truth + 1; tgt := truth; cnt := slot.c; b := slot.bit;
T2:     if b then cnt := cnt + ovf; end if;
        if cnt < tgt then touchOK := FALSE; end if;
      else
        if Variant = "checkOutsideStripe" then o := ovf; end if;
T3:     await stripe = "none"; stripe := "toucher";
T4:     await setLock = "none";
        if stash.k = "K" then got := stash; stash := None; else got := None; end if;
T5:     if Variant /= "checkOutsideStripe" then o := ovf; end if;
T6:     slot := [k |-> "K", c |-> got.c + 1,
                 bit |-> Variant /= "noBit" /\ (got.k = "K" \/ o > 0)];
        truth := truth + 1; tgt := truth; stripe := "none";
        if got.c + 1 + o < tgt then touchOK := FALSE; end if;
      end if;
T7:   n := n + 1;
    end while;
end process;

process reader = "reader"
variables r = 0, start = 0, rb = 0, rd = 0, found = FALSE, rc = 0, rbit = FALSE;
begin
R0: while r < Reads do
R1:   start := truth; rb := begun; rd := done;
      if slot.k = "K" then found := TRUE; rc := slot.c; rbit := slot.bit;
      else found := FALSE; end if;
R2:   if ~found then
        if rb /= rd \/ begun /= rb then goto R1;
        elsif ~gate then rc := 0; rbit := TRUE;
        else
R4:       await stripe = "none"; stripe := "reader";
          if slot.k = "K" then rc := slot.c; rbit := slot.bit;
          elsif stash.k = "K" then rc := stash.c; rbit := stash.bit;
          else rc := 0; rbit := TRUE;
          end if;
R5:       stripe := "none";
        end if;
      end if;
R3:   if rbit then rc := rc + ovf; end if;
      if rc < start then readOK := FALSE; end if;
      r := r + 1;
    end while;
end process;

end algorithm; *)
=============================================================================
