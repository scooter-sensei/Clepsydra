// go/timing_test.go: what one request's response time tells about a key
// (TestTimingLeak, TF_TIMING=1), and ConstantTime's dithered pad end.
#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <string>
#include <tuple>
#include <vector>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

namespace {

// ks is the Kolmogorov-Smirnov distance between two sorted samples.
double ks(const std::vector<double>& a, const std::vector<double>& b) {
  double d = 0;
  std::size_t i = 0, j = 0;
  while (i < a.size() && j < b.size()) {
    double x = std::min(a[i], b[j]);
    while (i < a.size() && a[i] <= x) i++;
    while (j < b.size() && b[j] <= x) j++;
    d = std::max(d, std::abs(static_cast<double>(i) / static_cast<double>(a.size()) -
                             static_cast<double>(j) / static_cast<double>(b.size())));
  }
  return d;
}

}  // namespace

// TestTimingLeak measures how well one request's response time tells a seen
// key from an unseen one, without ConstantTime and with a 3 µs pad. A table
// at design load (85%) larger than the last-level cache; seen and unseen
// probes in random order, each timed alone. It reports each side's median
// and the Kolmogorov-Smirnov distance D between the two latency
// distributions: an observer who classifies by a threshold on one timing is
// right at most (1+D)/2 of the time. Set TF_TIMING=1 to run. (In C++ the
// compiler, not Go's, decides what the pad hides; this is the port's
// reading of S41's measurement.)
TEST(TestTimingLeak) {
  if (const char* e = std::getenv("TF_TIMING"); !e || !*e) SKIP("set TF_TIMING=1");
  const std::int64_t capacity = 1 << 21;
  const int probes = 40'000;
  for (Duration ct : {Duration{0}, Duration{3us}}) {
    for (std::string op : {"Count", "Touch"}) {
      u64 now = 1;
      Config cfg = cfgOf(capacity, std::chrono::minutes(1), std::chrono::milliseconds(1));
      cfg.Clock = [&now] { return now; };
      cfg.ConstantTime = ct;
      Filter f(cfg);
      gorand::Rand rng(3, 4);
      std::vector<u64> seen(static_cast<std::size_t>(capacity));
      for (auto& s : seen) {
        s = f.HashUint64(rng.Uint64());
        f.TouchAt(s, 0, now);
      }
      now += 1000;
      // Probes are drawn up front and read in order, so fetching one doesn't
      // add a cache miss to the timed call.
      std::vector<int> kind(probes);
      std::vector<u64> hs(probes);
      for (int i = 0; i < probes; i++) {
        kind[static_cast<std::size_t>(i)] = static_cast<int>(rng.IntN(2));
        hs[static_cast<std::size_t>(i)] = f.HashUint64(rng.Uint64());
        if (kind[static_cast<std::size_t>(i)] == 1)
          hs[static_cast<std::size_t>(i)] =
              seen[static_cast<std::size_t>(rng.IntN(static_cast<std::int64_t>(seen.size())))];
      }
      std::vector<double> ts[2];  // [unseen, seen]
      for (int i = 0; i < probes; i++) {
        auto t0 = std::chrono::steady_clock::now();
        if (op == "Count") {
          std::ignore = f.CountAt(hs[static_cast<std::size_t>(i)], 60'000, now);
        } else {
          f.TouchAt(hs[static_cast<std::size_t>(i)], 60'000, now);
        }
        ts[kind[static_cast<std::size_t>(i)]].push_back(static_cast<double>(
            std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::steady_clock::now() - t0).count()));
      }
      std::sort(ts[0].begin(), ts[0].end());
      std::sort(ts[1].begin(), ts[1].end());
      auto q = [](const std::vector<double>& x, double p) {
        return x[static_cast<std::size_t>(p * static_cast<double>(x.size() - 1))];
      };
      double d = ks(ts[0], ts[1]);
      LOGF(
          "ConstantTime %-5v %-5s: unseen p50/p99/p99.9 %4.0f/%5.0f/%5.0f ns, seen %4.0f/%5.0f/%5.0f ns; KS D = %.3f "
          "(best single-timing guess right %.1f%%)",
          ct, op, q(ts[0], .5), q(ts[0], .99), q(ts[0], .999), q(ts[1], .5), q(ts[1], .99), q(ts[1], .999), d,
          50 + 50 * d);
    }
  }
}

// TestCTPadEndIsDithered: a ConstantTime pad ends at a random point over a
// span of several clock reads. A spin that ends at a fixed time ends at a
// phase of the clock read that follows the call's own work, and keys whose
// work time is narrow show it (claims S41). Calls with no work of their own,
// spread end to end: their middle 80% must span more than one clock read.
// Noise only widens the spread.
TEST(TestCTPadEndIsDithered) {
  Config cfg = cfgOf(1024, std::chrono::minutes(1));
  cfg.ConstantTime = 1us;
  Filter f(cfg);
  Core& c = core(f);
  const int n = 20'000;
  std::vector<double> ds(n);
  for (auto& d : ds) {
    auto [t0, pad] = c.ctBegin(opTouch, 1);
    c.ctEnd(opTouch, 0, t0, t0, pad);
    d = static_cast<double>(std::chrono::duration_cast<std::chrono::nanoseconds>(Clock::now() - t0).count());
  }
  std::sort(ds.begin(), ds.end());
  if (double spread = ds[n * 9 / 10] - ds[n / 10]; spread < 80)
    FATALF("padded calls end within %.0f ns (p10 %.0f, p90 %.0f): the pad's end follows the clock's phase", spread,
           ds[n / 10], ds[n * 9 / 10]);
}
