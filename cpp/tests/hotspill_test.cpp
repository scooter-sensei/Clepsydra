// go/hotspill_test.go and go/hotdrop_test.go: a hot key's history leaving its
// slot (claims A32, A40).
#include "check.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

namespace {
u64 word(Core& f, i64 i) { return f.words[static_cast<std::size_t>(i)].load(); }

std::unique_ptr<Filter> plainFilter(Config cfg) {
  auto f = std::make_unique<Filter>(cfg);
  core(*f).ct = false;  // the race is in the plain path's order of steps (TF_CT, TF_BG)
  core(*f).bg = false;
  return f;
}
}  // namespace

// TestHotAddAfterSpillCounts (claims A32): K is hot, its 40 events at t0. K's
// touch T at t1 moves word0 and reads K's slot (the hot pointer). Before T
// adds to the entry, an insert of K2 at t1 + 6 s evicts K and, with no stash,
// spills K's history to the overflow table. T's add by K's identity in K's
// entry must fail, and T count afresh.
TEST(TestHotAddAfterSpillCounts) {
  Hooks hooks;
  Config cfg = cfgOf(4, 1min, 1ms);
  cfg.Clock = [] { return u64{1}; };
  cfg.Stash = -1;
  auto fp = plainFilter(cfg);
  Filter& f = *fp;
  Core& c = core(f);
  if (c.lines != 2) FATALF("lines %d, want 2", c.lines);
  const u64 t0 = 60'000;
  u64 t1 = t0 + 59'000;  // within a horizon of K's last touch
  u64 t2 = t1 + 6'000;   // K's events past the horizon (their unit too)
  auto fps = twoLineFPs(c, 0, 0x1100, 9);
  u64 k = keyAt(c, 1, fps[7], 0x5a5);
  for (int n = 0; n < 40; n++) f.TouchAt(k, 0, t0);
  auto ks = slotsOf(c, k);
  if (ks.size() != 1 || !isHot(word(c, ks[0] + 1))) FATALF("setup: K in %v, not hot", ks);
  for (int j = 0; j < 7; j++) {  // two events each at t1: heavier than K at t2
    u64 h = keyAt(c, static_cast<u64>(j % 2), fps[static_cast<std::size_t>(j)], 0x100 + static_cast<u64>(j));
    f.TouchAt(h, 0, t1);
    f.TouchAt(h, 0, t1);
  }
  if (f.Occupancy(t1) != 8) FATALF("setup: occupancy %d, want 8", f.Occupancy(t1));
  u64 k2 = keyAt(c, 0, fps[8], 0x5a6);
  i64 i = ks[0];
  bool fired = false, gone = false;
  testHookHotAdding = [&](i64 j) {
    if (fired || j != i) return;
    fired = true;
    f.TouchAt(k2, 0, t2);
    gone = slotsOf(c, k).empty();
  };
  f.TouchAt(k, 0, t1);  // T
  testHookHotAdding = nullptr;
  if (!fired || !gone) FATALF("K2 did not evict K during K's hot add");
  if (Result r = f.CountAt(k, c.horizon, t2); r.Upper < 1 && !r.Saturated) FATALF("K reads %+v after a touch at t1", r);
}

// TestHotAddAfterStashDropCounts (claims A32): as TestHotAddAfterSpillCounts,
// but K's history goes to the stash (one set of 4 ways), and is dropped from
// it before T adds. T's add must fail, and T count afresh.
TEST(TestHotAddAfterStashDropCounts) {
  Hooks hooks;
  Config cfg = cfgOf(4, 1min, 1ms);
  cfg.Clock = [] { return u64{1}; };
  cfg.Stash = 4;
  auto fp = plainFilter(cfg);
  Filter& f = *fp;
  Core& c = core(f);
  if (c.lines != 2 || c.stashN != 1) FATALF("lines %d, stash sets %d; want 2, 1", c.lines, c.stashN);
  const u64 t0 = 60'000;
  u64 t1 = t0 + 59'000, t2 = t1 + 6'000;
  auto fps = twoLineFPs(c, 0, 0x1100, 13);
  u64 k = keyAt(c, 1, fps[7], 0x5a5);
  for (int n = 0; n < 40; n++) f.TouchAt(k, 0, t0);
  auto ks = slotsOf(c, k);
  if (ks.size() != 1 || !isHot(word(c, ks[0] + 1))) FATALF("setup: K in %v, not hot", ks);
  for (int j = 0; j < 7; j++) {
    u64 h = keyAt(c, static_cast<u64>(j % 2), fps[static_cast<std::size_t>(j)], 0x100 + static_cast<u64>(j));
    f.TouchAt(h, 0, t1);
    f.TouchAt(h, 0, t1);
  }
  if (f.Occupancy(t1) != 8 || f.Evictions() != 0)
    FATALF("setup: occupancy %d, evictions %d; want 8, 0", f.Occupancy(t1), f.Evictions());
  i64 i = ks[0];
  u64 id = c.stashID(k, fingerprint(k), tag(k));
  bool fired = false, gone = false, stashed = false, dropped = false;
  u64 ev = 0;
  testHookHotAdding = [&](i64 j) {
    if (fired || j != i) return;
    fired = true;
    // K2 evicts K (into the stash's first way); K3-K5 each evict the newcomer
    // before them into the next ways; K6's eviction finds all four ways live
    // and K's the lightest.
    for (int n = 0; n < 5; n++) {
      f.TouchAt(keyAt(c, static_cast<u64>(n % 2), fps[static_cast<std::size_t>(8 + n)], 0x5a6 + static_cast<u64>(n)), 0,
                t2);
      if (n == 0) {
        gone = slotsOf(c, k).empty();
        u64 a, b;
        stashed = c.stashGet(id, t2, false, a, b);
      }
    }
    u64 a, b;
    dropped = !c.stashGet(id, t2, false, a, b);
    ev = f.Evictions();
  };
  f.TouchAt(k, 0, t1);  // T
  testHookHotAdding = nullptr;
  if (!fired || !gone || !stashed || !dropped || ev != 5)
    FATALF("during K's hot add: K evicted %v, stashed %v, then dropped %v; evictions %d, want 5", gone, stashed,
           dropped, ev);
  if (Result r = f.CountAt(k, c.horizon, t2); r.Upper < 1 && !r.Saturated) FATALF("K reads %+v after a touch at t1", r);
}

// TestSpillOfForeignHotEntryIsNotLost: without an overflow table, a stash drop
// whose hot entry is no longer the history's own has nothing left to lose:
// Lost and the Degraded window stay as they were. A history that is its own
// is still lost.
TEST(TestSpillOfForeignHotEntryIsNotLost) {
  Config cfg = cfgOf(1000, 1min, 1ms);
  cfg.Overflow = -1;
  cfg.Hot = 4;
  cfg.Clock = [] { return u64{1}; };
  Filter f(cfg);
  Core& c = core(f);
  c.hotTable();  // entry 1, never handed out, is no one's
  const u64 now = 60'000;
  u64 k = mix64(7);
  u64 fp = fingerprint(k), tg = tag(k);
  u64 id = c.stashID(k, fp, tg), w0 = (fp << 48) | now;
  u64 foreign = (tg << tagShift) | (u64{1} << hotIdxShift);
  if (!isHot(foreign)) FATALF("not a hot pointer");
  c.spill(id, w0, foreign, now);
  if (f.Lost() != 0 || c.lostRecently(now))
    FATALF("a drop whose hot entry is another's: Lost %d, lost recently %v; want 0, false", f.Lost(),
           c.lostRecently(now));
  u64 own = c.nextReg(0, tg, now, now, true).first;
  c.spill(id, w0, own, now);
  if (f.Lost() != 1 || !c.lostRecently(now))
    FATALF("a drop of a history with one event: Lost %d, lost recently %v; want 1, true", f.Lost(),
           c.lostRecently(now));
}

namespace {
const u64 dropT0 = 60'000;
std::pair<std::unique_ptr<Filter>, u64> promotedK() {
  Config cfg = cfgOf(1000, 1min, 1ms);
  cfg.Hot = 4;
  cfg.Clock = [] { return u64{1}; };
  auto f = std::make_unique<Filter>(cfg);
  u64 k = mix64(1);
  for (int n = 0; n < 32; n++) f->TouchAt(k, 0, dropT0);  // past the register's 31 events in a unit: promoted
  if (f->Promoted() != 1 || f->HotInUse() != 1)
    FATALF("K not promoted (promoted %d, in use %d)", f->Promoted(), f->HotInUse());
  return {std::move(f), k};
}
}  // namespace

// TestSlotExitFreesHotEntry (claims A40): a hot key's history that its slot
// discards past the horizon gives its entry up at once: by a touch's reset
// (realign, with its 16-byte CAS and with its hold), by the sweep, and by an
// insert over the expired occupant.
TEST(TestSlotExitFreesHotEntry) {
  LOGF("-- reset");
  realignModes([] {
    auto [f, k] = promotedK();
    Core& c = core(*f);
    u64 now = dropT0 + c.horizon + c.unit;  // K's history is past the horizon: its touch starts afresh
    f->TouchAt(k, 0, now);
    if (i64 n = f->HotInUse(); n != 0) FATALF("hot entries in use after K's reset: %d, want 0", n);
    if (Result r = f->CountAt(k, c.horizon, now); r.Count != 1 || r.Saturated)
      FATALF("K reads %+v after its reset, want count 1", r);
  });
  LOGF("-- sweep");
  {
    auto [f, k] = promotedK();
    Core& c = core(*f);
    i64 i = c.locate(k, fingerprint(k), tag(k));
    c.sweepLine(static_cast<u64>(i / wordsPerLine), dropT0 + c.horizon + 1);
    if (word(c, i) != 0) FATALF("K's expired slot not swept");
    if (i64 n = f->HotInUse(); n != 0) FATALF("hot entries in use after the sweep: %d, want 0", n);
  }
  LOGF("-- insert");
  {
    auto [f, k] = promotedK();
    Core& c = core(*f);
    i64 i = c.locate(k, fingerprint(k), tag(k));
    u64 line = static_cast<u64>(i / wordsPerLine), tL = dropT0 + c.horizon + 1;
    // Fill K's line's other slots, live at tL, so a key of this line takes K's.
    for (int s = 0; s < wordsPerLine; s += 2) {
      if (i64 j = static_cast<i64>(line) * wordsPerLine + s; j != i) {
        c.words[static_cast<std::size_t>(j)].store((u64{0x7777} << 48) | tL);
        c.words[static_cast<std::size_t>(j) + 1].store((u64{0x777} << tagShift) | 1);
      }
    }
    for (u64 j = u64{1} << 40;; j++) {
      if (u64 h = mix64(j); c.primary(h) == line && fingerprint(h) != fingerprint(k)) {
        f->TouchAt(h, 0, tL);
        break;
      }
    }
    if (c.locate(k, fingerprint(k), tag(k)) == i) FATALF("K's expired slot not taken");
    if (i64 n = f->HotInUse(); n != 0)
      FATALF("hot entries in use after an insert over K's expired slot: %d, want 0", n);
  }
}

// TestExpiredStashWayKeepsSharedEntry (claims A40): an expired stash way of
// K's that points to the hot entry K's live slot uses keeps the entry when
// another key's history takes the way. Only a slot's own exits close its
// entry.
TEST(TestExpiredStashWayKeepsSharedEntry) {
  if (forceCT || forceBG) SKIP("ConstantTime and Background inserts don't install on the call path");
  Config cfg = cfgOf(1000, 1min, 1ms);
  cfg.Hot = 4;
  cfg.Clock = [] { return u64{1}; };
  Filter f(cfg);
  Core& c = core(f);
  const u64 t0 = 200'000;
  u64 k = mix64(1);
  for (int n = 0; n < 32; n++) f.TouchAt(k, 0, t0);
  u64 fp = fingerprint(k), tg = tag(k);
  i64 i = c.locate(k, fp, tg);
  u64 id = c.stashID(k, fp, tg);
  if (f.Promoted() != 1 || i < 0 || !isHot(word(c, i + 1))) FATALF("K not promoted");
  // K's old history in the stash, long expired, pointing to K's entry.
  c.stashPut(id, (fp << 48) | 1'000, word(c, i + 1), 1'000);
  StashEntry* set = c.stash.load()->set(id).first;
  u64 tL = t0 + 10;
  for (u64 j = u64{1} << 40;; j++) {  // another key of the set takes the expired way
    u64 h = mix64(j);
    u64 oid = c.stashID(h, fingerprint(h), tag(h));
    if (c.stash.load()->set(oid).first != set || oid == id) continue;
    u64 w1 = c.nextReg(0, tag(h), tL, tL, true).first;
    c.stashPut(oid, (fingerprint(h) << 48) | tL, w1, tL);
    break;
  }
  if (i64 n = f.HotInUse(); n != 1) FATALF("hot entries in use: %d, want 1 (K's live one)", n);
  f.TouchAt(k, 0, tL);
  if (Result r = f.CountAt(k, c.horizon, tL); r.Upper < 33)
    FATALF("K reads %+v after another key took its expired stash way, want Upper >= 33", r);
}
