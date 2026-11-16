// Tests of what the format says each operation MUST keep
// (docs/specification/format.md), for the operations the conformance vectors
// leave out (conformance.md: "an implementation that offers them shows the
// properties of the format instead"), and two Go tests ported as they are.
#include <algorithm>
#include <atomic>
#include <barrier>
#include <mutex>
#include <sstream>
#include <thread>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace std::chrono_literals;

namespace {
std::array<std::uint8_t, 16> testKey() {
  std::array<std::uint8_t, 16> k{};
  for (int i = 0; i < 16; i++) k[i] = static_cast<std::uint8_t>(i);
  return k;
}
bool modes() { return forceCT || forceBG; }
}  // namespace

// TestSprayCannotEvictRepeatOffender (go/filter_test.go; claims D4, E1): 20x
// capacity of one-off keys can't flush 50 keys with 20 events each.
TEST(TestSprayCannotEvictRepeatOffender) {
  const std::int64_t n = 10'000;
  Config cfg;
  cfg.Capacity = n;
  cfg.Horizon = 1min;
  Filter f(cfg);
  std::uint64_t now = 1000;
  std::vector<std::uint64_t> offenders(50);
  for (std::size_t i = 0; i < offenders.size(); i++) {
    offenders[i] = mix64((std::uint64_t{1} << 40) + i);
    for (int k = 0; k < 20; k++) f.CheckAndTouchAt(offenders[i], 0, now);
  }
  for (std::int64_t i = 0; i < 20 * n; i++)
    f.CheckAndTouchAt(mix64(static_cast<std::uint64_t>(i)), 0, now + static_cast<std::uint64_t>(i % 30'000));
  now += 30'000;
  for (std::size_t i = 0; i < offenders.size(); i++) {
    Result r = f.CountAt(offenders[i], f.Ticks(1min), now);
    if (r.Count < 20 || (r.Count > 20 && !r.Degraded)) {
      check::fail(__FILE__, __LINE__,
                  "offender " + std::to_string(i) + ": " + helpers::show(r) + " after spray, want 20");
      break;
    }
  }
  check::log("evictions during spray: " + std::to_string(f.Evictions()));
}

// TestBatchMatchesSingle (go/filter_test.go): a batch answers as its keys'
// single calls do, in order.
TEST(TestBatchMatchesSingle) {
  Config cfg;
  cfg.Capacity = 10'000;
  cfg.Horizon = 1min;
  Filter a(cfg), b(cfg);
  std::vector<std::uint64_t> hs(5000);
  for (std::size_t i = 0; i < hs.size(); i++) hs[i] = mix64(i % 3000);  // repeats inside one batch too
  std::unique_ptr<bool[]> out(new bool[hs.size()]);
  for (int round = 0; round < 3; round++) {
    std::uint64_t now = 1000 + static_cast<std::uint64_t>(round) * 2000;
    a.CheckAndTouchBatch(hs, 3000, now, std::span<bool>(out.get(), hs.size()));
    for (std::size_t i = 0; i < hs.size(); i++) {
      if (bool want = b.CheckAndTouchAt(hs[i], 3000, now); out[i] != want) {
        check::fail(__FILE__, __LINE__, "round " + std::to_string(round) + " key " + std::to_string(i));
        return;
      }
    }
  }
}

namespace {

// Workload drives one or two filters with the same seeded stream of every
// operation (touches, counts, conditional touches, bursts that promote, a key
// pool past the table's capacity), so a pair's answers can be compared.
struct Workload {
  gorand::Rand rng;
  std::uint64_t& now;
  std::vector<std::uint64_t> heavy;
  Workload(std::uint64_t seed, std::uint64_t& clock) : rng(seed, 0x5a7), now(clock) {
    for (int i = 0; i < 8; i++) heavy.push_back(mix64(0xbeef0000ull + static_cast<std::uint64_t>(i)));
  }
  // step runs one operation on every filter in fs and reports whether their
  // answers agree.
  bool step(std::vector<Filter*> fs, std::string& why) {
    now += static_cast<std::uint64_t>(rng.IntN(40));
    if (rng.IntN(500) == 0) now += 1 + rng.Uint64N(20'000);
    std::uint64_t h =
        rng.IntN(10) == 0 ? heavy[static_cast<std::size_t>(rng.IntN(8))] : mix64(0x1000 + rng.Uint64N(700));
    std::uint64_t w = 1 + rng.Uint64N(12'000);
    std::int64_t op = rng.IntN(100);
    std::vector<std::string> got;
    for (Filter* f : fs) {
      std::ostringstream o;
      if (op < 40) {
        o << helpers::show(f->TouchAt(h, w, now));
      } else if (op < 45) {
        for (int k = 0; k < 50; k++) f->TouchAt(h, 0, now);
        o << helpers::show(f->CountAt(h, w, now));
      } else if (op < 70) {
        o << helpers::show(f->CountAt(h, w, now));
      } else if (op < 85) {
        auto [r, ok] = f->TouchIfUnderAt(h, w, 1 + (h >> 7) % 20, now);
        o << helpers::show(r) << ok;
      } else if (op < 90) {
        o << f->ContainsAt(h, w, now) << f->CheckAndTouchAt(h, w, now);
      } else if (op < 95) {
        std::uint64_t hs[8];
        bool out[8];
        std::int64_t cnt[8];
        for (int k = 0; k < 8; k++) hs[k] = mix64(0x1000 + ((h + static_cast<std::uint64_t>(k) * 31) % 700));
        f->TouchAndCountBatch(hs, w, now, out, cnt);
        for (int k = 0; k < 8; k++) o << out[k] << cnt[k] << ",";
      } else {
        TestAccess::core(*f).decayStep(now, 4);
      }
      got.push_back(o.str());
    }
    for (std::size_t i = 1; i < got.size(); i++) {
      if (got[i] != got[0]) {
        why = "op " + std::to_string(op) + " key " + helpers::hex(h) + " window " + std::to_string(w) + " now " +
              std::to_string(now) + ": " + got[0] + " vs " + got[i];
        return false;
      }
    }
    return true;
  }
};

Config snapConfig(std::uint64_t& now, int variant) {
  Config cfg;
  cfg.Capacity = 256;
  cfg.Horizon = 10s;
  cfg.Resolution = 1ms;
  cfg.Key = testKey();
  cfg.Clock = [&now] { return now; };
  if (variant == 1) {
    cfg.Stash = 8;
    cfg.SpillMin = 1;
  }
  if (variant == 2) cfg.Overflow = -1;  // the reference's WriteTo divides by zero here; this port writes 0
  if (variant == 3) cfg.Hot = 2;
  return cfg;
}

}  // namespace

// TestSnapshotRoundTrip: a restore into a filter of the same configuration
// answers every later call as the uninterrupted filter would, conditional
// touches included (format.md, Snapshots; claims D27, A46). The stream fills
// the stash, the overflow table, hot entries and buzz entries first.
TEST(TestSnapshotRoundTrip) {
  if (modes()) SKIP("ConstantTime and Background filters refuse snapshots");
  for (int variant = 0; variant < 4; variant++) {
    std::uint64_t now = 1;
    Config cfg = snapConfig(now, variant);
    Filter f(cfg);
    Workload wl(static_cast<std::uint64_t>(variant) + 1, now);
    std::string why;
    for (int i = 0; i < 30'000; i++) wl.step({&f}, why);
    std::stringstream ss;
    std::int64_t n = f.WriteTo(ss);
    Filter g(cfg);
    CHECK_EQ(g.ReadFrom(ss), n);
    CHECK_EQ(g.Bytes(), f.Bytes());
    CHECK_EQ(g.Evictions(), f.Evictions());
    CHECK_EQ(g.Promoted(), f.Promoted());
    CHECK_EQ(g.Folded(), f.Folded());
    for (int i = 0; i < 30'000; i++) {
      if (!wl.step({&f, &g}, why)) {
        check::fail(__FILE__, __LINE__,
                    "variant " + std::to_string(variant) + " step " + std::to_string(i) + ": " + why);
        break;
      }
    }
    CHECK_EQ(g.Evictions(), f.Evictions());
    CHECK_EQ(g.Occupancy(now), f.Occupancy(now));
    check::log("variant " + std::to_string(variant) + ": " + std::to_string(n) + " bytes; evictions " +
               std::to_string(f.Evictions()) + ", folded " + std::to_string(f.Folded()) + ", promoted " +
               std::to_string(f.Promoted()) + ", lost " + std::to_string(f.Lost()));
  }
}

// TestSnapshotRefusals: a restore refuses another configuration, a truncated
// stream, a damaged one and trailing bytes (format.md, Snapshots).
TEST(TestSnapshotRefusals) {
  if (modes()) SKIP("ConstantTime and Background filters refuse snapshots");
  std::uint64_t now = 1;
  Config cfg = snapConfig(now, 0);
  Filter f(cfg);
  Workload wl(9, now);
  std::string why;
  for (int i = 0; i < 5'000; i++) wl.step({&f}, why);
  std::stringstream ss;
  f.WriteTo(ss);
  const std::string good = ss.str();
  auto refused = [&](const std::string& bytes, const Config& c) {
    Filter g(c);
    std::istringstream in(bytes);
    try {
      g.ReadFrom(in);
    } catch (const Error&) {
      return true;
    }
    return false;
  };
  CHECK(!refused(good, cfg));
  Config other = cfg;
  other.Capacity = 512;
  CHECK_MSG(refused(good, other), "another Config was restored");
  for (std::size_t cut : {std::size_t{0}, std::size_t{1}, std::size_t{8}, std::size_t{100}, good.size() / 2,
                          good.size() - 8, good.size() - 1})
    CHECK_MSG(refused(good.substr(0, cut), cfg), "a stream cut at " + std::to_string(cut) + " was restored");
  CHECK_MSG(refused(good + "x", cfg), "trailing bytes were restored");
  int flips = 0, missed = 0;
  for (std::size_t i = 0; i < good.size(); i += 61) {
    std::string bad = good;
    bad[i] = static_cast<char>(bad[i] ^ (1 << (i % 8)));
    flips++;
    if (!refused(bad, cfg)) missed++;
  }
  CHECK_MSG(missed == 0, std::to_string(missed) + " of " + std::to_string(flips) + " single-bit flips were restored");
}

// TestMergeSums: a key in both filters gets the two registers summed and the
// newer last touch; a key only the other holds is installed; Merge refuses
// itself, another Config and hot entries in use (format.md, Merge; D24).
TEST(TestMergeSums) {
  if (modes()) SKIP("ConstantTime and Background filters refuse Merge");
  std::uint64_t now = 1000;
  Config cfg;
  cfg.Capacity = 1000;
  cfg.Horizon = 1min;
  cfg.Resolution = 1ms;
  cfg.Key = testKey();
  cfg.Clock = [&now] { return now; };
  Filter f(cfg), g(cfg);
  for (int i = 0; i < 3; i++) f.Touch("both", 1min);
  for (int i = 0; i < 2; i++) g.Touch("both", 1min);
  f.Touch("f-only", 1min);
  for (int i = 0; i < 4; i++) g.Touch("g-only", 1min);
  now += 30'000;  // g's later touch of a key both hold sets the merged last touch
  g.Touch("both", 1min);
  f.Merge(g);
  now += 1;
  CHECK_EQ(f.Count("both", 1min).Count, std::int64_t{6});
  CHECK(f.Count("both", 1s).Seen);
  CHECK_EQ(f.Count("f-only", 1min).Count, std::int64_t{1});
  CHECK_EQ(f.Count("g-only", 1min).Count, std::int64_t{4});
  CHECK_EQ(f.Count("none", 1min).Count, std::int64_t{0});
  auto refused = [](auto&& fn) {
    try {
      fn();
    } catch (const Error&) {
      return true;
    }
    return false;
  };
  CHECK_MSG(refused([&] { f.Merge(f); }), "Merge with itself");
  Config other = cfg;
  other.Capacity = 2000;
  Filter o(other);
  CHECK_MSG(refused([&] { f.Merge(o); }), "Merge of another Config");
  Filter hot(cfg);
  for (int i = 0; i < 40; i++) hot.Touch("burst", 1min);  // past a register's 31 in one unit: promoted
  CHECK_EQ(hot.Promoted(), std::uint64_t{1});
  CHECK_MSG(refused([&] { f.Merge(hot); }), "Merge with hot entries in use");
}

namespace {
// overLimit: the most events of admitted (sorted) inside any window [t-w, t].
std::int64_t mostInWindow(std::vector<std::uint64_t> admitted, std::uint64_t w) {
  std::sort(admitted.begin(), admitted.end());
  std::int64_t most = 0;
  std::size_t lo = 0;
  for (std::size_t hi = 0; hi < admitted.size(); hi++) {
    while (admitted[hi] - admitted[lo] > w) lo++;
    most = std::max(most, static_cast<std::int64_t>(hi - lo + 1));
  }
  return most;
}
}  // namespace

// TestTouchIfUnderNeverOverLimit: conditional touches admit no more than limit
// events in any window, buzz entries (limits up to 16) or not, from one
// thread and from eight racing on one key (format.md, Conditional touch;
// claims A45, A46).
TEST(TestTouchIfUnderNeverOverLimit) {
  for (std::uint64_t limit : {1, 3, 5, 16, 17, 40}) {
    for (std::uint64_t w : {1'000, 5'000, 30'000}) {
      for (int threads : {1, 8}) {
        std::uint64_t now = 1000;
        Config cfg;
        cfg.Capacity = 1000;
        cfg.Horizon = 1min;
        cfg.Resolution = 1ms;
        cfg.Key = testKey();
        Filter f(cfg);
        std::uint64_t h = f.Hash("limited") ^ limit;
        std::vector<std::uint64_t> admitted;
        std::mutex mu;
        gorand::Rand rng(limit * 1000 + w + static_cast<std::uint64_t>(threads), 3);
        const std::uint64_t gap = std::max<std::uint64_t>(1, w / (2 * limit));  // offered at twice the limit
        std::vector<std::uint64_t> nows(400);
        for (auto& x : nows) x = now += rng.Uint64N(2 * gap + 1);
        // Every thread makes one request per round, all at the round's clock;
        // the barrier keeps rounds from overlapping.
        std::barrier sync(threads);
        std::vector<std::thread> ts;
        for (int t = 0; t < threads; t++) {
          ts.emplace_back([&] {
            for (std::uint64_t at : nows) {
              sync.arrive_and_wait();
              if (f.TouchIfUnderAt(h, w, limit, at).second) {
                std::lock_guard<std::mutex> lk(mu);
                admitted.push_back(at);
              }
            }
          });
        }
        for (auto& t : ts) t.join();
        std::int64_t most = mostInWindow(admitted, w);
        CHECK_MSG(most <= static_cast<std::int64_t>(limit),
                  "limit " + std::to_string(limit) + " window " + std::to_string(w) + " threads " +
                      std::to_string(threads) + ": " + std::to_string(most) + " admitted in one window");
        CHECK_MSG(!admitted.empty(), "nothing admitted");
      }
    }
  }
}

// TestHotKeepsBurstsExact: with hot entries a burst of 2,000 events in one
// tick reads exactly in windows of 5 s and more, unsaturated; without them a
// register saturates (claims A15, D7).
TEST(TestHotKeepsBurstsExact) {
  for (bool withHot : {true, false}) {
    Config cfg;
    cfg.Capacity = 1000;
    cfg.Horizon = 1min;
    cfg.Resolution = 1ms;
    if (!withHot) cfg.Hot = -1;
    Filter f(cfg);
    std::uint64_t h = mix64(42);
    for (int i = 0; i < 2000; i++) f.TouchAt(h, 0, 1000);
    for (std::uint64_t w : {5'000, 10'000, 60'000}) {
      Result r = f.CountAt(h, w, 1001);
      if (withHot) {
        CHECK_MSG(r.Count == 2000 && r.Upper >= 2000 && !r.Saturated, "hot: " + helpers::show(r));
      } else {
        CHECK_MSG(r.Saturated, "no hot entries: " + helpers::show(r));
      }
    }
    CHECK_EQ(f.Promoted(), std::uint64_t{withHot ? 1u : 0u});
  }
}

// TestResizeKeepsCounts: during a resize's transition a key keeps its events
// (reads add the old filter's answer, a touch moves the key's slot), and once
// a horizon has passed the new filter answers alone (format.md, Resizing;
// claims D28).
TEST(TestResizeKeepsCounts) {
  if (modes()) SKIP("ConstantTime and Background filters can't be Resizable");
  Config cfg;
  cfg.Capacity = 500;
  cfg.Horizon = 1min;
  cfg.Resolution = 1ms;
  cfg.Hot = -1;
  cfg.Key = testKey();
  Resizable z(cfg);
  Filter hasher(cfg);
  std::uint64_t now = 1000;
  for (int k = 0; k < 300; k++)
    for (int i = 0; i < 3; i++) z.TouchAt(hasher.HashUint64(static_cast<std::uint64_t>(k)), 60'000, now);
  z.Resize(4000, now);
  now += 10;
  for (int k = 0; k < 300; k += 2) z.TouchAt(hasher.HashUint64(static_cast<std::uint64_t>(k)), 60'000, now);
  now += 10;
  for (int k = 0; k < 300; k++) {
    std::int64_t want = k % 2 == 0 ? 4 : 3;
    Result r = z.CountAt(hasher.HashUint64(static_cast<std::uint64_t>(k)), 60'000, now);
    if (r.Count != want) {
      check::fail(__FILE__, __LINE__,
                  "key " + std::to_string(k) + ": " + helpers::show(r) + ", want " + std::to_string(want));
      break;
    }
  }
  now += 70'000;  // past the transition: the old filter is gone with what it held
  z.TouchAt(hasher.HashUint64(0), 60'000, now);
  CHECK_EQ(z.CountAt(hasher.HashUint64(0), 60'000, now).Count, std::int64_t{1});
}
