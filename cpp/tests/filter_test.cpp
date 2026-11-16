// go/filter_test.go: the filter's basic contract. TestSprayCannotEvictRepeatOffender
// and TestBatchMatchesSingle are in properties_test.cpp.
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

TEST(TestAlignment) {
  auto f = newTest(1000, 1min);
  auto p = reinterpret_cast<std::uintptr_t>(core(*f).words.data());
  if (p % 64 != 0) FATALF("table not cache-line aligned: %#x", p);
}

TEST(TestCheckAndTouchWindows) {
  auto fp = newTest(1000, 1min);
  Filter& f = *fp;
  auto ms = [&](Duration d) { return f.Ticks(d); };
  u64 h = mix64(42);
  u64 now = 1000;
  if (f.CheckAndTouchAt(h, ms(10s), now)) FATALF("first touch reported seen");
  if (!f.CheckAndTouchAt(h, ms(10s), now + ms(9s))) FATALF("touch 9s later not seen within 10s");
  now += ms(9s);
  // 20s gap: outside a 10s window, inside a 30s one. The window is a query
  // parameter, not a table property.
  u64 k2 = mix64(43);
  f.CheckAndTouchAt(k2, 0, now);
  if (f.CheckAndTouchAt(h, ms(10s), now + ms(20s))) FATALF("20s gap reported seen within 10s");
  if (!f.CheckAndTouchAt(k2, ms(30s), now + ms(20s))) FATALF("20s gap not seen within 30s");
  // Past the horizon the key is gone regardless of window.
  if (f.CheckAndTouchAt(k2, ms(1h), now + ms(20s) + ms(61s))) FATALF("key seen after horizon expired");
}

TEST(TestExactBoundary) {
  auto f = newTest(1000, 1min);
  u64 h = mix64(7);
  f->CheckAndTouchAt(h, 0, 5000);
  if (!f->CheckAndTouchAt(h, 3000, 8000)) FATALF("age == window must be seen");
  if (f->CheckAndTouchAt(h, 2999, 11000)) FATALF("age == window+1 must not be seen");
}

// TestVelocityAccuracy: velocity estimate vs exact sliding count for Poisson
// and bursty traffic. The interpolation error is bounded by the count in the
// one bucket the window's trailing edge cuts; at age W that bucket lies
// within ages (W/2 - unit, 2W + unit].
TEST(TestVelocityAccuracy) {
  const Duration horizon = 60s;
  auto fp = newTest(1000, horizon);
  Filter& f = *fp;
  gorand::Rand r(1, 2);
  u64 u = core(f).unit;
  for (int trial = 0; trial < 200; trial++) {
    u64 h = mix64(static_cast<u64>(trial) + 1000);
    double rate = 0.5 + r.Float64() * 3;  // events per second
    std::vector<u64> events;
    u64 now = 10'000'000 + static_cast<u64>(trial) * 1'000'000;
    u64 end = now + f.Ticks(horizon) * 2;
    while (now < end) {
      now += static_cast<u64>(r.ExpFloat64() / rate * 1000);
      if (r.IntN(10) == 0) {  // occasional burst
        for (int k = 0; k < 5; k++) {
          events.push_back(now);
          f.CheckAndTouchAt(h, 0, now);
        }
        continue;
      }
      events.push_back(now);
      f.CheckAndTouchAt(h, 0, now);
    }
    for (Duration w : {Duration(1s), Duration(5s), Duration(30s), horizon}) {
      u64 wt = f.Ticks(w);
      i64 exact = 0, edge = 0;
      for (u64 e : events) {
        if (e + wt > now) exact++;
        if (u64 a = now - e; a + 2 * u > wt / 2 && a <= 2 * wt + 2 * u) edge++;
      }
      i64 got = f.VelocityCountAt(h, wt, now);
      if (i64 d = got - exact; d > edge + 1 || -d > edge + 1)
        FATALF("trial %d w=%dms: est %d exact %d (edge bound %d)", trial, w.count() / 1'000'000, got, exact, edge);
    }
  }
}

TEST(TestVelocityBurstAndDecay) {
  auto fp = newTest(1000, 1min);
  Filter& f = *fp;
  u64 h = mix64(99);
  for (int i = 0; i < 10; i++) f.CheckAndTouchAt(h, 0, 100'000 + static_cast<u64>(i) * 100);  // 10 hits in 1s
  u64 now = 101'000;
  if (i64 got = f.VelocityCountAt(h, f.Ticks(1min), now); got != 10) FATALF("60s count = %d, want 10", got);
  // Windows at or above one sub-window resolve the burst.
  if (i64 got = f.VelocityCountAt(h, f.Ticks(30s), now + f.Ticks(20s)); got < 9 || got > 10)
    FATALF("30s count 20s later = %d, want ~10", got);
  // Below sub-window resolution the estimate is bounded, never zero while the
  // key is fresh, and exactly zero once the window passes the last touch.
  if (i64 got = f.VelocityCountAt(h, f.Ticks(1s), now); got < 1 || got > 10)
    FATALF("1s count = %d, want within [1,10]", got);
  if (i64 got = f.VelocityCountAt(h, f.Ticks(1s), now + f.Ticks(2s)); got != 0)
    FATALF("1s count 2s after last touch = %d, want 0", got);
  if (i64 got = f.VelocityCountAt(h, f.Ticks(1min), now + f.Ticks(61s)); got != 0)
    FATALF("count after horizon = %d, want 0", got);
  // A returning key starts a fresh run.
  auto [seen, c] = f.TouchAndCountAt(h, f.Ticks(1min), now + f.Ticks(90s));
  (void)seen;
  if (c != 1) FATALF("count after return = %d, want 1", c);
}

TEST(TestHeavyHitterSpill) {
  auto f = newTest(100, 1min);
  u64 h = mix64(5);
  for (int i = 0; i < 1000; i++) f->CheckAndTouchAt(h, 0, 1000);
  // A full bucket spills into older ones, so a burst keeps its count up to
  // 1273 events (all buckets full; tfbench -exact, heavy_hitter).
  if (i64 got = f->VelocityCountAt(h, f->Ticks(1min), 1000); got < 950 || got > 1000)
    FATALF("1000 hits counted as %d", got);
}

TEST(TestFalsePositiveRate) {
  const i64 n = 100'000;
  auto fp = newTest(n, 1min);
  Filter& f = *fp;
  u64 now = 1000;
  for (i64 i = 0; i < n; i++) f.CheckAndTouchAt(mix64(static_cast<u64>(i)), 0, now);
  i64 fpn = 0;
  const i64 probes = 1'000'000;
  for (i64 i = 0; i < probes; i++)
    if (f.ContainsAt(mix64(static_cast<u64>(n + i)), f.Ticks(1min), now)) fpn++;
  double rate = static_cast<double>(fpn) / probes;
  LOGF("FPR at capacity: %.2e (%d/%d), slots=%d", rate, fpn, probes, f.Slots());
  if (rate > 1e-5) FATALF("FPR %.2e too high", rate);
}

TEST(TestRetentionAtCapacity) {
  const i64 n = 100'000;
  auto fp = newTest(n, 1min);
  Filter& f = *fp;
  u64 now = 1000;
  for (i64 i = 0; i < n; i++) f.CheckAndTouchAt(mix64(static_cast<u64>(i)), 0, now);
  i64 lost = 0;
  for (i64 i = 0; i < n; i++)
    if (!f.CheckAndTouchAt(mix64(static_cast<u64>(i)), f.Ticks(1min), now + 1)) lost++;
  LOGF("false negatives at capacity: %d/%d, evictions=%d", lost, n, f.Evictions());
  if (lost > n / 1000) FATALF("lost %d of %d live keys", lost, n);
}

TEST(TestContinuousDecayReclaims) {
  u64 clock = 1;
  Config cfg = cfgOf(10'000, 1s, 1ms);
  cfg.Clock = [&clock] { return clock; };
  Filter f(cfg);
  for (u64 i = 0; i < 10'000; i++) f.CheckAndTouchAt(mix64(i), 0, clock);
  if (f.Occupancy(clock) == 0) FATALF("nothing inserted");
  settle(f);  // with Background, keys placed after the last pass would stay
  // Advance the clock in small steps, as a live system would; each step
  // sweeps only the lines its elapsed ticks pay for.
  i64 maxStep = 0;
  while (clock < 3000) {
    clock++;
    for (;;) {
      i64 n = f.ContinuousDecay();
      maxStep = std::max(maxStep, n);
      if (n == 0) break;
    }
  }
  Core& c = core(f);
  i64 live = 0;
  for (std::size_t i = 0; i < c.nwords; i += 2)
    if (c.words[i].load() != 0) live++;
  if (live != 0) FATALF("%d expired slots not reclaimed", live);
  if (maxStep > 4) FATALF("decay step swept %d lines; must stay bounded", maxStep);
}

TEST(TestConcurrentTouchNoLostUpdates) {
  auto fp = newTest(1000, 1min);
  Filter& f = *fp;
  u64 h = mix64(1);
  f.CheckAndTouchAt(h, 0, 1000);
  const int workers = 8, per = 25;
  std::vector<std::thread> ts;
  for (int w = 0; w < workers; w++)
    ts.emplace_back([&] {
      for (int k = 0; k < per; k++) f.CheckAndTouchAt(h, 0, 1000);
    });
  for (auto& t : ts) t.join();
  if (i64 got = f.VelocityCountAt(h, f.Ticks(1min), 1000); got != 1 + workers * per)
    FATALF("count = %d, want %d", got, 1 + workers * per);
}

TEST(TestConcurrentMixedRace) {
  auto fp = newTest(1000, 100ms);
  Filter& f = *fp;
  std::vector<std::thread> ts;
  for (int w = 0; w < 8; w++)
    ts.emplace_back([&f, w] {
      gorand::Rand r(static_cast<u64>(w), 9);
      bool out[3];
      for (int k = 0; k < 20'000; k++) {
        u64 h = mix64(static_cast<u64>(r.IntN(3000)));
        u64 now = f.Now();
        switch (r.IntN(3)) {
          case 0:
            f.CheckAndTouchAt(h, f.Ticks(50ms), now);
            break;
          case 1: {
            f.VelocityCountAt(h, f.Ticks(50ms), now);
            u64 hs[3] = {h, h ^ 1, h ^ 2};
            f.CheckAndTouchBatch(hs, f.Ticks(50ms), now, out);
            break;
          }
          default:
            f.ContinuousDecay();
        }
      }
    });
  for (auto& t : ts) t.join();
}

TEST(TestStringAPI) {
  Filter f(cfgOf(100, 1min));
  if (f.CheckAndTouch("10.0.0.1", 1min)) FATALF("first seen");
  if (!f.CheckAndTouch("10.0.0.1", 1min)) FATALF("second not seen");
  if (auto [seen, c] = f.TouchAndCount("10.0.0.1", 1min); !seen || c != 3) FATALF("TouchAndCount = %v,%d", seen, c);
  if (i64 c = f.VelocityCount("10.0.0.1", 1min); c != 3) FATALF("VelocityCount = %d", c);
}

TEST(TestCountBatchesMatchSingle) {
  auto ap = newTest(10'000, 1min), bp = newTest(10'000, 1min);
  Filter &a = *ap, &b = *bp;
  std::vector<u64> hs(5000);
  for (std::size_t i = 0; i < hs.size(); i++) hs[i] = mix64(i % 3000);  // repeats inside one batch too
  std::unique_ptr<bool[]> seen(new bool[hs.size()]);
  std::vector<i64> counts(hs.size()), vel(hs.size());
  for (int round = 0; round < 3; round++) {
    u64 now = 1000 + static_cast<u64>(round) * 2000;
    a.TouchAndCountBatch(hs, 3000, now, std::span<bool>(seen.get(), hs.size()), counts);
    for (std::size_t i = 0; i < hs.size(); i++) {
      if (auto [s, c] = b.TouchAndCountAt(hs[i], 3000, now); seen[i] != s || counts[i] != c)
        FATALF("round %d key %d: batch %v/%d single %v/%d", round, i, seen[i], counts[i], s, c);
    }
    a.VelocityCountBatch(hs, 5000, now + 500, vel);
    for (std::size_t i = 0; i < hs.size(); i++) {
      if (i64 want = b.VelocityCountAt(hs[i], 5000, now + 500); vel[i] != want)
        FATALF("round %d key %d: velocity batch %d single %d", round, i, vel[i], want);
    }
  }
  a.CheckAndTouchBatch({}, 1, 1, {});  // empty batches are fine
  a.TouchAndCountBatch({}, 1, 1, {}, {});
  a.VelocityCountBatch({}, 1, 1, {});
}

TEST(TestCoarseClock) {
  Config cfg = cfgOf(100, 1min);
  cfg.CoarseClock = 1ms;
  Filter f(cfg);
  u64 t0 = f.Now();
  std::this_thread::sleep_for(20ms);
  if (u64 d = f.Now() - t0; d < f.Ticks(10ms) || d > f.Ticks(200ms))
    FATALF("coarse clock advanced %d ticks over 20ms", d);
  if (f.CheckAndTouch("k", 1s) || !f.CheckAndTouch("k", 1s)) FATALF("string API with coarse clock");
  f.Close();
  f.Close();  // idempotent
}

// TestMemoryOnFirstUse: a filter that never evicts holds only its table; the
// stash comes with the first eviction, the overflow table with the first
// history the stash drops.
TEST(TestMemoryOnFirstUse) {
  if (forceCT || forceBG)
    SKIP("ConstantTime and Background allocate the stash on the first insert that waits for a slot");
  auto fp = newTest(4096, 1min);
  Filter& f = *fp;
  Core& c = core(f);
  i64 table = 16 * f.Slots();
  for (u64 i = 0; i < static_cast<u64>(f.Slots() * 85 / 100); i++) f.TouchAt(mix64(i), 0, 1);
  if (f.Evictions() != 0 || f.Bytes() != table)
    FATALF("at 85%% load: %d evictions, %d bytes, want 0 and the table's %d", f.Evictions(), f.Bytes(), table);
  i64 stash = static_cast<i64>(c.stashN * stashWays * sizeof(StashEntry) + 8 * ((c.stashN + 63) / 64));
  for (u64 i = u64{1} << 40; f.Evictions() == 0; i++) f.TouchAt(mix64(i), 0, 1);
  if (f.Bytes() != table + stash)
    FATALF("after the first eviction: %d bytes, want table %d + stash %d", f.Bytes(), table, stash);
  for (u64 i = u64{1} << 41; f.Folded() == 0; i++) {
    for (int k = 0; k < 4; k++) f.TouchAt(mix64(i), 0, 1);  // SpillMin: lighter histories are shed, not kept
  }
  if (Ghost* g = c.gh.load(); !g || f.Bytes() != table + stash + static_cast<i64>(8 * g->words.size()))
    FATALF("after the first fold: %d bytes, want the overflow table's too", f.Bytes());
}

// TestTickRange: inserts, realigns and the decay step record ticks up to
// MaxTick and throw past it, before taking any lock, so a caller that catches
// it keeps a usable filter; the constructor rejects a Clock already past it
// and a horizon of MaxTick ticks.
TEST(TestTickRange) {
  const u64 w = 1000;
  const u64 h = 0x9e3779b97f4a7c15;  // h&decayEvery != 0: no inline decay step
  struct Ins {
    const char* name;
    std::function<void(Filter&, u64)> call;
  };
  std::vector<Ins> inserts{
      {"TouchAt", [&](Filter& f, u64 now) { f.TouchAt(h, w, now); }},
      {"CheckAndTouchAt", [&](Filter& f, u64 now) { f.CheckAndTouchAt(h, w, now); }},
      {"TouchAndCountAt", [&](Filter& f, u64 now) { f.TouchAndCountAt(h, w, now); }},
      {"CheckAndTouchBatch",
       [&](Filter& f, u64 now) {
         u64 hs[1] = {h};
         bool out[1];
         f.CheckAndTouchBatch(hs, w, now, out);
       }},
      {"TouchAndCountBatch",
       [&](Filter& f, u64 now) {
         u64 hs[1] = {h};
         bool seen[1];
         i64 counts[1];
         f.TouchAndCountBatch(hs, w, now, seen, counts);
       }},
  };
  for (auto& c : inserts) {
    auto f = newTest(1000, 1min);
    if (!panics([&] { c.call(*f, MaxTick + 1); })) ERRORF("%s inserting past MaxTick did not throw", c.name);
    if (panics([&] { c.call(*f, MaxTick); })) ERRORF("%s inserting at MaxTick (after a caught throw) threw", c.name);
    if (Result r = f->CountAt(h, w, MaxTick); !r.Seen || r.Count != 1)
      ERRORF("%s: the key inserted at MaxTick reads %+v", c.name, r);
  }
  {
    auto f = newTest(1000, 1min);
    Core& c = core(*f);
    f->TouchAt(h, w, MaxTick - 10 * c.unit);
    if (!panics([&] { f->TouchAt(h, w, MaxTick + 1); })) ERRORF("a realigning touch past MaxTick did not throw");
    f->TouchAt(h, w, MaxTick);
    if (Result r = f->CountAt(h, c.horizon, MaxTick); !r.Seen || r.Count != 2)
      ERRORF("touches at MaxTick-10 units and MaxTick (after a caught throw) read %+v", r);
  }
  for (u64 now : {MaxTick, MaxTick + 1}) {
    auto f = newTest(1000, 1min);
    core(*f).clock = [now] { return now; };
    if (bool p = panics([&] { f->ContinuousDecay(); }); p != (now > MaxTick))
      ERRORF("ContinuousDecay at %d: threw %v", now, p);
  }
  {
    Config cfg = cfgOf(1000, 1min);
    cfg.Clock = [] {
      return static_cast<u64>(
          std::chrono::duration_cast<std::chrono::microseconds>(std::chrono::system_clock::now().time_since_epoch())
              .count());
    };
    bool refused = false;
    try {
      Filter f(cfg);
    } catch (const Error&) {
      refused = true;
    }
    if (!refused) ERRORF("the constructor accepted a clock in Unix microseconds");
  }
  {
    bool refused = false;
    try {
      Filter f(cfgOf(1000, 24h, 1ns));
    } catch (const Error&) {
      refused = true;
    }
    if (!refused) ERRORF("the constructor accepted a horizon of a day in nanoseconds (8.6e13 ticks)");
  }
}
