------------------------------ MODULE StashSet ------------------------------
(***************************************************************************)
(* One stash set (go/stash.go; claims A44). W ways; key K, in the table   *)
(* or not, with its last touch kt; other keys Others; a clock. Every       *)
(* action is atomic: puts and recalls hold the set's lock, and K's own      *)
(* hold K's claim stripe. A put (stashPutAs) scans the ways in order and   *)
(* stops at the first that holds the key's identity (keeping the newer of   *)
(* the two) or is empty or expired; with every way live and another key's, *)
(* the lightest (here: oldest) leaves unless the newcomer is lighter. A     *)
(* recall (tableGet, take) finds the first way of the identity and takes   *)
(* it if live; K's first touch then installs K (fresh if nothing was       *)
(* taken). Variant "merge" adds an install of K with no recall (Merge's    *)
(* install of a key only g holds); "mergefix" the same install recalling  *)
(* K's stash entry first, as Merge does since D26 (here the same          *)
(* step as a first touch). Invariants: NoLivePair (no two live            *)
(* entries of K's identity), NoPair (no two at all: the probe's case).     *)
(***************************************************************************)
EXTENDS Integers, TLC

CONSTANTS W, H, Tmax, Others, Variant

None == [id |-> "none", t |-> 0]

(* --algorithm StashSet
variables set = [i \in 1..W |-> None], kIn = TRUE, kt = 0, now = 0;

define
  Live(e) == e.id # "none" /\ now - e.t <= H
  FirstOf(x) == IF \E i \in 1..W : set[i].id = x
                THEN CHOOSE i \in 1..W : set[i].id = x /\ \A j \in 1..(i-1) : set[j].id # x
                ELSE 0
  Usable(x, i) == set[i].id = x \/ ~Live(set[i])
  FirstUsable(x) == IF \E i \in 1..W : Usable(x, i)
                    THEN CHOOSE i \in 1..W : Usable(x, i) /\ \A j \in 1..(i-1) : ~Usable(x, j)
                    ELSE 0
  Lightest == CHOOSE j \in 1..W : \A k \in 1..W : set[j].t <= set[k].t
  Put(x, t) == LET i == FirstUsable(x) IN
               IF i # 0
               THEN IF set[i].id = x /\ set[i].t >= t THEN set
                    ELSE [set EXCEPT ![i] = [id |-> x, t |-> t]]
               ELSE IF set[Lightest].t > t THEN set
                    ELSE [set EXCEPT ![Lightest] = [id |-> x, t |-> t]]
  Take(x) == LET i == FirstOf(x) IN
             IF i # 0 /\ Live(set[i]) THEN [set EXCEPT ![i] = None] ELSE set
  NoLivePair == ~\E i, j \in 1..W : i # j /\ set[i].id = "K" /\ set[j].id = "K" /\ Live(set[i]) /\ Live(set[j])
  NoPair == ~\E i, j \in 1..W : i # j /\ set[i].id = "K" /\ set[j].id = "K"
end define;

process p = "p"
begin
L: while TRUE do
  either
    await now < Tmax; now := now + 1;
  or
    await kIn; kt := now;
  or
    await kIn; set := Put("K", kt); kIn := FALSE;
  or
    await ~kIn; set := Take("K"); kIn := TRUE; kt := now;
  or
    await ~kIn /\ Variant = "merge"; kIn := TRUE; kt := now;
  or
    await ~kIn /\ Variant = "mergefix"; set := Take("K"); kIn := TRUE; kt := now;
  or
    with o \in Others do set := Put(o, now); end with;
  or
    with o \in Others do set := Take(o); end with;
  end either;
end while;
end process;

end algorithm; *)
=============================================================================
