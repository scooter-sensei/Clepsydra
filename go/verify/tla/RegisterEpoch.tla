---------------------------- MODULE RegisterEpoch ----------------------------
(***************************************************************************)
(* A slot's two words (go/filter.go, go/register.go): word0 holds the key's*)
(* last touch, word1 its register, which keeps only 4 bits of its unit and *)
(* is read against word0's unit (regEpoch: the unit nearest the reference  *)
(* with those bits). Each event is held as its age in units from the       *)
(* register's unit; a register read with a wrong unit places every event  *)
(* wrongly by the difference. Time here is in units.                       *)
(*                                                                         *)
(* Touchers touch one key at clocks drawn from Clocks; a reader reads it.  *)
(* Variant:                                                                *)
(*   "old"       before the fix: word0 moves first (CAS, if later), then   *)
(*               the register is read against the toucher's own earlier    *)
(*               read of word0 (bump's caller's last), advanced and CASed; *)
(*               a gap past the horizon resets it. The reader reads word0  *)
(*               and word1 once.                                           *)
(*   "fixed"     as committed off amd64 (and with TF_NOCAS2=1): a touch 8  *)
(*               or more units past the register's unit holds word1 busy,  *)
(*               moves word0, then publishes the register (realign); a     *)
(*               touch that finds word1 busy looks again; others move      *)
(*               word0 first and read the register against word0 as it is  *)
(*               when they count (bump's ref). The reader reads word0,     *)
(*               word1, then word0 again.                                  *)
(*   "noHold"    fixed without realign (every touch lock-free)             *)
(*   "ownRef"    fixed, but bump reads against the caller's last           *)
(*   "noWait"    fixed, but a touch that finds word1 busy routes as if not *)
(*   "noReread"  fixed, but the reader does not read word0 again           *)
(*   "cas2"      experiment E21: realign moves both words by one CAS from  *)
(*               those it read (no hold), and bump's CAS compares word0    *)
(*               too                                                       *)
(*   "cas2NoReread" cas2, but the reader does not read word0 again         *)
(*   "cas2r"     as committed on amd64 (claims A33, experiment E22):       *)
(*               realign as cas2, bump's CAS of word1 alone                *)
(* Invariants: NotAged (no event placed older than it happened), Kept (no  *)
(* event within the horizon of the newest clock lost), ReaderRight (a     *)
(* reader that accepts a pair reads the register's unit).                  *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS Touchers, Clocks, Variant, H

Start == 100                       \* the key's first event, and its unit
Resolve(bits, ref) == LET d == (ref - bits) % 16 IN IF d > 7 THEN ref - d + 16 ELSE ref - d
Hold == Variant \notin {"old", "noHold"}
CAS2 == {"cas2", "cas2NoReread", "cas2r"}    \* realign by one CAS of both words
CountCAS2 == {"cas2", "cas2NoReread"}        \* bump's CAS compares word0 too

(* --algorithm RegisterEpoch
variables
  ts = Start,                      \* word0: the last touch (a unit)
  bits = Start % 16,               \* word1: the register's unit, 4 bits
  age = [k \in {"first"} |-> 0],   \* word1: each event's age from the register's unit
  regE = Start,                    \* ghost: the unit the register's writer meant
  ver = 0,                         \* word1's value changes (a CAS compares it)
  busy = FALSE,                    \* word1 held (realign)
  truth = [k \in {"first"} |-> Start], \* ghost: when each event happened
  newest = Start,                  \* ghost: the newest clock any touch used
  readOK = TRUE;                   \* ghost: no reader accepted a wrong unit

define
  Placed(k) == regE - age[k]
  NotAged == busy \/ \A k \in DOMAIN age : Placed(k) >= truth[k]
  Kept == busy \/ \A k \in DOMAIN truth : truth[k] + H < newest \/ k \in DOMAIN age
  ReaderRight == readOK
end define;

fair process toucher \in Touchers
variables c = 0, last = 0, cur = 0, rb = 0, rv = 0, hv = 0, rage = <<>>, e = 0;
begin
t0:
  with x \in Clocks do c := x; end with;
  newest := IF c > newest THEN c ELSE newest;
t_load:                                      \* word0
  last := ts;
t_route:                                     \* farAhead: word1, read against word0
  if Hold /\ c > last /\ busy /\ Variant /= "noWait" then
    goto t_load;                             \* held: its unit can't be read; look again
  elsif Hold /\ c > last /\ ~busy /\ c - Resolve(bits, last) >= 8 then
    hv := ver || rb := bits || rage := age;
    goto t_hold;
  end if;
t_cas0:                                      \* word0 moves first, if later
  if c > last /\ ts /= last then
    goto t_load;
  elsif c > last then
    ts := c;
  end if;
t_w1:                                        \* bump: word1, then word0
  if busy then
    goto t_load;
  else
    rb := bits || rv := ver || rage := age;
  end if;
t_w0:
  cur := ts;
t_bump:                                      \* the register read against a reference, advanced, CASed
  if busy \/ ver /= rv \/ (Variant \in CountCAS2 /\ ts /= cur) then
    goto t_w1;
  else
    e := Resolve(rb, IF Variant \in {"old", "ownRef"} THEN last ELSE cur);
    if Variant = "old" /\ c - last > H then  \* stale: reset (lock-free, before the fix)
      age := [k \in {self} |-> 0];
      regE := c;
      bits := c % 16;
    elsif c > e then                         \* advance to c, the event in the newest bucket
      age := [k \in DOMAIN rage \cup {self} |-> IF k = self THEN 0 ELSE rage[k] + (c - e)];
      regE := c;
      bits := c % 16;
    else                                     \* a lagging clock: count at e
      age := [k \in DOMAIN rage \cup {self} |-> IF k = self THEN 0 ELSE rage[k]];
      regE := e;
      bits := e % 16;
    end if;
    ver := ver + 1;
    truth := [k \in DOMAIN truth \cup {self} |-> IF k = self THEN c ELSE truth[k]];
    goto t_done;
  end if;
t_hold:                                      \* realign: CAS word1 (as routed) to busy
  if Variant \in CAS2 then                   \* both words by one CAS from those read
    if busy \/ ver /= hv \/ ts /= last then
      goto t_load;
    else
      e := Resolve(rb, last);
      ts := c;
      if c - last > H then
        age := [k \in {self} |-> 0];
      else
        age := [k \in DOMAIN rage \cup {self} |-> IF k = self THEN 0 ELSE rage[k] + (c - e)];
      end if;
      regE := c;
      bits := c % 16;
      ver := ver + 1;
      truth := [k \in DOMAIN truth \cup {self} |-> IF k = self THEN c ELSE truth[k]];
      goto t_done;
    end if;
  elsif busy \/ ver /= hv then
    goto t_load;
  else
    busy := TRUE || rb := bits || rage := age;
  end if;
t_hold0:                                     \* CAS word0, or put word1 back
  if ts /= last then
    busy := FALSE;
    goto t_load;
  else
    ts := c;
  end if;
t_hold1:                                     \* publish the register at c
  e := Resolve(rb, last);
  if c - last > H then
    age := [k \in {self} |-> 0];
  else
    age := [k \in DOMAIN rage \cup {self} |-> IF k = self THEN 0 ELSE rage[k] + (c - e)];
  end if;
  regE := c;
  bits := c % 16;
  ver := ver + 1;
  busy := FALSE;
  truth := [k \in DOMAIN truth \cup {self} |-> IF k = self THEN c ELSE truth[k]];
t_done:
  skip;
end process;

fair process reader = "reader"
variables a = 0, b = 0, g = 0;
begin
r_w0:
  a := ts;
r_w1:
  if busy then
    goto r_w0;
  else
    b := bits || g := regE;
  end if;
r_again:
  if Variant \notin {"old", "noReread", "cas2NoReread"} /\ ts /= a then
    goto r_w0;
  end if;
r_check:
  readOK := readOK /\ Resolve(b, a) = g;
end process;

end algorithm; *)
=============================================================================
