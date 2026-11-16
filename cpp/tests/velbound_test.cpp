// go/velbound_test.go: the velocity error split into what the register's
// information allows and what the estimator adds. The regression tier
// compares the table with go/verify/expected/velocity-bound.txt.
#include <algorithm>
#include <cmath>
#include <cstdlib>
#include <string>
#include <vector>

#include "check.hpp"
#include "helpers.hpp"
#include "workload.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

namespace {

std::string pad7(double x) { return check::format("%7.4f", x); }

// binomMedian returns a median of Binomial(n, p) and E|X - median|.
std::pair<i64, double> binomMedian(i64 n, double p) {
  if (n <= 0 || p <= 0) return {0, 0};
  if (p >= 1) return {n, 0};
  double ln = std::lgamma(static_cast<double>(n + 1));
  double lp = std::log(p), lq = std::log1p(-p);
  std::vector<double> pmf(static_cast<std::size_t>(n + 1));
  for (i64 k = 0; k <= n; k++) {
    double a = std::lgamma(static_cast<double>(k + 1));
    double b = std::lgamma(static_cast<double>(n - k + 1));
    pmf[static_cast<std::size_t>(k)] =
        std::exp(ln - a - b + static_cast<double>(k) * lp + static_cast<double>(n - k) * lq);
  }
  i64 m = n;
  double cum = 0;
  for (i64 k = 0; k <= n; k++) {
    cum += pmf[static_cast<std::size_t>(k)];
    if (cum >= 0.5) {
      m = k;
      break;
    }
  }
  double d = 0;
  for (i64 k = 0; k <= n; k++) d += pmf[static_cast<std::size_t>(k)] * std::abs(static_cast<double>(k - m));
  return {m, d};
}

// regEstimate is estimate's interpolation (register.go) for a register view,
// with three options: anchor (the newest bucket's last event, at lastTouch,
// counts whole), median (the cut bucket's share is the median of
// Binomial(events, share) instead of its mean) and onset (the newest bucket's
// events lie from its onset, as estimate reads it). The second value is the
// expected |error| of that median under the uniform model.
std::pair<double, double> regEstimate(const Core& f, const View& v, u64 last, u64 now, u64 window, bool anchor,
                                      bool median, bool onset) {
  double est = 0, mad = 0;
  window = std::min(window, f.horizon);
  const i64 e = v.e;
  const double unit = static_cast<double>(f.unit);
  const double from = static_cast<double>(now) - static_cast<double>(window);
  double top = static_cast<double>(e + 1) * unit;
  bool newest = true;
  for (int j = 0; j < nBuckets; j++) {
    const double lo = static_cast<double>(bucketStart(e, j)) * unit;
    const u64 c = v.c[static_cast<std::size_t>(j)];
    double hi = top;
    top = lo;
    if (c == 0) {
      if (from >= lo) break;
      continue;
    }
    double a = 0;
    if (newest) {
      hi = std::max(lo, std::min(static_cast<double>(last), hi));
      newest = false;
      if (anchor && from < hi) a = 1;
    }
    if (from <= lo) {
      est += static_cast<double>(c);
      continue;
    }
    if (onset && j == 0 && v.on != 0 && static_cast<double>(c) > a) {
      const double on = lo + static_cast<double>(v.on) * unit / 16;
      if (from <= on) {
        est += static_cast<double>(c);
      } else if (hi > on) {
        est += a + (static_cast<double>(c) - a) * std::max(0.0, std::min(1.0, (hi - from) / (hi - on)));
      } else {
        est += a;
      }
      break;
    }
    if (from < hi) {
      const double p = (hi - from) / (hi - lo);
      if (median) {
        auto [m, d] = binomMedian(static_cast<i64>(c) - static_cast<i64>(a), p);
        est += a + static_cast<double>(m);
        mad += d;
      } else {
        est += a + (static_cast<double>(c) - a) * p;
      }
    }
    break;
  }
  return {est, mad};
}

// sortSearch is Go's sort.Search over ts: the first index where pred holds.
template <class P>
std::size_t sortSearch(const std::vector<u64>& ts, P pred) {
  std::size_t lo = 0, hi = ts.size();
  while (lo < hi) {
    std::size_t mid = lo + (hi - lo) / 2;
    if (!pred(ts[mid])) {
      lo = mid + 1;
    } else {
      hi = mid;
    }
  }
  return lo;
}

// idealEstimate counts h's events (ts, sorted) in [now-w, now] from exact
// counts in aligned buckets of width u: whole buckets exactly, the bucket the
// window's edge cuts uniformly, with the newest event counted whole when that
// bucket is the newest. Gated and rounded as estimate is.
double idealEstimate(const std::vector<u64>& ts, u64 u, u64 now, u64 w) {
  if (ts.empty() || now - ts.back() > w) return 0;
  const u64 last = ts.back();
  const u64 from = now - std::min(w, now);
  const u64 bs = from / u * u;
  const u64 be = bs + u;
  const i64 n = static_cast<i64>(ts.size());
  const i64 after = n - static_cast<i64>(sortSearch(ts, [&](u64 x) { return x >= be; }));
  const i64 nB = n - after - static_cast<i64>(sortSearch(ts, [&](u64 x) { return x >= bs; }));
  double est = static_cast<double>(after);
  if (nB > 0) {
    double hi = static_cast<double>(be), a = 0;
    if (last < be) {
      hi = static_cast<double>(last);
      a = 1;
    }
    if (hi > static_cast<double>(bs)) {
      est += a + (static_cast<double>(nB) - a) * (hi - static_cast<double>(from)) / (hi - static_cast<double>(bs));
    } else {
      est += static_cast<double>(nB);
    }
  }
  return static_cast<double>(std::max<i64>(1, static_cast<i64>(est + 0.5)));
}

}  // namespace

// TestVelocityBound (TF_EXP=1) splits the velocity error (tfbench's vel MAE)
// into what the register's information allows and what the estimator adds.
// It replays tfbench's velocity workload and scores, per window,
// sum|estimate - exact| / sum exact: the filter (VelocityCountAt); estimate's
// interpolation as it was until eed54e6 (spread), with the newest bucket's
// last event counted whole (anchored, until claims D29), and with the newest
// bucket's events spread from its onset (what estimate does now, re-derived
// from the key's register: it must equal the filter); the median of the cut
// bucket's binomial share, and that median's expected |error| (the model
// floor); and exact counts in aligned buckets of width u (flat u).
TEST(TestVelocityBound) {
  if (const char* e = std::getenv("TF_EXP"); !e || !*e) SKIP("set TF_EXP=1");
  const i64 capacity = 200'000;
  auto fp = newTest(capacity, 1min);
  Filter& f = *fp;
  Core& c = core(f);
  const int nkeys = 2000;
  std::vector<u64> hs(nkeys);
  for (int k = 0; k < nkeys; k++) hs[static_cast<std::size_t>(k)] = mix64(static_cast<u64>(k) + (u64{1} << 50));
  const std::vector<u64>& wins = workload::velWins;
  const std::vector<u64> flat{2500, 1250, 625, 250, 100};  // H/24 (today's unit), H/48, H/96, H/240, H/600
  std::vector<std::string> names{
      "filter",     "spread (until 10-22)", "anchored (until D29)", "onset (re-derived)", "median (anchored)",
      "model floor"};
  for (u64 u : flat) names.push_back("flat " + check::goDuration(static_cast<i64>(u * 1'000'000)));
  std::vector<std::vector<double>> errSum(names.size(), std::vector<double>(wins.size()));
  std::vector<double> exactSum(wins.size());
  auto round = [](double est, bool gated) {  // estimate's own rounding: at least 1 once lastTouch is in the window
    if (!gated) return 0.0;
    return static_cast<double>(std::max<i64>(1, static_cast<i64>(est + 0.5)));
  };
  workload::velWorkload([&](int k, u64 t) { f.CheckAndTouchAt(hs[static_cast<std::size_t>(k)], 0, t); },
                        [&](int k, const std::vector<u64>& ts, int wi, u64 now) {
                          const u64 w = wins[static_cast<std::size_t>(wi)];
                          const u64 h = hs[static_cast<std::size_t>(k)];
                          const double exact =
                              static_cast<double>(ts.size() - sortSearch(ts, [&](u64 x) { return x + w >= now; }));
                          exactSum[static_cast<std::size_t>(wi)] += exact;
                          std::vector<double> got{static_cast<double>(f.VelocityCountAt(h, w, now))};
                          Found fd = c.read(h, now, -1);
                          const u64 last = fd.w0 & tsMask;
                          View v;
                          bool ok = fd.src != 0 && age(now, last) <= c.horizon;
                          if (ok) std::tie(v, ok) = c.viewOf(c.stashID(h, fingerprint(h), tag(h)), fd.w1, last);
                          const bool in = ok && age(now, last) <= std::min(w, c.horizon);
                          for (int mode : {0, 1, 2, 3}) {  // spread, anchored, onset, median
                            double est = 0, mad = 0;
                            if (in)
                              std::tie(est, mad) = regEstimate(c, v, last, now, w, mode >= 1, mode == 3, mode == 2);
                            got.push_back(round(est, in));
                            if (mode == 3) got.push_back(mad);  // the floor is an expected |error|, scored as is
                          }
                          for (u64 u : flat) got.push_back(idealEstimate(ts, u, now, w));
                          for (std::size_t e = 0; e < got.size(); e++) {
                            if (names[e] == "model floor") {
                              errSum[e][static_cast<std::size_t>(wi)] += got[e];
                              continue;
                            }
                            errSum[e][static_cast<std::size_t>(wi)] += std::abs(got[e] - exact);
                          }
                        });
  LOGF("normalized MAE (sum|est-exact| / sum exact), tfbench's velocity workload");
  LOGF("%-22s %7s %7s %7s %7s %7s %7s", "estimator", "1s", "5s", "15s", "30s", "60s", "mean");
  for (std::size_t e = 0; e < names.size(); e++) {
    std::string line;
    double mean = 0;
    for (std::size_t wi = 0; wi < wins.size(); wi++) {
      double x = errSum[e][wi] / exactSum[wi];
      mean += x / static_cast<double>(wins.size());
      line += " " + pad7(x);
    }
    LOGF("%-22s%s %s", names[e], line, pad7(mean));
  }
  if (errSum[0] != errSum[3]) ERRORF("re-derived estimate differs from the filter's: %v vs %v", errSum[3], errSum[0]);
}
