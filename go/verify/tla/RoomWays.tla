------------------------------ MODULE RoomWays ------------------------------
(***************************************************************************)
(* One set of Background's wait room (go/filter.go: stashWait, roomGet,   *)
(* placeRoomKey's put), shared by two waiting keys. L waits in way 1; its *)
(* last touch is more than a horizon before K's clock, so to K its entry  *)
(* looks expired. The placer commits L: under the set's lock it marks L's *)
(* entry busy and keeps a pointer to its way, then without the lock       *)
(* installs L in the table (which may fail), then under the lock again    *)
(* removes the way, or, if the install failed, puts L's word1 back in it. *)
(* Meanwhile K's first touch finds its lines full and waits (stashWait:   *)
(* the first way that is free, K's own, or expired), and K's next touch   *)
(* takes K out of the room (roomGet), or, finding nothing, starts K       *)
(* afresh.                                                                 *)
(*                                                                         *)
(* Variant: "fixed" is the protocol as committed (claims A24): stashWait  *)
(* never takes a busy way. "old" takes an expired way busy or not.        *)
(* Ways: the set's ways (the code has 4; one or two show everything).     *)
(* Invariants: KKept (K's next touch finds K's event: in the room, or in  *)
(* the table if a touch placed it), KOwn (a room entry with K's id holds  *)
(* K's register, not another key's), LOnce (L never in the room and the   *)
(* table both once the placer is done).                                   *)
(***************************************************************************)
EXTENDS Integers, TLC

CONSTANTS Variant, Ways

W == 1..Ways
Empty == [k |-> "none", busy |-> FALSE, reg |-> "none"]

(* --algorithm RoomWays
variables
  way = [i \in W |-> IF i = 1 THEN [k |-> "L", busy |-> FALSE, reg |-> "L"] ELSE Empty],
  lock = "none",
  lInTable = FALSE,
  kFound = "unknown",                  \* what K's next touch found
  kOwn = TRUE;                         \* no K entry ever held another key's register

define
  KKept == kFound /= "fresh"
  KOwn == kOwn /\ \A i \in W : way[i].k = "K" => way[i].reg \in {"K", "busy"}
  LOnce == pc["placer"] = "Done" => ~(lInTable /\ \E i \in W : way[i].k = "L")
end define;

fair process placer = "placer"
variables c = 0, ok = FALSE;
begin
p_lock:
  await lock = "none";
  lock := "placer";
p_find:                                   \* L's entry, waiting and not busy: mark it busy, keep its way
  if \E i \in W : way[i].k = "L" /\ ~way[i].busy then
    with i \in {j \in W : way[j].k = "L" /\ ~way[j].busy} do
      c := i;
      way[i].busy := TRUE || way[i].reg := "busy";
    end with;
    lock := "none";
  else
    lock := "none";
    goto p_end;
  end if;
p_install:                                \* without the lock: the install succeeds, or another writer got the slot
  either ok := TRUE; lInTable := TRUE; or ok := FALSE; end either;
p_relock:
  await lock = "none";
  lock := "placer";
p_after:                                  \* remove the way, or put L's word1 back in it
  if ok then
    way[c] := Empty;
  else
    kOwn := kOwn /\ way[c].k /= "K";
    way[c].busy := FALSE || way[c].reg := "L";
  end if;
  lock := "none";
p_end:
  skip;
end process;

fair process k = "k"
begin
k_lock:
  await lock = "none";
  lock := "k";
k_wait:                                   \* stashWait: the first way that is free, K's own, or expired (L's)
  if \E i \in W : (Variant = "old" \/ ~way[i].busy) /\ way[i].k \in {"none", "K", "L"} then
    with i \in {j \in W : (Variant = "old" \/ ~way[j].busy) /\ way[j].k \in {"none", "K", "L"}
                          /\ \A m \in W : m < j => ~((Variant = "old" \/ ~way[m].busy) /\ way[m].k \in {"none", "K", "L"})} do
      way[i] := [k |-> "K", busy |-> FALSE, reg |-> "K"];
    end with;
    lock := "none";
  else
    lock := "none";                       \* the set is full: K searches inline (not modeled)
    kFound := "table";
    goto k_end;
  end if;
k_next:                                   \* K's next touch: roomGet with take
  await lock = "none";
  lock := "k";
k_get:
  if \E i \in W : way[i].k = "K" then
    with i \in {j \in W : way[j].k = "K"} do
      kOwn := kOwn /\ way[i].reg = "K";
      way[i] := Empty;
    end with;
    kFound := "room";
  else
    kFound := "fresh";
  end if;
  lock := "none";
k_end:
  skip;
end process;

end algorithm; *)
=============================================================================
