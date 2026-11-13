// TestSmoke (go/smoke_test.go): every public operation once, under three
// configs, checking the obvious answers. It is the smoke tier: a build that
// fails it is not worth testing further.
#include <sstream>

#include "check.hpp"
#include "clepsydra/clepsydra.hpp"
#include "core.hpp"

using namespace clepsydra;
using namespace std::chrono_literals;

TEST(TestSmoke) {
  std::array<std::uint8_t, 16> key{};
  for (int i = 0; i < 16; i++) key[i] = static_cast<std::uint8_t>(i);
  std::vector<Config> cfgs(3);
  cfgs[0].Capacity = 1000;
  cfgs[0].Horizon = 1min;
  cfgs[1].Capacity = 1000;
  cfgs[1].Horizon = 1min;
  cfgs[1].CoarseClock = 1ms;
  cfgs[2].Capacity = 1000;
  cfgs[2].Horizon = 1min;
  cfgs[2].Key = key;
  cfgs[2].ConstantTime = 1us;
  for (const Config& cfg : cfgs) {
    Filter f(cfg);
    Duration w = 1min;
    CHECK_MSG(!f.CheckAndTouch("a", w), "first CheckAndTouch reported seen");
    CHECK_MSG(f.CheckAndTouch("a", w), "second CheckAndTouch reported unseen");
    auto [seen, n] = f.TouchAndCount("a", w);
    CHECK_MSG(seen && n == 3, "TouchAndCount = " + std::to_string(seen) + ", " + std::to_string(n) + "; want true, 3");
    CHECK_EQ(f.VelocityCount("a", w), std::int64_t{3});
    Result r = f.Touch("a", w);
    CHECK_MSG(r.Seen && r.Count == 4 && r.Upper >= 4 && !r.Saturated && !r.Degraded, "Touch");
    r = f.Count("b", w);
    CHECK_MSG(!r.Seen && r.Count == 0, "Count of an unseen key");
    auto [r1, ok1] = f.TouchIfUnder("c", w, 1);
    CHECK_MSG(ok1 && r1.Count == 1, "TouchIfUnder under the limit");
    auto [r2, ok2] = f.TouchIfUnder("c", w, 1);
    CHECK_MSG(!ok2 && r2.Upper >= 1, "TouchIfUnder at the limit");
    std::uint64_t now = f.Now();
    std::uint64_t hs[3] = {f.HashUint64(1), f.HashUint64(2), f.HashUint64(1)};
    bool seenB[3] = {};
    std::int64_t counts[3] = {};
    f.CheckAndTouchBatch(hs, f.Ticks(w), now, seenB);
    f.TouchAndCountBatch(hs, f.Ticks(w), now, seenB, counts);
    CHECK_MSG(seenB[0] && counts[0] == 3 && counts[1] == 2, "batches");
    f.VelocityCountBatch(hs, f.Ticks(w), now, counts);
    CHECK_MSG(counts[0] == 4 && counts[1] == 2, "VelocityCountBatch");  // key 1 four times, key 2 twice
    CHECK_MSG(!f.ContainsAt(f.HashUint64(3), f.Ticks(w), now), "ContainsAt of an unseen key");
    f.ContinuousDecay();
    std::int64_t occ = f.Occupancy(f.Now());
    CHECK_MSG(occ >= 3 && occ <= f.Slots(), "Occupancy " + std::to_string(occ) + " of " + std::to_string(f.Slots()));
    // The counters and hashing, once each.
    std::uint64_t hk[2] = {7, 8}, ho[2];
    f.HashUint64s(hk, ho);
    CHECK_EQ(ho[0], f.HashUint64(7));
    CHECK(f.Bytes() >= 16 * f.Slots());
    (void)(f.Evictions() + f.Relocations() + f.Lost() + f.Folded() + f.Shed() + f.Waited() + f.Restored() +
           f.Promoted() + f.HotRefused() + f.Demoted() + f.Floored());
    (void)(f.Waiting() + f.HotInUse());
    f.Close();
  }
  bool refused = false;
  try {
    Filter f{Config{}};
  } catch (const Error&) {
    refused = true;
  }
  CHECK_MSG(refused, "the constructor accepted a zero Config");
}

// TestSmokeOptional calls the optional operations once (the smoke tier calls
// every public operation): Merge, a snapshot round trip, and a Resizable's
// transition. ConstantTime and Background filters refuse all three.
TEST(TestSmokeOptional) {
  std::array<std::uint8_t, 16> key{};
  for (int i = 0; i < 16; i++) key[i] = static_cast<std::uint8_t>(i);
  const bool modes = detail::forceCT || detail::forceBG;
  std::uint64_t now = 1000;
  Config cfg;
  cfg.Capacity = 1000;
  cfg.Horizon = 1min;
  cfg.Key = key;
  cfg.Clock = [&now] { return now; };
  Filter f(cfg), g(cfg);
  f.Touch("a", 1min);
  g.Touch("a", 1min);
  g.Touch("b", 1min);
  try {
    f.Merge(g);
    CHECK(!modes);
    CHECK_EQ(f.Count("a", 1min).Count, std::int64_t{2});
    CHECK_EQ(f.Count("b", 1min).Count, std::int64_t{1});
  } catch (const Error&) {
    CHECK_MSG(modes, "Merge refused");
  }
  try {
    std::stringstream ss;
    std::int64_t n = f.WriteTo(ss);
    Filter h(cfg);
    CHECK_EQ(h.ReadFrom(ss), n);
    CHECK(!modes);
    CHECK(h.Count("a", 1min) == f.Count("a", 1min));
    CHECK(h.Count("b", 1min) == f.Count("b", 1min));
  } catch (const Error& e) {
    CHECK_MSG(modes, std::string("snapshot refused: ") + e.what());
  }
  Config rc = cfg;
  rc.Hot = -1;
  try {
    Resizable z(rc);
    CHECK(!modes);
    std::uint64_t h1 = f.HashUint64(1), win = f.Ticks(1min);
    z.TouchAt(h1, win, now);
    z.Resize(4000, now);
    now++;
    CHECK_EQ(z.TouchAt(h1, win, now).Count, std::int64_t{2});
    CHECK(z.ContainsAt(h1, win, now));
    CHECK_EQ(z.VelocityCountAt(h1, win, now), std::int64_t{2});
    CHECK_EQ(z.CountAt(h1, win, now).Upper, std::int64_t{2});
    CHECK(z.Bytes() > 0);
    CHECK_EQ(z.Evictions(), std::uint64_t{0});
  } catch (const Error& e) {
    CHECK_MSG(modes, std::string("Resizable refused: ") + e.what());
  }
}
