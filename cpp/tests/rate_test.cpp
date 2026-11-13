// go/rate_test.go: one key at a steady rate, against the exact count. The
// regression tier compares the log with go/verify/expected/rates.txt.
#include <algorithm>
#include <limits>
#include <string>
#include <vector>

#include "check.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

// TestSustainedRate: one key sent at a steady rate for three horizons of 60 s
// (1 ms ticks; past 1,000/s, several events per tick), queried at every touch
// of the last horizon for 1, 5, 15 and 60 s windows. Per rate and window:
// Count / exact (min, max), the share of answers flagged Saturated, and
// unflagged answers with Upper below the exact count (the fail-closed
// contract: there must be none). With hot entries (the default) and without
// (Hot: -1, the 48-bit register alone). Deterministic: the clock is the
// caller's and nothing is random.
TEST(TestSustainedRate) {
  const u64 hTicks = 60'000;
  const std::vector<int> rates{10, 12, 13, 20, 50, 100, 200, 500, 1000, 2000, 5000};
  const std::vector<u64> windows{1'000, 5'000, 15'000, 60'000};
  i64 low = 0;
  for (int hot : {0, -1}) {
    for (int rate : rates) {
      const u64 every = static_cast<u64>(std::max(1, 1000 / rate));  // ticks between sends
      const int per = std::max(1, rate / 1000);                       // events per send
      Config cfg = cfgOf(1000, 1min, 1ms);
      cfg.Hot = hot;
      Filter f(cfg);
      const u64 h = mix64(static_cast<u64>(rate));
      struct Acc {
        double lo = std::numeric_limits<double>::infinity(), hi = 0;
        i64 sat = 0, n = 0, bad = 0;
      };
      std::vector<Acc> st(windows.size());
      for (u64 now = 1; now < 3 * hTicks; now += every) {
        for (int i = 0; i < per; i++) f.TouchAt(h, 0, now);
        if (now < 2 * hTicks) continue;
        for (std::size_t wi = 0; wi < windows.size(); wi++) {
          const u64 w = windows[wi];
          const i64 exact = per * static_cast<i64>(w / every + 1);  // sends in [now-w, now], as the filter counts
          Result r = f.CountAt(h, w, now);
          Acc& a = st[wi];
          a.n++;
          if (r.Saturated) {
            a.sat++;
          } else if (r.Upper < exact) {
            a.bad++;
          }
          double q = static_cast<double>(r.Count) / static_cast<double>(exact);
          a.lo = std::min(a.lo, q);
          a.hi = std::max(a.hi, q);
        }
      }
      const std::string name = hot < 0 ? "register only" : "hot entries";
      for (std::size_t wi = 0; wi < windows.size(); wi++) {
        const Acc& a = st[wi];
        low += a.bad;
        LOGF("%-13s rate=%d/s window=%ds count/exact min=%.3f max=%.3f saturated=%.1f%% unflagged_upper_low=%d", name,
             rate, windows[wi] / 1000, a.lo, a.hi, 100 * static_cast<double>(a.sat) / static_cast<double>(a.n), a.bad);
      }
      if (hot == 0) LOGF("%-13s rate=%d/s promoted=%d refused=%d", name, rate, f.Promoted(), f.HotRefused());
    }
  }
  if (low > 0) ERRORF("%d unflagged answers read Upper below the exact count", low);
}
