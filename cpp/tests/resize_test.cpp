// go/resize_test.go: Resizable (claims D28).
#include <algorithm>
#include <cmath>
#include <vector>

#include "check.hpp"
#include "helpers.hpp"
#include "workload.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

// TestResize (claims D28): a Resizable resized at 120 s of tfbench's velocity
// workload (2,000 keys, hot entries off) against a filter built at the new
// size and fed every event, every 5 s from the resize: every key at windows
// of 1-60 s. Grown and shrunk with no filter evicting, it must give the same
// Seen, a count error per window within 1.05x the reference's and no
// unflagged Upper below the truth. Shrunk into a size that evicts, the error
// within 1.05x and no more unflagged lows than the reference's. Grown from a
// filter that had evicted, the old filter's losses stay until the transition
// ends and none after it.
TEST(TestResize) {
  if (forceCT || forceBG) SKIP("Resizable refuses ConstantTime and Background filters");
  const u64 H = 60'000;
  struct Case {
    const char* name;
    i64 from, to;
    bool evicts, lossy;
  };
  for (const Case& c : {Case{"grow", 2560, 4096, false, false}, Case{"shrink", 8192, 4096, false, false},
                        Case{"shrink, evicting", 4096, 1280, true, false},
                        Case{"grow from an evicting filter", 1280, 1792, true, true}}) {
    Config cfg = cfgOf(c.from, 1min, 1ms);
    cfg.Hot = -1;
    Resizable z(cfg);
    cfg.Capacity = c.to;
    Filter s(cfg);
    std::vector<u64> hs(2000);
    for (std::size_t k = 0; k < hs.size(); k++) hs[k] = mix64(k + (u64{1} << 50));
    std::vector<std::vector<u64>> times(hs.size());
    u64 resizeAt = 0;
    double errM[5]{}, errS[5]{}, ex[5]{};
    i64 seenDiff = 0, lowM = 0, lowMflag = 0, lowS = 0, lowIn = 0, lowAfter = 0, checkpoints = 0, inTransition = 0,
        falseM = 0, falseS = 0;
    bool probed = false;
    workload::velWorkload(
        [&](int k, u64 tk) {
          if (resizeAt == 0 && tk >= 2 * H) {
            z.Resize(c.to, tk);
            resizeAt = tk;
          }
          z.TouchAt(hs[static_cast<std::size_t>(k)], 0, tk);
          s.TouchAt(hs[static_cast<std::size_t>(k)], 0, tk);
          times[static_cast<std::size_t>(k)].push_back(tk);
        },
        [&](int, const std::vector<u64>&, int, u64 now) {
          if (resizeAt == 0 || now % 5000 != 0) return;
          checkpoints++;
          bool in = TestAccess::inTransition(z);
          if (in) inTransition++;
          if (in && now + 5000 > resizeAt + H && !probed) {  // the last checkpoint in transition
            probed = true;
            for (u64 i = 0; i < 100'000; i++) {
              u64 a = mix64(i + (u64{1} << 52));
              if (z.ContainsAt(a, H, now)) falseM++;
              if (s.ContainsAt(a, H, now)) falseS++;
            }
          }
          for (std::size_t k = 0; k < hs.size(); k++) {
            const auto& ts = times[k];
            for (std::size_t wi = 0; wi < workload::velWins.size(); wi++) {
              u64 w = workload::velWins[wi];
              auto it = std::lower_bound(ts.begin(), ts.end(), now, [w](u64 t, u64 n) { return !(t + w >= n); });
              i64 exact = static_cast<i64>(ts.end() - it);
              Result rm = z.CountAt(hs[k], w, now), rs = s.CountAt(hs[k], w, now);
              if (!c.evicts && rm.Seen != rs.Seen) seenDiff++;
              errM[wi] += std::fabs(static_cast<double>(rm.Count - exact));
              errS[wi] += std::fabs(static_cast<double>(rs.Count - exact));
              ex[wi] += static_cast<double>(exact);
              if (rm.Upper < exact) {
                if (rm.Saturated) {
                  lowMflag++;
                } else if (in) {
                  lowM++;
                  lowIn++;
                } else {
                  lowM++;
                  lowAfter++;
                }
              }
              if (rs.Upper < exact && !rs.Saturated) lowS++;
            }
          }
        });
    double ratio[5];
    for (int wi = 0; wi < 5; wi++) ratio[wi] = errM[wi] / errS[wi];
    LOGF("%s %d -> %d: checkpoints %d (%d in transition); evictions resized %d, reference %d; moved %d", c.name, c.from,
         c.to, checkpoints, inTransition, z.Evictions(), s.Evictions(), TestAccess::moved(z));
    LOGF("%s: count error ratio resized/reference at 1s 5s 15s 30s 60s: %.4f %.4f %.4f %.4f %.4f", c.name, ratio[0],
         ratio[1], ratio[2], ratio[3], ratio[4]);
    LOGF("%s: Seen differs %d; Upper < exact: resized unflagged %d (in transition %d, after %d; flagged %d), reference "
         "unflagged %d; false Seen of 100,000 absent keys in transition %d, reference %d",
         c.name, seenDiff, lowM, lowIn, lowAfter, lowMflag, lowS, falseM, falseS);
    if (c.lossy) {
      if (lowAfter != 0) ERRORF("%s: %d unflagged lows after the transition", c.name, lowAfter);
      continue;
    }
    if (seenDiff != 0) ERRORF("%s: Seen differs %d times", c.name, seenDiff);
    for (int wi = 0; wi < 5; wi++)
      if (ratio[wi] > 1.05)
        ERRORF("%s: count error at %d ms %.4fx the reference's", c.name, workload::velWins[static_cast<std::size_t>(wi)], ratio[wi]);
    if (lowM > lowS) ERRORF("%s: %d unflagged lows against the reference's %d", c.name, lowM, lowS);
    if (!c.evicts && (z.Evictions() != 0 || s.Evictions() != 0 || lowM != 0))
      ERRORF("%s: evictions or unflagged lows where none were expected", c.name);
  }
}

// TestResizeRefuses: hot entries, ConstantTime and Background are refused, and
// so is a resize during a transition.
TEST(TestResizeRefuses) {
  auto refused = [](Config cfg) {
    try {
      Resizable z(cfg);
    } catch (const Error&) {
      return true;
    }
    return false;
  };
  if (!refused(cfgOf(1000, 1min))) ERRORF("hot entries on: no error");
  Config ct = cfgOf(1000, 1min);
  ct.Hot = -1;
  ct.ConstantTime = 1us;
  if (!refused(ct)) ERRORF("ConstantTime: no error");
  Config bg = cfgOf(1000, 1min);
  bg.Hot = -1;
  bg.Background = true;
  if (!refused(bg)) ERRORF("Background: no error");
  if (forceCT || forceBG) return;
  Config cfg = cfgOf(1000, 1min, 1ms);
  cfg.Hot = -1;
  Resizable z(cfg);
  z.Resize(2000, 10);
  bool threw = false;
  try {
    z.Resize(4000, 20);
  } catch (const Error&) {
    threw = true;
  }
  if (!threw) ERRORF("a resize during a transition: no error");
  try {
    z.Resize(4000, 10 + core(TestAccess::cur(z)).horizon + 1);
  } catch (const Error& e) {
    ERRORF("a resize after the transition: %s", e.what());
  }
}
