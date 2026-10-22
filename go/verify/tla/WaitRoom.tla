------------------------------ MODULE WaitRoom ------------------------------
(***************************************************************************)
(* Background's wait room (go/filter.go) against lookups and first        *)
(* touches, for one key K and its two candidate slots P and A.            *)
(*                                                                         *)
(* K starts waiting in the wait room with one event; P and A hold another *)
(* key X, which can expire at any time (process time), freeing its slot.  *)
(*   - placer: the Background placer (placeNext, placeRoomSet,            *)
(*     placeRoomKey): scans K's set, clears the set's wait bit if nothing *)
(*     waits, else commits K to a free slot: room entry busy, the move    *)
(*     count's begun, install (word1 busy, word0, word1), entry removed or *)
(*     restored, done.                                                    *)
(*   - toucher: touch: a hit, or under K's stripe lookupStable, recall    *)
(*     (roomGet with take), miss (claim without search: a free slot, else *)
(*     back to the room).                                                 *)
(*   - reader: read: the lock-free path (scan, wait-bit gate, move        *)
(*     counts), then the locked one.                                      *)
(* A find reads word0 then word1; an install writes word1 busy, word0,    *)
(* then word1, so every interleaving of those is explored.                *)
(*                                                                         *)
(* Variant: "fixed" is the protocol as committed. Each other variant      *)
(* undoes one guard, and TLC must find an invariant broken:              *)
(*   noTouchMove   a touch that takes K out of the room does not count   *)
(*                 as a move of K's stripe (the code before the fix)     *)
(*   lateBegin     it counts, but begins after K leaves the room         *)
(*   earlyDone     it counts, but ends before K is placed                *)
(*   noPlacerMove  the placer's commit does not count as a move          *)
(*   noBusyWait    a recall does not wait out an entry being committed   *)
(***************************************************************************)
EXTENDS Integers, Sequences, TLC

CONSTANTS Touchers, Rounds, Variant

TouchMove == Variant /= "noTouchMove"
MoveFirst == Variant /= "lateBegin"
DoneLast == Variant /= "earlyDone"
PlacerMove == Variant /= "noPlacerMove"
BusyWait == Variant /= "noBusyWait"

Slots == {"P", "A"}
None == "none"
Free(v, s, ex) == v = None \/ (v = "X" /\ ex[s])

(* --algorithm WaitRoom
variables
  w0 = [s \in Slots |-> "X"],          \* word0's owner
  w1 = [s \in Slots |-> "X"],          \* word1's owner, or "busy"
  cnt = [s \in Slots |-> 0],           \* K's register in that slot
  expired = [s \in Slots |-> FALSE],   \* X has aged out of that slot
  inRoom = TRUE, roomBusy = FALSE, roomCnt = 1,
  waitBit = TRUE,                      \* the set's wait bit
  roomLock = None, stripe = None,      \* the set's lock, K's claim stripe
  begun = 0, done = 0,                 \* K's stripe's move counts
  touchesDone = 0,                     \* touches whose event is counted
  readFloor = 0, readResult = -1,      \* K's count when the read began; its answer
  fresh = FALSE,                       \* a touch found K nowhere: its history lost
  loc = [p \in Touchers \cup {"reader"} |-> None];

define
  Settled(b, d) == b = d /\ begun = b
  NoDup == ~(w0["P"] = "K" /\ w0["A"] = "K")
  ReadNotLow == readResult = -1 \/ readResult >= readFloor
  NoFresh == ~fresh
  Owns(s) == w0[s] = "K" /\ w1[s] = "K"
  AllDone == \A p \in Touchers \cup {"placer", "reader", "time"} : pc[p] = "Done"
  Conserved == AllDone =>
      \/ inRoom /\ ~Owns("P") /\ ~Owns("A") /\ roomCnt = 1 + touchesDone
      \/ ~inRoom /\ \E s \in Slots : Owns(s) /\ ~Owns(IF s = "P" THEN "A" ELSE "P")
                                    /\ cnt[s] = 1 + touchesDone
end define;

\* locate: find in P, then in A; loc[self] is the slot or None.
procedure locate()
variables ls = "P", lw0 = None;
begin
l1: lw0 := w0[ls];
l2: if lw0 = "K" /\ w1[ls] = "K" then
      loc[self] := ls;
      return;
    elsif ls = "P" then
      ls := "A";
      goto l1;
    else
      loc[self] := None;
      return;
    end if;
end procedure;

\* lookupStable: a miss counts only if no move of K's stripe began or ended around the scan.
procedure lookupStable()
variables sb = 0, sd = 0;
begin
s1: sb := begun || sd := done;
s2: call locate();
s3: if loc[self] /= None then
      if w0[loc[self]] = "K" /\ w1[loc[self]] = "K" then return; else goto s1; end if;
    elsif Settled(sb, sd) then
      return;
    else
      goto s1;
    end if;
end procedure;

fair process placer = "placer"
variables round = 0, ptake = FALSE, ps = "P", pw0 = None, pw1 = None, pk = 0, pok = FALSE;
begin
p_loop:
  while round < Rounds do
    round := round + 1;
    if ~waitBit then goto p_loop; end if;    \* placeNext: no set waits
p_lock:
    await roomLock = None; roomLock := "placer";
p_scan:                                     \* placeRoomSet
    if inRoom then ptake := TRUE; else ptake := FALSE; waitBit := FALSE; end if;
    roomLock := None;
p_claim:
    if ~ptake then goto p_loop; else ps := "P"; end if;
p_c1:                                       \* claimAt: a free slot (the search and eviction aren't modeled)
    pw0 := w0[ps];
    if Free(pw0, ps, expired) then goto p_put;
    elsif ps = "P" then ps := "A"; goto p_c1;
    else goto p_loop;
    end if;
p_put:
    pw1 := w1[ps];                          \* putNew loads word1
p_pl:
    await roomLock = None; roomLock := "placer";
p_pf:                                       \* put: the entry goes busy, unless a touch took it
    if inRoom /\ ~roomBusy then roomBusy := TRUE; pk := roomCnt; pok := TRUE;
    else pok := FALSE;
    end if;
    roomLock := None;
p_pb:
    if ~pok then goto p_loop; elsif PlacerMove then begun := begun + 1; end if;
p_i1:                                       \* install
    if pw1 /= "busy" /\ w1[ps] = pw1 then w1[ps] := "busy"; else pok := FALSE; goto p_after; end if;
p_i2:
    if w0[ps] = pw0 then w0[ps] := "K"; else w1[ps] := pw1; pok := FALSE; goto p_after; end if;
p_i3:
    w1[ps] := "K"; cnt[ps] := pk;
p_after:
    await roomLock = None; roomLock := "placer";
p_a2:
    inRoom := inRoom /\ ~pok; roomBusy := FALSE; roomLock := None;
p_d:
    if PlacerMove then done := done + 1; end if;
  end while;
end process;

fair process toucher \in Touchers
variables b = 0, d = 0, tk = 0, tsrc = "none", retry = FALSE, moved = FALSE, ts = "P", tw0 = None, tw1 = None;
begin
t_start:
  call locate();
t_route:
  if loc[self] /= None then goto t_hit; end if;
t_lock:
  await stripe = None; stripe := self;
t_bd:
  b := begun || d := done;
t_ls:
  call lookupStable();
t_ls2:
  if loc[self] /= None then stripe := None; goto t_hit; else retry := FALSE; end if;  \* another touch got there first
t_rec:                                      \* recall: the room only if the set's bit is set
  if ~waitBit then tsrc := "none"; goto t_after; end if;
t_rgl:
  await roomLock = None; roomLock := self;
t_rg:
  if inRoom /\ roomBusy /\ BusyWait then
    roomLock := None; retry := TRUE; goto t_rgl;
  elsif inRoom then
    tk := roomCnt;
    if TouchMove /\ MoveFirst then begun := begun + 1; moved := TRUE; end if;
  else
    roomLock := None;
    tsrc := IF retry THEN "retry" ELSE "none";
    goto t_after;
  end if;
t_take:
  inRoom := FALSE; roomLock := None; tsrc := "room";
t_take2:
  if TouchMove /\ ~MoveFirst then begun := begun + 1; moved := TRUE; end if;
t_after:
  if tsrc = "retry" then stripe := None; goto t_start;
  elsif tsrc = "none" /\ ~Settled(b, d) then stripe := None; goto t_start;
  elsif tsrc = "none" then fresh := TRUE; stripe := None; goto t_end;
  else ts := "P";
  end if;
t_c0:
  if moved /\ ~DoneLast then done := done + 1; moved := FALSE; end if;
t_c1:                                       \* miss -> claim (Background: free slot only)
  tw0 := w0[ts];
  if Free(tw0, ts, expired) then goto t_put;
  elsif ts = "P" then ts := "A"; goto t_c1;
  else goto t_wait;
  end if;
t_put:
  tw1 := w1[ts];
t_i1:
  if tw1 /= "busy" /\ w1[ts] = tw1 then w1[ts] := "busy"; else goto t_back; end if;
t_i2:
  if w0[ts] = tw0 then w0[ts] := "K"; else w1[ts] := tw1; goto t_back; end if;
t_i3:
  w1[ts] := "K"; cnt[ts] := tk + 1; touchesDone := touchesDone + 1;
  goto t_moved;
t_back:                                     \* lost the slot: putBack, then the touch retries
  await roomLock = None; roomLock := self;
t_back2:
  inRoom := TRUE; roomCnt := tk; waitBit := TRUE; roomLock := None;
t_back3:
  if moved then done := done + 1; moved := FALSE; end if;
  stripe := None;
  goto t_start;
t_wait:                                     \* no free slot: stashWait, back to the room with the touch
  await roomLock = None; roomLock := self;
t_wait2:
  inRoom := TRUE; roomCnt := tk + 1; waitBit := TRUE; roomLock := None;
  touchesDone := touchesDone + 1;
t_moved:
  if moved then done := done + 1; moved := FALSE; end if;
  stripe := None;
  goto t_end;
t_hit:                                      \* hit: bump the register unless the slot changed hands
  if w0[loc[self]] = "K" /\ w1[loc[self]] = "K" then
    cnt[loc[self]] := cnt[loc[self]] + 1; touchesDone := touchesDone + 1;
  else
    goto t_start;
  end if;
t_end:
  skip;
end process;

fair process reader = "reader"
variables rb = 0, rd = 0, may = FALSE, rsrc = "none", rcnt = 0, rretry = FALSE;
begin
r_start:
  readFloor := 1 + touchesDone;
r_bd:                                       \* the lock-free path
  rb := begun || rd := done;
r_loc:
  call locate();
r_found:
  if loc["reader"] /= None then
    if w0[loc["reader"]] = "K" /\ w1[loc["reader"]] = "K" then readResult := cnt[loc["reader"]]; goto r_end; end if;
  end if;
r_may:
  may := waitBit;                           \* stashMay (no stash here: the wait bit)
r_chk:
  if loc["reader"] = None /\ rb = rd /\ begun = rb then
    if ~may then readResult := 0; goto r_end; end if;
  else
    goto r_bd;
  end if;
r_lock:                                     \* the locked path
  await stripe = None; stripe := "reader";
r_lbd:
  rb := begun || rd := done;
r_lls:
  call lookupStable();
r_lrec:
  rretry := FALSE;
r_lrec2:
  if ~waitBit then rsrc := "none"; goto r_lafter; end if;
r_rgl:
  await roomLock = None; roomLock := "reader";
r_rg:
  if inRoom /\ roomBusy /\ BusyWait then roomLock := None; rretry := TRUE; goto r_rgl;
  elsif inRoom then rsrc := "room"; rcnt := roomCnt; roomLock := None;
  else rsrc := IF rretry THEN "retry" ELSE "none"; roomLock := None;
  end if;
r_lafter:
  if rsrc = "retry" then goto r_lbd;
  elsif loc["reader"] /= None then readResult := cnt[loc["reader"]];
  elsif rsrc = "none" /\ ~Settled(rb, rd) then goto r_lbd;
  elsif rsrc = "none" then readResult := 0;
  else readResult := rcnt;
  end if;
r_unlock:
  stripe := None;
r_end:
  skip;
end process;

fair process time = "time"
begin
tm:
  while \E s \in Slots : ~expired[s] do
    either
      with s \in {x \in Slots : ~expired[x]} do expired[s] := TRUE; end with;
    or
      goto tm_end;
    end either;
  end while;
tm_end:
  skip;
end process;

end algorithm; *)
=============================================================================
