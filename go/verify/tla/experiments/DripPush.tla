------------------------------ MODULE DripPush ------------------------------
(***************************************************************************)
(* The drip log's one push per touch (lab/sw-2026-11, arm W8; tfdrip).     *)
(* Relocate.tla's protocol as committed ("hidden": a relocation's copy     *)
(* goes in with word1 busy and is published after the hand-off; an         *)
(* abandoned copy is cleared; evictions and moves of the stripe exclude    *)
(* each other with counts), with word0 carrying the key's drip log as the  *)
(* sequence of the events pushed into it (event 0 before the run, event n  *)
(* the owner's n-th touch). The log's 11-bit bound, its quanta and its     *)
(* valid bit are not modelled: only which events it holds, and how often.  *)
(*                                                                         *)
(* The owner touches K Touches times, one at a time, touch n at tick       *)
(* Tick(n) (Burst: every touch at tick 0, the tick of event 0). A touch:   *)
(* a lookup; word0's CAS by next0, which pushes the event (touch.go,       *)
(* touch: next0's CAS); bump, the count's CAS of word1, which fails if the *)
(* slot changed hands meanwhile and sends the touch back to its lookup     *)
(* (touch.go: !ok, continue); or the miss path, under K's claim stripe,    *)
(* which restores K's history from the stash and counts the event there    *)
(* (cuckoo.go, miss: low = next0(s0, now), or s0's log as it is). A touch  *)
(* that pushed its event and starts over pushes it again only into a       *)
(* history whose last touch is behind its tick (pushed && now <= last: a   *)
(* history from before its push). A mover relocates K from src to dst and  *)
(* hands src to a key N; an evictor stashes K's words (word0 with its log, *)
(* word1) and gives its slot to a key M.                                   *)
(*                                                                         *)
(* Variant:                                                                *)
(*   "w4d"  as W4d's tree: the flag on both paths                          *)
(*   "w4c"  as W4c's tree (0837f9f): the flag on the table path only; the  *)
(*          miss pushes into a restored history whatever its last touch    *)
(*   "w4b"  as W4b's tree (06940e8): no flag; every attempt pushes         *)
(*   "tickOnly"  a wrong mend, for sight: the tick test without the flag   *)
(*          (no push into a history at or after the touch's tick), which   *)
(*          loses pushes                                                   *)
(* Invariants: PushOnce (no copy of K's history, in a slot or the stash,   *)
(* holds an event twice), LogComplete (once every process is done, K's     *)
(* history holds every event exactly once), with Relocate's TouchNotLow,   *)
(* ReadNotLow, OneHome and Conserved (the moves still keep every event).  *)
(* NotMoved, NotEvicted and NotRepushed check that a move, an eviction and *)
(* a second push by a touch that pushed already (into a history behind its *)
(* tick) are reachable.                                                    *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, Sequences, TLC

CONSTANTS Touches, Burst, Reads, Evict, CopyFirst, Variant

Slots == {"src", "dst"}
Order == IF CopyFirst THEN <<"dst", "src">> ELSE <<"src", "dst">>
FlagTable == Variant \in {"w4c", "w4d"}
FlagMiss == Variant = "w4d"
TickOnly == Variant = "tickOnly"
Tick(n) == IF Burst THEN 0 ELSE n
Max(a, b) == IF a > b THEN a ELSE b
Free0 == [k |-> "free", t |-> 0, log |-> <<>>]
Free1 == [k |-> "free", c |-> 0]
Busy == [k |-> "busy", c |-> 0]
NoStash == [k |-> "none", c |-> 0, t |-> 0, log |-> <<>>]
Push(x, now, e) == [x EXCEPT !.t = Max(x.t, now), !.log = Append(x.log, e)]  \* next0: the last touch moves to now if later, the log takes the event

(* --algorithm DripPush
variables
  w0 = [s \in Slots |-> IF s = "src" THEN [k |-> "K", t |-> 0, log |-> <<0>>] ELSE Free0],
  w1 = [s \in Slots |-> IF s = "src" THEN [k |-> "K", c |-> 1] ELSE Free1],
  stash = NoStash,
  begun = 0, done = 0, evicting = 0,
  lock = "none",                   \* K's claim stripe
  truth = 1,                       \* ghost: K's events recorded (one before the run)
  readOK = TRUE, touchOK = TRUE,   \* ghost
  seen = {};                       \* ghost: "moved", "abandoned", "evicted" (reachability)

define
  Match(s) == w0[s].k = "K" /\ w1[s].k = "K"
  Where == {s \in Slots : Match(s)}
  Dup(l) == \E a, b \in 1..Len(l) : a < b /\ l[a] = l[b]
  Logs == {w0[s].log : s \in {x \in Slots : w0[x].k = "K"}} \cup (IF stash.k = "K" THEN {stash.log} ELSE {})
  Final == \A p \in {"owner", "reader", "mover", "evictor"} : pc[p] = "Done"
  Live == IF Where = {} THEN (IF stash.k = "K" THEN stash.log ELSE <<>>)
          ELSE w0[CHOOSE s \in Where : TRUE].log
  Total == IF Where = {} THEN (IF stash.k = "K" THEN stash.c ELSE 0)
           ELSE w1[CHOOSE s \in Where : TRUE].c
  PushOnce == \A l \in Logs : ~Dup(l)
  LogComplete == Final => Len(Live) = Touches + 1 /\ {Live[x] : x \in 1..Len(Live)} = 0..Touches
  ReadNotLow == readOK
  TouchNotLow == touchOK
  OneHome == Cardinality(Where) <= 1
  Conserved == Final => Cardinality(Where) + (IF stash.k = "K" THEN 1 ELSE 0) = 1 /\ Total = truth
  NotMoved == "moved" \notin seen
  NotEvicted == "evicted" \notin seen
  NotRepushed == "repushed" \notin seen
end define;

fair process owner = "owner"
variables i = 0, os = "none", last = Free0, old = Free1, start = 0, b = 0, d = 0, pushed = FALSE;
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
  if b /= d \/ begun /= b then goto o_s1; end if;
o_lock:
  await lock = "none";
  lock := "owner";
o_st:                                         \* miss: restore from the stash (its log pushed, or as it is), or a fresh history
  if stash.k = "K" then
    touchOK := touchOK /\ stash.c + 1 >= start + 1;
    if (FlagMiss /\ pushed /\ Tick(i + 1) <= stash.t) \/ (TickOnly /\ Tick(i + 1) <= stash.t) then
      stash.c := stash.c + 1;                 \* the event is in the log already (W4d)
    else
      stash := [Push(stash, Tick(i + 1), i + 1) EXCEPT !.c = stash.c + 1];
      seen := IF pushed THEN seen \cup {"repushed"} ELSE seen;
    end if;
  else
    touchOK := touchOK /\ 1 >= start + 1;
    stash := [k |-> "K", c |-> 1, t |-> Tick(i + 1), log |-> <<i + 1>>];
  end if;
  truth := truth + 1;
  lock := "none";
  i := i + 1;
  os := "none" || last := Free0 || old := Free1 || b := 0 || d := 0 || pushed := FALSE;
  goto o_start;
o_w0:
  if w0[os].k /= "K" then goto o_s1; else last := w0[os]; end if;
o_cas0:                                       \* next0's CAS, skipped by a touch that pushed into a history at or after its tick
  if (FlagTable /\ pushed /\ Tick(i + 1) <= last.t) \/ (TickOnly /\ Tick(i + 1) <= last.t) then
    pushed := TRUE;
  elsif w0[os] = last then
    w0[os] := Push(last, Tick(i + 1), i + 1) || pushed := TRUE || seen := IF pushed THEN seen \cup {"repushed"} ELSE seen;
  else
    goto o_s1;
  end if;
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
    os := "none" || last := Free0 || old := Free1 || b := 0 || d := 0 || pushed := FALSE;
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
  if rb /= rd \/ begun /= rb then goto r_s1; end if;
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
variables m0 = Free0, m1 = Free1;
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
  if evicting /= 0 then goto v_dn; end if;
v_i1:                                         \* install the copy: word1 busy, word0 (with its log), word1 busy
  if w1["dst"] /= Free1 then goto v_dn; else w1["dst"] := Busy; end if;
v_i0:
  if w0["dst"] /= Free0 then w1["dst"] := Free1; goto v_dn; else w0["dst"] := m0; end if;
v_i2:
  w1["dst"] := Busy;
v_h1:                                         \* the hand-off: src to N, if it still holds m0/m1
  if w1["src"] = m1 then w1["src"] := Busy; else goto v_ab; end if;
v_h0:
  if w0["src"] = m0 then
    w0["src"] := [k |-> "N", t |-> 0, log |-> <<>>];
  else
    w1["src"] := m1;
    goto v_ab;
  end if;
v_h2:
  w1["src"] := [k |-> "N", c |-> 0];
v_pub:
  w1["dst"] := m1;
  seen := seen \cup {"moved"};
  goto v_dn;
v_ab:                                         \* abandoned: the hidden copy is cleared
  seen := seen \cup {"abandoned"};
  w0["dst"] := Free0;
v_ab1:
  w1["dst"] := Free1;
v_dn:
  done := done + 1;
  m0 := Free0 || m1 := Free1;
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
  if begun /= done then goto e_end; end if;
e_lock:                                       \* TryLock K's stripe
  if lock /= "none" then goto e_end; else lock := "evictor"; end if;
e_ev:
  evicting := evicting + 1;
e_chk:
  if begun /= done then
    evicting := evicting - 1;
    lock := "none";
    goto e_end;
  end if;
e_beg:
  begun := begun + 1;
e_i1:
  if w1[vs] /= e1 then goto e_fail; else w1[vs] := Busy; end if;
e_i0:
  if w0[vs] /= e0 then w1[vs] := e1; goto e_fail; else w0[vs] := [k |-> "M", t |-> 0, log |-> <<>>]; end if;
e_i2:
  w1[vs] := [k |-> "M", c |-> 0];
e_st:                                         \* the stash takes both words: the last touch and its log, the register
  stash := [k |-> "K", c |-> e1.c, t |-> e0.t, log |-> e0.log];
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
