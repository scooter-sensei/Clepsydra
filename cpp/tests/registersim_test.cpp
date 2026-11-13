// go/internal/registersim/registers_experiment_test.go: three 48-bit per-key
// velocity encodings, all sharing the exact lastTouch from word0. Time unit is
// 1 ms; horizon H = 60 s. The regression tier compares the table with
// go/verify/expected/register-experiment.txt.
#include <algorithm>
#include <array>
#include <cmath>
#include <functional>
#include <limits>
#include <map>
#include <memory>
#include <string>
#include <vector>

#include "check.hpp"
#include "gorand.hpp"

namespace {

using i64 = std::int64_t;
constexpr double expH = 60'000.0;

// goLog2 is Go's math.Log2: Frexp, then Log(frac)/Ln2 + exp, exact for powers
// of two (not C's log2, which rounds differently).
double goLog2(double x) {
  int exp;
  double frac = std::frexp(x, &exp);
  if (frac == 0.5) return static_cast<double>(exp - 1);
  return std::log(frac) * (1 / 0.693147180559945309417232121458176568) + static_cast<double>(exp);
}

struct VelReg {
  virtual ~VelReg() = default;
  virtual std::string name() const = 0;
  virtual void add(double t) = 0;                                 // record an event at t (non-decreasing)
  virtual double count(double now, double w, double last) = 0;    // estimate events in (now-w, now]
};

// gate applies the two exact facts every encoding gets from word0.
double gate(double est, double now, double w, double last) {
  if (now - last > w) return 0;
  return std::max(1.0, est);
}

// ---- Linear: 6x8-bit shift register, sub-window H/5 ----
struct LinearReg final : VelReg {
  double g = expH / 5;
  i64 e = -(i64{1} << 40);
  std::array<double, 6> c{};
  double last = 0;
  std::string name() const override { return "linear 6x8 (H/5)"; }
  void advance(double t) {
    i64 ne = static_cast<i64>(t / g);
    i64 d = ne - e;
    if (d <= 0) return;
    if (d >= 6) {
      c.fill(0);
    } else {
      std::copy_backward(c.begin(), c.begin() + (6 - d), c.end());
      std::fill(c.begin(), c.begin() + d, 0.0);
    }
    e = ne;
  }
  void add(double t) override {
    advance(t);
    c[0] = std::min(255.0, c[0] + 1);
    last = t;
  }
  double count(double now, double w, double lastT) override {
    LinearReg cp = *this;
    cp.advance(now);
    double sub = static_cast<double>(cp.e) * g;
    double from = now - w;
    double est = cp.c[0];
    if (from > sub && lastT > sub) est *= std::min(1.0, (lastT - std::min(from, lastT)) / (lastT - sub));
    double rem = w / g - (now - sub) / g;
    for (int k = 1; k < 6 && rem > 0; k++) {
      est += cp.c[static_cast<std::size_t>(k)] * std::min(1.0, rem);
      rem--;
    }
    return gate(est, now, w, lastT);
  }
};

// ---- Dyadic carry-shift: buckets whose widths grow 1,1,1-2,2-4,4-8,... units ----
//
// Boundaries: b0 = u (current unit), bj = floor(u/2^(j-1))*2^(j-1) - 2^(j-1).
// When the unit index becomes u with m trailing zeros, buckets 0..m shift up
// one place and bucket m+1 merges into m+2 (or drops past the top). Counter
// widths grow with bucket size; they sum to 48.
int bitsTrailing(i64 u) {
  int m = 0;
  while ((u & 1) == 0 && m < 62) {
    u >>= 1;
    m++;
  }
  return m;
}

struct DyadicReg final : VelReg {
  double unit;
  i64 u = -(i64{1} << 40);
  std::vector<double> c, max;
  explicit DyadicReg(std::vector<int> widths) {
    const int n = static_cast<int>(widths.size());
    unit = expH / static_cast<double>(i64{1} << (n - 2));
    c.assign(widths.size(), 0);
    for (int w : widths) max.push_back(static_cast<double>((i64{1} << w) - 1));
  }
  std::string name() const override {
    return check::format("dyadic %d buckets (H/%d near now)", c.size(), i64{1} << (c.size() - 2));
  }
  void advance(double t) {
    const int n = static_cast<int>(c.size());
    i64 nu = static_cast<i64>(t / unit);
    if (nu - u > (i64{4} << n)) {
      std::fill(c.begin(), c.end(), 0.0);
      u = nu;
      return;
    }
    while (u < nu) {
      u++;
      int top = bitsTrailing(u) + 1;  // buckets 0..top-1 shift; top merges up
      if (top + 1 < n) {
        c[static_cast<std::size_t>(top + 1)] =
            std::min(max[static_cast<std::size_t>(top + 1)], c[static_cast<std::size_t>(top + 1)] + c[static_cast<std::size_t>(top)]);
      } else {
        top = n - 1;  // shift everything, oldest drops
      }
      for (int j = top; j >= 1; j--)
        c[static_cast<std::size_t>(j)] = std::min(max[static_cast<std::size_t>(j)], c[static_cast<std::size_t>(j - 1)]);
      c[0] = 0;
    }
  }
  double bound(int j) const {  // start of bucket j, in ms
    if (j == 0) return static_cast<double>(u) * unit;
    int s = j - 1;
    return static_cast<double>(((u >> s) << s) - (i64{1} << s)) * unit;
  }
  void add(double t) override {
    advance(t);
    c[0] = std::min(max[0], c[0] + 1);
  }
  double count(double now, double w, double last) override {
    DyadicReg cp = *this;
    cp.advance(now);
    double from = now - w;
    double est = 0;
    double hi = now;
    for (std::size_t j = 0; j < cp.c.size(); j++) {
      double lo = cp.bound(static_cast<int>(j));
      double top = hi;
      if (j == 0) top = std::max(lo, std::min(last, now));  // c0's events lie in [lo, last]
      if (from <= lo) {
        est += cp.c[j];
      } else if (from < top) {
        est += cp.c[j] * (top - from) / (top - lo);
      }
      hi = lo;
    }
    return gate(est, now, w, last);
  }
};

// ---- Laplace: k exponential decays (log-encoded), window synthesized per query ----
//
// Stores S_j = sum_i exp(-(last-t_i)/tau_j) as of lastTouch. The box window
// 1[age <= W] is approximated by sum_j a_j exp(-age/tau_j) with a_j from a
// least-squares fit over ages in [0, 1.5H]; the count estimate is
// sum_j a_j S_j(now).
struct LaplaceReg final : VelReg {
  std::vector<double> tau, s;
  int bits;  // per-register encoding width (log2(1+S) fixed point)
  double last = -std::numeric_limits<double>::infinity();
  std::map<double, std::vector<double>> fit;
  LaplaceReg(std::vector<double> t, int b) : tau(std::move(t)), s(tau.size()), bits(b) {}
  std::string name() const override { return check::format("laplace %dx%d", tau.size(), bits); }
  double quant(double x) const {
    double frac = static_cast<double>(i64{1} << (bits - 5));  // 5 integer bits of log2: S < 2^32
    return std::exp2(std::round(goLog2(1 + x) * frac) / frac) - 1;
  }
  void add(double t) override {
    for (std::size_t j = 0; j < tau.size(); j++) s[j] = quant(s[j] * std::exp(-(t - last) / tau[j]) + 1);
    last = t;
  }
  const std::vector<double>& weights(double w) {
    if (auto it = fit.find(w); it != fit.end()) return it->second;
    const std::size_t k = tau.size();
    std::vector<std::vector<double>> A(k, std::vector<double>(k + 1));
    std::vector<double> b(k);
    const double L = 1.5 * expH;
    for (std::size_t i = 0; i < k; i++) {
      for (std::size_t j = 0; j < k; j++) {
        double sij = 1 / tau[i] + 1 / tau[j];
        A[i][j] = (1 - std::exp(-sij * L)) / sij;
      }
      b[i] = tau[i] * (1 - std::exp(-w / tau[i]));
      A[i][k] = b[i];
    }
    for (std::size_t c = 0; c < k; c++) {  // Gauss-Jordan, k <= 4
      std::size_t p = c;
      for (std::size_t i = c + 1; i < k; i++)
        if (std::abs(A[i][c]) > std::abs(A[p][c])) p = i;
      std::swap(A[c], A[p]);
      for (std::size_t i = 0; i < k; i++) {
        if (i != c) {
          double f = A[i][c] / A[c][c];
          for (std::size_t j = c; j <= k; j++) A[i][j] -= f * A[c][j];
        }
      }
    }
    std::vector<double> a(k);
    for (std::size_t i = 0; i < k; i++) a[i] = A[i][k] / A[i][i];
    return fit[w] = a;
  }
  double count(double now, double w, double lastT) override {
    const std::vector<double>& a = weights(w);
    double est = 0;
    for (std::size_t j = 0; j < tau.size(); j++) est += a[j] * s[j] * std::exp(-(now - last) / tau[j]);
    return gate(std::max(0.0, est), now, w, lastT);
  }
};

// ---- traffic ----

std::vector<double> poisson(gorand::Rand& r, double t0, double t1, double rate) {  // rate per ms
  std::vector<double> ev;
  for (double t = t0 + r.ExpFloat64() / rate; t < t1; t += r.ExpFloat64() / rate) ev.push_back(t);
  return ev;
}

struct Gen {
  std::string name;
  std::function<std::vector<double>(gorand::Rand&, double)> f;
};

const std::vector<Gen>& gens() {
  static const std::vector<Gen> g{
      {"steady 2/s", [](gorand::Rand& r, double end) { return poisson(r, 0, end, 2e-3); }},
      {"bursty",
       [](gorand::Rand& r, double end) {
         std::vector<double> ev;
         for (double c : poisson(r, 0, end, 1e-4)) {  // bursts every ~10s
           i64 n = 5 + r.IntN(15);
           for (i64 i = 0; i < n; i++) ev.push_back(c + r.Float64() * 200);
         }
         std::sort(ev.begin(), ev.end());
         return ev;
       }},
      {"ramp 0.1->10/s",
       [](gorand::Rand& r, double end) {
         std::vector<double> ev;
         for (double t = 0.0; t < end;) {
           double rate = (0.1 + 9.9 * t / end) * 1e-3;
           t += r.ExpFloat64() / rate;
           ev.push_back(t);
         }
         return ev;
       }},
      {"sparse 1/20s", [](gorand::Rand& r, double end) { return poisson(r, 0, end, 5e-5); }},
      {"on/off 5s",
       [](gorand::Rand& r, double end) {
         std::vector<double> ev;
         for (double t = 0.0; t < end; t += 10'000) {
           std::vector<double> p = poisson(r, t, t + 5000, 4e-3);
           ev.insert(ev.end(), p.begin(), p.end());
         }
         return ev;
       }},
  };
  return g;
}

}  // namespace

// TestRegisterExperiment: normalized MAE of each encoding over five traffic
// shapes and five windows, 40 seeded trials each.
TEST(TestRegisterExperiment) {
  if (check::short_()) SKIP("");
  const std::vector<double> wins{1000, 5000, 15'000, 30'000, 60'000};
  const std::vector<std::function<std::unique_ptr<VelReg>()>> makers{
      [] { return std::make_unique<LinearReg>(); },
      [] { return std::make_unique<DyadicReg>(std::vector<int>{6, 6, 7, 8, 9, 12}); },
      [] { return std::make_unique<DyadicReg>(std::vector<int>{5, 5, 6, 6, 7, 8, 11}); },
      [] { return std::make_unique<LaplaceReg>(std::vector<double>{expH / 16, expH / 4, expH}, 16); },
      [] { return std::make_unique<LaplaceReg>(std::vector<double>{expH / 32, expH / 8, expH / 3, expH}, 12); },
  };
  std::string out = check::format("\nnormalized MAE (sum|est-exact| / sum exact), by window\n%-34s %-15s", "register",
                                  "traffic");
  for (double w : wins) out += check::format(" %6.0fs", w / 1000);
  out += "\n";
  std::map<std::string, std::vector<double>> total;
  for (const auto& mk : makers) {
    for (const Gen& g : gens()) {
      std::vector<double> errs(wins.size()), sums(wins.size());
      std::string name;
      for (std::uint64_t trial = 0; trial < 40; trial++) {
        gorand::Rand r(trial, 77);
        std::vector<double> ev = g.f(r, 10 * expH);
        std::unique_ptr<VelReg> reg = mk();
        name = reg->name();
        std::vector<double> qs = poisson(r, 2 * expH, 10 * expH, 1 / 500.0);  // query every ~0.5s
        std::size_t i = 0;
        double last = -std::numeric_limits<double>::infinity();
        for (double q : qs) {
          while (i < ev.size() && ev[i] <= q) {
            reg->add(ev[i]);
            last = ev[i];
            i++;
          }
          for (std::size_t wi = 0; wi < wins.size(); wi++) {
            const double w = wins[wi];
            i64 exact = 0;
            for (i64 k = static_cast<i64>(i) - 1; k >= 0 && ev[static_cast<std::size_t>(k)] > q - w; k--) exact++;
            errs[wi] += std::abs(reg->count(q, w, last) - static_cast<double>(exact));
            sums[wi] += static_cast<double>(exact);
          }
        }
      }
      out += check::format("%-34s %-15s", name, g.name);
      for (std::size_t wi = 0; wi < wins.size(); wi++) {
        double e = errs[wi] / std::max(1.0, sums[wi]);
        out += check::format(" %7.3f", e);
        total[name].push_back(e);
      }
      out += "\n";
    }
  }
  out += "\nmean over all traffic x windows:\n";
  for (const auto& mk : makers) {
    std::string n = mk()->name();
    double s = 0;
    for (double e : total[n]) s += e;
    out += check::format("  %-34s %.3f\n", n, s / static_cast<double>(total[n].size()));
  }
  LOGF("%s", out);
}
