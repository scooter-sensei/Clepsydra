// go/background_test.go, go/ctwait_test.go and go/disp_test.go: keys waiting
// for a slot (Background's wait room, ConstantTime's stash) and the
// displaced bit.
#include <atomic>
#include <thread>
#include <vector>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

namespace {
// whereKey counts h's entries: in the table (as lookups match) and in the
// stash.
std::pair<int, int> whereKey(Core& f, u64 h) {
  u64 fp = fingerprint(h), tg = tag(h);
  u64 p = f.primary(h);
  int table = 0, stash = 0;
  if (f.find(p, fp << 1, tg) >= 0) table++;
  if (f.find(f.alt(p, fp), (fp << 1) | 1, tg) >= 0) table++;
  if (StashTable* st = f.stash.load()) {
    u64 id = f.stashID(h, fp, tg);
    auto [set, mu] = st->set(id);
    for (u64 i = 0; i < stashWays; i++)
      if (set[i].id == id) stash++;
  }
  return {table, stash};
}

// ctFilter is a ConstantTime filter with a pad long enough to leave room for
// placing waiting keys, and a stash big enough never to drop a history.
std::unique_ptr<Filter> ctFilter(i64 capacity) {
  Config cfg = cfgOf(capacity, 1min, 1ms);
  cfg.ConstantTime = 4us;
  cfg.Stash = capacity;
  return std::make_unique<Filter>(cfg);
}

// findAny is find ignoring the displaced bit.
i64 findAny(Core& f, u64 line, u64 fp, u64 tg) {
  if (i64 i = f.find(line, fp << 1, tg); i >= 0) return i;
  return f.find(line, (fp << 1) | 1, tg);
}
}  // namespace

// TestBackgroundWaitsAndPlaces: with Background, a first touch whose lines are
// full and that needs a move waits in the wait room instead of searching. Its
// touches and counts find it there, the placer moves it into the table with
// its history, and a filter that never evicts holds no stash. The placer runs
// by hand here (bgManual).
TEST(TestBackgroundWaitsAndPlaces) {
  if (forceCT) SKIP("Background has no effect with ConstantTime");
  Config cfg = cfgOf(4096, 1min, 1ms);
  cfg.Clock = [] { return u64{1}; };
  cfg.Background = true;
  Filter f(cfg);
  Core& c = core(f);
  c.bgManual = true;
  u64 key = 0;
  for (u64 i = 1; key == 0; i++) {
    u64 w = f.Waited();
    f.TouchAt(mix64(i), 0, 1);
    if (f.Waited() != w) key = mix64(i);
  }
  u64 fp = fingerprint(key), tg = tag(key);
  if (c.locate(key, fp, tg) >= 0 || f.Waiting() != 1) FATALF("waiting key in the table, or %d waiting", f.Waiting());
  for (int n = 2; n <= 5; n++) {
    if (Result r = f.TouchAt(key, 60'000, 1); r.Count != n || !r.Seen || r.Restored)
      FATALF("touch %d of a waiting key: %+v", n, r);
  }
  if (Result r = f.CountAt(key, 60'000, 1); r.Count != 5 || r.Upper != 5) FATALF("count of a waiting key: %+v", r);
  while (f.Waiting() > 0) c.placeNext(c.room.load(), 0);
  if (c.locate(key, fp, tg) < 0) FATALF("placed key not in the table");
  if (Result r = f.CountAt(key, 60'000, 1); r.Count != 5 || r.Upper != 5) FATALF("count of the placed key: %+v", r);
  if (c.stash.load() || f.Evictions() != 0)
    FATALF("stash allocated (%v) or %d evictions without a full table", c.stash.load() != nullptr, f.Evictions());
  LOGF("waited %d, relocations %d, bytes %d (table %d)", f.Waited(), f.Relocations(), f.Bytes(), 16 * f.Slots());
}

// TestBackgroundConcurrentCounts: 8 workers touch 32 keys 20 times each in a
// table of ~76 slots while flooding it with heavier keys, with Background on:
// keys wait in the wait room, are evicted to the stash and come back, and the
// placer moves waiting keys into the table without their claim stripe, all
// concurrently with the keys' own touches. Every key must end with its exact
// count, in at most one slot.
TEST(TestBackgroundConcurrentCounts) {
  if (forceCT) SKIP("Background has no effect with ConstantTime");
  const int workers = 8, hotN = 32, perKey = 20;
  int rounds = check::short_() ? 40 : 200;
  u64 waited = 0;
  for (int round = 0; round < rounds; round++) {
    std::atomic<u64> clock{1};
    Config cfg = cfgOf(64, 1min, 1ms);
    cfg.Clock = [&clock] { return clock.load(); };
    cfg.Stash = 1 << 12;
    cfg.SpillMin = 1;
    cfg.Background = true;
    Filter f(cfg);
    Core& c = core(f);
    std::vector<u64> keys(hotN);
    for (int i = 0; i < hotN; i++) keys[i] = mix64((static_cast<u64>(round) << 32) | static_cast<u64>(i));
    std::vector<std::thread> ts;
    for (int w = 0; w < workers; w++)
      ts.emplace_back([&, w] {
        gorand::Rand rng(static_cast<u64>(round), static_cast<u64>(w));
        for (int i = 0; i < hotN * perKey / workers; i++) {
          u64 now = clock.fetch_add(25) + 25;
          f.TouchAt(keys[static_cast<std::size_t>((i * workers + w) % hotN)], 0, now);
          if (i % 2 == 0) {
            u64 h = rng.Uint64();
            for (int k = 0; k < 25; k++) f.TouchAt(h, 0, now);
          }
        }
      });
    for (auto& t : ts) t.join();
    u64 now = clock.load();
    for (int i = 0; i < hotN; i++) {
      Result r = f.CountAt(keys[i], f.Ticks(1min), now);
      if (r.Count != perKey || r.Saturated)
        FATALF("round %d key %d: %+v, want count %d (waited %d, evictions %d, restored %d, lost %d)", round, i, r,
               perKey, f.Waited(), f.Evictions(), f.Restored(), f.Lost());
    }
    settle(f);
    for (int i = 0; i < hotN; i++) {
      u64 h = keys[i];
      u64 fp = fingerprint(h), tg = tag(h);
      u64 p = c.primary(h);
      int n = 0;
      u64 ls[2] = {p, c.alt(p, fp)};
      for (int li = 0; li < 2; li++) {
        if (li == 1 && ls[1] == p) break;
        for (int s = 0; s < wordsPerLine; s += 2) {
          std::size_t j = static_cast<std::size_t>(ls[li] * wordsPerLine) + static_cast<std::size_t>(s);
          u64 w0 = c.words[j].load(), w1 = c.words[j + 1].load();
          if ((w0 >> 48) == fp && (w1 >> tagShift) == tg && ((w0 & dispBit) != 0) == (li == 1)) n++;
        }
      }
      if (n > 1) FATALF("round %d key %d: in %d slots", round, i, n);
    }
    waited += f.Waited();
  }
  LOGF("%d rounds: %d waits", rounds, waited);
}

// TestCTWaitingKeyNotRestored: a key that waited for a slot and is touched
// again once one is free was never evicted: its answer isn't Restored, and
// Restored() doesn't count it.
TEST(TestCTWaitingKeyNotRestored) {
  Config cfg = cfgOf(256, 1min, 1ms);
  cfg.ConstantTime = 1us;
  cfg.Stash = 256;
  Filter f(cfg);
  Core& c = core(f);
  for (u64 i = 0; i < static_cast<u64>(f.Slots() * 85 / 100); i++) f.TouchAt(mix64(i), 0, 1);
  u64 key = 0;
  for (u64 i = u64{1} << 40; key == 0; i++) {
    u64 w = f.Waited();
    f.TouchAt(mix64(i), 0, 30'000);
    if (f.Waited() != w) key = mix64(i);
  }
  if (auto [tb, st] = whereKey(c, key); tb != 0 || st != 1)
    FATALF("key not waiting: %d table and %d stash entries", tb, st);
  Result r = f.TouchAt(key, 60'000, 60'002);
  if (int tb = whereKey(c, key).first; tb != 1) FATALF("key not placed by its touch: %d table entries", tb);
  if (r.Count != 2 || !r.Seen || r.Restored || f.Restored() != 0)
    FATALF("touch of a key that waited: %+v, Restored() %d; want count 2, seen, not restored", r, f.Restored());
}

// TestCTWaitingKeysKeepCounts: in ConstantTime an insert that finds its two
// lines full waits in the stash instead of searching, and the pads of later
// calls place it. Filled to 97% of the slots, every key must keep its exact
// count, in one place, before and after placement has drained the stash.
TEST(TestCTWaitingKeysKeepCounts) {
  const i64 capacity = 4096;
  const int per = 3;
  auto fp = ctFilter(capacity);
  Filter& f = *fp;
  Core& c = core(f);
  i64 n = f.Slots() * 97 / 100;
  for (int round = 0; round < per; round++)
    for (i64 i = 0; i < n; i++) f.TouchAt(mix64(static_cast<u64>(i)), 0, 1000 + static_cast<u64>(round));
  auto checkAll = [&](const char* when) {
    for (i64 i = 0; i < n; i++) {
      u64 h = mix64(static_cast<u64>(i));
      if (Result r = f.CountAt(h, 60'000, 2000); r.Count != per || r.Saturated)
        FATALF("%s: key %d reads %+v, want %d events", when, i, r, per);
      if (auto [tb, st] = whereKey(c, h); tb + st != 1)
        FATALF("%s: key %d has %d table and %d stash entries", when, i, tb, st);
    }
  };
  checkAll("after the inserts");
  i64 waiting = f.Waiting();
  for (int k = 0; k < 1000; k++) {  // pads of any length (the race detector slows a step past 4 µs)
    if (f.Waiting() == 0) break;
    c.place(Clock::now(), 1ms, 2000);
  }
  LOGF(
      "%d keys at 97%% of %d slots: %d inserts waited, %d waiting after the inserts and Counts, %d after placing; "
      "evictions %d, folded %d",
      n, f.Slots(), f.Waited(), waiting, f.Waiting(), f.Evictions(), f.Folded());
  if (f.Waited() == 0) FATALF("no insert waited: the test doesn't exercise placement");
  if (f.Waiting() != 0) FATALF("%d keys still wait", f.Waiting());
  checkAll("after placing");
}

// TestCTWaitingKeysUnderRaces: workers touch shared keys at 98% load in
// ConstantTime, so inserts wait in the stash and pads place them while other
// workers touch the same keys. Every key must end in one place with its exact
// count.
TEST(TestCTWaitingKeysUnderRaces) {
  int rounds = check::short_() ? 3 : 10;
  const i64 capacity = 4096;
  const int workers = 8, per = 4;
  for (int round = 0; round < rounds; round++) {
    auto fp = ctFilter(capacity);
    Filter& f = *fp;
    Core& c = core(f);
    i64 n = f.Slots() * 98 / 100;
    std::vector<u64> keys(static_cast<std::size_t>(n));
    for (i64 i = 0; i < n; i++)
      keys[static_cast<std::size_t>(i)] = mix64((static_cast<u64>(round) << 40) | static_cast<u64>(i));
    std::vector<std::thread> ts;
    for (int w = 0; w < workers; w++)
      ts.emplace_back([&, w] {
        for (i64 p = 0; p < per * n / workers; p++)
          f.TouchAt(keys[static_cast<std::size_t>((p * workers + w) % n)], 0, 1000);
      });
    for (auto& t : ts) t.join();
    i64 total = 0;
    for (i64 i = 0; i < n; i++) {
      u64 h = keys[static_cast<std::size_t>(i)];
      total += f.CountAt(h, 60'000, 1000).Count;
      if (auto [tb, st] = whereKey(c, h); tb + st != 1)
        FATALF("round %d key %d has %d table and %d stash entries", round, i, tb, st);
    }
    if (i64 want = per * n / workers * workers; total != want)
      FATALF("round %d: counted %d of %d touches (waiting %d, evictions %d, folded %d)", round, total, want,
             c.deferred.load(), f.Evictions(), f.Folded());
  }
}

// TestDisplacedBitTracksLine (go/disp_test.go): under concurrent churn with
// relocations, evictions and sweeps, every entry's displaced bit must say
// which of its two lines it is in; otherwise locate would miss it.
TEST(TestDisplacedBitTracksLine) {
  const u64 capN = 20'000;
  const int workers = 8;
  auto fp = newTest(static_cast<i64>(capN), 1s);
  Filter& f = *fp;
  Core& c = core(f);
  u64 horizon = f.Ticks(1s);
  std::atomic<u64> clock{1000};
  auto key = [&](u64 k, u64 now) { return mix64(k + now / horizon * capN); };
  std::vector<std::thread> ts;
  for (int w = 0; w < workers; w++)
    ts.emplace_back([&, w] {
      for (int i = 0; i < 300'000; i++) {
        u64 now = (clock.fetch_add(1) + 1) * horizon / 100'000;   // ~24 horizons overall
        u64 k = static_cast<u64>(w * 7919 + i) % (capN * 6 / 5);  // 1.2x capacity live
        f.CheckAndTouchAt(key(k, now), horizon, now);
        if (i % 97 == 0) c.decayStep(now, 4);
      }
    });
  for (auto& t : ts) t.join();
  LOGF("relocations %d, evictions %d", f.Relocations(), f.Evictions());
  u64 now = clock.load() * horizon / 100'000;
  int found = 0;
  for (u64 k = 0; k < capN * 6 / 5; k++) {
    u64 h = key(k, now);
    u64 hfp = fingerprint(h), tg = tag(h);
    u64 p = c.primary(h);
    i64 inP = findAny(c, p, hfp, tg), inAlt = findAny(c, c.alt(p, hfp), hfp, tg);
    if (inP >= 0 && (c.words[static_cast<std::size_t>(inP)].load() & dispBit) != 0)
      FATALF("key %d in its primary line marked displaced", k);
    if (inP < 0 && inAlt >= 0 && (c.words[static_cast<std::size_t>(inAlt)].load() & dispBit) == 0)
      FATALF("key %d in its alternate line not marked displaced", k);
    if ((c.locate(h, hfp, tg) >= 0) != (inP >= 0 || inAlt >= 0))
      FATALF("key %d: locate disagrees with a two-line search", k);
    if (inP >= 0 || inAlt >= 0) found++;
  }
  if (found == 0) FATALF("no keys survived; test exercises nothing");
}
