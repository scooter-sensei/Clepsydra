// go/split_race_test.go, go/sweep_race_test.go and go/install_race_test.go:
// moves, evictions, the decay sweep and installs racing a key's touches
// (claims A23, A26, A27).
#include <atomic>
#include <set>
#include <thread>

#include "check.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

namespace {
// Lines of moveTable's 4-line table: X is homed at P and sits displaced in A;
// every line is full.
const u64 mvP = 0, mvA = 1, mvB = 2, mvQ = 3;
const u64 sweepT0 = 60'000;  // a unit boundary (units of 2,500 ticks)

// MoveTable is a full 4-line table at t1 = 1000: P holds one entry from t0 = 1
// (expired once t0 + horizon has passed) and three of 5 events; X, homed at
// P, sits displaced in A with 1 event; A, B and Q fill up with 5-event entries
// homed there. key(primary, alt) returns an unused key with those lines (alt
// 4: any) on a claim stripe of its own.
struct MoveTable {
  std::unique_ptr<Filter> f;
  u64 x = 0;
  std::set<u64> stripes;
  u64 next = 0;
  u64 key(u64 primary, u64 alt) {
    Core& c = core(*f);
    for (;; next++) {
      u64 h = mix64(next + (u64{1} << 52));
      u64 p = c.primary(h), s = h >> (64 - claimStripeBits);
      if (p == primary && (alt == 4 || c.alt(p, fingerprint(h)) == alt) && !stripes.count(s)) {
        stripes.insert(s);
        return h;
      }
    }
  }
  void touch(u64 h, int n, u64 now) {
    for (int i = 0; i < n; i++) f->TouchAt(h, 0, now);
  }
};

std::unique_ptr<MoveTable> moveTable() {
  const u64 t0 = 1, t1 = 1000;
  auto m = std::make_unique<MoveTable>();
  Config cfg = cfgOf(13, 1min, 1ms);
  cfg.Stash = 64;
  cfg.Overflow = -1;
  cfg.Hot = -1;
  m->f = std::make_unique<Filter>(cfg);
  Core& c = core(*m->f);
  if (m->f->Slots() != 16) FATALF("want 4 lines, got %d slots", m->f->Slots());
  m->touch(m->key(mvP, 4), 1, t0);
  for (int i = 0; i < 3; i++) m->touch(m->key(mvP, 4), 5, t1);
  m->x = m->key(mvP, mvA);
  m->touch(m->x, 1, t1);
  for (u64 l : {mvA, mvB, mvQ})
    while (countLine(c, l, t1) < 4) m->touch(m->key(l, 4), 5, t1);
  for (u64 l = 0; l < 4; l++)
    if (int n = countLine(c, l, t1); n != 4) FATALF("line %d holds %d live entries at t1, want 4", l, n);
  if (c.find(mvA, (fingerprint(m->x) << 1) | 1, tag(m->x)) < 0) FATALF("X is not displaced in A");
  return m;
}

void evictDuringMove(bool source) {
  Hooks hooks;
  auto m = moveTable();
  Core& c = core(*m->f);
  u64 t2 = 1 + c.horizon + 1;  // P's oldest entry has expired: room for X's copy
  // k's first touch moves X to P; y's evicts X's copy, the lightest in P and Q.
  u64 k = m->key(mvA, mvB), y = m->key(mvP, mvQ);
  if (source) y = m->key(mvA, mvQ);  // or X's source, the lightest in A and Q
  bool fired = false;
  testHookCopied = [&] {
    if (fired) return;
    fired = true;
    m->touch(m->x, 1, t2);  // counts in the source
    m->touch(y, 1, t2);     // evicts X's copy or source, unless its move is left alone
    m->touch(m->x, 1, t2);
  };
  m->touch(k, 1, t2);
  testHookCopied = nullptr;
  if (!fired) FATALF("k's insert did not relocate X");
  if (Result r = m->f->CountAt(m->x, c.horizon, t2); r.Count != 3)
    FATALF("X reads %+v, want 3 events (relocations %d, evictions %d)", r, m->f->Relocations(), m->f->Evictions());
}

// sweepSetup: a filter holding one key K, touched once at sweepT0, whose
// touches don't sweep inline; i is K's slot (word index).
struct SweepSetup {
  std::unique_ptr<Filter> f;
  u64 k = 0;
  i64 i = -1;
};
SweepSetup sweepSetup() {
  SweepSetup s;
  Config cfg = cfgOf(1000, 1min, 1ms);
  cfg.Clock = [] { return u64{1}; };
  s.f = std::make_unique<Filter>(cfg);
  for (u64 j = 1; s.k == 0; j++)
    if (u64 h = mix64(j); (h & decayEvery) != 0) s.k = h;
  s.f->TouchAt(s.k, 0, sweepT0);
  Core& c = core(*s.f);
  if ((s.i = c.locate(s.k, fingerprint(s.k), tag(s.k))) < 0) FATALF("K is not in the table");
  return s;
}

// laggingTouchDuringHold returns a hook for testHookInstalling: when an
// install holds slot i (word1 busy), a touch of k at clock tL, which found the
// slot before, moves its word0 on (the install then loses the slot and puts
// k's register back); having found word1 busy when it counted, the touch
// looks k up again, on a thread of its own (done). moved reports whether the
// touch moved word0.
std::function<void(i64)> laggingTouchDuringHold(Filter& f, u64 k, i64 i, u64 tL, DonePtr& done, bool& moved) {
  Core& c = core(f);
  u64 w0 = c.words[static_cast<std::size_t>(i)].load();
  moved = false;
  return [&f, &c, &done, &moved, k, i, tL, w0](i64 j) {
    if (moved || j != i) return;
    if (!(moved = cas(c.words[static_cast<std::size_t>(i)], w0, (w0 & ~tsMask) | tL))) return;
    done = during([&f, k, tL] { f.TouchAt(k, 0, tL); });  // the touch runs whole, or waits for the hold
  };
}
}  // namespace

// TestEvictionDuringMoveKeepsHistory drives one interleaving in moveTable. A
// relocation copies X from A to its primary line P; before the move hands
// off, X is touched, another key's first touch evicts the lightest entry of
// its two lines, and X is touched again. The copy is hidden until the
// hand-off: both touches count in the source. X must end with all its events
// (claims A23).
TEST(TestEvictionDuringMoveKeepsHistory) {
  if (forceCT || forceBG) SKIP("ConstantTime and Background inserts don't search on the call path");
  LOGF("-- copy");
  evictDuringMove(false);
  LOGF("-- source");
  evictDuringMove(true);
}

// TestEvictionRacingMoveStart: an insert picks X's source as its eviction
// victim before any move of X has begun; a relocation then copies X to its
// primary line and X is touched; then the evictor goes on, and backs off from
// a key whose move is under way. X must end with both its events.
TEST(TestEvictionRacingMoveStart) {
  if (forceCT || forceBG) SKIP("ConstantTime and Background inserts don't search or evict on the call path");
  Hooks hooks;
  auto m = moveTable();
  Core& c = core(*m->f);
  u64 tE = 1 + c.horizon;      // the evictor still sees P's oldest entry live: its search finds no room
  u64 t2 = 1 + c.horizon + 1;  // the relocation sees it expired: room for X's copy
  u64 k = m->key(mvA, mvB), y = m->key(mvA, mvQ);
  std::atomic<bool> paused{false}, resume{false}, victimFired{false};
  // Go sets each hook to nil inside it; here a fire-once flag (clearing a
  // std::function from inside its own call would destroy the running lambda).
  testHookVictim = [&] {
    if (victimFired.exchange(true)) return;
    paused.store(true);
    while (!resume.load()) std::this_thread::yield();
  };
  DonePtr doneE = go([&] { m->touch(y, 1, tE); });  // chooses X's source, the lightest of A and Q, then waits
  while (!paused.load()) std::this_thread::yield();
  bool fired = false;
  testHookCopied = [&] {
    if (fired) return;
    fired = true;
    m->touch(m->x, 1, t2);
    resume.store(true);  // the evictor goes on
    doneE->wait(std::chrono::hours(1));
  };
  m->touch(k, 1, t2);  // relocates X to P
  resume.store(true);
  if (!fired) FATALF("k's insert did not relocate X");
  if (Result r = m->f->CountAt(m->x, c.horizon, t2); r.Count != 2)
    FATALF("X reads %+v, want 2 events (relocations %d, evictions %d)", r, m->f->Relocations(), m->f->Evictions());
}

// TestMoveAfterEvictionKeepsTouches drives one interleaving in moveTable. A
// relocation reads X in A (to copy it to P); before it copies, another key's
// first touch evicts X to the stash. The relocation then copies X's old words
// to P and hands off, which fails. X's next touch waits out the move and
// finds X in the stash. X must end with every event.
TEST(TestMoveAfterEvictionKeepsTouches) {
  if (forceCT || forceBG) SKIP("ConstantTime and Background inserts don't search or evict on the call path");
  Hooks hooks;
  auto m = moveTable();
  Core& c = core(*m->f);
  u64 tE = 1 + c.horizon;
  u64 t2 = 1 + c.horizon + 1;
  i64 src = c.find(mvA, (fingerprint(m->x) << 1) | 1, tag(m->x));
  u64 k = m->key(mvA, mvB), y = m->key(mvA, mvQ);
  bool picked = false, copied = false;
  DonePtr done;
  testHookPicked = [&](i64 i) {
    if (i != src || picked) return;
    picked = true;
    m->touch(y, 1, tE);  // evicts X: no move of X has begun
  };
  testHookCopied = [&] {
    if (!picked || copied) return;
    copied = true;
    done = during([&] { m->touch(m->x, 1, t2); });  // the touch runs whole, or waits for the move
  };
  m->touch(k, 1, t2);  // relocates X to P
  if (!picked || !copied) FATALF("k's insert did not relocate X after reading it (picked %v, copied %v)", picked, copied);
  WAIT_FOR(done, "X's touch");
  testHookPicked = nullptr;
  testHookCopied = nullptr;
  if (m->f->Evictions() == 0) FATALF("y's insert evicted nothing");
  if (Result r = m->f->CountAt(m->x, c.horizon, t2); r.Count != 2)
    FATALF("X reads %+v, want 2 events (relocations %d, evictions %d)", r, m->f->Relocations(), m->f->Evictions());
}

// TestSweepRacingReinsertKeepsKey (claims A26): the decay sweep clears K's
// expired slot. Between its first and last write to the slot, K's next touch,
// 32 units on, puts K back in the slot with a fresh register bit-identical to
// the old one. The sweep must leave the new register alone.
TEST(TestSweepRacingReinsertKeepsKey) {
  Hooks hooks;
  SweepSetup s = sweepSetup();
  Core& c = core(*s.f);
  u64 t1 = sweepT0 + 32 * c.unit;  // past the horizon (24 units); 32 units: the same epoch bits
  bool fired = false;
  DonePtr done;
  testHookSweeping = [&] {
    if (fired) return;
    fired = true;
    done = during([&] { s.f->TouchAt(s.k, 0, t1); });  // the touch runs whole, or waits for the sweep
  };
  c.sweepLine(static_cast<u64>(s.i / wordsPerLine), t1);
  if (!fired) FATALF("the sweep did not take K's slot");
  WAIT_FOR(done, "K's touch");
  testHookSweeping = nullptr;
  s.f->TouchAt(s.k, 0, t1);
  if (Result r = s.f->CountAt(s.k, c.horizon, t1); r.Upper < 2 && !r.Saturated)
    FATALF("K reads %+v after two touches at t1, want Upper >= 2", r);
}

// TestSweepRacingTouchPutsSlotBack: the decay sweep, at a clock past K's
// horizon, takes K's slot; a touch of K at a clock a horizon behind moves its
// last touch on first. The sweep's clear of word0 then fails, and it must put
// K's register back.
TEST(TestSweepRacingTouchPutsSlotBack) {
  Hooks hooks;
  SweepSetup s = sweepSetup();
  Core& c = core(*s.f);
  u64 tS = sweepT0 + c.horizon + 2 * c.unit;  // the sweep's clock: K expired
  u64 tL = sweepT0 + 1;                       // the touch's: K's unit
  auto i = static_cast<std::size_t>(s.i);
  u64 w0 = c.words[i].load(), w1 = c.words[i + 1].load();
  bool moved = false;
  testHookSweeping = [&] {
    if (!moved) moved = cas(c.words[i], w0, (w0 & ~tsMask) | tL);
  };
  c.sweepLine(static_cast<u64>(s.i / wordsPerLine), tS);
  testHookSweeping = nullptr;
  if (!moved) FATALF("the touch did not move K's last touch while the sweep held the slot");
  if (u64 got = c.words[i + 1].load(); got != w1) ERRORF("K's word1 is %#x after the sweep, want its register %#x back", got, w1);
  s.f->TouchAt(s.k, 0, tL);  // the touch's count, after the sweep
  if (Result r = s.f->CountAt(s.k, c.horizon, tL); r.Upper < 2 && !r.Saturated)
    FATALF("K reads %+v after two touches, want Upper >= 2", r);
}

// TestSweepHoldLaggingTouchFindsSlot: the lagging touch, 5 units after K's
// last, is live at the sweep's clock, and it counts while the sweep holds the
// slot: its lookup must wait out the hold, then find K's slot put back.
TEST(TestSweepHoldLaggingTouchFindsSlot) {
  Hooks hooks;
  SweepSetup s = sweepSetup();
  Core& c = core(*s.f);
  u64 tS = sweepT0 + c.horizon + c.unit;  // the sweep's clock: K expired
  u64 tL = sweepT0 + 5 * c.unit;          // the touch's: live at tS
  auto i = static_cast<std::size_t>(s.i);
  u64 w0 = c.words[i].load();
  bool moved = false;
  DonePtr done;
  testHookSweeping = [&] {
    if (moved) return;
    if (!(moved = cas(c.words[i], w0, (w0 & ~tsMask) | tL))) return;
    done = during([&] { s.f->TouchAt(s.k, 0, tL); });  // the touch's next look, its first count having failed
  };
  c.sweepLine(static_cast<u64>(s.i / wordsPerLine), tS);
  if (!moved) FATALF("the touch did not move K's last touch while the sweep held the slot");
  WAIT_FOR(done, "K's touch");
  testHookSweeping = nullptr;
  if (i64 n = s.f->Occupancy(tL); n != 1) FATALF("K holds %d slots, want 1", n);
  s.f->TouchAt(s.k, 0, tS);
  if (Result r = s.f->CountAt(s.k, c.horizon, tS); r.Upper < 2 && !r.Saturated)
    FATALF("K reads %+v after touches at %d and %d, want Upper >= 2", r, tL, tS);
}

// TestInsertOverExpiredHoldLaggingTouch (claims A27): a first touch of L, at a
// clock a horizon past K's last touch, takes K's expired slot. Meanwhile a
// touch of K at a clock 5 units after K's last moves word0 on: L's install
// loses the slot and puts K's register back. That touch's next look at K,
// during the hold, must not put K in a second slot.
TEST(TestInsertOverExpiredHoldLaggingTouch) {
  Hooks hooks;
  SweepSetup s = sweepSetup();
  Core& c = core(*s.f);
  u64 tS = sweepT0 + c.horizon + c.unit;  // L's clock: K expired
  u64 tL = sweepT0 + 5 * c.unit;          // K's touch's: live at tS
  u64 line = static_cast<u64>(s.i / wordsPerLine);
  u64 l = 0;  // L: K's line its primary, its own claim stripe, no inline sweep
  for (u64 j = u64{1} << 40; l == 0; j++) {
    if (u64 h = mix64(j); c.primary(h) == line && (h & decayEvery) != 0 &&
                          (h >> (64 - claimStripeBits)) != (s.k >> (64 - claimStripeBits)) && fingerprint(h) != fingerprint(s.k))
      l = h;
  }
  DonePtr done;
  bool moved = false;
  testHookInstalling = laggingTouchDuringHold(*s.f, s.k, s.i, tL, done, moved);
  s.f->TouchAt(l, 0, tS);
  if (!moved) FATALF("L's insert did not take K's slot");
  WAIT_FOR(done, "K's touch");
  testHookInstalling = nullptr;
  if (int n = keySlots(c, s.k); n != 1) FATALF("K holds %d slots, want 1", n);
  s.f->TouchAt(s.k, 0, tS);
  if (Result r = s.f->CountAt(s.k, c.horizon, tS); r.Upper < 2 && !r.Saturated)
    FATALF("K reads %+v after touches at %d and %d, want Upper >= 2", r, tL, tS);
}

// TestMoveOverExpiredHoldLaggingTouch: as TestInsertOverExpiredHoldLaggingTouch,
// with the hold a relocation's. k's first touch at t2 moves X to P, into E's
// expired slot, while a lagging touch of E moves E's word0 on. E's other line,
// Q, has room: the touch's next look at E, during the hold, must not put E
// there.
TEST(TestMoveOverExpiredHoldLaggingTouch) {
  if (forceCT || forceBG) SKIP("ConstantTime and Background inserts don't search or evict on the call path");
  Hooks hooks;
  Config cfg = cfgOf(13, 1min, 1ms);
  cfg.Stash = 64;
  cfg.Overflow = -1;
  cfg.Hot = -1;
  Filter f(cfg);
  Core& c = core(f);
  if (f.Slots() != 16) FATALF("want 4 lines, got %d slots", f.Slots());
  std::set<u64> stripes;
  u64 next = 0;
  auto key = [&](u64 primary, u64 alt) {  // an unused key with those lines, on a stripe of its own
    for (;; next++) {
      u64 h = mix64(next + (u64{1} << 52));
      u64 p = c.primary(h), s = h >> (64 - claimStripeBits);
      if (p == primary && c.alt(p, fingerprint(h)) == alt && (h & decayEvery) != 0 && !stripes.count(s)) {
        stripes.insert(s);
        return h;
      }
    }
  };
  const u64 t0 = 1, t1 = 1000;
  u64 t2 = t0 + c.horizon + 1;  // E expired, the rest (from t1) live
  u64 tL = t0 + 5 * c.unit;     // E's touch's clock: live at t2
  u64 e = key(mvP, mvQ);
  f.TouchAt(e, 0, t0);
  for (int j = 0; j < 3; j++) f.TouchAt(key(mvP, mvB), 0, t1);
  u64 x = key(mvP, mvA);
  f.TouchAt(x, 0, t1);  // P is full: X goes to A, displaced
  for (auto [l0, l1] : {std::pair{mvA, mvB}, std::pair{mvB, mvA}})
    while (countLine(c, l0, t1) < 4) f.TouchAt(key(l0, l1), 0, t1);
  if (c.find(mvA, (fingerprint(x) << 1) | 1, tag(x)) < 0 || countLine(c, mvP, t1) != 4 || countLine(c, mvQ, tL) != 0)
    FATALF("the table is not as the test needs");
  DonePtr done;
  bool moved = false;
  testHookInstalling = laggingTouchDuringHold(f, e, c.locate(e, fingerprint(e), tag(e)), tL, done, moved);
  f.TouchAt(key(mvA, mvB), 0, t2);  // moves X to P, into E's slot
  if (!moved) FATALF("no relocation into E's slot");
  WAIT_FOR(done, "E's touch");
  testHookInstalling = nullptr;
  if (int n = keySlots(c, e); n != 1) FATALF("E holds %d slots, want 1", n);
}

// TestReadDuringMoveSeesEveryEvent (go/movecopy_test.go; claims A23) drives
// one interleaving in moveTable. A relocation copies X from A to its primary
// line P; before the move hands off, a touch that found X at the source just
// before the copy went in counts there. X's next count, and its next touch,
// must see that event.
TEST(TestReadDuringMoveSeesEveryEvent) {
  if (forceCT || forceBG) SKIP("ConstantTime and Background inserts don't search on the call path");
  Hooks hooks;
  auto m = moveTable();
  Filter& f = *m->f;
  Core& c = core(f);
  u64 t2 = 1 + c.horizon + 1;  // P's oldest entry has expired: room for X's copy
  i64 src = c.find(mvA, (fingerprint(m->x) << 1) | 1, tag(m->x));
  u64 k = m->key(mvA, mvB);  // its first touch moves X to P
  bool fired = false;
  Result count, touch;
  testHookCopied = [&] {
    if (fired) return;
    fired = true;
    c.touch(m->x, 0, t2, false, src);  // starts from the slot it found X in: the source
    count = f.CountAt(m->x, c.horizon, t2);
    touch = f.TouchAt(m->x, c.horizon, t2);
  };
  f.TouchAt(k, 0, t2);
  testHookCopied = nullptr;
  if (!fired) FATALF("k's insert did not relocate X");
  if (count.Upper < 2 && !count.Saturated) ERRORF("during the move, X's count read %+v, want Upper >= 2", count);
  if (touch.Upper < 3 && !touch.Saturated) ERRORF("during the move, X's touch read %+v, want Upper >= 3", touch);
  if (Result r = f.CountAt(m->x, c.horizon, t2); r.Count != 3)
    ERRORF("after the move, X reads %+v, want 3 events (relocations %d)", r, f.Relocations());
}
