// tfbench: the port of go/cmd/tfbench, the scorecard's harness. Only -exact is
// ported: every deterministic scorecard metric as the raw counts it is
// computed from, which the regression tier compares with
// go/verify/expected/exact.txt. The workloads use seeded PCG streams and the
// unkeyed mix64 and run single-threaded (static FPR sums per-worker counts),
// so the output is a pure function of the code: it must match the Go
// reference's byte for byte.
//
//	tfbench -exact
#include <algorithm>
#include <atomic>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <functional>
#include <limits>
#include <string>
#include <thread>
#include <unordered_map>
#include <vector>

#include "check.hpp"
#include "clepsydra/clepsydra.hpp"
#include "gorand.hpp"
#include "workload.hpp"

namespace {

using clepsydra::Config;
using clepsydra::Filter;
using u64 = std::uint64_t;
using i64 = std::int64_t;
using namespace std::chrono_literals;

constexpr i64 capacity = 200'000;
constexpr u64 hTicks = 60'000;  // ms

// mix64 is the SplitMix64 finalizer, the Go package's former HashUint64:
// unkeyed, so the workloads hash the same in every run.
u64 mix64(u64 x) {
  x ^= x >> 30;
  x *= 0xbf58476d1ce4e5b9ULL;
  x ^= x >> 27;
  x *= 0x94d049bb133111ebULL;
  return x ^ (x >> 31);
}

Config cfgOf(i64 cap, bool ms = true) {
  Config c;
  c.Capacity = cap;
  c.Horizon = 60s;
  if (ms) c.Resolution = 1ms;
  return c;
}

void out(const std::string& s) {
  std::fputs(s.c_str(), stdout);
  std::fputc('\n', stdout);
}

struct ChurnResult {
  double fnr, fpr, live;
  i64 pos, fn, neg, fp, nlive;
  u64 evictions, relocations;
};

// churn replays the churn stream (go/internal/workload) through CheckAndTouch
// and scores every answer after the first horizon against exact ground truth.
ChurnResult churn(double load, u64 seed) {
  Filter f(cfgOf(capacity));
  std::unordered_map<u64, u64> last;
  i64 pos = 0, neg = 0, fn = 0, fp = 0;
  u64 pruned = 0;
  workload::Churn(load, seed, capacity, hTicks, mix64, [&](u64 h, u64 w, u64 now) {
    if (now % hTicks == 0 && now != pruned) {  // keep ground truth bounded
      pruned = now;
      for (auto it = last.begin(); it != last.end();) {
        if (now - it->second > 2 * hTicks) {
          it = last.erase(it);
        } else {
          ++it;
        }
      }
    }
    bool got = f.CheckAndTouchAt(h, w, now);
    auto it = last.find(h);
    bool want = it != last.end() && now - it->second <= w;
    last[h] = now;
    if (now < hTicks) return;  // warm-up
    if (want) {
      pos++;
      if (!got) fn++;
    } else {
      neg++;
      if (got) fp++;
    }
  });
  i64 live = 0;
  for (const auto& [h, t] : last)
    if (4 * hTicks - t <= hTicks) live++;
  return ChurnResult{static_cast<double>(fn) / static_cast<double>(std::max<i64>(pos, 1)),
                     static_cast<double>(fp) / static_cast<double>(std::max<i64>(neg, 1)),
                     static_cast<double>(live) / capacity,
                     pos,
                     fn,
                     neg,
                     fp,
                     live,
                     f.Evictions(),
                     f.Relocations()};
}

// VelWin is one window's velocity error: sum |estimate-exact| and sum exact
// over its queries (both integers).
struct VelWin {
  u64 w;
  double errSum, exact;
};

// velocityMAE: 2000 keys with mixed traffic shapes (steady, bursty, ramp,
// sparse, on/off) over 5 horizons; every ~1 ms one random key is queried for
// a random window. Returns the mean over windows of sum|estimate-exact| / sum
// exact, so short windows weigh as much as long ones.
std::pair<double, std::vector<VelWin>> velocityMAE() {
  Filter f(cfgOf(capacity));
  const int nkeys = 2000;
  std::vector<u64> hs(nkeys);
  for (int k = 0; k < nkeys; k++) hs[static_cast<std::size_t>(k)] = mix64(static_cast<u64>(k) + (u64{1} << 50));
  const std::vector<u64>& wins = workload::velWins;
  std::vector<double> errSum(wins.size()), exactSum(wins.size());
  workload::velWorkload([&](int k, u64 t) { f.CheckAndTouchAt(hs[static_cast<std::size_t>(k)], 0, t); },
                        [&](int k, const std::vector<u64>& ts, int wi, u64 now) {
                          const u64 w = wins[static_cast<std::size_t>(wi)];
                          std::size_t lo = 0, hi = ts.size();  // sort.Search: the first ts[j]+w >= now
                          while (lo < hi) {
                            std::size_t mid = lo + (hi - lo) / 2;
                            if (!(ts[mid] + w >= now)) {
                              lo = mid + 1;
                            } else {
                              hi = mid;
                            }
                          }
                          const i64 exact = static_cast<i64>(ts.size() - lo);  // in [now-w, now], as the filter counts
                          const i64 got = f.VelocityCountAt(hs[static_cast<std::size_t>(k)], w, now);
                          errSum[static_cast<std::size_t>(wi)] += std::abs(static_cast<double>(got - exact));
                          exactSum[static_cast<std::size_t>(wi)] += static_cast<double>(exact);
                        });
  double mean = 0;
  std::vector<VelWin> per;
  for (std::size_t wi = 0; wi < wins.size(); wi++) {
    double perWin = errSum[wi] / exactSum[wi];
    mean += perWin / static_cast<double>(wins.size());
    per.push_back(VelWin{wins[wi], errSum[wi], exactSum[wi]});
  }
  return {mean, per};
}

// staticFPR: table at capacity, probe never-inserted keys with read-only
// Contains, the probes split across cores.
i64 staticFPR(i64 probes) {
  Filter f(cfgOf(capacity));
  for (i64 i = 0; i < capacity; i++) f.CheckAndTouchAt(mix64(static_cast<u64>(i)), 0, 1);
  std::atomic<i64> fp{0};
  const i64 workers = std::max<i64>(1, static_cast<i64>(std::thread::hardware_concurrency()));
  std::vector<std::thread> ts;
  for (i64 w = 0; w < workers; w++) {
    ts.emplace_back([&, w] {
      i64 n = 0;
      for (i64 i = w; i < probes; i += workers)
        if (f.ContainsAt(mix64(static_cast<u64>(capacity + i)), hTicks, 2)) n++;
      fp.fetch_add(n);
    });
  }
  for (auto& t : ts) t.join();
  return fp.load();
}

constexpr i64 fprProbes = 1'000'000'000;
constexpr i64 sprayOffenders = 100;

// spray: 100 repeat offenders (20 hits each), then 20x capacity of one-off
// keys inside the window. Returns how many offenders still read exact.
i64 spray() {
  Filter f(cfgOf(capacity));
  std::vector<u64> off(sprayOffenders);
  for (std::size_t i = 0; i < off.size(); i++) {
    off[i] = mix64((u64{1} << 40) + i);
    for (int j = 0; j < 20; j++) f.CheckAndTouchAt(off[i], 0, 1);
  }
  for (i64 i = 0; i < 20 * capacity; i++) f.CheckAndTouchAt(mix64(static_cast<u64>(i)), 0, 1 + static_cast<u64>(i % 30'000));
  i64 ok = 0;
  for (u64 h : off)
    if (f.VelocityCountAt(h, hTicks, 30'001) == 20) ok++;
  return ok;
}

// burstCount returns the smallest and largest VelocityCount(window) for one
// key right after n touches in a single tick, over 64 burst times spread
// across the register's 8-unit bucket cycle.
std::pair<i64, i64> burstCount(int n, u64 window) {
  const u64 unit = hTicks / 24;
  i64 lo = std::numeric_limits<i64>::max(), hi = 0;
  for (u64 k = 0; k < 64; k++) {
    Filter f(cfgOf(1000));
    u64 h = mix64(42);
    u64 now = 10 * hTicks + k * 8 * unit / 64;
    for (int i = 0; i < n; i++) f.CheckAndTouchAt(h, 0, now);
    i64 c = f.VelocityCountAt(h, window, now);
    lo = std::min(lo, c);
    hi = std::max(hi, c);
  }
  return {lo, hi};
}

// steadyCount returns the smallest and largest ratio of VelocityCount(window)
// to the exact count for one key touched every `every` ticks for 3 horizons,
// queried at each touch in the last horizon.
std::pair<double, double> steadyCount(u64 every, u64 window) {
  Filter f(cfgOf(1000));
  u64 h = mix64(43);
  double lo = std::numeric_limits<double>::infinity(), hi = 0;
  for (u64 now = 1; now < 3 * hTicks; now += every) {
    f.CheckAndTouchAt(h, 0, now);
    if (now < 2 * hTicks) continue;
    double exact = static_cast<double>(window / every + 1);  // touches in [now-window, now], as the filter counts
    double r = static_cast<double>(f.VelocityCountAt(h, window, now)) / exact;
    lo = std::min(lo, r);
    hi = std::max(hi, r);
  }
  return {lo, hi};
}

// poissonCI95 is the exact (Garwood) 95% interval for a Poisson mean given k
// observed events: the means at which P(X >= k) and P(X <= k) are 2.5%.
std::pair<double, double> poissonCI95(i64 k) {
  auto cdf = [](double mu, i64 kk) {  // P(X <= k)
    double term = std::exp(-mu), sum = 0;
    for (i64 j = 0; j <= kk; j++) {
      sum += term;
      term *= mu / static_cast<double>(j + 1);
    }
    return sum;
  };
  auto solve = [&](const std::function<double(double)>& f) {  // f decreasing in mu, root in (0, 10k+10)
    double a = 0, b = 10 * static_cast<double>(k) + 10;
    for (int i = 0; i < 200; i++) {
      double m = (a + b) / 2;
      if (f(m) > 0) {
        a = m;
      } else {
        b = m;
      }
    }
    return (a + b) / 2;
  };
  double lo = 0;
  if (k > 0) lo = solve([&](double mu) { return 0.025 - (1 - cdf(mu, k - 1)); });
  double hi = solve([&](double mu) { return cdf(mu, k) - 0.025; });
  return {lo, hi};
}

double bytesPerKey() {
  Filter probe(cfgOf(capacity, false));
  return static_cast<double>(probe.Bytes()) / capacity;  // the table: stash, overflow and hot entries come on first use
}

void printExact() {
  struct Case {
    double load;
    u64 seed;
  };
  for (const Case& c : {Case{1.0, 2}, Case{1.1, 3}, Case{1.15, 4}}) {
    ChurnResult r = churn(c.load, c.seed);
    out(check::format("churn load=%.2f seed=%d positives=%d missed=%d negatives=%d false_seen=%d live_at_end=%d "
                      "evictions=%d relocations=%d  => FN %.3f%% FP %.2e live/capacity %.2f",
                      c.load, c.seed, r.pos, r.fn, r.neg, r.fp, r.nlive, r.evictions, r.relocations, 100 * r.fnr, r.fpr,
                      r.live));
  }
  i64 n = staticFPR(fprProbes);
  auto [lo, hi] = poissonCI95(n);
  out(check::format("static_fpr probes=%d false_positives=%d  => %.2e (95%% interval %.2e-%.2e)", fprProbes, n,
                    static_cast<double>(n) / fprProbes, lo / fprProbes, hi / fprProbes));
  i64 ok = spray();
  out(check::format("spray offenders=%d exact=%d  => %.0f%%", sprayOffenders, ok,
                    100 * static_cast<double>(ok) / sprayOffenders));
  auto [mean, per] = velocityMAE();
  for (const VelWin& v : per)
    out(check::format("velocity window=%ds abs_error_sum=%.0f exact_sum=%.0f  => %.4f", v.w / 1000, v.errSum, v.exact,
                      v.errSum / v.exact));
  out(check::format("velocity mean_of_windows => %.4f", mean));
  for (u64 w : {u64{1'000}, u64{5'000}, u64{15'000}, u64{60'000}}) {
    for (int bn : {10, 31, 62, 100, 500, 762, 1000, 1273, 2000}) {
      auto [blo, bhi] = burstCount(bn, w);
      out(check::format("burst window=%ds touches=%d reported_min=%d reported_max=%d", w / 1000, bn, blo, bhi));
    }
  }
  for (u64 w : {u64{1'000}, u64{5'000}, u64{15'000}, u64{60'000}}) {
    for (u64 perSec : {1, 5, 10, 12, 13, 20, 50, 100}) {
      auto [slo, shi] = steadyCount(1000 / perSec, w);
      out(check::format("steady window=%ds rate=%d/s reported/exact min=%.3f max=%.3f", w / 1000, perSec, slo, shi));
    }
  }
  Filter probe(cfgOf(capacity, false));
  out(check::format("memory capacity=%d slots=%d bytes=%d  => %.1f B/key", capacity, probe.Slots(), probe.Bytes(),
                    bytesPerKey()));
}

}  // namespace

int main(int argc, char** argv) {
  if (argc == 2 && std::strcmp(argv[1], "-exact") == 0) {
    printExact();
    return 0;
  }
  std::fprintf(stderr,
               "usage: %s -exact\n(only -exact is ported: the deterministic metrics; the speed scorecard is the perf "
               "tier's benchmarks)\n",
               argv[0]);
  return 2;
}
