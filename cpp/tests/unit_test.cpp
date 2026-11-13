// Port-only unit tests: what the Go suite checks elsewhere (TestTickRange, the
// conformance vectors), from the C++ API's side.
#include "check.hpp"
#include "clepsydra/clepsydra.hpp"
#include "core.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace std::chrono_literals;

namespace {
std::array<std::uint8_t, 16> testKey() {
  std::array<std::uint8_t, 16> k{};
  for (int i = 0; i < 16; i++) k[i] = static_cast<std::uint8_t>(i);
  return k;
}
}  // namespace

// TestExactBoundaryContains: lastTouch is exact, so W is checked to the tick by
// ContainsAt and CountAt too (a port-only companion of TestExactBoundary).
TEST(TestExactBoundaryContains) {
  std::uint64_t now = 1000;
  Config cfg;
  cfg.Capacity = 100;
  cfg.Horizon = 1min;
  cfg.Resolution = 1ms;
  cfg.Key = testKey();
  cfg.Clock = [&now] { return now; };
  Filter f(cfg);
  std::uint64_t h = f.Hash("k");
  f.TouchAt(h, 0, now);
  now += 500;
  CHECK(f.ContainsAt(h, 500, now));
  CHECK(!f.ContainsAt(h, 499, now));
  CHECK(f.CountAt(h, 500, now).Seen);
  CHECK(!f.CountAt(h, 499, now).Seen);
  CHECK(f.CheckAndTouchAt(h, 500, now));
}

// TestMaxTick: a Clock past MaxTick is refused at construction; an insert past
// it throws before any lock and leaves the filter usable (claims A35).
TEST(TestMaxTick) {
  std::uint64_t now = MaxTick + 1;
  Config cfg;
  cfg.Capacity = 100;
  cfg.Horizon = 1min;
  cfg.Clock = [&now] { return now; };
  bool threw = false;
  try {
    Filter f(cfg);
  } catch (const TickError&) {
    threw = true;
  }
  CHECK(threw);
  now = 5;
  Filter f(cfg);
  threw = false;
  try {
    f.TouchAt(f.Hash("x"), 10, MaxTick + 1);
  } catch (const TickError&) {
    threw = true;
  }
  CHECK(threw);
  CHECK_EQ(f.TouchAt(f.Hash("x"), 10, 6).Count, std::int64_t{1});
  CHECK_EQ(f.TouchAt(f.Hash("x"), 10, 7).Count, std::int64_t{2});
  bool big = false;
  try {
    Config c2;
    c2.Capacity = 10;
    c2.Horizon = std::chrono::nanoseconds(static_cast<std::int64_t>(MaxTick));
    c2.Resolution = 1ns;
    Filter g(c2);
  } catch (const Error&) {
    big = true;
  }
  CHECK_MSG(big, "a horizon of MaxTick ticks was accepted");
}

// TestTicksNegative: a negative duration converts to 0 ticks.
TEST(TestTicksNegative) {
  Config cfg;
  cfg.Capacity = 10;
  cfg.Horizon = 1min;
  Filter f(cfg);
  CHECK_EQ(f.Ticks(-5s), std::uint64_t{0});
  CHECK_EQ(f.Ticks(3ms), std::uint64_t{3000});
}

// TestConfigDefaults: the sizes the specification's defaults give.
TEST(TestConfigDefaults) {
  Config cfg;
  cfg.Capacity = 1000;
  cfg.Horizon = 1min;
  Filter f(cfg);
  // lines = max(2, ceil(1000 / (0.85 * 4))) = 295
  CHECK_EQ(f.Slots(), std::int64_t{295 * 4});
  CHECK_EQ(f.Bytes(), std::int64_t{295 * 64});
}
