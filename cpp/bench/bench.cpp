// clepsydra_bench: the perf tier's benchmarks, the port of the twelve of
// go/test.sh's PERF_BENCH (go/bench_test.go), run as Go's testing.B runs
// them: each benchmark function is called again for every N, its setup
// included and its timer reset after it, N grows by Go's predictN until a run
// takes -benchtime (default 1s), and RunParallel hands out iterations in Go's
// grains. Output lines read as go test -bench prints them
// ("BenchmarkName-<threads>  N  x ns/op"), so verify scripts that parse one
// parse the other.
//
//	clepsydra_bench [-bench REGEX] [-count N] [-benchtime 1s]
#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <functional>
#include <regex>
#include <string>
#include <thread>
#include <vector>

#include "clepsydra/clepsydra.hpp"
#include "core.hpp"

namespace {

using clepsydra::Config;
using clepsydra::Filter;
using clepsydra::detail::mix64;
using u64 = std::uint64_t;
using i64 = std::int64_t;
using Clock = std::chrono::steady_clock;
using namespace std::chrono_literals;

// sink keeps the reads' answers alive.
std::atomic<u64> sink{0};

// B is testing.B: N, the timer, and RunParallel.
struct B {
  i64 N = 1;
  Clock::time_point start;
  Clock::duration duration{};
  bool timerOn = false;
  i64 previousN = 0;
  Clock::duration previousDuration{};
  std::string extra;  // ReportMetric's "x unit" fields

  void ResetTimer() {
    if (timerOn) start = Clock::now();
    duration = {};
  }
  void StartTimer() {
    if (!timerOn) {
      start = Clock::now();
      timerOn = true;
    }
  }
  void StopTimer() {
    if (timerOn) {
      duration += Clock::now() - start;
      timerOn = false;
    }
  }
  void ReportMetric(double v, const char* unit) {
    char buf[64];
    std::snprintf(buf, sizeof buf, "\t%10.4g %s", v, unit);
    extra += buf;
  }

  // PB is testing.PB: Next hands out the grain of iterations each worker
  // takes from the shared count at a time.
  struct PB {
    std::atomic<u64>* globalN;
    u64 grain, cache = 0, bN;
    bool Next() {
      if (cache == 0) {
        u64 n = globalN->fetch_add(grain) + grain;
        if (n <= bN) {
          cache = grain;
        } else if (n < bN + grain) {
          cache = bN + grain - n;
        } else {
          return false;
        }
      }
      cache--;
      return true;
    }
  };

  void RunParallel(const std::function<void(PB&)>& body) {
    // Iterations that take ~100 µs, at most 1e4.
    u64 grain = 0;
    if (previousN > 0 && previousDuration.count() > 0)
      grain = static_cast<u64>(1e5 * static_cast<double>(previousN) /
                               static_cast<double>(std::chrono::duration_cast<std::chrono::nanoseconds>(previousDuration).count()));
    grain = std::clamp<u64>(grain, 1, 10'000);
    std::atomic<u64> n{0};
    const unsigned procs = std::max(1u, std::thread::hardware_concurrency());
    std::vector<std::thread> ts;
    for (unsigned p = 0; p < procs; p++) {
      ts.emplace_back([&] {
        PB pb{&n, grain, 0, static_cast<u64>(N)};
        body(pb);
      });
    }
    for (auto& t : ts) t.join();
  }
};

constexpr i64 benchKeys = i64{1} << 20;  // 1M live keys: table is 32 MiB, far beyond L2/L3

Config cfg(i64 capacity) {
  Config c;
  c.Capacity = capacity;
  c.Horizon = 1min;
  return c;
}

struct Bench {
  std::unique_ptr<Filter> f;
  std::vector<u64> hs;
};

Bench benchFilter() {
  Bench b{std::make_unique<Filter>(cfg(benchKeys)), std::vector<u64>(static_cast<std::size_t>(benchKeys))};
  for (std::size_t i = 0; i < b.hs.size(); i++) {
    b.hs[i] = mix64(i);
    b.f->CheckAndTouchAt(b.hs[i], 0, 1000);
  }
  return b;
}

// ---- the benchmarks (go/bench_test.go) ----

// Hit path on a cold table: one random cache-line miss per op.
void CheckAndTouchHit(B& b) {
  Bench x = benchFilter();
  u64 w = x.f->Ticks(30s);
  b.ResetTimer();
  for (i64 i = 0; i < b.N; i++) x.f->CheckAndTouchAt(x.hs[static_cast<std::size_t>(i & (benchKeys - 1))], w, 2000 + static_cast<u64>(i >> 10));
}

void CheckAndTouchMissInsert(B& b) {
  Bench x = benchFilter();
  u64 w = x.f->Ticks(30s);
  b.ResetTimer();
  for (i64 i = 0; i < b.N; i++) x.f->CheckAndTouchAt(mix64(static_cast<u64>(benchKeys + i)), w, 2000 + static_cast<u64>(i >> 10));
}

void TouchAndCount(B& b) {
  Bench x = benchFilter();
  u64 w = x.f->Ticks(30s);
  b.ResetTimer();
  for (i64 i = 0; i < b.N; i++) x.f->TouchAndCountAt(x.hs[static_cast<std::size_t>(i & (benchKeys - 1))], w, 2000 + static_cast<u64>(i >> 10));
}

void VelocityCount(B& b) {
  Bench x = benchFilter();
  u64 w = x.f->Ticks(30s);
  b.ResetTimer();
  u64 s = 0;
  for (i64 i = 0; i < b.N; i++) s += static_cast<u64>(x.f->VelocityCountAt(x.hs[static_cast<std::size_t>(i & (benchKeys - 1))], w, 2000));
  sink += s;
}

void CheckAndTouchParallel(B& b) {
  Bench x = benchFilter();
  u64 w = x.f->Ticks(30s);
  std::atomic<u64> seed{0};
  b.ResetTimer();
  b.RunParallel([&](B::PB& pb) {
    u64 i = (seed.fetch_add(1) + 1) * 7919;
    while (pb.Next()) {
      i++;
      x.f->CheckAndTouchAt(x.hs[static_cast<std::size_t>(i & static_cast<u64>(benchKeys - 1))], w, 2000 + (i >> 10));
    }
  });
}

// Hit path with a cache-resident working set (4K keys).
void CheckAndTouchHitHot(B& b) {
  const i64 n = 1 << 12;
  Filter f(cfg(n));
  std::vector<u64> hs(static_cast<std::size_t>(n));
  for (std::size_t i = 0; i < hs.size(); i++) {
    hs[i] = mix64(i);
    f.CheckAndTouchAt(hs[i], 0, 1000);
  }
  u64 w = f.Ticks(30s);
  b.ResetTimer();
  for (i64 i = 0; i < b.N; i++) f.CheckAndTouchAt(hs[static_cast<std::size_t>(i & (n - 1))], w, 2000 + static_cast<u64>(i >> 10));
}

// Worst case: every insert lands in two full lines (no expiry).
void InsertSaturated(B& b) {
  Filter f(cfg(benchKeys / 4));  // 512K slots
  for (i64 i = 0; i < benchKeys; i++) f.CheckAndTouchAt(mix64(static_cast<u64>(i)), 0, 1000);
  u64 w = f.Ticks(30s);
  b.ResetTimer();
  for (i64 i = 0; i < b.N; i++) f.CheckAndTouchAt(mix64((u64{1} << 40) + static_cast<u64>(i)), w, 1000);
}

void CheckAndTouchBatch(B& b) {
  Bench x = benchFilter();
  u64 w = x.f->Ticks(30s);
  bool out[64];
  b.ResetTimer();
  for (i64 i = 0; i < b.N; i += 64) {
    i64 lo = i & (benchKeys - 1);
    x.f->CheckAndTouchBatch(std::span<const u64>(x.hs.data() + lo, 64), w, 2000 + static_cast<u64>(i >> 10), out);
  }
}

void InsertNearFull(B& b) {
  const i64 n = 1 << 18;
  Filter f(cfg(n));
  const u64 live = static_cast<u64>(n * 115 / 100);  // ~98% of slots
  const u64 per = f.Ticks(1min) / live;
  u64 ctr = 0;
  for (; ctr < live; ctr++) f.CheckAndTouchAt(mix64(ctr), 0, 1 + ctr * per);
  b.ResetTimer();
  for (i64 i = 0; i < b.N; i++) {
    ctr++;
    f.CheckAndTouchAt(mix64(ctr), 0, 1 + ctr * per);
  }
  b.ReportMetric(static_cast<double>(f.Evictions()) / static_cast<double>(b.N), "evict/op");
}

// VelocityCount with the table cache-resident: the estimator's CPU cost.
void VelocityCountHot(B& b) {
  const i64 n = 1 << 12;
  Filter f(cfg(n));
  std::vector<u64> hs(static_cast<std::size_t>(n));
  for (std::size_t i = 0; i < hs.size(); i++) {
    hs[i] = mix64(i);
    for (u64 k = 0; k < 5; k++) f.CheckAndTouchAt(hs[i], 0, 1000 + k * 3'000'000);
  }
  u64 w = f.Ticks(30s);
  b.ResetTimer();
  u64 s = 0;
  for (i64 i = 0; i < b.N; i++) s += static_cast<u64>(f.VelocityCountAt(hs[static_cast<std::size_t>(i & (n - 1))], w, 20'000'000));
  sink += s;
}

void ContainsAbsent(B& b) {
  Bench x = benchFilter();
  u64 w = x.f->Ticks(30s);
  b.ResetTimer();
  u64 s = 0;
  for (i64 i = 0; i < b.N; i++) s += x.f->ContainsAt(mix64((u64{1} << 40) + static_cast<u64>(i)), w, 2000);
  sink += s;
}

// Steady-state insert: time advances so keys expire as fast as they arrive,
// holding ~1.0x capacity live (the scorecard's insert column).
void InsertSteady(B& b) {
  Bench x = benchFilter();
  u64 w = x.f->Ticks(30s);
  const u64 perKey = x.f->Ticks(1min) / static_cast<u64>(benchKeys);
  u64 ctr = 0;
  for (i64 i = 0; i < 2 * benchKeys; i++) {  // until the initial keys have expired
    ctr++;
    x.f->CheckAndTouchAt(mix64(static_cast<u64>(benchKeys) + ctr), w, 1000 + ctr * perKey);
  }
  b.ResetTimer();
  for (i64 i = 0; i < b.N; i++) {
    ctr++;
    x.f->CheckAndTouchAt(mix64(static_cast<u64>(benchKeys) + ctr), w, 1000 + ctr * perKey);
  }
}

struct Entry {
  const char* name;
  void (*fn)(B&);
};

const Entry benchmarks[] = {
    {"CheckAndTouchHit", CheckAndTouchHit},
    {"CheckAndTouchMissInsert", CheckAndTouchMissInsert},
    {"TouchAndCount", TouchAndCount},
    {"VelocityCount", VelocityCount},
    {"CheckAndTouchParallel", CheckAndTouchParallel},
    {"CheckAndTouchHitHot", CheckAndTouchHitHot},
    {"InsertSaturated", InsertSaturated},
    {"CheckAndTouchBatch", CheckAndTouchBatch},
    {"InsertNearFull", InsertNearFull},
    {"VelocityCountHot", VelocityCountHot},
    {"ContainsAbsent", ContainsAbsent},
    {"InsertSteady", InsertSteady},
};

// runN calls the benchmark once with b.N = n, timing what follows its
// ResetTimer.
void runN(B& b, const Entry& e, i64 n) {
  b.N = n;
  b.extra.clear();
  b.duration = {};
  b.timerOn = false;
  b.StartTimer();
  e.fn(b);
  b.StopTimer();
  b.previousN = n;
  b.previousDuration = b.duration;
}

// predictN is Go's: the iterations that would take goal at the last rate,
// plus a fifth, growing at most 100x and at least by one, under 1e9.
i64 predictN(i64 goalns, i64 prevIters, i64 prevns, i64 last) {
  if (prevns <= 0) prevns = 1;
  i64 n = goalns * prevIters / prevns;  // as Go: int64 (goal ~1e9 x at most 1e9 iterations)
  n += n / 5;
  n = std::min(n, 100 * last);
  n = std::max(n, last + 1);
  n = std::min<i64>(n, 1'000'000'000);
  return n;
}

}  // namespace

int main(int argc, char** argv) {
  std::string pattern = ".";
  int count = 1;
  std::chrono::nanoseconds benchtime = 1s;
  for (int i = 1; i < argc; i++) {
    if (std::strcmp(argv[i], "-bench") == 0 && i + 1 < argc) {
      pattern = argv[++i];
    } else if (std::strcmp(argv[i], "-count") == 0 && i + 1 < argc) {
      count = std::max(1, std::atoi(argv[++i]));
    } else if (std::strcmp(argv[i], "-benchtime") == 0 && i + 1 < argc) {
      benchtime = std::chrono::nanoseconds(static_cast<i64>(std::strtod(argv[++i], nullptr) * 1e9));
    } else {
      std::fprintf(stderr, "usage: %s [-bench REGEX] [-count N] [-benchtime SECONDS]\n", argv[0]);
      return 2;
    }
  }
  std::regex re(pattern);
  const unsigned procs = std::max(1u, std::thread::hardware_concurrency());
  for (const Entry& e : benchmarks) {
    std::string name = std::string("Benchmark") + e.name;
    if (!std::regex_search(name, re)) continue;
    for (int c = 0; c < count; c++) {
      B b;
      runN(b, e, 1);
      for (i64 n = 1; b.duration < benchtime && n < 1'000'000'000;) {
        i64 last = n;
        n = predictN(benchtime.count(), b.previousN,
                     std::chrono::duration_cast<std::chrono::nanoseconds>(b.duration).count(), last);
        runN(b, e, n);
      }
      double ns = static_cast<double>(std::chrono::duration_cast<std::chrono::nanoseconds>(b.duration).count()) /
                  static_cast<double>(b.N);
      std::printf("%s-%u\t%10lld\t%12.2f ns/op%s\n", name.c_str(), procs, static_cast<long long>(b.N), ns,
                  b.extra.c_str());
      std::fflush(stdout);
    }
  }
  std::printf("sink %llu\n", static_cast<unsigned long long>(sink.load() & 1));
  return 0;
}
