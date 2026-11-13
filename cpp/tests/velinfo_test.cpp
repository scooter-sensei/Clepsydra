// go/velinfo_test.go: what information about a key's recent events the
// velocity target needs. The regression tier compares the table with
// go/verify/expected/velocity-information.txt. (velWorkload, which the Go
// file defines, is in workload.hpp.)
#include <algorithm>
#include <array>
#include <cmath>
#include <cstdlib>
#include <functional>
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

// Geom is an aligned register geometry: unit in ticks, and per bucket (newest
// first) its alignment step 2^logStep and offset, in units, as register.go's
// bucketLogStep and bucketOff.
struct Geom {
  u64 unit;
  std::vector<unsigned> logStep;
  std::vector<i64> off;
  i64 start(i64 u, std::size_t j) const { return (u & ~((i64{1} << logStep[j]) - 1)) - off[j]; }
};

// SimBucket is one bucket of a model's state: [lo, hi) in ticks, c events.
struct SimBucket {
  double lo = 0, hi = 0;
  i64 c = 0;
};

// buckets lays ts (events up to last) into g's buckets at the epoch of last's
// unit, as the register holds them (no counter limits). A touch more than a
// horizon after the one before starts the register afresh (bump's reset of a
// stale slot), so only events since the last such gap count.
std::vector<SimBucket> buckets(const Geom& g, const std::vector<u64>& all) {
  std::size_t from = 0;
  for (std::size_t i = all.size() - 1; i > 0; i--) {
    if (all[i] - all[i - 1] > 60'000) {
      from = i;
      break;
    }
  }
  const u64 last = all.back();
  const i64 e = static_cast<i64>(last / g.unit);
  std::vector<SimBucket> out(g.logStep.size());
  double top = static_cast<double>(e + 1) * static_cast<double>(g.unit);
  for (std::size_t j = 0; j < out.size(); j++) {
    double lo = static_cast<double>(g.start(e, j)) * static_cast<double>(g.unit);
    out[j] = SimBucket{lo, top, 0};
    top = lo;
  }
  for (std::size_t i = from; i < all.size(); i++) {
    for (SimBucket& b : out) {
      if (static_cast<double>(all[i]) >= b.lo) {
        b.c++;
        break;
      }
    }
  }
  return out;
}

// simEstimate is register.go's estimate over a bucket list (newest first):
// the newest non-empty bucket ends at last, which holds one event counted
// whole; the cut bucket is spread uniformly. known lists exact event times
// (newest first) taken out of their buckets' counts and counted exactly;
// (*onsets)[j] >= 0 is a lower bound on bucket j's events (its first one lies
// in [onsets[j], onsets[j]+onStep)).
double simEstimate(const std::vector<SimBucket>& bs, u64 last, u64 now, u64 w, const std::vector<u64>& known,
                   const std::vector<double>* onsets, double /*onStep*/) {
  if (now - last > w) return 0;
  const double from = static_cast<double>(now) - static_cast<double>(w);
  double est = 0;
  for (u64 t : known)
    if (static_cast<double>(t) >= from) est++;
  bool newest = true;
  for (std::size_t j = 0; j < bs.size(); j++) {
    const SimBucket& b = bs[j];
    double c = static_cast<double>(b.c);
    double lo = b.lo, hi = b.hi;
    double nk = 0;
    for (u64 t : known)
      if (static_cast<double>(t) >= lo && static_cast<double>(t) < hi) nk++;
    c -= nk;
    if (b.c == 0) {
      if (from >= lo) break;
      continue;
    }
    double a = 0;
    if (newest) {
      newest = false;
      hi = std::max(lo, std::min(static_cast<double>(last), hi));
      if (nk == 0 && from < hi) a = 1;  // the event at last, counted whole
    }
    if (!known.empty()) {
      hi = std::min(hi, static_cast<double>(known.back()));  // older events lie before the oldest known one
      if (hi < lo) hi = lo;
    }
    if (c - a > 0 && onsets && (*onsets)[j] >= 0) {
      // one event in [on, on+step), the others in [on, hi]
      const double on = std::max(lo, (*onsets)[j]);
      if (from <= on) {
        est += c;
        if (from <= lo) continue;
        break;
      }
      const double rest = c - a;
      double p = 0;
      if (hi > on) p = std::max(0.0, std::min(1.0, (hi - from) / (hi - on)));
      est += a + rest * p;
      if (from > lo) break;
      continue;
    }
    if (from <= lo) {
      est += c;
      continue;
    }
    if (from < hi) est += a + (c - a) * (hi - from) / (hi - lo);
    break;
  }
  return est;
}

double simRound(double est, bool in) {
  if (!in) return 0;
  return static_cast<double>(std::max<i64>(1, static_cast<i64>(est + 0.5)));
}

using Model = std::function<double(const std::vector<u64>&, u64, u64)>;

}  // namespace

// TestVelocityInformation (TF_EXP=1) asks what information about a key's
// recent events target 7 (velocity error <= 0.05, tfbench's vel MAE) needs,
// beyond TestVelocityBound's model floor. It replays tfbench's velocity
// workload and scores models built from each key's exact event times: the
// register as laid out (no counter limits; the filter until claims D29), the
// register plus side information a few more bits could carry (the offset of
// each bucket's first event, the exact times of the key's last m events; with
// the newest bucket's offset in 4 bits it must equal the filter, which keeps
// it in the bits its register's unit took before D29), and wider geometries.
// Then the filter's error by traffic shape, as a share of each window's total.
TEST(TestVelocityInformation) {
  if (const char* e = std::getenv("TF_EXP"); !e || !*e) SKIP("set TF_EXP=1");
  auto fp = newTest(200'000, 1min);
  Filter& f = *fp;
  const Geom cur{2500, {0, 0, 1, 2, 3, 3, 3}, {0, 1, 2, 4, 8, 16, 24}};
  auto onset = [](Geom g, int bits, bool all) -> Model {
    return [g, bits, all](const std::vector<u64>& ts, u64 now, u64 w) {
      const u64 last = ts.back();
      std::vector<SimBucket> bs = buckets(g, ts);
      std::vector<double> on(bs.size());
      const double step = static_cast<double>(g.unit) / static_cast<double>(i64{1} << bits);
      for (std::size_t j = 0; j < on.size(); j++) {
        on[j] = -1;
        if (j > 0 && !all) continue;
        for (u64 x : ts) {  // first event in bucket j
          if (static_cast<double>(x) >= bs[j].lo && static_cast<double>(x) < bs[j].hi) {
            const double width = bs[j].hi - bs[j].lo;
            const double s = step * width / static_cast<double>(g.unit);
            if (bits >= 30) {
              on[j] = static_cast<double>(x);
            } else {
              on[j] = bs[j].lo + std::floor((static_cast<double>(x) - bs[j].lo) / s) * s;
            }
            break;
          }
        }
      }
      const double s = bits >= 30 ? 0 : step;
      return simRound(simEstimate(bs, last, now, w, {}, &on, s), now - last <= w);
    };
  };
  auto lastM = [](Geom g, std::size_t m) -> Model {
    return [g, m](const std::vector<u64>& ts, u64 now, u64 w) {
      const u64 last = ts.back();
      std::vector<u64> known;
      for (std::size_t i = ts.size(); i > 0 && known.size() < m; i--) known.push_back(ts[i - 1]);
      return simRound(simEstimate(buckets(g, ts), last, now, w, known, nullptr, 0), now - last <= w);
    };
  };
  auto plain = [](Geom g) -> Model {
    return [g](const std::vector<u64>& ts, u64 now, u64 w) {
      const u64 last = ts.back();
      return simRound(simEstimate(buckets(g, ts), last, now, w, {}, nullptr, 0), now - last <= w);
    };
  };
  // H/48 near now, 8 buckets to cover the horizon; and H/24 with two buckets
  // per width.
  const Geom h48{1250, {0, 0, 1, 2, 3, 4, 4, 4}, {0, 1, 2, 4, 8, 16, 32, 48}};
  const Geom h24x9{2500, {0, 0, 0, 1, 1, 2, 2, 3, 3}, {0, 1, 2, 3, 5, 7, 11, 15, 23}};
  const std::vector<std::pair<std::string, Model>> models{
      {"register", plain(cur)},
      {"+onset newest 2b", onset(cur, 2, false)},
      {"+onset newest 4b", onset(cur, 4, false)},
      {"+onset each 3b", onset(cur, 3, true)},
      {"+onset each exact", onset(cur, 30, true)},
      {"+last 2 exact", lastM(cur, 2)},
      {"+last 4 exact", lastM(cur, 4)},
      {"+last 8 exact", lastM(cur, 8)},
      {"H/48, 8 buckets", plain(h48)},
      {"H/24, 9 buckets", plain(h24x9)},
  };
  constexpr int nshapes = 5;
  using Grid = std::array<std::array<double, 5>, nshapes>;
  std::vector<Grid> errSum(models.size() + 1, Grid{});
  Grid exactSum{};
  std::vector<u64> hs(2000);
  for (std::size_t k = 0; k < hs.size(); k++) hs[k] = mix64(static_cast<u64>(k) + (u64{1} << 50));
  workload::velWorkload([&](int k, u64 tk) { f.CheckAndTouchAt(hs[static_cast<std::size_t>(k)], 0, tk); },
                        [&](int k, const std::vector<u64>& ts, int wi, u64 now) {
                          const u64 w = workload::velWins[static_cast<std::size_t>(wi)];
                          std::size_t first = 0, hi = ts.size();  // sort.Search: the first ts[j]+w >= now
                          while (first < hi) {
                            std::size_t mid = first + (hi - first) / 2;
                            if (!(ts[mid] + w >= now)) {
                              first = mid + 1;
                            } else {
                              hi = mid;
                            }
                          }
                          const double exact = static_cast<double>(ts.size() - first);
                          const std::size_t sh = static_cast<std::size_t>(k % 5), wj = static_cast<std::size_t>(wi);
                          exactSum[sh][wj] += exact;
                          errSum[0][sh][wj] += std::abs(
                              static_cast<double>(f.VelocityCountAt(hs[static_cast<std::size_t>(k)], w, now)) - exact);
                          if (ts.empty()) return;
                          for (std::size_t m = 0; m < models.size(); m++)
                            errSum[m + 1][sh][wj] += std::abs(models[m].second(ts, now, w) - exact);
                        });
  std::vector<std::string> names{"filter"};
  for (const auto& m : models) names.push_back(m.first);
  LOGF("normalized MAE (sum|est-exact| / sum exact), tfbench's velocity workload");
  LOGF("%-20s %7s %7s %7s %7s %7s %7s", "model", "1s", "5s", "15s", "30s", "60s", "mean");
  for (std::size_t m = 0; m < names.size(); m++) {
    std::string line;
    double mean = 0;
    for (std::size_t wi = 0; wi < 5; wi++) {
      double e = 0, x = 0;
      for (std::size_t sh = 0; sh < nshapes; sh++) {
        e += errSum[m][sh][wi];
        x += exactSum[sh][wi];
      }
      mean += e / x / 5;
      line += " " + pad7(e / x);
    }
    LOGF("%-20s%s %s", names[m], line, pad7(mean));
  }
  LOGF("the filter's error by traffic shape, share of each window's total");
  const std::vector<std::string> shapes{"steady", "bursts", "ramp", "sparse", "on/off"};
  for (std::size_t sh = 0; sh < nshapes; sh++) {
    std::string line;
    for (std::size_t wi = 0; wi < 5; wi++) {
      double tot = 0;
      for (std::size_t s = 0; s < nshapes; s++) tot += errSum[0][s][wi];
      line += " " + pad7(errSum[0][sh][wi] / tot);
    }
    LOGF("%-20s%s", shapes[sh], line);
  }
  for (std::size_t wi = 0; wi < 5; wi++) {  // the filter is the register with its newest bucket's 4-bit onset (claims D29)
    for (std::size_t sh = 0; sh < nshapes; sh++) {
      if (errSum[0][sh][wi] != errSum[3][sh][wi])
        ERRORF("the register model with its onset differs from the filter: shape %d window %d: %v vs %v", sh, wi,
               errSum[3][sh][wi], errSum[0][sh][wi]);
    }
  }
}
