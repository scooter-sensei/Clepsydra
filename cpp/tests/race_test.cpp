// The Go suite's remaining hook-driven and racy tests, each under its Go name:
// go/slotunit_race_test.go (claims D29), go/roomrace_test.go,
// go/gaprace_test.go (A21), go/reloc_race_test.go, go/dup_test.go,
// go/abandon_test.go (A23, A29), go/read_race_test.go,
// go/restorewait_test.go and go/mirror_test.go.
#include <atomic>
#include <cstdlib>
#include <map>
#include <set>
#include <sstream>
#include <thread>
#include <vector>

#include "check.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

namespace {
const u64 sweepT0 = 60'000;
u64 word(Core& f, i64 i) { return f.words[static_cast<std::size_t>(i)].load(); }

// ---- go/slotunit_race_test.go ----

// slotUnitTicks: K's burst at t0 (unit 24), touches at t1 (unit 34, 700 ticks
// in, onset 4) and t2 (unit 47, within a horizon of t0).
const u64 slotUnitT1 = sweepT0 + 10 * 2'500 + 700;
const u64 slotUnitT2 = sweepT0 + 23 * 2'500 + 1'900;

// slotUnitKey: a filter and K, promoted by a burst of 40 touches at t0; with
// reclaim, the entry is then taken from K as the hot table's reclaim does.
std::pair<std::unique_ptr<Filter>, u64> slotUnitKey(bool reclaim) {
  auto f = newMinuteFilter(1000);
  Core& c = core(*f);
  u64 k = keyAt(c, 7, twoLineFPs(c, 7, 0x6600, 1)[0], 0x321);
  for (int n = 0; n < 40; n++) f->TouchAt(k, 0, sweepT0);
  auto sl = slotsOf(c, k);
  if (sl.size() != 1 || f->Promoted() != 1 || !isHot(word(c, sl[0] + 1)))
    FATALF("setup: K in %v, promoted %d, not hot", sl, f->Promoted());
  if (reclaim) {
    HotEntry& e = c.hot.load()->e[static_cast<std::size_t>(hotIdx(word(c, sl[0] + 1)))];
    u64 ver = e.lock();
    e.id.store(0);
    e.unlock(ver);
  }
  return {std::move(f), k};
}

// slotUnitState: K's slot words and its answers at every window, at t2 and
// later, with the hot table's counters.
std::string slotUnitState(Filter& f, u64 k) {
  Core& c = core(f);
  std::ostringstream b;
  auto sl = slotsOf(c, k);
  b << "slots " << sl.size();
  for (i64 i : sl) b << check::format(" words %#x %#x", word(c, i), word(c, i + 1));
  for (u64 at : {slotUnitT2, slotUnitT2 + c.unit, slotUnitT2 + 10 * c.unit})
    for (u64 w : {500, 1'000, 2'500, 5'000, 15'000, 30'000, 60'000})
      b << check::format("\n  at %d window %5d: %+v", at, w, f.CountAt(k, w, at));
  b << check::format("\n  promoted %d demoted %d", f.Promoted(), f.Demoted());
  return b.str();
}

void slotUnitCompare(const std::string& raced, const std::string& want) {
  if (raced != want) FATALF("after the race:\n%s\nsequential:\n%s", raced, want);
}

// awaitFlag waits for another touch to reach its hook, up to 5 s (the
// subtest then fails on what did not happen).
void awaitFlag(const std::atomic<bool>& b) {
  auto end = std::chrono::steady_clock::now() + 5s;
  while (!b.load() && std::chrono::steady_clock::now() < end) std::this_thread::yield();
}

void slotUnitDemote() {
  Hooks hooks;
  auto [ref, k] = slotUnitKey(false);
  ref->TouchAt(k, 0, slotUnitT1);  // B: demotes at u1
  if (ref->Demoted() != 1) FATALF("setup: B's touch did not demote K (demoted %d)", ref->Demoted());
  ref->TouchAt(k, 0, slotUnitT2);  // A: realigns to u2
  std::string want = slotUnitState(*ref, k);

  auto [f, k2] = slotUnitKey(false);
  (void)k2;
  std::atomic<int> stamped{0}, adding{0};
  std::atomic<bool> aStamped{false}, release{false};
  DonePtr aDone;
  testHookStamped = [&] {
    if (stamped.fetch_add(1) + 1 == 2) {  // A's stamp, into u2: hold A there
      aStamped.store(true);
      while (!release.load()) std::this_thread::yield();
    }
  };
  testHookHotAdding = [&](i64) {
    if (adding.fetch_add(1) + 1 == 1) {  // B, before it adds at u1: A stamps first
      aDone = go([&] { f->TouchAt(k, 0, slotUnitT2); });
      awaitFlag(aStamped);
    }
  };
  f->TouchAt(k, 0, slotUnitT1);  // B
  release.store(true);
  WAIT_FOR(aDone, "A's touch");
  testHookStamped = nullptr;
  testHookHotAdding = nullptr;
  if (stamped.load() < 2) FATALF("A did not stamp during B's touch");
  slotUnitCompare(slotUnitState(*f, k), want);
}

void slotUnitStamp() {
  Hooks hooks;
  auto [ref, k] = slotUnitKey(false);
  ref->TouchAt(k, 0, slotUnitT1);  // E (as D's stamp and E's count): demotes at u1
  if (ref->Demoted() != 1) FATALF("setup: the touch at t1 did not demote K (demoted %d)", ref->Demoted());
  ref->TouchAt(k, 0, slotUnitT2);  // A: realigns to u2
  ref->TouchAt(k, 0, slotUnitT1);  // D's count: behind the last touch, counted at it
  std::string want = slotUnitState(*ref, k);

  auto [f, k2] = slotUnitKey(false);
  (void)k2;
  std::atomic<int> stamping{0}, stamped{0};
  Filter* fp = f.get();
  testHookStamped = [&] {
    if (stamped.fetch_add(1) + 1 == 1) fp->TouchAt(k, 0, slotUnitT2);  // D has stamped word0 into u1: A's touch
  };
  testHookStamping = [&] {
    if (stamping.fetch_add(1) + 1 == 2) fp->TouchAt(k, 0, slotUnitT1);  // A has read word0 and the pointer: E demotes
  };
  f->TouchAt(k, 0, slotUnitT1);  // D
  testHookStamped = nullptr;
  testHookStamping = nullptr;
  if (stamping.load() < 2 || f->Demoted() != 1)
    FATALF("the interleaving did not happen (stamps %d, demoted %d)", stamping.load(), f->Demoted());
  slotUnitCompare(slotUnitState(*f, k), want);
}

void slotUnitUnpoint() {
  Hooks hooks;
  auto [ref, k] = slotUnitKey(true);
  ref->TouchAt(k, 0, slotUnitT2);  // T: resets the pointer at u2
  ref->TouchAt(k, 0, slotUnitT1);  // S: behind the last touch, counted at it
  std::string want = slotUnitState(*ref, k);

  auto [f, k2] = slotUnitKey(true);
  (void)k2;
  std::atomic<int> stamped{0}, adding{0};
  std::atomic<bool> tStamped{false}, release{false};
  DonePtr tDone;
  testHookStamped = [&] {
    if (stamped.fetch_add(1) + 1 == 2) {  // T's stamp, into u2: hold T there
      tStamped.store(true);
      while (!release.load()) std::this_thread::yield();
    }
  };
  testHookHotAdding = [&](i64) {
    if (adding.fetch_add(1) + 1 == 1) {  // S has stamped word0 into u1 and read it: T stamps
      tDone = go([&] { f->TouchAt(k, 0, slotUnitT2); });
      awaitFlag(tStamped);
    }
  };
  f->TouchAt(k, 0, slotUnitT1);  // S
  release.store(true);
  WAIT_FOR(tDone, "T's touch");
  testHookStamped = nullptr;
  testHookHotAdding = nullptr;
  if (stamped.load() < 2) FATALF("T did not stamp during S's touch");
  slotUnitCompare(slotUnitState(*f, k), want);
}

void touchesRacingAfterGap() {
  Hooks hooks;
  for (u64 g = 1; g <= 30; g++) {
    Config cfg = cfgOf(1000, 48ms, 1ms);
    cfg.Clock = [] { return u64{1}; };
    cfg.Hot = -1;
    Filter f(cfg);
    Core& c = core(f);
    u64 k = mix64(99), u = c.unit, t0 = 10'000;
    f.TouchAt(k, 0, t0);
    u64 nowX = t0 + g * u;
    DonePtr done;
    bool fired = false;
    testHookStamped = [&] {
      if (fired) return;
      fired = true;
      done = go([&] { f.TouchAt(k, 0, nowX + 1); });  // Y
      done->wait(10ms);                               // Y runs whole, or waits for X
    };
    f.TouchAt(k, 0, nowX);  // X
    if (done) done->wait(std::chrono::hours(1));
    testHookStamped = nullptr;
    if (!fired) FATALF("gap %d: X's touch never moved the last touch on", g);
    if (Result r = f.CountAt(k, 2 * u, nowX + 1); r.Upper < 2 && !r.Saturated)
      FATALF("gap %d units: the last two units read %+v, want Upper >= 2 (X and Y)", g, r);
    i64 want = (nowX + 1 - t0 <= c.horizon) ? 3 : 2;
    if (Result r = f.CountAt(k, c.horizon, nowX + 1); r.Upper < want && !r.Saturated)
      FATALF("gap %d units: the horizon reads %+v, want Upper >= %d", g, r, want);
  }
}

i64 findAny(Core& f, u64 line, u64 fp, u64 tg) {
  if (i64 i = f.find(line, fp << 1, tg); i >= 0) return i;
  return f.find(line, (fp << 1) | 1, tg);
}
}  // namespace

// TestSlotUnitRaces (claims D29): a register stores no unit and is read at
// word0's, so the three writers that put a register beside a word0 they did
// not move, or move word0 beside a register they did not write, are pinned
// to word0's unit (SlotUnit.tla). Each subtest drives the interleaving the
// model's plain variant breaks, by 16-byte CAS and by hold, and requires the
// slot and every answer to be what a sequential order of the same touches
// leaves.
TEST(TestSlotUnitRaces) {
  LOGF("-- demote");
  realignModes(slotUnitDemote);
  LOGF("-- stamp");
  realignModes(slotUnitStamp);
  LOGF("-- unpoint");
  realignModes(slotUnitUnpoint);
}

// TestReadWhileTouchTakesWaitingKey (go/roomrace_test.go): with Background, a
// touch of a key waiting in the wait room takes it out under the key's
// stripe and then installs it: meanwhile the key is in neither place. If the
// placer clears the set's wait bit then, a lock-free read must still find
// the key's history.
TEST(TestReadWhileTouchTakesWaitingKey) {
  if (forceCT) SKIP("Background has no effect with ConstantTime");
  Hooks hooks;
  Config cfg = cfgOf(4096, 1min, 1ms);
  cfg.Clock = [] { return u64{1}; };
  cfg.Background = true;
  Filter f(cfg);
  Core& c = core(f);
  c.bgManual = true;
  u64 key = 0;
  for (u64 i = 1; key == 0; i++) {  // a key whose lines are full waits
    u64 w = f.Waited();
    f.TouchAt(mix64(i), 0, 1);
    if (f.Waited() != w) key = mix64(i);
  }
  f.TouchAt(key, 0, 1);
  f.TouchAt(key, 0, 1);  // three events, still waiting
  if (f.Waiting() != 1 || c.locate(key, fingerprint(key), tag(key)) >= 0)
    FATALF("%d waiting; want the key alone, not in the table", f.Waiting());
  Result got;
  DonePtr done;
  bool fired = false;
  testHookTaken = [&] {
    if (fired) return;
    fired = true;
    c.placeNext(c.room.load(), 0);  // the placer finds the set empty and clears its bit
    done = during([&] { got = f.CountAt(key, 60'000, 1); });
  };
  f.TouchAt(key, 0, 1);
  if (!fired) FATALF("the touch did not take the key from the wait room");
  WAIT_FOR(done, "the read");
  testHookTaken = nullptr;
  if (got.Upper < 3 && !got.Saturated) FATALF("read during the touch: %+v, want Upper >= 3", got);
  if (Result r = f.CountAt(key, 60'000, 1); r.Count != 4) FATALF("after: %+v, want 4 events", r);
}

// TestWaitBesidePlacementKeepsKey (go/roomrace_test.go; claims A24): with
// Background, the placer commits a waiting key L by marking its wait-room
// entry busy, installing it, and then removing the entry. A first touch of K
// meanwhile, whose room set is L's, waits in that set, where L's entry looks
// expired. K must not take L's busy way, and must keep its event.
TEST(TestWaitBesidePlacementKeepsKey) {
  if (forceCT) SKIP("Background has no effect with ConstantTime");
  Hooks hooks;
  Config cfg = cfgOf(4096, 1min, 1ms);
  cfg.Clock = [] { return u64{1}; };
  cfg.Background = true;
  cfg.Hot = -1;
  Filter f(cfg);
  Core& c = core(f);
  c.bgManual = true;
  const u64 t0 = 1;
  u64 t1 = t0 + c.horizon + 1;  // entries last touched at t0 have expired
  u64 l = 0;
  for (u64 i = 1; l == 0; i++) {  // a key whose lines are full waits
    u64 w = f.Waited();
    f.TouchAt(mix64(i), 0, t0);
    if (f.Waited() != w) l = mix64(i);
  }
  StashTable* p = c.room.load();
  auto set = [&](u64 h) { return mulHi(mix64(c.stashID(h, fingerprint(h), tag(h))), p->n); };
  auto lines = [&](u64 h) {
    u64 pl = c.primary(h);
    return std::array<u64, 2>{pl, c.alt(pl, fingerprint(h))};
  };
  auto ll = lines(l);
  u64 k = 0;  // L's room set; two lines, neither of them L's; a claim stripe of its own
  for (u64 i = u64{1} << 40; k == 0; i++) {
    u64 h = mix64(i);
    auto kl = lines(h);
    if (set(h) == set(l) && (h >> 56) != (l >> 56) && kl[0] != kl[1] && kl[0] != ll[0] && kl[0] != ll[1] &&
        kl[1] != ll[0] && kl[1] != ll[1])
      k = h;
  }
  for (u64 line : lines(k)) {  // K's lines full of entries live at t1, so its first touch waits
    for (u64 i = u64{1} << 44; countLine(c, line, t1) < 4; i++)
      if (u64 h = mix64(i); c.primary(h) == line) f.TouchAt(h, 0, t1);
  }
  for (u64 line : ll) c.sweepLine(line, t1);  // room for L: its lines' entries from t0 are swept
  if (f.Waiting() != 1) FATALF("%d keys waiting, want L alone", f.Waiting());
  bool fired = false;
  testHookPlacing = [&] {
    if (fired) return;
    fired = true;
    u64 w = f.Waited();
    f.TouchAt(k, 0, t1);  // K waits in L's set while L's entry is busy
    if (f.Waited() == w) ERRORF("K's first touch did not wait");
  };
  c.placeNext(p, 0);  // places L
  testHookPlacing = nullptr;
  if (!fired) FATALF("the placer did not commit L");
  if (c.locate(l, fingerprint(l), tag(l)) < 0) FATALF("L is not in the table");
  if (Result r = f.CountAt(k, c.horizon, t1); r.Upper < 1 && !r.Saturated)
    FATALF("K reads %+v after L's placement, want Upper >= 1", r);
  if (Result r = f.TouchAt(k, c.horizon, t1); r.Upper < 2 && !r.Saturated)
    FATALF("K's second touch reads %+v, want Upper >= 2", r);
}

// TestTouchesRacingAfterGap (go/gaprace_test.go; claims A21): K's last touch is
// g units back when touch X moves its last touch to the current unit; before
// X's register follows, a second touch Y of K in the same unit runs. Y must
// count in the current unit.
TEST(TestTouchesRacingAfterGap) {
  if (forceCT) SKIP("ConstantTime touches of a key run one at a time under its stripe");
  realignModes(touchesRacingAfterGap);
}

// TestTouchRacingRelocationKeepsCounts (go/reloc_race_test.go): hot keys
// touched continuously while inserts at ~98% load keep relocating and
// evicting entries: no hot key may end up in two slots or lose a count.
TEST(TestTouchRacingRelocationKeepsCounts) {
  const i64 capN = 20'000;
  auto fp = newTest(capN, 1min);
  Filter& f = *fp;
  Core& c = core(f);
  const u64 hot = 2000, at = 60'000;
  const int initTouches = 3;
  const i64 passes = 200;
  for (u64 k = 0; k < hot; k++)
    for (int n = 0; n < initTouches; n++) f.CheckAndTouchAt(mix64(k), 0, at);
  std::atomic<bool> stop{false};
  std::atomic<i64> touches{0};
  std::vector<std::thread> ts;
  for (int w = 0; w < 6; w++)
    ts.emplace_back([&, w] {
      i64 n = 0;
      for (u64 i = static_cast<u64>(w); !stop.load() && n < passes * static_cast<i64>(hot); i++) {
        f.CheckAndTouchAt(mix64(i % hot), 0, at);
        n++;
      }
      touches.fetch_add(n);
    });
  // fill to ~98% slot load with cold keys, forcing cuckoo moves
  i64 fill = static_cast<i64>(static_cast<double>(f.Slots()) * 0.98) - static_cast<i64>(hot);
  for (u64 k = 0; k < static_cast<u64>(fill); k++) f.CheckAndTouchAt(mix64((u64{1} << 40) + k), 0, at);
  stop.store(true);
  for (auto& t : ts) t.join();
  i64 dups = 0, total = 0;
  for (u64 k = 0; k < hot; k++) {
    u64 h = mix64(k);
    u64 hfp = fingerprint(h), tg = tag(h);
    u64 p = c.primary(h);
    if (findAny(c, p, hfp, tg) >= 0 && findAny(c, c.alt(p, hfp), hfp, tg) >= 0) dups++;
    total += f.VelocityCountAt(h, f.Ticks(1min), at);
  }
  LOGF("relocations %d, evictions %d, hot dups %d, counted %d of %d touches", f.Relocations(), f.Evictions(), dups,
       total, touches.load() + static_cast<i64>(hot) * initTouches);
  if (dups > 0) FATALF("%d hot keys in two slots", dups);
  if (i64 want = touches.load() + static_cast<i64>(hot) * initTouches; total != want)
    FATALF("counted %d of %d touches", total, want);
}

// TestConcurrentFirstInsertNoDuplicates (go/dup_test.go): concurrent first
// touches of the same keys must not leave duplicate slots.
TEST(TestConcurrentFirstInsertNoDuplicates) {
  const i64 keys = 20'000;
  const int workers = 8;
  auto fp = newTest(keys, 1min);
  Filter& f = *fp;
  std::atomic<bool> start{false};
  std::vector<std::thread> ts;
  for (int w = 0; w < workers; w++)
    ts.emplace_back([&] {
      while (!start.load()) std::this_thread::yield();
      for (i64 k = 0; k < keys; k++) f.CheckAndTouchAt(mix64(static_cast<u64>(k)), 0, 1000);
    });
  start.store(true);
  for (auto& t : ts) t.join();
  i64 occ = f.Occupancy(1000);
  // Keys waiting for a slot: ConstantTime's stay in the stash, and
  // Background's placer drains its wait room, so first wait until the count
  // is 0 or has held still for 50 ms, then count slots between two equal
  // reads of it.
  for (i64 n = f.Waiting(), still = 0; n > 0 && still < 50; still++) {
    std::this_thread::sleep_for(1ms);
    if (i64 m = f.Waiting(); m != n) {
      n = m;
      still = 0;
    }
  }
  for (;;) {
    i64 n = f.Waiting();
    occ = f.Occupancy(1000);
    if (f.Waiting() == n) {
      occ += n;
      break;
    }
  }
  // Keys in the stash: Background's placer races the workers' touches for its
  // moves and now and then evicts one.
  occ += static_cast<i64>(f.Evictions() - f.Restored());
  i64 total = 0;
  for (i64 k = 0; k < keys; k++) total += f.VelocityCountAt(mix64(static_cast<u64>(k)), f.Ticks(1min), 1000);
  LOGF("slots used %d for %d keys; counted %d of %d touches", occ, keys, total, keys * workers);
  if (occ != keys) FATALF("%d duplicate slots", occ - keys);
  if (total != keys * workers) FATALF("counts split or lost: %d of %d", total, keys * workers);
}

// TestAbandonedMoveKeepsCounts (go/abandon_test.go; claims A23, A29): few hot
// keys, eight workers and a fill to 99% slot load, repeated: two touches of
// one key often overlap a move of it, so moves get abandoned. No touch may be
// lost. TF_DISTINCT_TAGS=1 keeps cold keys off the hot keys' tags;
// TF_SHARED_TAGS=1 puts every cold key on one.
TEST(TestAbandonedMoveKeepsCounts) {
  int iters = check::short_() ? 30 : 300;
  const char* de = std::getenv("TF_DISTINCT_TAGS");
  const char* se = std::getenv("TF_SHARED_TAGS");
  bool distinct = de && *de, shared = se && *se;
  const u64 hot = 64, at = 60'000;
  const int workers = 8;
  int failed = 0;
  i64 lost = 0;
  for (int it = 0; it < iters; it++) {
    auto fp = newTest(4000, 1min);
    Filter& f = *fp;
    Core& c = core(f);
    u64 salt = static_cast<u64>(it) << 32;
    std::set<u64> hotTags, hotIDs;
    std::vector<u64> hotTagList;
    for (u64 k = 0; k < hot; k++) {
      u64 h = mix64(salt + k);
      hotTags.insert(tag(h));
      hotTagList.push_back(tag(h));
      hotIDs.insert((fingerprint(h) << tagBits) | tag(h));
      for (int n = 0; n < 3; n++) f.CheckAndTouchAt(h, 0, at);
    }
    std::atomic<bool> stop{false};
    std::atomic<i64> touches{0};
    std::vector<i64> ns(workers);
    std::vector<std::thread> ts;
    for (int w = 0; w < workers; w++)
      ts.emplace_back([&, w] {
        i64 n = 0;
        for (u64 i = static_cast<u64>(w / 2); !stop.load() && n < 150 * static_cast<i64>(hot); i++) {
          f.CheckAndTouchAt(mix64(salt + i % hot), 0, at);
          n++;
        }
        ns[static_cast<std::size_t>(w)] = n;
        touches.fetch_add(n);
      });
    i64 fill = static_cast<i64>(static_cast<double>(f.Slots()) * 0.99) - static_cast<i64>(hot);
    std::map<u64, int> fillTags;
    for (u64 k = 0, n = 0; static_cast<i64>(n) < fill; k++) {
      u64 h = mix64(salt + (u64{1} << 40) + k);
      if (shared) {  // the tag is h's bits 36-47
        h = (h & ~(((u64{1} << tagBits) - 1) << 36)) | (hotTagList[k % hotTagList.size()] << 36);
        if (hotIDs.count((fingerprint(h) << tagBits) | tag(h))) continue;
      }
      if (distinct && hotTags.count(tag(h))) continue;
      fillTags[tag(h)]++;
      f.CheckAndTouchAt(h, 0, at);
      n++;
    }
    stop.store(true);
    for (auto& t : ts) t.join();
    i64 total = 0;
    for (u64 k = 0; k < hot; k++) total += f.VelocityCountAt(mix64(salt + k), f.Ticks(1min), at);
    if (i64 want = touches.load() + static_cast<i64>(hot) * 3; total != want) {
      failed++;
      lost += want - total;
      // logOffKeys: each hot key whose count is off, its true count, who shares
      // its tag, and where its history is.
      std::vector<i64> wantK(hot, 3);
      for (int w = 0; w < workers; w++)
        for (u64 i = static_cast<u64>(w / 2);
             i < static_cast<u64>(w / 2) + static_cast<u64>(ns[static_cast<std::size_t>(w)]); i++)
          wantK[i % hot]++;
      for (u64 k = 0; k < hot; k++) {
        u64 h = mix64(salt + k);
        i64 got = f.VelocityCountAt(h, f.Ticks(1min), at);
        if (got == wantK[k]) continue;
        std::vector<u64> same;
        for (u64 j = 0; j < hot; j++)
          if (j != k && hotTagList[j] == tag(h)) same.push_back(j);
        LOGF("round %d, hot key %d: count %d, true %d (%+d); tag %d: hot keys %v and %d cold keys share it; %s", it, k,
             got, wantK[k], got - wantK[k], tag(h), same, fillTags[tag(h)], whereIs(c, h, at));
      }
    }
  }
  LOGF("%d of %d rounds lost counts (%d in all)", failed, iters, lost);
  if (failed > 0) FATALF("%d of %d rounds lost counts", failed, iters);
}

// TestReadDuringRelocationFindsKey (go/read_race_test.go): keys never touched
// again are read continuously while a fill to 98% slot load keeps relocating
// them. A read must find the key wherever it is, and in ConstantTime a miss
// must not insert it a second time.
TEST(TestReadDuringRelocationFindsKey) {
  int rounds = check::short_() ? 8 : 40;
  const i64 capN = 4000;
  const int keys = 256, readers = 6;
  const i64 k = 3;
  const u64 at = 60'000;
  std::atomic<i64> bad{0}, reads{0};
  for (int round = 0; round < rounds; round++) {
    auto fp = newTest(capN, 1min);
    Filter& f = *fp;
    u64 salt = static_cast<u64>(round) << 40;
    std::vector<u64> hs(keys);
    for (int i = 0; i < keys; i++) {
      hs[i] = mix64(salt + static_cast<u64>(i));
      for (int n = 0; n < k; n++) f.CheckAndTouchAt(hs[i], 0, at);
    }
    std::atomic<bool> stop{false};
    std::vector<std::thread> ts;
    for (int w = 0; w < readers; w++)
      ts.emplace_back([&, w] {
        for (int i = w; !stop.load(); i++) {
          u64 h = hs[static_cast<std::size_t>(i % keys)];
          Result r;
          if ((i & 1) == 0) {
            r = f.CountAt(h, 60'000, at);
          } else {
            r.Seen = f.ContainsAt(h, 60'000, at);
            r.Upper = k;
          }
          reads.fetch_add(1);
          if (!r.Seen || r.Upper < k) {
            if (bad.fetch_add(1) + 1 <= 5) LOGF("round %d key %d read %+v", round, i % keys, r);
          }
        }
      });
    i64 fill = static_cast<i64>(static_cast<double>(f.Slots()) * 0.98) - keys;
    for (u64 cc = 0; cc < static_cast<u64>(fill); cc++) f.CheckAndTouchAt(mix64(salt + (u64{1} << 36) + cc), 0, at);
    stop.store(true);
    for (auto& t : ts) t.join();
    for (int i = 0; i < keys; i++) {
      if (i64 n = f.VelocityCountAt(hs[i], 60'000, at); n != k) {
        bad.fetch_add(1);
        LOGF("round %d key %d ends with count %d", round, i, n);
      }
    }
  }
  LOGF("%d reads, %d missed a key or read it low", reads.load(), bad.load());
  if (bad.load() > 0) FATALF("%d reads missed a key that was there", bad.load());
}

// TestEvictedKeyThatWaitsIsRestored (go/restorewait_test.go): in Background and
// ConstantTime a first touch that finds both lines full waits for a slot. An
// evicted key's touch that does so still brought its history back from the
// stash: that answer is Restored and Restored() counts it, once; the touch
// that later installs the waiting key isn't.
TEST(TestEvictedKeyThatWaitsIsRestored) {
  for (const char* mode : {"Background", "ConstantTime"}) {
    LOGF("-- %s", mode);
    // A 64-entry stash: one waiting key doesn't force placement.
    Config cfg = cfgOf(4, 10s, 1ms);
    cfg.Clock = [] { return u64{1}; };
    cfg.Stash = 64;
    Filter f(cfg);
    Core& c = core(f);
    c.ct = false;  // evict on the call path first (TF_CT or TF_BG would make inserts wait)
    c.bg = false;
    u64 k = 0;
    for (; k < 64 && f.Evictions() == 0; k++) f.TouchAt(mix64(k), 0, 1000);
    u64 v = 0;
    for (u64 i = 0; i < k; i++) {
      std::string w = whereIs(c, mix64(i), 1000);
      if (w.find("stash") != std::string::npos && w.find("slot") == std::string::npos) v = mix64(i);
    }
    if (v == 0) FATALF("no evicted key in the stash after %d touches (evictions %d)", k, f.Evictions());
    if (std::string(mode) == "Background") {
      c.bg = true;
      c.bgManual = true;
    } else {
      c.ct = true;
    }
    // Both of v's lines are full and live: v waits, its history back.
    Result r = f.TouchAt(v, c.horizon, 1000);
    if (f.Waited() != 1) FATALF("v didn't wait for a slot (waited %d; %s)", f.Waited(), whereIs(c, v, 1000));
    if (!r.Restored || r.Count != 2 || !r.Seen || f.Restored() != 1)
      FATALF("waiting touch of an evicted key: %+v, Restored() %d; want count 2, seen, restored, Restored() 1", r,
             f.Restored());
    // Free a slot of v's primary line: v's next touch installs it.
    std::size_t base = static_cast<std::size_t>(c.primary(v) * wordsPerLine);
    c.words[base].store(0);
    c.words[base + 1].store(0);
    r = f.TouchAt(v, c.horizon, 1000);
    if (c.locate(v, fingerprint(v), tag(v)) < 0) FATALF("v not installed: %s", whereIs(c, v, 1000));
    if (r.Restored || r.Count != 3 || f.Restored() != 1)
      FATALF("touch that installs the waiting key: %+v, Restored() %d; want count 3, not restored, Restored() 1", r,
             f.Restored());
  }
}

// TestMirrorKeyInFoundSlotNotCounted (go/mirror_test.go): keys H and K share
// their fingerprint and tag, and K's primary line is H's alternate, so K
// lives in H's primary line displaced: only the displaced bit tells their
// entries apart. When K's entry takes the slot a lookup found H in, H's touch
// and read must not take K's slot for H's.
TEST(TestMirrorKeyInFoundSlotNotCounted) {
  Config cfg = cfgOf(54, 1min, 1ms);
  cfg.Clock = [] { return u64{1}; };
  Filter f(cfg);
  Core& c = core(f);
  const u64 fp = 0x1234, tg = 0x567;
  u64 p = 0;
  if (c.alt(p, fp) == p) p = 1;
  u64 q = c.alt(p, fp);
  auto withPrimary = [&](u64 line) { return (fp << 48) | (tg << 36) | (((line << 36) + c.lines - 1) / c.lines); };
  u64 h = withPrimary(p), k = withPrimary(q);
  if (c.primary(h) != p || c.primary(k) != q || fingerprint(h) != fp || tag(h) != tg || q == p)
    FATALF("keys not built as meant: lines %d, %d (want %d, %d)", c.primary(h), c.primary(k), p, q);
  const u64 t0 = 60'000;
  f.TouchAt(h, 0, t0);
  i64 i0 = c.locate(h, fp, tg);
  if (i0 < 0 || static_cast<u64>(i0 / wordsPerLine) != p) FATALF("H not in its primary line (slot %d)", i0);
  // K's entry, displaced, in the slot H was found in.
  u64 kw1 = c.nextReg(0, tg, t0, t0, true).first;
  c.words[static_cast<std::size_t>(i0)].store((fp << 48) | dispBit | t0);
  c.words[static_cast<std::size_t>(i0) + 1].store(kw1);
  if (c.locate(k, fp, tg) != i0) FATALF("K not found in H's old slot");
  if (Found r = c.read(h, t0, i0); r.src == srcTable && (r.w0 & dispBit) != 0)
    ERRORF("H's read from the found slot returned K's words");
  if (forceCT) {
    f.TouchAt(h, 0, t0 + 1);  // ConstantTime's touch takes no slot from warm: the lookup path only
  } else {
    c.touch(h, c.horizon, t0 + 1, true, i0);  // as a batch does, from warm's slot
  }
  if (Result r = f.CountAt(k, c.horizon, t0 + 1); r.Count != 1)
    FATALF("K reads %+v after H's touch, want count 1: H's touch counted in K's slot", r);
  if (c.locate(h, fp, tg) < 0) FATALF("H's touch went to no slot");
}
