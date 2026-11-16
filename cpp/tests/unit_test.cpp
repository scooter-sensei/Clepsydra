// Port-only unit tests: what the Go suite checks elsewhere (TestTickRange, the
// conformance vectors), from the C++ API's side.
#include "check.hpp"
#include "clepsydra/clepsydra.hpp"
#include "core.hpp"
#include "helpers.hpp"

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

// TestConfigUnaddressable: a Config whose table no 64-bit address space holds
// (2^58 lines of 64 B or more), or whose Load is NaN, is refused at
// construction. The reference wraps such a table's size and fails at its make
// or its first index; a wrapped size here would write past a short buffer.
TEST(TestConfigUnaddressable) {
  auto refusal = [](const Config& cfg) -> std::string {
    try {
      Filter f(cfg);
    } catch (const std::length_error&) {
      return "length_error";
    } catch (const Error&) {
      return "Error";
    }
    return "accepted";
  };
  Config cfg;
  cfg.Horizon = 1min;
  cfg.Capacity = std::int64_t{1} << 62;
  cfg.Load = 1;  // 2^60 lines: 2^66 bytes, a byte count that wraps to 0
  CHECK_EQ(refusal(cfg), std::string("length_error"));
  cfg.Capacity = std::numeric_limits<std::int64_t>::max();
  cfg.Load = 0.85;  // 2^61.4 lines: the word count wraps
  CHECK_EQ(refusal(cfg), std::string("length_error"));
  cfg.Load = 1e-300;  // more lines than a 64-bit integer holds
  CHECK_EQ(refusal(cfg), std::string("length_error"));
  cfg.Capacity = 1000;
  cfg.Load = std::numeric_limits<double>::quiet_NaN();
  CHECK_EQ(refusal(cfg), std::string("Error"));
  cfg.Load = 1;
  CHECK_EQ(refusal(cfg), std::string("accepted"));
  cfg.Stash = std::numeric_limits<std::int64_t>::max();  // sized in uint64, as Go's wrapping int sum
  CHECK_EQ(refusal(cfg), std::string("accepted"));       // (the stash itself is allocated at first use)
}

// TestThrowReleasesStripe: an exception out of a first touch releases the
// claim stripe the touch held, so the next first touch on that stripe goes
// ahead. The exception stands in for a lazy table's std::bad_alloc, which
// reaches the slot search with the stripe held (evict's stashPut: the stash,
// or the overflow table); in Go an allocation failure is fatal instead. Plain
// paths hold the stripe from the lookup to the install, ConstantTime's for the
// whole touch.
TEST(TestThrowReleasesStripe) {
  for (bool constantTime : {false, true}) {
    Config cfg;
    cfg.Capacity = 1000;
    cfg.Horizon = 1min;
    cfg.Resolution = 1ms;
    cfg.Key = testKey();
    cfg.Clock = [] { return u64{1}; };
    if (constantTime) cfg.ConstantTime = 1us;
    auto f = std::make_unique<Filter>(cfg);
    helpers::Hooks hooks;
    const u64 h = f->Hash("a");
    testHookOpenSeen = [](i64) { throw std::bad_alloc(); };  // the first slot search of an empty table
    bool threw = false;
    try {
      f->TouchAt(h, 10, 1);
    } catch (const std::bad_alloc&) {
      threw = true;
    }
    testHookOpenSeen = nullptr;
    CHECK(threw);
    u64 h2 = 0;  // another key on h's stripe
    for (int i = 0; h2 == 0 || (h2 >> (64 - claimStripeBits)) != (h >> (64 - claimStripeBits)); i++)
      h2 = f->Hash("b" + std::to_string(i));
    Filter* fp = f.get();
    auto d = helpers::go([fp, h2] { fp->TouchAt(h2, 10, 1); });
    if (!helpers::awaitDone(d)) {
      CHECK_MSG(false, "constantTime=" + std::to_string(constantTime) +
                           ": a first touch on the stripe never returned (the stripe stayed held)");
      (void)f.release();  // its thread still waits for the stripe: the filter must outlive it
      return;
    }
    CHECK(f->CountAt(h2, 10, 1).Seen);
    CHECK_EQ(f->TouchAt(h, 10, 1).Count, std::int64_t{1});  // the throw came before h's install
  }
}

// TestThrowBalancesMoves: a first touch of a key waiting in Background's wait
// room takes it out with its stripe's move begun, and ends the move once the
// key is placed. An exception out of the placement (a lazy table's
// std::bad_alloc, or the placer thread's start) ends the move too: otherwise
// every later first touch on the stripe would wait for it forever. Here the
// slot search's hook throws at an occupant that expired while the key waited;
// the key's history is lost with the exception.
TEST(TestThrowBalancesMoves) {
  if (forceCT) SKIP("Background has no effect with ConstantTime");
  Config cfg;
  cfg.Capacity = 4096;
  cfg.Horizon = 1min;
  cfg.Resolution = 1ms;
  cfg.Key = testKey();
  cfg.Clock = [] { return u64{1}; };
  cfg.Background = true;
  auto f = std::make_unique<Filter>(cfg);
  Core& c = helpers::core(*f);
  c.bgManual = true;
  u64 key = 0;  // a first touch that waits: its lines full at tick 1
  for (u64 i = 1; key == 0; i++) {
    const u64 w = f->Waited();
    f->TouchAt(mix64(i), 0, 1);
    if (f->Waited() != w) key = mix64(i);
  }
  f->TouchAt(key, 0, 2);  // waits again, now with its last touch at 2
  CHECK_EQ(f->Waiting(), std::int64_t{1});
  const u64 now = 2 + c.horizon;  // its lines' occupants (tick 1) expired; the key not yet
  MoveCount& m = c.moveOf(tag(key));
  helpers::Hooks hooks;
  testHookOpenSeen = [](i64) { throw std::bad_alloc(); };
  bool threw = false;
  try {
    f->TouchAt(key, 0, now);
  } catch (const std::bad_alloc&) {
    threw = true;
  }
  testHookOpenSeen = nullptr;
  CHECK(threw);
  CHECK_EQ(m.begun.load(), m.done.load());
  u64 h2 = 0;  // a new key on the waiting key's move stripe
  for (int i = 0; h2 == 0 || ((tag(h2) ^ tag(key)) & ((u64{1} << claimStripeBits) - 1)) != 0; i++)
    h2 = f->Hash("m" + std::to_string(i));
  Filter* fp = f.get();
  auto d = helpers::go([fp, h2, now] { fp->TouchAt(h2, 0, now); });
  if (!helpers::awaitDone(d)) {
    CHECK_MSG(false, "a first touch on the stripe never returned (its move count stayed unbalanced)");
    (void)f.release();  // its thread still spins: the filter must outlive it
    return;
  }
  CHECK(f->CountAt(h2, 10, now).Seen);
}
