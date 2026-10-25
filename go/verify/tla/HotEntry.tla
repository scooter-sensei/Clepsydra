------------------------------ MODULE HotEntry ------------------------------
(***************************************************************************)
(* A hot entry's counts against its writers (go/hot.go: add, lock, unlock, *)
(* reclaim, hotAlloc). The entry is K's, with one event in unit 1, its     *)
(* epoch (the unit of bucket 0); K's slot points to it. Bucket 0's count   *)
(* shares a word (c0) with a closed bit and a generation: a touch in the   *)
(* entry's own unit loads c0, then the owner, then the epoch, and adds by  *)
(* one CAS of c0 if it was open, the owner K and the epoch its unit.       *)
(* Otherwise it locks the entry (version odd, then c0 closed), loads it,   *)
(* adds at its unit (bucket 0, or an older bucket) and stores it; it       *)
(* unlocks by reopening c0 with the next generation, then the version. A   *)
(* touch whose entry is no longer K's goes back to K's slot (bump): a      *)
(* register there (demoted) takes the event; a pointer still (the entry    *)
(* reclaimed) starts afresh with it.                                       *)
(*                                                                         *)
(* Adders: K's touches at unit 1. Writer: "advance", a touch of K at unit  *)
(* 2 (locks, moves bucket 0 to the older buckets); "demote", the same      *)
(* touch handing the counts back to K's slot as a register and freeing the *)
(* entry; "reclaim", the entry reclaimed (K's events past the horizon at   *)
(* the reclaimer's clock, which can be ahead of the adders') and given to  *)
(* K3 with one event at unit 1, the counts K had.                          *)
(*                                                                         *)
(* Variant "fixed": as committed. "noGen": unlock reopens c0 without a new *)
(* generation. "noClose": lock doesn't close c0. "idFirst": the fast path  *)
(* loads the owner and the epoch before c0.                                *)
(* Invariants: KExact (once all are done, K's events per unit, in the      *)
(* entry while K's and in its slot's register, are exactly its touches;    *)
(* not checked with "reclaim", which drops K's history), K3Exact (K3's     *)
(* entry holds exactly K3's event). Reachable: NeverRetried (a fast-path   *)
(* CAS fails), NeverLocked (an adder takes the lock path).                 *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS Variant, Writer, Adders

Units == 1..2
Zero == [u \in Units |-> 0]

(* --algorithm HotEntry
variables
  ver = 0, owner = "K", epoch = 1, cnt = 1, closed = FALSE, gen = 0,
  older = Zero,                 \* counts of units before epoch
  slot = "ptr",                 \* K's slot's word1: "ptr" (points here) or "reg"
  reg = Zero,                   \* K's register, once in the slot
  retried = FALSE, locked = FALSE;

define
  C0 == [n |-> cnt, closed |-> closed, gen |-> gen]
  Count(o, u) == IF owner /= o THEN 0 ELSE IF u = epoch THEN cnt ELSE older[u]
  KTrue == [u \in Units |-> IF u = 1 THEN 1 + Cardinality(Adders)
                            ELSE IF Writer \in {"advance", "demote"} THEN 1 ELSE 0]
  KHas(u) == Count("K", u) + (IF slot = "reg" THEN reg[u] ELSE 0)
  AllDone == \A p \in DOMAIN pc : pc[p] = "Done"
  KExact == AllDone /\ Writer /= "reclaim" => \A u \in Units : KHas(u) = KTrue[u]
  K3Exact == AllDone /\ Writer = "reclaim" => Count("K3", 1) = 1 /\ Count("K3", 2) = 0
  NeverRetried == ~retried
  NeverLocked == ~locked
end define;

macro lock() begin
  await ver % 2 = 0;
  ver := ver + 1;
end macro;

macro unlock() begin
  closed := FALSE;
  if Variant /= "noGen" then gen := gen + 1; end if;
  ver := ver + 1;
end macro;

fair process adder \in Adders
variables lc = C0, lid = "none", lep = 0, lo = Zero, le = 0, ln = 0;
begin
a_load1:                                  \* the fast path: three loads, then the CAS
  if Variant = "idFirst" then lid := owner; else lc := C0; end if;
a_load2:
  if Variant = "idFirst" then lep := epoch; else lid := owner; end if;
a_load3:
  if Variant = "idFirst" then lc := C0; else lep := epoch; end if;
a_check:
  if lc.closed \/ lid /= "K" \/ lep /= 1 then goto a_lock; end if;
a_cas:
  if C0 = lc then cnt := cnt + 1; goto a_out;
  else retried := TRUE; goto a_load1;
  end if;
a_lock:
  lock();
  locked := TRUE;
a_close:
  if Variant /= "noClose" then closed := TRUE; end if;
a_owner:
  if owner /= "K" then goto a_unlockFail; end if;
a_read:                                   \* load(): the entry's state
  lo := older; le := epoch; ln := cnt;
a_store:                                  \* store(): the event at unit 1, c0's count as loaded
  if le = 1 then cnt := ln + 1; else cnt := ln || older := [lo EXCEPT ![1] = @ + 1]; end if;
a_unlock:
  unlock();
  goto a_out;
a_unlockFail:
  unlock();
a_slot:                                   \* bump: back to K's slot
  if slot = "reg" then reg[1] := reg[1] + 1;          \* demoted: count in the register
  else slot := "reg" || reg := [Zero EXCEPT ![1] = 1]; \* the entry is gone: start afresh
  end if;
a_out:
  skip;
end process;

fair process writer = "w"
variables wo = Zero, we = 0, wn = 0;
begin
w_start:
  if Writer = "reclaim" then goto r_lock; end if;
w_lock:                                   \* K's touch at unit 2, by the lock path
  lock();
w_close:
  if Variant /= "noClose" then closed := TRUE; end if;
w_read:
  wo := older; we := epoch; wn := cnt;
w_store:
  if Writer = "demote" then               \* the counts, advanced, back to the slot as a register
    slot := "reg" || reg := [wo EXCEPT ![we] = wn, ![2] = 1];
    owner := "free";
  else
    older := [wo EXCEPT ![we] = wn] || epoch := 2 || cnt := 1;
  end if;
w_unlock:
  unlock();
  goto w_out;
r_lock:                                   \* reclaim: the entry freed
  lock();
r_close:
  if Variant /= "noClose" then closed := TRUE; end if;
r_free:
  owner := "free";
r_unlock:
  unlock();
r_lock2:                                  \* hotAlloc: K3 takes it with one event at unit 1
  lock();
r_close2:
  if Variant /= "noClose" then closed := TRUE; end if;
r_alloc:
  owner := "K3" || epoch := 1 || cnt := 1 || older := Zero;
r_unlock2:
  unlock();
w_out:
  skip;
end process;

end algorithm; *)
=============================================================================
