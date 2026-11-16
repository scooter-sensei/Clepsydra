// go/tagstripe_test.go and go/cas2_amd64_test.go: races where a CAS from a
// value read earlier takes a slot that changed hands: a slot's word1 isn't
// unique to its occupant, since two keys with the same 12-bit tag have
// bit-identical first registers in one unit, and a holder reads word0 and
// word1 in two loads (claims A28-A30); realign's 16-byte CAS (claims A33).
#include <atomic>
#include <thread>

#include "check.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

namespace {
const u64 sweepT0 = 60'000;  // a unit boundary (units of 2,500 ticks)

u64 word(Core& f, i64 i) { return f.words[static_cast<std::size_t>(i)].load(); }

// keyPair: K and L with primary line 7 (empty), different fingerprints, claim
// stripes and tags (the hooks run K's insert inside L's).
struct KeyPair {
  std::unique_ptr<Filter> f;
  u64 k, l, p;
};
KeyPair keyPair() {
  KeyPair kp;
  kp.f = newMinuteFilter(1000);
  kp.p = 7;
  Core& c = core(*kp.f);
  kp.k = keyAt(c, kp.p, twoLineFPs(c, kp.p, 0x2200, 1)[0], 0x123);
  kp.l = keyAt(c, kp.p, twoLineFPs(c, kp.p, 0x6600, 1)[0], 0x456);
  return kp;
}

// tornOpenHold runs L's insert, which reads line p's first slot open (word0
// 0); K's insert then takes the slot; L reads its word1 (K's) and its install
// takes word1 busy from K's register, then fails on word0 and puts it back.
// during runs while L holds K's slot.
void tornOpenHold(Filter& f, u64 k, u64 l, u64 p, u64 t0, const std::function<void()>& during) {
  Hooks hooks;
  i64 s = static_cast<i64>(p * wordsPerLine);
  int stage = 0;
  testHookOpenSeen = [&](i64 i) {
    if (stage == 0 && i == s) {
      stage = 1;
      f.TouchAt(k, 0, t0);
      stage = 2;
    }
  };
  testHookInstalling = [&](i64 i) {
    if (stage == 2 && i == s) {
      stage = 3;
      during();
    }
  };
  f.TouchAt(l, 0, t0);
  if (stage != 3) FATALF("stage %d: L's install did not take K's slot", stage);
}

// sweepOverJ: a filter whose line 7's first slot (word index s) holds J,
// tagged tgJ, touched once at sweepT0.
struct SweepJ {
  std::unique_ptr<Filter> f;
  u64 p;
  i64 s;
};
SweepJ sweepOverJ(u64 tgJ) {
  SweepJ sj;
  sj.f = newMinuteFilter(1000);
  sj.p = 7;
  Core& c = core(*sj.f);
  u64 j = keyAt(c, sj.p, twoLineFPs(c, sj.p, 0x3300, 1)[0], tgJ);
  sj.f->TouchAt(j, 0, sweepT0);
  sj.s = static_cast<i64>(sj.p * wordsPerLine);
  if (auto sl = slotsOf(c, j); sl.size() != 1 || sl[0] != sj.s) FATALF("setup: J in %v", sl);
  return sj;
}

// sweepHolds runs the decay sweep of line p at tS; when the hook named by seen
// fires on slot s, K's touch at tS takes the slot (J expired there), and ok
// checks the result; during runs while the sweep holds the slot.
void sweepHolds(Filter& f, u64 p, i64 s, u64 k, u64 tS, std::function<void(i64)>& seen, const std::function<bool()>& ok,
                const std::function<void()>& during) {
  Hooks hooks;
  int stage = 0;
  seen = [&](i64 i) {
    if (stage == 0 && i == s) {
      stage = 1;
      f.TouchAt(k, 0, tS);
      if (ok()) stage = 2;
    }
  };
  testHookSweeping = [&] {
    if (stage == 2) {
      stage = 3;
      during();
    }
  };
  core(f).sweepLine(p, tS);
  if (stage != 3) FATALF("stage %d: the sweep did not hold K's slot", stage);
}

void countIntoHeldSameTagSlot() {
  Hooks hooks;
  auto fp = newMinuteFilter(4);
  Filter& f = *fp;
  Core& c = core(f);
  if (c.lines != 2) FATALF("lines %d, want 2", c.lines);
  const u64 t0 = 60'000, T = 0x5a5;
  auto fps = twoLineFPs(c, 0, 0x1100, 9);
  for (int j = 0; j < 7; j++) {  // two events each
    u64 h = keyAt(c, static_cast<u64>(j % 2), fps[static_cast<std::size_t>(j)], 0x100 + static_cast<u64>(j));
    f.TouchAt(h, 0, t0);
    f.TouchAt(h, 0, t0);
  }
  u64 k = keyAt(c, 1, fps[7], T), k2 = keyAt(c, 0, fps[8], T);
  f.TouchAt(k, 0, t0);
  auto ks = slotsOf(c, k);
  if (ks.size() != 1 || f.Occupancy(t0) != 8) FATALF("setup: K in %v, occupancy %d", ks, f.Occupancy(t0));
  i64 i = ks[0];
  u64 kw1 = word(c, i + 1);
  u64 t1 = t0 + 10 * c.unit;
  std::atomic<int> stage{0};
  std::atomic<bool> inHold{false}, held{false}, release{false};
  DonePtr done;
  testHookStamped = [&] {  // K2's realign, word1 busy
    bool f0 = false;
    if (inHold.compare_exchange_strong(f0, true)) {
      held.store(true);
      while (!release.load()) std::this_thread::yield();
    }
  };
  testHookBumping = [&](i64 j) {
    if (stage == 0 && j == i) {
      stage = 1;
      f.TouchAt(k2, 0, t0);
      if (auto sl = slotsOf(c, k2); sl.size() == 1 && sl[0] == i && word(c, i + 1) == kw1) stage = 2;
    }
  };
  testHookBumped = [&](i64 j) {
    if (stage == 2 && j == i) {
      stage = 3;
      done = go([&] { f.TouchAt(k2, 0, t1); });
      while (!held.load()) std::this_thread::yield();
      std::thread([&release] {
        std::this_thread::sleep_for(20ms);
        release.store(true);
      }).detach();
    }
  };
  f.TouchAt(k, 0, t0);
  testHookBumping = nullptr;
  testHookBumped = nullptr;
  if (stage != 3) FATALF("stage %d: K's count did not land in K2's slot", stage.load());
  WAIT_FOR(done, "K2's touch");
  if (Result r = f.CountAt(k, c.horizon, t0); r.Upper < 2 && !r.Saturated) FATALF("K reads %+v after 2 touches", r);
}

void staleHotReset(u64 gap) {
  Hooks hooks;
  auto fp = newMinuteFilter(1000);
  Filter& f = *fp;
  Core& c = core(f);
  u64 k = keyAt(c, 7, twoLineFPs(c, 7, 0x6600, 1)[0], 0x321);
  for (int j = 0; j < 300; j++) f.TouchAt(k, 0, sweepT0);
  if (auto sl = slotsOf(c, k); sl.size() != 1 || !isHot(word(c, sl[0] + 1))) FATALF("setup: K in %v, not hot", sl);
  u64 t1 = sweepT0 + gap * c.unit;
  bool fired = false;
  DonePtr done;
  testHookStamped = [&] {
    if (!fired) {
      fired = true;
      done = during([&] { f.TouchAt(k, 0, t1); });
    }
  };
  f.TouchAt(k, 0, t1);
  if (!fired) FATALF("A did not move K's last touch");
  WAIT_FOR(done, "B's touch");
  testHookStamped = nullptr;
  if (Result r = f.CountAt(k, c.horizon, t1); r.Upper < 2 && !r.Saturated)
    FATALF("K reads %+v after 2 touches at t1", r);
}
}  // namespace

// TestCountAcrossSameTagHandOver (claims A29): K's touch at the tick of its
// last checks K's slot, then, before its count's CAS, an insert of K2, whose
// tag is K's, evicts K and takes its slot with a fresh register bit-identical
// to K's. The CAS from K's register then succeeds, into K2's.
TEST(TestCountAcrossSameTagHandOver) {
  Hooks hooks;
  auto fp = newMinuteFilter(4);
  Filter& f = *fp;
  Core& c = core(f);
  if (c.lines != 2) FATALF("lines %d, want 2", c.lines);
  const u64 t0 = 60'000, T = 0x5a5;
  auto fps = twoLineFPs(c, 0, 0x1100, 9);
  for (int j = 0; j < 7; j++) {  // two events each
    u64 h = keyAt(c, static_cast<u64>(j % 2), fps[static_cast<std::size_t>(j)], 0x100 + static_cast<u64>(j));
    f.TouchAt(h, 0, t0);
    f.TouchAt(h, 0, t0);
  }
  u64 k = keyAt(c, 1, fps[7], T), k2 = keyAt(c, 0, fps[8], T);
  f.TouchAt(k, 0, t0);
  auto ks = slotsOf(c, k);
  if (ks.size() != 1 || f.Occupancy(t0) != 8) FATALF("setup: K in %v, occupancy %d", ks, f.Occupancy(t0));
  i64 i = ks[0];
  u64 kw1 = word(c, i + 1);
  bool fired = false, same = false;
  testHookBumping = [&](i64 j) {
    if (fired || j != i) return;
    fired = true;
    f.TouchAt(k2, 0, t0);
    auto sl = slotsOf(c, k2);
    same = sl.size() == 1 && sl[0] == i && word(c, i + 1) == kw1;
  };
  f.TouchAt(k, 0, t0);
  testHookBumping = nullptr;
  if (!fired || !same) FATALF("K2 did not take K's slot with K's word1 (%#x) during K's count", kw1);
  if (Result r = f.CountAt(k, c.horizon, t0); r.Upper < 2 && !r.Saturated) FATALF("K reads %+v after 2 touches", r);
  if (Result r = f.CountAt(k2, c.horizon, t0); r.Upper < 1) FATALF("K2 reads %+v after 1 touch", r);
}

// TestCountThenEvictionCountsOnce (claims A29): right after K's count's CAS,
// an insert of K2, of another tag, evicts K and takes its slot. The count
// went with K's history to the stash: K must not count it again.
TEST(TestCountThenEvictionCountsOnce) {
  Hooks hooks;
  auto fp = newMinuteFilter(4);
  Filter& f = *fp;
  Core& c = core(f);
  if (c.lines != 2) FATALF("lines %d, want 2", c.lines);
  const u64 t0 = 60'000;
  auto fps = twoLineFPs(c, 0, 0x1100, 9);
  for (int j = 0; j < 7; j++) {  // three events each
    u64 h = keyAt(c, static_cast<u64>(j % 2), fps[static_cast<std::size_t>(j)], 0x100 + static_cast<u64>(j));
    for (int n = 0; n < 3; n++) f.TouchAt(h, 0, t0);
  }
  u64 k = keyAt(c, 1, fps[7], 0x5a5), k2 = keyAt(c, 0, fps[8], 0x5a6);
  f.TouchAt(k, 0, t0);
  auto ks = slotsOf(c, k);
  if (ks.size() != 1 || f.Occupancy(t0) != 8) FATALF("setup: K in %v, occupancy %d", ks, f.Occupancy(t0));
  i64 i = ks[0];
  bool fired = false, taken = false;
  testHookBumped = [&](i64 j) {
    if (fired || j != i) return;
    fired = true;
    f.TouchAt(k2, 0, t0);
    auto sl = slotsOf(c, k2);
    taken = sl.size() == 1 && sl[0] == i;
  };
  f.TouchAt(k, 0, t0);
  testHookBumped = nullptr;
  if (!fired || !taken) FATALF("K2 did not take K's slot after K's count");
  if (Result r = f.CountAt(k, c.horizon, t0); r.Count != 2 || r.Upper != 2) FATALF("K reads %+v after 2 touches", r);
}

// TestCountThenMoveCountsOnce (claims A29): right after K's count's CAS, an
// insert of N, whose two lines are full, moves K to K's other line and takes
// its slot. The count went with the copy: K must not count it again.
TEST(TestCountThenMoveCountsOnce) {
  Hooks hooks;
  auto fp = newMinuteFilter(1000);
  Filter& f = *fp;
  Core& c = core(f);
  const u64 p = 7, t0 = 60'000;
  u64 fk = twoLineFPs(c, p, 0x7100, 1)[0];
  u64 a = c.alt(p, fk);
  u64 fn = 0;
  for (u64 cand : twoLineFPs(c, p, 0x7200, 64)) {
    if (c.alt(p, cand) != a) {
      fn = cand;
      break;
    }
  }
  u64 b = c.alt(p, fn);
  u64 k = keyAt(c, p, fk, 0x5a5);
  f.TouchAt(k, 0, t0);
  const u64 ls[7] = {p, p, p, b, b, b, b};  // N's lines full, K's other line empty
  for (int j = 0; j < 7; j++) {
    u64 h = keyAt(c, ls[j], 0x7300 + static_cast<u64>(j), 0x100 + static_cast<u64>(j));
    f.TouchAt(h, 0, t0);
    f.TouchAt(h, 0, t0);
  }
  u64 n = keyAt(c, p, fn, 0x5a6);
  auto ks = slotsOf(c, k);
  if (ks.size() != 1 || ks[0] != static_cast<i64>(p * wordsPerLine) || f.Occupancy(t0) != 8)
    FATALF("setup: K in %v, occupancy %d", ks, f.Occupancy(t0));
  i64 i = ks[0];
  bool fired = false, moved = false;
  testHookBumped = [&](i64 j) {
    if (fired || j != i) return;
    fired = true;
    f.TouchAt(n, 0, t0);
    auto sn = slotsOf(c, n), sk = slotsOf(c, k);
    moved = sn.size() == 1 && sn[0] == i && sk.size() == 1 && static_cast<u64>(sk[0] / wordsPerLine) == a;
  };
  f.TouchAt(k, 0, t0);
  testHookBumped = nullptr;
  if (!fired || !moved) FATALF("N did not move K out of its slot after K's count");
  if (Result r = f.CountAt(k, c.horizon, t0); r.Count != 2 || r.Upper != 2) FATALF("K reads %+v after 2 touches", r);
}

// TestCountIntoHeldSameTagSlotLooksAgain (claims A29): K's count lands in
// K2's register; then, before K's touch reads word0 again, a touch of K2 10
// units on holds the slot (realign) for 20 ms. The touch must wait out the
// hold, which puts back a register of K's tag, and count afresh. With
// realign's 16-byte CAS there is no hold.
TEST(TestCountIntoHeldSameTagSlotLooksAgain) { realignModes(countIntoHeldSameTagSlot); }

// TestTornOpenScanHoldHidesKey (claims A28): K's second touch runs while L
// holds K's slot. The hold must count on K's stripe (the tag of the word1 it
// took).
TEST(TestTornOpenScanHoldHidesKey) {
  KeyPair kp = keyPair();
  Filter& f = *kp.f;
  const u64 t0 = 60'000;
  DonePtr done;
  tornOpenHold(f, kp.k, kp.l, kp.p, t0, [&] { done = during([&] { f.TouchAt(kp.k, 0, t0); }); });
  WAIT_FOR(done, "K's touch");
  if (auto sl = slotsOf(core(f), kp.k); sl.size() != 1) FATALF("K in slots %v", sl);
  if (Result r = f.CountAt(kp.k, core(f).horizon, t0); r.Upper < 2 && !r.Saturated)
    FATALF("K reads %+v after 2 touches", r);
}

// TestTornOpenScanHoldRead (claims A28): a Count of K during L's hold must
// wait it out.
TEST(TestTornOpenScanHoldRead) {
  KeyPair kp = keyPair();
  Filter& f = *kp.f;
  const u64 t0 = 60'000;
  DonePtr done;
  Result r;
  tornOpenHold(f, kp.k, kp.l, kp.p, t0, [&] { done = during([&] { r = f.CountAt(kp.k, core(f).horizon, t0); }); });
  WAIT_FOR(done, "K's count");
  if (r.Upper < 1 && !r.Saturated) FATALF("K read %+v during another key's hold, after 1 touch", r);
}

// TestSweepTornHoldHidesKey (claims A28): the sweep reads the slot's word0
// (J's, expired), K's insert takes the slot, the sweep reads word1 (K's) and
// holds K's slot.
TEST(TestSweepTornHoldHidesKey) {
  SweepJ sj = sweepOverJ(0x321);
  Filter& f = *sj.f;
  Core& c = core(f);
  u64 tS = sweepT0 + c.horizon + 2 * c.unit;
  u64 k = keyAt(c, sj.p, twoLineFPs(c, sj.p, 0x4400, 1)[0], 0x654);
  DonePtr done;
  sweepHolds(
      f, sj.p, sj.s, k, tS, testHookSweepSeen,
      [&] {
        auto sl = slotsOf(c, k);
        return sl.size() == 1 && sl[0] == sj.s;
      },
      [&] { done = during([&] { f.TouchAt(k, 0, tS); }); });
  WAIT_FOR(done, "K's touch");
  auto sl = slotsOf(c, k);
  Result r = f.CountAt(k, c.horizon, tS);
  if (sl.size() != 1 || (r.Upper < 2 && !r.Saturated)) FATALF("K in slots %v, reads %+v after 2 touches", sl, r);
}

// TestSweepSameTagHoldHidesKey (claims A28): the sweep reads both of J's words;
// K, whose tag is J's, takes the slot 32 units on with a fresh register
// bit-identical to J's, and the sweep's CAS from J's register holds K's slot.
TEST(TestSweepSameTagHoldHidesKey) {
  const u64 T = 0x2a7;
  SweepJ sj = sweepOverJ(T);
  Filter& f = *sj.f;
  Core& c = core(f);
  u64 jw1 = word(c, sj.s + 1);
  u64 tS = sweepT0 + 32 * c.unit;
  u64 k = keyAt(c, sj.p, twoLineFPs(c, sj.p, 0x5500, 1)[0], T);
  DonePtr done;
  sweepHolds(
      f, sj.p, sj.s, k, tS, testHookSweepRead,
      [&] {
        auto sl = slotsOf(c, k);
        return sl.size() == 1 && sl[0] == sj.s && word(c, sj.s + 1) == jw1;
      },
      [&] { done = during([&] { f.TouchAt(k, 0, tS); }); });
  WAIT_FOR(done, "K's touch");
  auto sl = slotsOf(c, k);
  Result r = f.CountAt(k, c.horizon, tS);
  if (sl.size() != 1 || (r.Upper < 2 && !r.Saturated)) FATALF("K in slots %v, reads %+v after 2 touches", sl, r);
}

// TestStaleHotResetKeepsTouches (claims A30): K is hot. At t1 touch A finds
// K's slot past the horizon and resets it; touch B, after A's word0, finds K
// live. B's event must not be lost, 25 units on (B adds to the entry) or 50
// (B demotes it).
TEST(TestStaleHotResetKeepsTouches) {
  for (u64 gap : {u64{25}, u64{50}}) {
    LOGF("-- gap%d", gap);
    realignModes([gap] { staleHotReset(gap); });
  }
}

// TestCAS2 (go/cas2_amd64_test.go): CAS2 replaces both words of a slot only
// when both match, and touches no other word.
TEST(TestCAS2) {
  if (!cpu::HasCAS2) SKIP("no CMPXCHG16B, or TF_NOCAS2");
  auto fp = newMinuteFilter(1000);
  Core& c = core(*fp);
  AU64* p = &c.words[2];  // a slot: 16-byte aligned
  c.words[2].store(10);
  c.words[3].store(20);
  struct Case {
    u64 old0, old1;
    bool ok;
  };
  for (Case cs : {Case{11, 20, false}, Case{10, 21, false}, Case{10, 20, true}}) {
    u64 w0 = c.words[2].load(), w1 = c.words[3].load();
    if (bool ok = cpu::CAS2(p, cs.old0, cs.old1, 30, 40); ok != cs.ok)
      FATALF("cas2 from %d, %d over %d, %d: %v", cs.old0, cs.old1, w0, w1, ok);
    u64 want0 = cs.ok ? 30 : w0, want1 = cs.ok ? 40 : w1;
    if (c.words[2].load() != want0 || c.words[3].load() != want1 || c.words[1].load() != 0 || c.words[4].load() != 0)
      FATALF("after cas2 from %d, %d: words %d %d %d %d %d", cs.old0, cs.old1, c.words[0].load(), c.words[1].load(),
             c.words[2].load(), c.words[3].load(), c.words[4].load());
  }
}

// TestRealignCAS2 (claims A33): K's register is full and its next touch is 10
// units on, so realign's advance spills and K is promoted. The entry is
// published with both words by one CAS, with no move counted; if word0 moved
// since realign read it, the slot is left as it was and the entry closed
// again.
TEST(TestRealignCAS2) {
  if (!cpu::HasCAS2) SKIP("no CMPXCHG16B, or TF_NOCAS2");
  for (bool moved : {false, true}) {
    auto fp = newMinuteFilter(1000);
    Filter& f = *fp;
    Core& c = core(f);
    const u64 tg = 0x321;
    u64 k = keyAt(c, 7, twoLineFPs(c, 7, 0x6600, 1)[0], tg);
    f.TouchAt(k, 0, sweepT0);
    auto sl = slotsOf(c, k);
    if (sl.size() != 1) FATALF("setup: K in %v", sl);
    i64 i = sl[0];
    u64 full = (tg << tagShift) | ((static_cast<u64>(c.unitOf(sweepT0)) & onsetMask) << onsetShift) | regMask;
    c.words[static_cast<std::size_t>(i) + 1].store(full);
    u64 w0 = word(c, i);
    if (moved)
      c.words[static_cast<std::size_t>(i)].store(w0 + 1);  // a touch of K at the next tick, after realign's read
    MoveCount& m = c.moveOf(tg);
    u64 begun = m.begun.load();
    u64 now = sweepT0 + 10 * c.unit;
    u64 w1;
    bool over;
    bool ok = c.realign2U(i, w0, full, tg, now, c.unitOf(now), c.unitOf(w0 & tsMask), w1, over);
    u64 n0 = word(c, i), n1 = word(c, i + 1);
    if (m.begun.load() != begun) FATALF("moved %v: realign2 counted a move", moved);
    if (moved && (ok || n0 != w0 + 1 || n1 != full || f.HotInUse() != 0 || f.Promoted() != 0))
      FATALF("moved: ok %v, words %#x %#x, hot in use %d, promoted %d", ok, n0, n1, f.HotInUse(), f.Promoted());
    if (!moved && (!ok || over || !isHot(w1) || n1 != w1 || n0 != ((w0 & ~tsMask) | now) || f.HotInUse() != 1 ||
                   f.Promoted() != 1))
      FATALF("ok %v, over %v, w1 %#x, words %#x %#x, hot in use %d, promoted %d", ok, over, w1, n0, n1, f.HotInUse(),
             f.Promoted());
  }
}
