// The regression tests of fixed bugs (go/regress_test.go), the concurrent
// soak (go/soak_test.go), the Upper-contract fuzzer (go/fuzz_test.go), and
// two checks of the Go RNG port they rely on.
#include <algorithm>
#include <atomic>
#include <chrono>
#include <fstream>
#include <thread>
#include <unordered_map>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"
#include "json.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace std::chrono_literals;
using helpers::core;

namespace {
std::unique_ptr<Filter> newTest(std::int64_t capacity, Duration horizon) {
  Config cfg;
  cfg.Capacity = capacity;
  cfg.Horizon = horizon;
  return std::make_unique<Filter>(cfg);
}
}  // namespace

// ---- go/regress_test.go ----

// Fixed in a01e3dc: a negative window wrapped to a huge tick count, so every
// key read as seen.
TEST(TestRegressNegativeTicks) {
  auto f = newTest(10, 1min);
  CHECK_EQ(f->Ticks(-1s), std::uint64_t{0});
}

// Fixed in d1db914: CheckAndTouchBatch indexed out[len(hs)-1] and panicked on
// an empty batch.
TEST(TestRegressEmptyBatch) {
  auto f = newTest(10, 1min);
  f->CheckAndTouchBatch({}, 1, 1, {});
}

// A tick past the 46 bits of a slot's last touch ran into its flag and
// fingerprint bits: a caller passing Unix microseconds got every key back
// unseen. Such a tick now throws.
TEST(TestRegressTickPastRange) {
  auto f = newTest(1000, 1min);
  const std::uint64_t h = 0x9e3779b97f4a7c15;
  using namespace std::chrono;
  const std::uint64_t now = static_cast<std::uint64_t>(
      duration_cast<microseconds>(sys_days{year{2026} / 10 / 29}.time_since_epoch()).count());  // ~1.8e15 > 2^46
  try {
    f->CheckAndTouchAt(h, 60'000'000, now);
    bool seen = f->CheckAndTouchAt(h, 60'000'000, now + 1);
    check::fail(__FILE__, __LINE__,
                "touches at Unix microseconds returned; the second read seen " + std::to_string(seen) +
                    " and the key then counts " + std::to_string(f->VelocityCountAt(h, 60'000'000, now + 2)) +
                    ", want true and 2");
  } catch (const TickError&) {
  }
}

// ---- the Go RNG port ----

// TestGoRand: gorand reproduces Go's math/rand/v2 over PCG (values printed by
// Go 1.24.7).
TEST(TestGoRand) {
  gorand::Rand r(7, 7);
  const std::uint64_t u[3] = {17709787720544228798ull, 14938139214418829421ull, 3591903268871140211ull};
  const std::int64_t a[3] = {0, 2, 5}, b[3] = {303, 175, 280};
  const std::uint64_t c[3] = {378986463, 623969817, 456040026}, d[3] = {235820, 520562, 597958};
  const double e[3] = {0.13403964807970836, 0.8778376638805372, 0.3623008920975276};
  for (int i = 0; i < 3; i++) {
    CHECK_EQ(r.Uint64(), u[i]);
    CHECK_EQ(r.IntN(12), a[i]);
    CHECK_EQ(r.IntN(400), b[i]);
    CHECK_EQ(r.Uint64N(1 << 30), c[i]);
    CHECK_EQ(r.Uint64N(1000003), d[i]);
    CHECK_EQ(r.Float64(), e[i]);
  }
  gorand::Rand r2(5, 0x5eedf022);
  CHECK_EQ(r2.IntN(9), std::int64_t{3});
  CHECK_EQ(r2.IntN(1 << 20), std::int64_t{219989});
  CHECK_EQ(r2.Uint64(), 9432458690240917045ull);
  CHECK_EQ(r2.Float64(), 0.19443830499656145);
}

// TestConformanceScenarios regenerates the vectors' operations as
// go/conformance_test.go's scenarios draw them (rand.NewPCG(7, 7) per
// scenario) and compares them with spec/vectors: the RNG port, and the
// scenarios, agree with Go's to the operation.
TEST(TestConformanceScenarios) {
  struct Op {
    std::string op, key;
    std::int64_t w;
    std::uint64_t now;
  };
  auto gen = [](const std::string& name) {
    gorand::Rand rng(7, 7);
    std::vector<Op> ops;
    auto add = [&ops](const char* op, std::string k, std::int64_t w, std::uint64_t now) {
      ops.push_back({op, std::move(k), w, now});
    };
    if (name == "basic") {
      std::uint64_t now = 1000;
      const std::int64_t ws[6] = {1, 250, 1000, 5000, 30'000, 60'000};
      for (int i = 0; i < 400; i++) {
        std::string k = "user:" + std::to_string(rng.IntN(12));
        std::int64_t w = ws[rng.IntN(6)];
        add(i % 3 == 2 ? "count" : "touch", k, w, now);
        if (i % 25 == 0) add("count", k, 250, now + 250);
        now += static_cast<std::uint64_t>(rng.IntN(400));
      }
    } else if (name == "burst") {
      std::uint64_t now = 5;
      for (int i = 0; i < 600; i++) {
        add("touch", "hot", 1000, now);
        if (i % 20 == 0)
          for (std::int64_t w : {100, 2500, 10'000, 60'000}) add("count", "hot", w, now);
        now += static_cast<std::uint64_t>(i < 300 ? rng.IntN(3) : rng.IntN(400));
      }
      for (int i = 0; i < 40; i++) {
        now += 2000;
        add("count", "hot", 60'000, now);
      }
    } else if (name == "churn") {
      std::uint64_t now = 1;
      for (int i = 0; i < 300; i++) {
        add("touch", "rep:" + std::to_string(i % 8), 60'000, now);
        now += static_cast<std::uint64_t>(rng.IntN(20));
      }
      for (int i = 0; i < 4 * 64 * 4; i++) {
        add("touch", "flood:" + std::to_string(i), 60'000, now);
        if (i % 16 == 0) add("touch", "rep:" + std::to_string(rng.IntN(8)), 60'000, now);
        now += static_cast<std::uint64_t>(rng.IntN(15));
      }
      for (int i = 0; i < 8; i++) add("count", "rep:" + std::to_string(i), 60'000, now);
      for (int i = 0; i < 64; i++) add("count", "never:" + std::to_string(i), 60'000, now);
    } else {
      std::uint64_t now = 1;
      const int n = 3 * 64;
      for (int round = 0; round < 8; round++) {
        for (int i = 0; i < n; i++) {
          if (round < 4 || i % 2 == 0) add("touch", "mid:" + std::to_string(i), 60'000, now);
          now += static_cast<std::uint64_t>(rng.IntN(10));
        }
      }
      for (int i = 0; i < n; i++) add("count", "mid:" + std::to_string(i), i % 2 ? 60'000 : 5000, now);
      for (int i = 0; i < 64; i++) add("count", "never:" + std::to_string(i), 60'000, now);
    }
    return ops;
  };
  for (std::string name : {"basic", "burst", "churn", "overflow"}) {
    std::ifstream in(std::string(CLEPSYDRA_VECTOR_DIR) + "/" + name + ".json", std::ios::binary);
    std::string text((std::istreambuf_iterator<char>(in)), std::istreambuf_iterator<char>());
    json::Value v = json::parse(text);
    std::vector<Op> got = gen(name);
    const auto& want = v["ops"].a;
    CHECK_EQ(got.size(), want.size());
    for (std::size_t i = 0; i < std::min(got.size(), want.size()); i++) {
      const auto& w = want[i];
      if (got[i].op != w["op"].s || got[i].key != w["key"].s || got[i].w != w["window_ms"].i64() ||
          got[i].now != w["now_ms"].u64()) {
        check::fail(__FILE__, __LINE__, name + ": op " + std::to_string(i) + " differs");
        break;
      }
    }
  }
}

// ---- go/soak_test.go ----

// TestStressSoak runs every operation from many threads against one filter
// for TF_STRESS (a duration; skipped when unset), cycling the load from half
// the capacity to 4x it, and checks the fail-closed contract on every Count:
// unless Saturated, Upper is never below the key's true count in the window
// (a low answer behind a live overflow floor is counted, not failed).
TEST(TestStressSoak) {
  Duration d = helpers::parseDuration(std::getenv("TF_STRESS"));
  if (d <= Duration::zero()) SKIP("set TF_STRESS=<duration> to run");
  const std::int64_t capacity = 4096;
  const int workers = std::max(4, 2 * static_cast<int>(std::thread::hardware_concurrency()));
  std::atomic<std::uint64_t> clock{1};
  Config cfg;
  cfg.Capacity = capacity;
  cfg.Horizon = 10s;
  cfg.Resolution = 1ms;
  cfg.Clock = [&clock] { return clock.load(); };
  cfg.SpillMin = 1;
  cfg.Hot = 80 * workers;
  Filter f(cfg);
  Core& c = core(f);
  const std::uint64_t horizon = 10'000;
  std::atomic<bool> stop{false};
  auto t0 = std::chrono::steady_clock::now();
  std::thread ticker([&] {  // 1 ms of filter time per 20 µs of wall time
    while (!stop.load()) {
      clock.store(1 + static_cast<std::uint64_t>((std::chrono::steady_clock::now() - t0) / 20us));
      std::this_thread::sleep_for(20us);
    }
  });
  std::atomic<std::int64_t> ops{0}, queries{0}, sat{0}, low{0}, floored{0};
  auto amp = helpers::TagAmp::fromEnv("TF_STRESS_TAGS");
  std::mutex firstMu;
  std::string firstLow;
  std::vector<std::thread> ts;
  for (int g = 0; g < workers; g++) {
    ts.emplace_back([&, g] {
      gorand::Rand rng(static_cast<std::uint64_t>(g), 1);
      std::unordered_map<u64, u64> cache;
      std::vector<u64> heavy(8);
      for (auto& x : heavy) x = helpers::amp(amp.get(), rng.Uint64(), &cache);
      std::unordered_map<u64, std::vector<u64>> logs;
      std::uint64_t hs[16];
      bool res[16];
      std::int64_t cnt[16];
      for (std::uint64_t n = 0;; n++) {
        if (stop.load()) return;
        auto ms =
            std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::system_clock::now().time_since_epoch())
                .count();
        int phase = static_cast<int>((ms / 500) % 4);  // pool: 0.5x, 1x, 2x, 4x capacity in total
        std::uint64_t pool = static_cast<std::uint64_t>((capacity / 2) << phase) / static_cast<std::uint64_t>(workers);
        u64 h;
        if (rng.IntN(32) == 0) {
          h = heavy[static_cast<std::size_t>(rng.IntN(8))];
        } else {
          h = helpers::amp(amp.get(), mix64((static_cast<u64>(g) << 40) | rng.Uint64N(pool)), &cache);
        }
        std::uint64_t now = clock.load();
        std::int64_t op = rng.IntN(10);
        if (op < 6) {
          f.TouchAt(h, 0, now);
          logs[h].push_back(now);
        } else if (op < 9) {
          std::uint64_t w = 1 + rng.Uint64N(horizon);
          Result r = f.CountAt(h, w, now);
          std::uint64_t after = clock.load();  // other threads may have expired older events by then
          std::int64_t exact = 0;
          for (u64 tsv : logs[h])
            if (now - tsv <= w && after - tsv <= horizon) exact++;
          queries++;
          if (r.Saturated) {
            sat++;
          } else if (r.Upper < exact && helpers::floorLive(c, h, w, now)) {
            floored++;
          } else if (r.Upper < exact) {
            if (low.fetch_add(1) == 0) {
              Result again = f.CountAt(h, w, now);
              std::lock_guard<std::mutex> lk(firstMu);
              firstLow = "got " + helpers::show(r) + "; asked again: " + helpers::show(again) + "; window " +
                         std::to_string(w) + " ms: exact " + std::to_string(exact) + " (now " + std::to_string(now) +
                         "); " + helpers::whereIs(c, h, now);
            }
          }
        } else {
          for (auto& x : hs) x = heavy[static_cast<std::size_t>(rng.IntN(8))];
          f.CheckAndTouchBatch(hs, horizon, now, res);
          for (u64 x : hs) logs[x].push_back(now);
          f.VelocityCountBatch(hs, horizon, now, cnt);
        }
        ops++;
        if (n % 4096 == 0) {  // forget events older than the horizon
          f.ContinuousDecay();
          for (auto it = logs.begin(); it != logs.end();) {
            auto& l = it->second;
            std::size_t i = 0;
            while (i < l.size() && now - l[i] > horizon) i++;
            if (i == l.size()) {
              it = logs.erase(it);
            } else {
              l.erase(l.begin(), l.begin() + static_cast<std::ptrdiff_t>(i));
              ++it;
            }
          }
        }
      }
    });
  }
  std::this_thread::sleep_for(d);
  stop.store(true);
  for (auto& t : ts) t.join();
  ticker.join();
  std::printf(
      "    %d workers, %.0fs: %lld ops, %lld counts checked (%lld Saturated, %lld low behind a live bucket floor); "
      "evictions %llu, restored %llu, lost %llu, promoted %llu, hot refused %llu\n",
      workers, std::chrono::duration<double>(d).count(), static_cast<long long>(ops.load()),
      static_cast<long long>(queries.load()), static_cast<long long>(sat.load()),
      static_cast<long long>(floored.load()), static_cast<unsigned long long>(f.Evictions()),
      static_cast<unsigned long long>(f.Restored()), static_cast<unsigned long long>(f.Lost()),
      static_cast<unsigned long long>(f.Promoted()), static_cast<unsigned long long>(f.HotRefused()));
  if (std::int64_t n = low.load(); n > 0)
    check::fail(__FILE__, __LINE__,
                std::to_string(n) + " unflagged answers with Upper below the true count; first: " + firstLow);
  if (std::int64_t occ = f.Occupancy(clock.load()); occ > f.Slots())
    check::fail(__FILE__, __LINE__, "occupancy " + std::to_string(occ) + " > slots");
}

// ---- go/fuzz_test.go ----

namespace {

struct FuzzResult {
  std::uint64_t ops = 0, checks = 0, sat = 0, floored = 0, lostLow = 0, shared = 0;
  std::uint64_t ct = 0, bg = 0, noStash = 0, noOverflow = 0, smallOverflow = 0, noHot = 0, smallHot = 0;
  std::uint64_t evictions = 0, folded = 0, promoted = 0, waited = 0;
  std::string fail;

  void add(const FuzzResult& o) {
    ops += o.ops, checks += o.checks, sat += o.sat, floored += o.floored, lostLow += o.lostLow, shared += o.shared;
    ct += o.ct, bg += o.bg, noStash += o.noStash, noOverflow += o.noOverflow, smallOverflow += o.smallOverflow;
    noHot += o.noHot, smallHot += o.smallHot;
    evictions += o.evictions, folded += o.folded, promoted += o.promoted, waited += o.waited;
  }
  std::string summary() const {
    char b[640];
    std::snprintf(b, sizeof b,
                  "%llu ops, %llu answers checked (%llu Saturated, %llu low behind a live floor, %llu low after a loss "
                  "with the overflow off, %llu off for a key whose identity another shares); cases with ConstantTime "
                  "%llu, Background %llu, no stash %llu, no overflow %llu, small overflow %llu, no hot entries %llu, "
                  "few hot entries %llu; evictions %llu, folded %llu, promoted %llu, waited %llu",
                  (unsigned long long)ops, (unsigned long long)checks, (unsigned long long)sat,
                  (unsigned long long)floored, (unsigned long long)lostLow, (unsigned long long)shared,
                  (unsigned long long)ct, (unsigned long long)bg, (unsigned long long)noStash,
                  (unsigned long long)noOverflow, (unsigned long long)smallOverflow, (unsigned long long)noHot,
                  (unsigned long long)smallHot, (unsigned long long)evictions, (unsigned long long)folded,
                  (unsigned long long)promoted, (unsigned long long)waited);
    return b;
  }
};

// FuzzTruth keeps every key's event times (sorted) within the horizon of the
// newest clock any call has used, and keep ticks more.
struct FuzzTruth {
  std::unordered_map<u64, std::vector<u64>> ev;
  u64 horizon, newest, keep;

  std::int64_t since(u64 h, u64 lo) const {
    auto it = ev.find(h);
    if (it == ev.end()) return 0;
    const auto& l = it->second;
    return static_cast<std::int64_t>(l.end() - std::lower_bound(l.begin(), l.end(), lo));
  }
  void add(u64 h, u64 ts) {
    auto& l = ev[h];
    l.push_back(ts);
    for (std::size_t i = l.size() - 1; i > 0 && l[i - 1] > l[i]; i--) std::swap(l[i - 1], l[i]);
  }
  std::int64_t count(u64 h, u64 window, u64 now) const {
    auto it = ev.find(h);
    if (it == ev.end()) return 0;
    const auto& l = it->second;
    u64 lo = newest - std::min(newest, horizon);
    if (u64 w = std::min(window, horizon); now - std::min(now, w) > lo) lo = now - w;
    auto a = std::lower_bound(l.begin(), l.end(), lo);
    auto b = std::lower_bound(l.begin(), l.end(), now + 1);
    return std::max<std::int64_t>(0, b - a);
  }
  void trim() {
    u64 lo = newest - std::min(newest, horizon + keep);
    for (auto it = ev.begin(); it != ev.end();) {
      auto& l = it->second;
      auto i = std::lower_bound(l.begin(), l.end(), lo);
      if (i == l.end()) {
        it = ev.erase(it);
      } else {
        l.erase(l.begin(), i);
        ++it;
      }
    }
  }
};

std::string tailOf(const std::vector<u64>& l, std::size_t n) {
  std::string s = "[";
  for (std::size_t i = l.size() > n ? l.size() - n : 0; i < l.size(); i++) s += std::to_string(l[i]) + " ";
  return s + "]";
}

// fuzzConfig draws a case's configuration (horizon H in ticks of 1 ms) and
// tallies it in res.
Config fuzzConfig(gorand::Rand& rng, FuzzResult& res, std::int64_t& capacity, std::uint64_t& H) {
  capacity = std::int64_t{1} << (6 + rng.IntN(9));  // 64 .. 32K keys
  capacity += rng.IntN(capacity);
  const std::uint64_t horizons[6] = {48, 96, 240, 1'000, 10'000, 60'000};
  H = horizons[rng.IntN(6)];
  Config cfg;
  cfg.Capacity = capacity;
  cfg.Horizon = std::chrono::milliseconds(H);
  cfg.Resolution = 1ms;
  cfg.SpillMin = 1;
  if (rng.IntN(4) == 0) cfg.Load = 0.5 + 0.5 * rng.Float64();
  switch (rng.IntN(8)) {
    case 0:
      cfg.Stash = -1, res.noStash = 1;
      break;
    case 1:
    case 2:
      cfg.Stash = 1 + rng.IntN(64);
      break;
  }
  switch (rng.IntN(8)) {
    case 0:
      cfg.Overflow = -1, res.noOverflow = 1;
      break;
    case 1:
    case 2:
      cfg.Overflow = 1 + rng.IntN(256), res.smallOverflow = 1;
      break;
  }
  switch (rng.IntN(8)) {
    case 0:
      cfg.Hot = -1, res.noHot = 1;
      break;
    case 1:
    case 2:
      cfg.Hot = 1 + rng.IntN(8), res.smallHot = 1;
      break;
  }
  return cfg;
}

FuzzResult fuzzCase(std::uint64_t seed) {
  FuzzResult res;
  gorand::Rand rng(seed, 0x5eedf022);
  std::int64_t capacity;
  std::uint64_t H;
  Config cfg = fuzzConfig(rng, res, capacity, H);
  std::uint64_t T = 1 + rng.Uint64N(std::uint64_t{1} << 30);  // the newest clock
  cfg.Clock = [&T] { return T; };
  std::unique_ptr<Filter> fp;
  try {
    fp = std::make_unique<Filter>(cfg);
  } catch (const std::exception& e) {
    res.fail = std::string("New: ") + e.what();
    return res;
  }
  Filter& f = *fp;
  Core& c = core(f);
  if (rng.IntN(5) == 0) c.ct = true, res.ct = 1;  // functional ConstantTime (no pad), as TF_CT=1 runs it
  gorand::Rand bgRng(seed, 0xb6);
  std::int64_t placeEvery = 0;
  if (bgRng.IntN(4) == 0 && !c.ct) {
    c.bg = c.bgManual = true;
    res.bg = 1;
    const std::int64_t pe[4] = {1, 4, 32, 512};
    placeEvery = pe[bgRng.IntN(4)];
  }
  struct Tally {
    FuzzResult& r;
    Filter& f;
    ~Tally() { r.evictions = f.Evictions(), r.folded = f.Folded(), r.promoted = f.Promoted(), r.waited = f.Waited(); }
  } tally{res, f};
  const std::uint64_t ops = std::uint64_t{1} << (16 + rng.IntN(7));  // 64K .. 4M
  const u64 unit = c.unit;
  const u64 lags[4] = {0, 2, unit, 2 * unit};
  const u64 maxLag = lags[rng.IntN(4)];
  const double perHs[5] = {0.25, 1, 4, 16, 64};
  const double perH = static_cast<double>(capacity) * perHs[rng.IntN(5)];
  const double tick = static_cast<double>(H) / perH;
  double acc = 0.0;
  const u64 salt = rng.Uint64();
  auto key = [salt](u64 k) { return mix64(salt ^ k); };
  std::vector<u64> heavy(static_cast<std::size_t>(1 + rng.IntN(32)));
  for (std::size_t i = 0; i < heavy.size(); i++) heavy[i] = key((u64{1} << 62) | i);
  const u64 reach = 9 * unit + maxLag + 1;
  FuzzTruth tr{{}, H, T, reach + maxLag};
  const double pools[6] = {0.25, 0.5, 1, 2, 4, 8};
  u64 pool = static_cast<u64>(capacity);
  std::int64_t phaseLeft = 0;
  u64 trimAt = T + H;
  std::uint64_t hs[16];
  bool out[16];
  std::int64_t cnt[16];
  auto desc = [&] {
    return "capacity " + std::to_string(capacity) + " load " + std::to_string(cfg.Load) + " stash " +
           std::to_string(cfg.Stash) + " overflow " + std::to_string(cfg.Overflow) + " hot " + std::to_string(cfg.Hot) +
           " ct " + std::to_string(c.ct) + " bg " + std::to_string(c.bg) + " place 1/" + std::to_string(placeEvery) +
           " horizon " + std::to_string(H) + " ticks unit " + std::to_string(unit) +
           " lag <= " + std::to_string(maxLag) + " ops/horizon " + std::to_string(perH);
  };
  auto shared = [&](u64 h) {
    u64 id = c.stashID(h, fingerprint(h), tag(h));
    for (const auto& kv : tr.ev)
      if (kv.first != h && c.stashID(kv.first, fingerprint(kv.first), tag(kv.first)) == id) return true;
    return false;
  };
  auto evOf = [&](u64 h) {
    auto it = tr.ev.find(h);
    return it == tr.ev.end() ? std::vector<u64>{} : it->second;
  };
  // high checks the other direction, on what the register answers (the
  // overflow table's part taken out).
  auto high = [&](const char* op, u64 h, u64 window, u64 now, const Result& r) {
    Result s;
    if (Ghost* g = c.gh.load()) s = g->estimate(c.stashID(h, fingerprint(h), tag(h)), std::min(window, H), now).first;
    std::int64_t own = std::string(op) == "Touch" ? 1 : 0;
    u64 w = std::min(window, H);
    u64 lo = now - std::min(now, w);
    std::string bad;
    std::int64_t upper = r.Upper - s.Upper;
    u64 back = now - std::min(now, w + reach);
    u64 fresh = std::max(lo, tr.newest - std::min(tr.newest, H));
    if (upper > tr.since(h, back)) {
      bad = "Upper " + std::to_string(r.Upper) + " (the overflow's " + std::to_string(s.Upper) + ") > " +
            std::to_string(tr.since(h, back)) + " events back to " + std::to_string(back);
    } else if (r.Seen && !s.Seen && tr.since(h, lo) <= own) {
      bad = "Seen with no event since " + std::to_string(lo);
    } else if (!r.Seen && tr.since(h, fresh) > own && !helpers::floorLive(c, h, window, now) &&
               !(cfg.Overflow < 0 && c.lostRecently(now))) {
      bad = "not Seen with " + std::to_string(tr.since(h, fresh) - own) + " events since " + std::to_string(fresh);
    } else {
      return true;
    }
    if (shared(h)) {
      res.shared++;
      return true;
    }
    res.fail = "op " + std::to_string(res.ops) + " " + op + ": " + bad + " (result " + helpers::show(r) + "); key " +
               helpers::hex(h) + " window " + std::to_string(window) + " now " + std::to_string(now) + " newest " +
               std::to_string(tr.newest) + "; events " + tailOf(evOf(h), 12) + "; " + helpers::whereIs(c, h, now) +
               "; " + desc();
    return false;
  };
  auto checkR = [&](const char* op, u64 h, u64 window, u64 now, const Result& r) {
    res.checks++;
    if (!high(op, h, window, now, r)) return false;
    if (r.Saturated) {
      res.sat++;
      return true;
    }
    std::int64_t exact = tr.count(h, window, now);
    if (r.Upper >= exact) return true;
    if (helpers::floorLive(c, h, window, now)) {
      res.floored++;
    } else if (cfg.Overflow < 0 && c.lostRecently(now)) {
      res.lostLow++;
    } else {
      res.fail = "op " + std::to_string(res.ops) + " " + op + ": Upper " + std::to_string(r.Upper) + " < true " +
                 std::to_string(exact) + " (result " + helpers::show(r) + "); key " + helpers::hex(h) + " window " +
                 std::to_string(window) + " now " + std::to_string(now) + " newest " + std::to_string(tr.newest) +
                 "; events " + tailOf(evOf(h), 12) + "; " + helpers::whereIs(c, h, now) + "; " + desc();
      return false;
    }
    return true;
  };
  for (; res.ops < ops; res.ops++) {
    if (phaseLeft == 0) {  // the pool of live keys cycles across the table's capacity
      double pf = pools[rng.IntN(6)];
      pool = std::max<u64>(1, static_cast<u64>(pf * static_cast<double>(capacity)));
      phaseLeft = 1 + rng.IntN(4 * capacity);
    }
    phaseLeft--;
    acc += tick * 2 * rng.Float64();
    if (acc >= 1) {
      T += static_cast<u64>(acc);
      acc -= static_cast<double>(static_cast<u64>(acc));
    }
    if (rng.Float64() * perH < 0.1) T += 1 + rng.Uint64N(3 * H);  // about once per 10 horizons, a jump
    u64 now = T;
    if (maxLag > 0 && rng.IntN(4) == 0) now -= rng.Uint64N(maxLag + 1);
    tr.newest = std::max(tr.newest, now);
    u64 h;
    if (rng.IntN(16) == 0) {
      h = heavy[static_cast<std::size_t>(rng.IntN(static_cast<std::int64_t>(heavy.size())))];
    } else {
      h = key(rng.Uint64N(pool));
    }
    u64 window = 1 + rng.Uint64N(H + H / 4);  // windows past the horizon too
    std::int64_t op = rng.IntN(100);
    if (op < 40) {
      tr.add(h, now);
      if (!checkR("Touch", h, window, now, f.TouchAt(h, window, now))) return res;
    } else if (op < 45) {  // a burst in one tick (promotes heavy keys)
      std::int64_t n = 1 + rng.IntN(64);
      for (std::int64_t k = 0; k < n; k++) {
        f.TouchAt(h, 0, now);
        tr.add(h, now);
      }
      res.ops += static_cast<std::uint64_t>(n - 1);
    } else if (op < 50) {
      f.CheckAndTouchAt(h, window, now);
      tr.add(h, now);
    } else if (op < 55) {
      f.TouchAndCountAt(h, window, now);
      tr.add(h, now);
    } else if (op < 85) {
      if (!checkR("Count", h, window, now, f.CountAt(h, window, now))) return res;
    } else if (op < 90) {
      f.ContainsAt(h, window, now);
      f.VelocityCountAt(h, window, now);
      res.ops++;
    } else if (op < 97) {
      for (auto& x : hs) {
        if (rng.IntN(2) == 0) {
          x = heavy[static_cast<std::size_t>(rng.IntN(static_cast<std::int64_t>(heavy.size())))];
        } else {
          x = key(rng.Uint64N(pool));
        }
      }
      if (rng.IntN(2) == 0) {
        f.CheckAndTouchBatch(hs, window, now, out);
      } else {
        f.TouchAndCountBatch(hs, window, now, out, cnt);
      }
      for (u64 x : hs) tr.add(x, now);
      f.VelocityCountBatch(hs, window, now, cnt);
      res.ops++;  // two calls (of 16 keys each)
    } else {
      c.decayStep(now, static_cast<u64>(1 + rng.IntN(16)));
    }
    if (placeEvery > 0 && bgRng.IntN(placeEvery) == 0) c.placeNext(c.room.load(), 0);
    if (tr.newest >= trimAt && !tr.ev.empty()) {
      tr.trim();
      trimAt = tr.newest + H;
    }
  }
  if (std::int64_t occ = f.Occupancy(tr.newest); occ > f.Slots())
    res.fail = "occupancy " + std::to_string(occ) + " > slots; " + desc();
  return res;
}

}  // namespace

// TestFuzzUpper (TF_FUZZ=<operations>, e.g. 1e9) checks the fail-closed
// contract over random configurations and operation streams; TF_FUZZ_CASE
// runs one case, TF_FUZZ_SEED picks the first seed (default 1).
TEST(TestFuzzUpper) {
  if (const char* s = std::getenv("TF_FUZZ_CASE"); s && *s) {
    std::uint64_t seed = std::stoull(s);
    FuzzResult r = fuzzCase(seed);
    std::printf("    case %llu: %s\n", static_cast<unsigned long long>(seed), r.summary().c_str());
    if (!r.fail.empty()) check::fail(__FILE__, __LINE__, r.fail);
    return;
  }
  const char* tv = std::getenv("TF_FUZZ");
  double total = tv ? std::atof(tv) : 0;
  if (total <= 0) SKIP("set TF_FUZZ=<operations> to run");
  std::uint64_t first = 1;
  if (const char* s = std::getenv("TF_FUZZ_SEED"); s && *s) first = std::stoull(s);
  std::atomic<std::uint64_t> next{first}, done{0};
  std::mutex mu;
  FuzzResult sum;
  std::vector<std::string> fails;
  auto t0 = std::chrono::steady_clock::now();
  std::vector<std::thread> ts;
  unsigned n = std::max(1u, std::thread::hardware_concurrency());
  for (unsigned i = 0; i < n; i++) {
    ts.emplace_back([&] {
      while (done.load() < static_cast<std::uint64_t>(total)) {
        std::uint64_t seed = next.fetch_add(1);
        FuzzResult r = fuzzCase(seed);
        done.fetch_add(r.ops);
        std::lock_guard<std::mutex> lk(mu);
        sum.add(r);
        if (!r.fail.empty()) fails.push_back("case " + std::to_string(seed) + ": " + r.fail);
      }
    });
  }
  for (auto& t : ts) t.join();
  double el = std::chrono::duration<double>(std::chrono::steady_clock::now() - t0).count();
  std::printf("    %llu cases from seed %llu in %.0fs (%.1f M ops/s): %s\n",
              static_cast<unsigned long long>(next.load() - first), static_cast<unsigned long long>(first), el,
              static_cast<double>(sum.ops) / el / 1e6, sum.summary().c_str());
  for (const auto& m : fails) check::fail(__FILE__, __LINE__, m);
}

// ---- go/fuzzconc_test.go ----

namespace {

// CfuzzShared: the keys every thread of a case touches and counts, and their
// events whose touches have returned.
struct CfuzzShared {
  std::vector<u64> keys;
  std::mutex mu;
  FuzzTruth tr{{}, 0, 0, 0};

  void add(u64 h, u64 ts) {
    std::lock_guard<std::mutex> lk(mu);
    tr.add(h, ts);
  }
  // snapshot is h's events so far, as a truth of its own.
  FuzzTruth snapshot(u64 h) {
    std::lock_guard<std::mutex> lk(mu);
    FuzzTruth t{{}, tr.horizon, 0, 0};
    if (auto it = tr.ev.find(h); it != tr.ev.end()) t.ev[h] = it->second;
    return t;
  }
};

// cfuzzWorker is one thread of a concurrent case: ops operations on its own
// keys (g in the top bits), share its share of the capacity, and on the
// shared keys. A straggler's calls run half a horizon to a horizon behind the
// clock.
FuzzResult cfuzzWorker(Filter& f, u64 g, u64 seed, std::uint64_t ops, std::int64_t share, u64 H, u64 maxLag,
                       double perH, bool straggler, std::atomic<u64>& clock, std::atomic<u64>& newest, CfuzzShared& sh,
                       helpers::TagAmp* amp) {
  FuzzResult res;
  Core& c = core(f);
  gorand::Rand rng(seed, g);
  std::unordered_map<u64, u64> cache;
  auto key = [&](u64 k) { return helpers::amp(amp, mix64((g << 48) ^ k), &cache); };
  std::vector<u64> heavy(static_cast<std::size_t>(1 + rng.IntN(4)));
  for (std::size_t i = 0; i < heavy.size(); i++) heavy[i] = key((u64{1} << 40) | i);
  FuzzTruth tr{{}, H, newest.load(), 0};
  const double pools[6] = {0.25, 0.5, 1, 2, 4, 8};
  u64 pool = static_cast<u64>(share);
  std::int64_t phaseLeft = 0;
  const double tick = static_cast<double>(H) / perH;
  double acc = 0.0;
  u64 trimAt = tr.newest + H;
  std::uint64_t hs[16];
  bool out[16];
  std::int64_t cnt[16];
  auto evOf = [](const FuzzTruth& t, u64 h) {
    auto it = t.ev.find(h);
    return it == t.ev.end() ? std::vector<u64>{} : it->second;
  };
  auto checkR = [&](const char* op, u64 h, u64 window, u64 now, const Result& r) {
    res.checks++;
    if (r.Saturated) {
      res.sat++;
      return true;
    }
    tr.newest = std::max(tr.newest, newest.load());  // a call at this clock may have expired older events
    std::int64_t exact = tr.count(h, window, now);
    if (r.Upper >= exact) return true;
    if (helpers::floorLive(c, h, window, now)) {
      res.floored++;
    } else if (c.skW == 0 && c.lostRecently(now)) {
      res.lostLow++;
    } else {
      res.fail = "thread " + std::to_string(g) + " op " + std::to_string(res.ops) + " " + op + ": Upper " +
                 std::to_string(r.Upper) + " < true " + std::to_string(exact) + " (result " + helpers::show(r) +
                 "); key " + helpers::hex(h) + " window " + std::to_string(window) + " now " + std::to_string(now) +
                 " newest " + std::to_string(tr.newest) + "; events " + tailOf(evOf(tr, h), 12) + "; " +
                 helpers::whereIs(c, h, now);
      return false;
    }
    return true;
  };
  for (; res.ops < ops; res.ops++) {
    if (phaseLeft == 0) {
      double pf = pools[rng.IntN(6)];
      pool = std::max<u64>(1, static_cast<u64>(pf * static_cast<double>(share)));
      phaseLeft = 1 + rng.IntN(4 * share);
    }
    phaseLeft--;
    acc += tick * 2 * rng.Float64();
    if (acc >= 1) {
      clock.fetch_add(static_cast<u64>(acc));
      acc -= static_cast<double>(static_cast<u64>(acc));
    }
    if (rng.Float64() * perH < 0.1) clock.fetch_add(1 + rng.Uint64N(3 * H));
    u64 now = clock.load();
    if (maxLag > 0 && rng.IntN(4) == 0) now -= rng.Uint64N(maxLag + 1);
    if (straggler) now -= std::min(now - 1, H / 2 + rng.Uint64N(H / 2 + 1));
    for (u64 n = newest.load(); now > n && !newest.compare_exchange_strong(n, now);) {
    }
    tr.newest = std::max(tr.newest, now);
    u64 window = 1 + rng.Uint64N(H + H / 4);
    if (rng.IntN(8) == 0) {  // a shared key
      u64 h = sh.keys[static_cast<std::size_t>(rng.IntN(static_cast<std::int64_t>(sh.keys.size())))];
      std::int64_t op = rng.IntN(4);
      if (op == 0) {
        f.TouchAt(h, window, now);
      } else if (op == 1) {
        f.CheckAndTouchAt(h, window, now);
      } else if (op == 2) {
        f.TouchAndCountAt(h, window, now);
      } else {
        FuzzTruth before = sh.snapshot(h);  // touches that returned before the count began
        Result r = f.CountAt(h, window, now);
        res.checks++;
        if (r.Saturated) {
          res.sat++;
        } else {
          before.newest = std::max(tr.newest, newest.load());
          if (std::int64_t exact = before.count(h, window, now);
              r.Upper < exact && !helpers::floorLive(c, h, window, now) && !(c.skW == 0 && c.lostRecently(now))) {
            res.fail = "thread " + std::to_string(g) + " op " + std::to_string(res.ops) + " shared Count: Upper " +
                       std::to_string(r.Upper) + " < true " + std::to_string(exact) + " (result " + helpers::show(r) +
                       "); key " + helpers::hex(h) + " window " + std::to_string(window) + " now " +
                       std::to_string(now) + " newest " + std::to_string(before.newest) + "; events " +
                       tailOf(evOf(before, h), 12) + "; " + helpers::whereIs(c, h, now);
            return res;
          }
        }
      }
      if (op < 3) sh.add(h, now);
      if (res.ops % 4096 == 0) {
        std::lock_guard<std::mutex> lk(sh.mu);
        sh.tr.newest = std::max(sh.tr.newest, newest.load());
        sh.tr.trim();
      }
      continue;
    }
    u64 h = rng.IntN(16) == 0 ? heavy[static_cast<std::size_t>(rng.IntN(static_cast<std::int64_t>(heavy.size())))]
                              : key(rng.Uint64N(pool));
    std::int64_t op = rng.IntN(100);
    if (op < 40) {
      tr.add(h, now);
      if (!checkR("Touch", h, window, now, f.TouchAt(h, window, now))) return res;
    } else if (op < 45) {
      std::int64_t n = 1 + rng.IntN(64);
      for (std::int64_t k = 0; k < n; k++) {
        f.TouchAt(h, 0, now);
        tr.add(h, now);
      }
      res.ops += static_cast<std::uint64_t>(n - 1);
    } else if (op < 50) {
      f.CheckAndTouchAt(h, window, now);
      tr.add(h, now);
    } else if (op < 55) {
      f.TouchAndCountAt(h, window, now);
      tr.add(h, now);
    } else if (op < 85) {
      if (!checkR("Count", h, window, now, f.CountAt(h, window, now))) return res;
    } else if (op < 90) {
      f.ContainsAt(h, window, now);
      f.VelocityCountAt(h, window, now);
      res.ops++;
    } else if (op < 97) {  // a batch of 16, drawn from 4 to 16 keys
      std::vector<u64> ks(static_cast<std::size_t>(4 + rng.IntN(13)));
      for (auto& x : ks) {
        if (rng.IntN(2) == 0) {
          x = heavy[static_cast<std::size_t>(rng.IntN(static_cast<std::int64_t>(heavy.size())))];
        } else {
          x = key(rng.Uint64N(pool));
        }
      }
      for (auto& x : hs) x = ks[static_cast<std::size_t>(rng.IntN(static_cast<std::int64_t>(ks.size())))];
      if (rng.IntN(2) == 0) {
        f.CheckAndTouchBatch(hs, window, now, out);
      } else {
        f.TouchAndCountBatch(hs, window, now, out, cnt);
      }
      for (u64 x : hs) tr.add(x, now);
      f.VelocityCountBatch(hs, window, now, cnt);
      res.ops++;
    } else {
      c.decayStep(now, static_cast<u64>(1 + rng.IntN(16)));
    }
    if (tr.newest >= trimAt && !tr.ev.empty()) {
      tr.trim();
      trimAt = tr.newest + H;
    }
  }
  return res;
}

// cfuzzCase runs one concurrent case.
FuzzResult cfuzzCase(std::uint64_t seed) {
  FuzzResult res;
  gorand::Rand rng(seed, 0xc0f022);
  std::int64_t capacity;
  std::uint64_t H;
  Config cfg = fuzzConfig(rng, res, capacity, H);
  auto amp = helpers::TagAmp::fromEnv("TF_CFUZZ_TAGS");
  if (amp) {  // fewer than 65,536 keys a case
    capacity = std::min<std::int64_t>(capacity, 4096);
    cfg.Capacity = capacity;
  }
  std::atomic<u64> clock{1 + rng.Uint64N(std::uint64_t{1} << 30)};
  std::atomic<u64> newest{clock.load()};
  cfg.Clock = [&clock] { return clock.load(); };
  std::int64_t mode = rng.IntN(5);
  if (mode == 1) cfg.Background = true, res.bg = 1;
  std::unique_ptr<Filter> fp;
  try {
    fp = std::make_unique<Filter>(cfg);
  } catch (const std::exception& e) {
    res.fail = std::string("New: ") + e.what();
    return res;
  }
  Filter& f = *fp;
  Core& c = core(f);
  if (mode == 0) c.ct = true, res.ct = 1;
  const int procs = static_cast<int>(std::max(1u, std::thread::hardware_concurrency()));
  const int workers = 2 + static_cast<int>(rng.IntN(2 * procs - 1));
  const std::uint64_t ops = std::uint64_t{1} << (13 + rng.IntN(5));  // per thread: 8K .. 128K
  const u64 lags[4] = {0, 2, c.unit, 2 * c.unit};
  const u64 maxLag = lags[rng.IntN(4)];
  const double perHs[5] = {0.25, 1, 4, 16, 64};
  const double perH = static_cast<double>(capacity) * perHs[rng.IntN(5)];
  std::string desc = "capacity " + std::to_string(capacity) + " stash " + std::to_string(cfg.Stash) + " overflow " +
                     std::to_string(cfg.Overflow) + " hot " + std::to_string(cfg.Hot) + " ct " + std::to_string(c.ct) +
                     " bg " + std::to_string(c.bg) + " threads " + std::to_string(workers) + " horizon " +
                     std::to_string(H) + " unit " + std::to_string(c.unit) + " lag <= " + std::to_string(maxLag) +
                     " ops/horizon " + std::to_string(perH);
  CfuzzShared sh;
  sh.keys.resize(static_cast<std::size_t>(1 + rng.IntN(16)));
  sh.tr.horizon = H;
  const char* sv = std::getenv("TF_CFUZZ_STRAGGLE");
  const bool straggle = sv && *sv;
  if (straggle) desc += " straggler";
  for (std::size_t i = 0; i < sh.keys.size(); i++)
    sh.keys[i] = helpers::amp(amp.get(), mix64((u64{1} << 62) | i), nullptr);
  std::vector<u64> seeds(static_cast<std::size_t>(workers));
  for (auto& s : seeds) s = rng.Uint64();
  std::mutex mu;
  std::vector<std::thread> ts;
  for (int g = 0; g < workers; g++) {
    ts.emplace_back([&, g] {
      FuzzResult r =
          cfuzzWorker(f, static_cast<u64>(g), seeds[static_cast<std::size_t>(g)], ops, capacity / workers + 1, H,
                      maxLag, perH, straggle && g == 0, clock, newest, sh, amp.get());
      std::lock_guard<std::mutex> lk(mu);
      res.add(r);
      if (!r.fail.empty() && res.fail.empty()) res.fail = r.fail + "; " + desc;
    });
  }
  for (auto& t : ts) t.join();
  res.evictions = f.Evictions(), res.folded = f.Folded(), res.promoted = f.Promoted(), res.waited = f.Waited();
  if (std::int64_t occ = f.Occupancy(newest.load()); occ > f.Slots() && res.fail.empty())
    res.fail = "occupancy " + std::to_string(occ) + " > slots; " + desc;
  return res;
}

}  // namespace

// TestFuzzConcurrent (TF_CFUZZ=<duration>, e.g. 5m) is the soak over the
// fuzzer's configurations, with ConstantTime or Background in some cases: 2
// to 2x cores threads on one filter, each checking Upper on its own keys, and
// a few keys every thread shares. TF_CFUZZ_TAGS=n amplifies tag
// coincidences; TF_CFUZZ_STRAGGLE=1 makes thread 0 a straggler;
// TF_CFUZZ_CASE=<seed> runs one case TF_CFUZZ_RUNS times (default 20).
TEST(TestFuzzConcurrent) {
  if (const char* s = std::getenv("TF_CFUZZ_CASE"); s && *s) {
    std::uint64_t seed = std::stoull(s);
    const char* rv = std::getenv("TF_CFUZZ_RUNS");
    int runs = rv ? std::atoi(rv) : 0;
    if (runs <= 0) runs = 20;
    FuzzResult sum;
    for (int i = 0; i < runs; i++) {
      FuzzResult r = cfuzzCase(seed);
      sum.add(r);
      if (!r.fail.empty()) {
        check::fail(__FILE__, __LINE__, "case " + std::to_string(seed) + ": " + r.fail);
        return;
      }
    }
    std::printf("    case %llu: %s\n", static_cast<unsigned long long>(seed), sum.summary().c_str());
    return;
  }
  Duration d = helpers::parseDuration(std::getenv("TF_CFUZZ"));
  if (d <= Duration::zero()) SKIP("set TF_CFUZZ=<duration> to run");
  std::uint64_t seed = 1;
  if (const char* s = std::getenv("TF_CFUZZ_SEED"); s && *s) seed = std::stoull(s);
  const std::uint64_t first = seed;
  FuzzResult sum;
  auto t0 = std::chrono::steady_clock::now();
  for (; std::chrono::steady_clock::now() - t0 < d; seed++) {
    FuzzResult r = cfuzzCase(seed);
    sum.add(r);
    if (!r.fail.empty()) check::fail(__FILE__, __LINE__, "case " + std::to_string(seed) + ": " + r.fail);
  }
  double el = std::chrono::duration<double>(std::chrono::steady_clock::now() - t0).count();
  std::printf("    %llu cases from seed %llu in %.0fs (%.1f M ops/s): %s\n",
              static_cast<unsigned long long>(seed - first), static_cast<unsigned long long>(first), el,
              static_cast<double>(sum.ops) / el / 1e6, sum.summary().c_str());
}
