// go/secexp_test.go: overflow-sketch geometries and the spill threshold under
// floods. The regression tier compares the log with
// go/verify/expected/sketch-experiments.txt.
#include <algorithm>
#include <cstdlib>
#include <string>
#include <vector>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;

namespace {

// histStash is the stash these experiments ran with (slots/8 at 4,096 keys,
// the default until 3603076); with the ghost table's one bucket per history
// and one floor, it keeps their recorded results reproducible.
constexpr i64 histStash = 4820 / 8;

// FloodResult is one flood's outcome: offenders with Count >= 90% of the
// truth, offenders with Upper below it, and new keys read Degraded and turned
// away by a strict limit of k.
struct FloodResult {
  int rec = 0, below = 0, deg = 0, strict = 0;
};

// flood fills a filter shaped by shape to its design load, adds the
// offenders, floods it with mult x slots keys of weight events each, and
// reads the offenders and fresh probes.
FloodResult flood(const std::function<void(Filter&)>& shape, i64 mult, int weight) {
  const int k = 20, offenders = 200, probes = 20'000;
  const u64 window = 60'000;
  Attacked a = attackFilter(4096, histStash);
  Filter& f = *a.f;
  u64& now = *a.now;
  shape(f);
  gorand::Rand rng(11, static_cast<u64>(mult * 100 + weight));
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
  const i64 per = std::max<i64>(1, f.Slots() * mult / 30'000);
  for (i64 i = 0, n = f.Slots() * mult; i < n; i++) {
    u64 h = rng.Uint64();
    for (int j = 0; j < weight; j++) f.TouchAt(h, 0, now);
    if (i % per == 0) now++;  // the whole flood spans at most 30 s of the 60 s horizon
  }
  now++;
  FloodResult out;
  for (u64 h : offs) {
    Result r = f.CountAt(h, window, now);
    if (r.Upper < k) out.below++;
    if (r.Count >= k * 9 / 10) out.rec++;
  }
  for (int i = 0; i < probes; i++) {
    Result r = f.CountAt(rng.Uint64(), window, now);
    if (r.Degraded) out.deg++;
    if (r.Degraded || r.Upper > k) out.strict++;
  }
  return out;
}

}  // namespace

// TestSketchShapes compares overflow-sketch geometries, and the ghost table in
// the same bytes, under floods (set TF_EXP=1). Each sketch config is a counter
// width, row count, slices per horizon and columns per slot ("ghost /4": a
// ghost table of the bytes of a sketch of one column per 4 slots); floods are
// heavy (keys sent k+1 times, so they evict the offenders) or light (one event
// per key, the cheapest flood).
TEST(TestSketchShapes) {
  if (const char* e = std::getenv("TF_EXP"); !e || !*e) SKIP("set TF_EXP=1");
  const int k = 20, offenders = 200, probes = 20'000;
  struct Cfg {
    std::string name;
    SketchShape sh;
    i64 div;  // columns = slots / div
    bool gh;
  };
  const std::vector<Cfg> cfgs{
      {"16b r4 s4 /8 (dd30cbc)", {4, 16, 4}, 8, false},
      {"16b r4 s4 /16", {4, 16, 4}, 16, false},
      {"16b r4 s4 /4", {4, 16, 4}, 4, false},
      {"8b  r4 s4 /8", {4, 8, 4}, 8, false},
      {"8b  r4 s4 /4", {4, 8, 4}, 4, false},
      {"8b  r4 s4 /2", {4, 8, 4}, 2, false},
      {"16b r3 s4 /8", {3, 16, 4}, 8, false},
      {"8b  r3 s4 /4", {3, 8, 4}, 4, false},
      {"16b r4 s2 /8", {4, 16, 2}, 8, false},
      {"8b  r4 s2 /4", {4, 8, 2}, 4, false},
      {"8b  r4 s2 /2", {4, 8, 2}, 2, false},
      {"ghost /8", {4, 8, 2}, 8, true},
      {"ghost /4", {4, 8, 2}, 4, true},
      {"ghost /2", {4, 8, 2}, 2, true},
  };
  struct Flood {
    std::string name;
    i64 mult;
    int weight;
  };
  const std::vector<Flood> floods{{"heavy 1x", 1, k + 1}, {"heavy 2x", 2, k + 1}, {"heavy 3x", 3, k + 1},
                                  {"heavy 4x", 4, k + 1}, {"light 16x", 16, 1},   {"light 64x", 64, 1}};
  for (const Cfg& c : cfgs) {
    std::string line;
    double bpk = 0;
    for (const Flood& fl : floods) {
      FloodResult r = flood(
          [&](Filter& ff) {
            Core& f = core(ff);
            f.skW = static_cast<u64>(std::max<i64>(64, ff.Slots() / c.div));
            f.skShape = c.sh;
            f.ghChoices = 1;
            f.ghSub = 1;
            f.spillMin = 1;  // shapes alone; TestSpillThreshold varies this
            f.cms = !c.gh;
            bpk = static_cast<double>(c.sh.bytes(f.skW)) / 4096;
          },
          fl.mult, fl.weight);
      if (r.below > 0) ERRORF("%s %s: %d offenders Upper below the truth", c.name, fl.name, r.below);
      line += check::format(" | %3d %5.1f%% %5.1f%%", r.rec, 100 * static_cast<double>(r.deg) / probes,
                            100 * static_cast<double>(r.strict) / probes);
    }
    LOGF("%-24s %5.1f B/key%s", c.name, bpk, line);
  }
  LOGF(
      "columns per flood (heavy 1x, 2x, 3x, 4x, light 16x, 64x): offenders with Count >= 90%% of truth (of %d) | new "
      "keys Degraded | new keys turned away by a strict limit of %d",
      offenders, k);
}

// TestSpillThreshold: histories lighter than spillMin events are shed instead
// of spilled, so cheap one-event floods add no sketch noise (TF_EXP=1).
TEST(TestSpillThreshold) {
  if (const char* e = std::getenv("TF_EXP"); !e || !*e) SKIP("set TF_EXP=1");
  const int k = 20, offenders = 200, probes = 20'000;
  struct Flood {
    std::string name;
    i64 mult;
    int weight;
  };
  const std::vector<Flood> floods{{"heavy 1x", 1, k + 1}, {"heavy 2x", 2, k + 1}, {"w1 16x", 16, 1},
                                  {"w2 16x", 16, 2},      {"w4 16x", 16, 4},      {"w8 8x", 8, 8}};
  struct Shape {
    std::string name;
    SketchShape sh;
    i64 div;
    bool gh;
  };
  const std::vector<Shape> shapes{
      {"16b r4 s4 /8", {4, 16, 4}, 8, false}, {"8b r4 s2 /4", {4, 8, 2}, 4, false}, {"ghost /4", {4, 8, 2}, 4, true}};
  for (const Shape& sh : shapes) {
    for (u64 min : {u64{1}, u64{2}, u64{4}, u64{8}}) {
      std::string line;
      for (const Flood& fl : floods) {
        FloodResult r = flood(
            [&](Filter& ff) {
              Core& f = core(ff);
              f.skW = static_cast<u64>(std::max<i64>(64, ff.Slots() / sh.div));
              f.skShape = sh.sh;
              f.ghChoices = 1;
              f.ghSub = 1;
              f.spillMin = min;
              f.cms = !sh.gh;
            },
            fl.mult, fl.weight);
        if (r.below > 0) ERRORF("%s min %d %s: %d offenders Upper below the truth", sh.name, min, fl.name, r.below);
        line += check::format(" | %3d %5.1f%%", r.rec, 100 * static_cast<double>(r.strict) / probes);
      }
      LOGF("%-14s spillMin %d%s", sh.name, min, line);
    }
  }
  LOGF(
      "floods: heavy 1x, heavy 2x, 16x slots of 1-, 2-, 4-event keys, 8x of 8-event keys; per flood: offenders with "
      "Count >= 90%% (of %d) | new keys turned away by a strict limit of %d",
      offenders, k);
}
