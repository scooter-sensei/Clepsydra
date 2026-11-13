// go/floodexp_test.go: sizing the overflow table and the stash against heavy
// floods. The regression tier compares the log with
// go/verify/expected/flood-sizing.txt.
#include <algorithm>
#include <cstdio>
#include <cstdlib>
#include <string>
#include <vector>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;

// TestFloodSizing (TF_EXP=1) sizes the ghost overflow table and the stash
// against heavy floods of 1x-16x the table's slots (and a 16x flood of
// one-event keys): for each ghost size (units = slots / div), placement (one
// or two candidate buckets), floors per bucket and stash size, the share of
// new keys a strict limit of k turns away, split by cause, the offenders'
// answers, and the bytes per key the filter holds once the flood is in. Same
// workload as TestAttackFloodCollateral.
TEST(TestFloodSizing) {
  if (const char* e = std::getenv("TF_EXP"); !e || !*e) SKIP("set TF_EXP=1");
  const int k = 20, offenders = 200, probes = 20'000;
  const u64 window = 60'000;
  struct Cfg {
    std::string name;
    double div;  // units = slots / div
    int choices;
    int sub;    // floors per bucket
    int stash;  // stash entries: slots / stash
  };
  std::vector<Cfg> cfgs{
      {"one /4   f1 s/8 (17c55c8)", 4, 1, 1, 8},
      {"one /4   f3 s/8", 4, 1, 3, 8},
      {"one /1.8 f1 s/8", 1.8, 1, 1, 8},
      {"two /4   f1 s/8", 4, 2, 1, 8},
      {"two /2   f1 s/8", 2, 2, 1, 8},
      {"two /2   f3 s/8", 2, 2, 3, 8},
      {"two /1.8 f1 s/8", 1.8, 2, 1, 8},
      {"two /1.8 f3 s/8", 1.8, 2, 3, 8},
      {"two /1.8 f3 s/16 (default)", 1.8, 2, 3, 16},
      {"two /1.9 f3 s/16", 1.9, 2, 3, 16},
      {"two /1.75 f3 s/16", 1.75, 2, 3, 16},
      {"two /1.8 f3 s/32", 1.8, 2, 3, 32},
  };
  if (const char* s = std::getenv("TF_FLOOD_CFG"); s && *s) {
    Cfg c;
    if (std::sscanf(s, "%lf %d %d %d", &c.div, &c.choices, &c.sub, &c.stash) != 4) FATALF("TF_FLOOD_CFG %q", s);
    c.name = std::string("env ") + s;
    cfgs = {c};
  }
  struct Flood {
    i64 mult;
    int weight;
  };
  const std::vector<Flood> floods{{1, k + 1}, {2, k + 1}, {3, k + 1},  {4, k + 1},
                                  {6, k + 1}, {8, k + 1}, {16, k + 1}, {16, 1}};
  for (const Cfg& c : cfgs) {
    for (const Flood& fl : floods) {
      Attacked a = attackFilter(4096, 4820 / c.stash);  // 4,820 slots at 4,096 keys
      Filter& f = *a.f;
      Core& fc = core(f);
      u64& now = *a.now;
      fc.skW = static_cast<u64>(std::max<i64>(64, static_cast<i64>(static_cast<double>(f.Slots()) / c.div)));
      fc.ghChoices = c.choices;
      fc.ghSub = c.sub;
      gorand::Rand rng(11, static_cast<u64>(fl.mult * 100 + fl.weight));
      for (i64 i = 0, n = f.Slots() * 85 / 100; i < n; i++) {
        now++;
        f.TouchAt(rng.Uint64(), 0, now);
      }
      std::vector<u64> offs(offenders);
      for (u64& o : offs) {
        o = rng.Uint64();
        for (int i = 0; i < k; i++) f.TouchAt(o, 0, now);
        now++;
      }
      const i64 per = std::max<i64>(1, f.Slots() * fl.mult / 30'000);
      for (i64 i = 0, n = f.Slots() * fl.mult; i < n; i++) {
        u64 h = rng.Uint64();
        for (int j = 0; j < fl.weight; j++) f.TouchAt(h, 0, now);
        if (i % per == 0) now++;  // the flood spans at most 30 s of the 60 s horizon
      }
      now++;
      int below = 0, rec = 0;
      for (u64 h : offs) {
        Result r = f.CountAt(h, window, now);
        if (r.Upper < k) below++;
        if (r.Count >= k * 9 / 10) rec++;
      }
      // Turned away: as the attack tests count it (Degraded or Upper over k),
      // and as the fail-closed recipe in docs/paper/06-security.md does (Upper
      // reaching k). Causes: a fingerprint match (Count from the table) or
      // floors only (Upper, not Count).
      int deg = 0, test = 0, recipe = 0, byMatch = 0, byFloor = 0;
      for (int i = 0; i < probes; i++) {
        Result r = f.CountAt(rng.Uint64(), window, now);
        if (r.Degraded) deg++;
        if (r.Degraded || r.Upper > k) test++;
        if (r.Upper >= k || r.Saturated) {
          recipe++;
          if (r.Count > 0) {
            byMatch++;
          } else {
            byFloor++;
          }
        }
      }
      const i64 bytes = f.Bytes();
      auto pct = [&](int n) { return 100 * static_cast<double>(n) / probes; };
      LOGF("%-27s flood %2dx x %2d: offenders Upper<true %d, Count>=90%% %3d/%d | new keys Degraded %5.2f%%, turned "
           "away (test) %6.2f%% (recipe) %6.2f%% = match %5.2f%% + floor %5.2f%% | floored %6d folded %6d | %5.1f "
           "B/key",
           c.name, fl.mult, fl.weight, below, rec, offenders, pct(deg), pct(test), pct(recipe), pct(byMatch),
           pct(byFloor), f.Floored(), f.Folded(), static_cast<double>(bytes) / 4096);
      if (below > 0) ERRORF("%s flood %dx: %d offenders read Upper below the truth", c.name, fl.mult, below);
    }
  }
}
