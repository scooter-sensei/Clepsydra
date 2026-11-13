// go/register_test.go: the velocity register's arithmetic.
#include <algorithm>
#include <map>
#include <vector>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

namespace {
// lastGap reports whether some gap between consecutive events exceeded the
// reset threshold (the register then forgets everything before it).
bool lastGap(const std::map<i64, u64>& hist, i64 end) {
  std::vector<i64> ts;
  for (auto& [t, n] : hist) ts.push_back(t);
  std::sort(ts.begin(), ts.end());
  ts.push_back(end);
  for (std::size_t i = 1; i < ts.size(); i++)
    if (ts[i] - ts[i - 1] > 2 * unitsPerHorizon) return true;
  return false;
}
}  // namespace

// TestAdvanceMatchesHistogram: keep an exact per-unit histogram and bucket it
// directly at the end. The register, driven like bump drives it (advance,
// then add), must match whenever nothing saturates.
TEST(TestAdvanceMatchesHistogram) {
  gorand::Rand rng(3, 4);
  for (int it = 0; it < 3000; it++) {
    i64 u = rng.IntN(1 << 20);
    i64 start = u;
    Register r;
    std::map<i64, u64> hist;
    for (i64 k = 1 + rng.IntN(40); k > 0; k--) {
      i64 next = u + rng.IntN(12);
      if (rng.IntN(8) == 0) next += rng.IntN(3 * unitsPerHorizon);
      r.advance(u, next);
      u = next;
      r.add(0, 1);
      hist[u]++;
    }
    i64 end = u + rng.IntN(2 * unitsPerHorizon + 5);
    r.advance(u, end);
    Register want;
    if (end - u <= 2 * unitsPerHorizon) {
      for (auto& [t, n] : hist) {
        for (int j = 0; j < nBuckets; j++) {
          i64 hi = i64{1} << 62;
          if (j > 0) hi = bucketStart(end, j - 1);
          if (t >= bucketStart(end, j) && t < hi) want[j] += n;
        }
      }
      // history older than the last reset gap was legitimately dropped
      if (lastGap(hist, end)) continue;
    }
    if (r.c != want.c) FATALF("start %d end %d: register %v, histogram %v", start, end, r.c, want.c);
  }
}

TEST(TestBucketsCoverHorizon) {
  for (i64 u = 64; u < 4096; u++) {
    if (i64 span = u - bucketStart(u, nBuckets - 1); span < unitsPerHorizon)
      FATALF("u=%d: buckets cover only %d units", u, span);
  }
}

TEST(TestPackRoundTripAndSpill) {
  Register r;
  r.add(0, 100);  // bucket 0 holds 31; the rest spills to older buckets
  u64 sum = 0;
  for (u64 c : r.c) sum += c;
  if (sum != 100 || r[0] != 31) FATALF("spill: %v", r.c);
  if (Register got = unpack(r.pack()); got.c != r.c) FATALF("round trip %v -> %v", r.c, got.c);
}

TEST(TestUnitOfMatchesDivision) {
  gorand::Rand rng(5, 6);
  for (Duration h : {Duration(48us), Duration(1s), Duration(1min), Duration(7h)}) {
    Filter f(cfgOf(10, h));
    Core& c = core(f);
    for (int k = 0; k < 100'000; k++) {
      u64 x = rng.Uint64() & tsMask;
      if (rng.IntN(2) == 0) x = x / c.unit * c.unit + static_cast<u64>(rng.IntN(3)) - 1;  // around boundaries
      if (i64 got = c.unitOf(x), want = static_cast<i64>(x / c.unit); got != want)
        FATALF("unit %d: unitOf(%d) = %d, want %d", c.unit, x, got, want);
    }
  }
}

// TestCountsLastTouchWhole: the newest bucket's last event lies exactly at the
// slot's lastTouch, so a window that reaches it counts it whole and spreads
// only the bucket's other events.
TEST(TestCountsLastTouchWhole) {
  auto f = newTest(1000, 1min);  // unit 2,500 ticks: [25,000, 27,500) is one unit
  u64 h = mix64(42);
  for (u64 ts = 25'000; ts <= 27'000; ts += 200) f->TouchAt(h, 0, ts);
  struct C {
    u64 window, want;
  };
  for (C c : {C{200, 2}, C{400, 3}, C{2'000, 11}}) {
    if (Result got = f->CountAt(h, c.window, 27'000); static_cast<u64>(got.Count) != c.want)
      ERRORF("window %d ms: Count %d, want %d (%+v)", c.window, got.Count, c.want, got);
  }
}

// TestRegisterLayoutLiterals: unpack, pack, hot and viewOf write the register's
// layout out in literals; they must decode as bucketBits and bucketShift say,
// on words with each bucket at its edges (0, 1, satPerUnit-1, satPerUnit and
// its largest value, the others random) and on 1,000,000 random registers.
TEST(TestRegisterLayoutLiterals) {
  Filter f(cfgOf(64, 1min, 1ms));
  Core& c = core(f);
  auto ref = [](u64 w) {
    Register r;
    for (int j = 0; j < nBuckets; j++) r[j] = (w >> bucketShift[j]) & ((u64{1} << bucketBits[j]) - 1);
    return r;
  };
  gorand::Rand rng(9, 9);
  std::vector<u64> words;
  for (int j = 0; j < nBuckets; j++) {
    for (u64 cc : {u64{0}, u64{1}, satPerUnit - 1, satPerUnit, (u64{1} << bucketBits[j]) - 1}) {
      u64 w = 0;
      for (int k = 0; k < nBuckets; k++)
        if (k != j) w |= rng.Uint64N(u64{1} << bucketBits[k]) << bucketShift[k];
      words.push_back(w | (cc << bucketShift[j]));
    }
  }
  for (int k = 0; k < 1'000'000; k++) words.push_back(rng.Uint64() & regMask);
  for (u64 w : words) {
    Register r = ref(w);
    if (Register got = unpack(w); got.c != r.c) FATALF("unpack(%#x) = %v, want %v", w, got.c, r.c);
    if (u64 got = r.pack(); got != (w & regMask)) FATALF("pack(%v) = %#x, want %#x", r.c, got, w & regMask);
    bool want = false;
    for (u64 x : r.c) want = want || x >= satPerUnit;
    want = want && !isHot(w);
    if (bool got = hot(w); got != want) FATALF("hot(%#x) = %v, want %v", w, got, want);
    if (!isHot(w)) {
      if (auto [v, ok] = c.viewOf(1, w, 1000); !ok || v.c != r.c)
        FATALF("viewOf(%#x).c = %v (ok %v), want %v", w, v.c, ok, r.c);
    }
  }
}
