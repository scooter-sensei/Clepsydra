------------------------------ MODULE HotCells ------------------------------
(***************************************************************************)
(* HotEntry with cells (experiment E23, go/hot.go with cells.py): K's      *)
(* entry, one event in unit 1, may hold a block of cells, each a word like *)
(* c0 (count, closed bit, generation). An adder reads the entry's block;   *)
(* with one, it loads a cell's word, the owner, the epoch and the block    *)
(* again, and adds by one CAS of the cell if it was open, the owner K, the *)
(* epoch its unit and the block still the entry's. Without one it adds to  *)
(* c0 as in HotEntry; a CAS of c0 that fails may make it "contended": it   *)
(* takes the lock path, which gives the entry the block (cells zeroed and  *)
(* closed) if it is free and the entry still at unit 1. A writer's lock    *)
(* closes c0, then each of the entry's cells, one step each; load sums c0  *)
(* and the cells; store folds them into c0 and zeroes the cells; unlock    *)
(* reopens the cells (if the entry still holds the block) and c0 with the  *)
(* next generation. The block goes back (detach) when the entry is freed   *)
(* or its unit advances; its cells stay closed until the next attach.      *)
(*                                                                         *)
(* Adders: K's touches at unit 1. Writer: "advance" (K's touch at unit 2,  *)
(* by the lock path: the block goes back), "reclaim" (the entry freed,     *)
(* its block taken back, then the entry given to K3 with one event at unit *)
(* 1), or "none". J: another entry that may take the block once it is free *)
(* (zeroed and closed under J's lock, then reopened), add one event of J   *)
(* to a cell, and fold its cells. Start: the block the entry's ("E") or    *)
(* free ("free": a contended adder attaches it).                           *)
(*                                                                         *)
(* Variant "fixed": as in cells.py. "noCellClose": lock closes c0 but not  *)
(* the cells. "noCellGen": cells reopen without a new generation.          *)
(* "noBlkRecheck": the cell add doesn't load the block again. "noZero":    *)
(* attach doesn't zero the cells. "openAttach": attach leaves the cells    *)
(* open (unlock reopens them anyway).                                      *)
(* Invariants: KExact (once all are done, K's events per unit, in the      *)
(* entry and its cells while it holds the block, are its touches; not with *)
(* "reclaim", which drops K's history), K3Exact, JExact (J's folded count  *)
(* is its one event).                                                      *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS Variant, Writer, Adders, Late, Cells, Start   \* Late \subseteq Adders: touches at unit 2

Units == 1..2
Zero == [u \in Units |-> 0]
CellIds == 1..Cells
Closed0 == [n |-> 0, closed |-> TRUE, gen |-> 0]
Open0 == [n |-> 0, closed |-> FALSE, gen |-> 0]

(* --algorithm HotCells
variables
  ver = 0, owner = "K", epoch = 1, cnt = 1, closed = FALSE, gen = 0,
  older = Zero,
  blk = Start,                  \* who holds the block: "E" (the entry), "J" or "free"
  cell = [x \in CellIds |-> IF Start = "E" THEN Open0 ELSE Closed0],
  jdone = FALSE, jcnt = 0;

define
  C0 == [n |-> cnt, closed |-> closed, gen |-> gen]
  CellSum == LET S[i \in 0..Cells] == IF i = 0 THEN 0 ELSE S[i-1] + cell[i].n IN S[Cells]
  ECells == IF blk = "E" THEN CellSum ELSE 0
  Count(o, u) == IF owner /= o THEN 0 ELSE IF u = epoch THEN cnt + ECells ELSE older[u]
  KTrue == [u \in Units |-> IF u = 1 THEN 1 + Cardinality(Adders \ Late)
                            ELSE Cardinality(Late) + (IF Writer = "advance" THEN 1 ELSE 0)]
  Unit(p) == IF p \in Late THEN 2 ELSE 1
  AllDone == \A p \in DOMAIN pc : pc[p] = "Done"
  KExact == AllDone /\ Writer /= "reclaim" => \A u \in Units : Count("K", u) = KTrue[u]
  K3Exact == AllDone /\ Writer = "reclaim" => Count("K3", 1) = 1 /\ Count("K3", 2) = 0
  JExact == AllDone /\ jdone => jcnt = 1
  Zeroed(c) == [x \in CellIds |-> IF blk = "E" THEN [c[x] EXCEPT !.n = 0] ELSE c[x]]
end define;

macro unlock() begin
  if blk = "E" then
    cell := [x \in CellIds |-> [cell[x] EXCEPT !.closed = FALSE,
                                  !.gen = IF Variant = "noCellGen" THEN @ ELSE @ + 1]];
  end if;
  closed := FALSE || gen := gen + 1;
  ver := ver + 1;
end macro;

fair process adder \in Adders
variables lb = "free", lj = 1, lcell = Open0, lc = Open0,
          lid = "none", lep = 0, lb2 = "free", contended = FALSE, kc = 0, ln = 0, lo = Zero, le = 0;
begin
a_blk:
  lb := blk;
  if lb /= "E" then goto a_load1; end if;
c_load:                                   \* a cell: its word, the owner, the epoch, the block
  with x \in CellIds do lj := x; lcell := cell[x]; end with;
c_id:
  lid := owner;
c_ep:
  lep := epoch;
c_blk:
  if Variant /= "noBlkRecheck" then lb2 := blk; else lb2 := "E"; end if;
c_check:
  if lcell.closed \/ lid /= "K" \/ lep /= Unit(self) \/ lb2 /= "E" then goto a_lock; end if;
c_cas:
  if cell[lj] = lcell then cell[lj].n := cell[lj].n + 1; goto a_out;
  else goto a_blk;
  end if;
a_load1:                                  \* c0, as in HotEntry
  lc := C0;
a_load2:
  lid := owner;
a_load3:
  lep := epoch;
a_check:
  if lc.closed \/ lid /= "K" \/ lep /= Unit(self) then goto a_lock; end if;
a_cas:
  if C0 = lc then cnt := cnt + 1; goto a_out;
  else
    either goto a_blk;                    \* retry
    or contended := TRUE; goto a_lock;    \* hotCellAfter failures
    end either;
  end if;
a_lock:
  await ver % 2 = 0;
  ver := ver + 1;
a_close:
  closed := TRUE; kc := 1;
a_closeCells:                             \* one cell a step
  while blk = "E" /\ kc <= Cells /\ Variant /= "noCellClose" do
    cell[kc].closed := TRUE; kc := kc + 1;
  end while;
a_owner:
  if owner /= "K" then goto a_unlock; end if;
a_attach:
  if contended /\ epoch = Unit(self) /\ blk = "free" then
    blk := "E" ||
    cell := [x \in CellIds |-> [cell[x] EXCEPT !.n = IF Variant = "noZero" THEN @ ELSE 0,
                                               !.closed = (Variant /= "openAttach")]];
  end if;
a_read:
  le := epoch; ln := cnt; lo := older;
a_store:                                  \* at the entry's unit, c0 alone; else load, add, store
  if le = Unit(self) then cnt := ln + 1;
  elsif le > Unit(self) then              \* an older unit: to its bucket, the cells folded
    older := [lo EXCEPT ![1] = @ + 1] || cnt := ln + ECells || cell := Zeroed(cell);
  else                                    \* a newer unit: advance, the cells folded
    older := [lo EXCEPT ![le] = ln + ECells] || epoch := Unit(self) || cnt := 1 || cell := Zeroed(cell);
  end if;
a_detach:
  if le < Unit(self) /\ blk = "E" then blk := "free"; end if;
a_unlock:
  unlock();
a_out:
  skip;
end process;

fair process writer = "w"
variables wn = 0, wk = 0, wadv = FALSE;
begin
w_start:
  if Writer = "none" then goto w_out; end if;
w_lock:
  await ver % 2 = 0;
  ver := ver + 1;
w_close:
  closed := TRUE; wk := 1;
w_closeCells:
  while blk = "E" /\ wk <= Cells /\ Variant /= "noCellClose" do
    cell[wk].closed := TRUE; wk := wk + 1;
  end while;
w_read:                                   \* load: c0 and the cells
  wn := cnt + ECells; wadv := epoch < 2;
w_store:
  if Writer = "advance" then
    if wadv then                          \* store (the cells folded, zeroed), then detach
      older := [older EXCEPT ![epoch] = wn] || epoch := 2 || cnt := 1 || cell := Zeroed(cell);
    else cnt := cnt + 1;                  \* a late adder advanced it already
    end if;
  else
    owner := "free";                      \* reclaim: freed, its counts dropped
  end if;
w_detach:
  if blk = "E" /\ (Writer = "reclaim" \/ wadv) then blk := "free"; end if;
w_unlock:
  unlock();
  if Writer = "advance" then goto w_out; end if;
r_lock2:                                  \* hotAlloc: K3 takes the entry with one event at unit 1
  await ver % 2 = 0;
  ver := ver + 1;
r_close2:
  closed := TRUE;
r_alloc:
  owner := "K3" || epoch := 1 || cnt := 1 || older := Zero;
r_unlock2:
  unlock();
w_out:
  skip;
end process;

fair process jp = "J"
variables jc = Open0;
begin
j_wait:
  either await blk = "free";
  or goto j_out;
  end either;
j_attach:                                 \* under J's lock: zeroed and closed
  if blk = "free" then
    blk := "J" ||
    cell := [x \in CellIds |-> [cell[x] EXCEPT !.n = IF Variant = "noZero" THEN @ ELSE 0, !.closed = TRUE]];
  else goto j_out;
  end if;
j_open:                                   \* J's unlock: reopened with the next generation
  cell := [x \in CellIds |-> [cell[x] EXCEPT !.closed = FALSE, !.gen = IF Variant = "noCellGen" THEN @ ELSE @ + 1]];
j_load:
  jc := cell[1];
j_cas:
  if ~jc.closed /\ cell[1] = jc then cell[1].n := cell[1].n + 1; else goto j_load; end if;
j_fold:                                   \* J's lock: closed, summed
  cell := [x \in CellIds |-> [cell[x] EXCEPT !.closed = TRUE]];
  jcnt := CellSum; jdone := TRUE;
j_out:
  skip;
end process;

end algorithm; *)
=============================================================================
