// Conformance vectors (go/conformance_test.go): spec/vectors/*.json pin the
// filter's observable behaviour bit for bit, and this port must reproduce
// every hash, every op's full Result and the end counters
// (docs/specification/conformance.md).
#include <cinttypes>
#include <fstream>
#include <sstream>

#include "check.hpp"
#include "clepsydra/clepsydra.hpp"
#include "core.hpp"
#include "json.hpp"

using namespace clepsydra;

namespace {

std::string readFile(const std::string& path) {
  std::ifstream in(path, std::ios::binary);
  if (!in) throw std::runtime_error("cannot read " + path);
  std::ostringstream o;
  o << in.rdbuf();
  return o.str();
}

std::array<std::uint8_t, 16> vecKey() {
  std::array<std::uint8_t, 16> k{};
  for (int i = 0; i < 16; i++) k[i] = static_cast<std::uint8_t>(i);
  return k;
}

std::string hex16(std::uint64_t x) {
  char b[17];
  std::snprintf(b, sizeof b, "%016" PRIx64, x);
  return b;
}

std::string show(const Result& r) {
  std::ostringstream o;
  o << "{seen:" << r.Seen << " count:" << r.Count << " upper:" << r.Upper << " saturated:" << r.Saturated
    << " restored:" << r.Restored << " degraded:" << r.Degraded << "}";
  return o.str();
}

void runScenario(const std::string& name) {
  json::Value v = json::parse(readFile(std::string(CLEPSYDRA_VECTOR_DIR) + "/" + name + ".json"));
  const json::Value& c = v["config"];
  const json::Value& end = v["end"];
  if ((detail::forceCT || detail::forceBG) && end["evictions"].u64() > 0) {
    check::log(name + ": skipped (ConstantTime and Background defer inserts that find no free slot)");
    return;
  }
  CHECK_EQ(v["key_hex"].s, std::string("000102030405060708090a0b0c0d0e0f"));
  std::uint64_t now = 0;
  Config cfg;
  cfg.Capacity = c["capacity"].i64();
  cfg.Horizon = std::chrono::milliseconds(c["horizon_ms"].i64());
  cfg.Resolution = std::chrono::milliseconds(c["resolution_ms"].i64());
  if (c.has("load")) cfg.Load = c["load"].f64();
  if (c.has("stash")) cfg.Stash = c["stash"].i64();
  if (c.has("overflow")) cfg.Overflow = c["overflow"].i64();
  if (c.has("spill_min")) cfg.SpillMin = c["spill_min"].i64();
  cfg.Key = vecKey();
  cfg.Clock = [&now] { return now; };
  Filter f(cfg);
  if (v.has("hashes")) {
    for (const auto& h : v["hashes"].a) {
      std::uint64_t got = h.has("string") ? f.Hash(h["string"].s) : f.HashUint64(h["uint64"].u64());
      CHECK_MSG(hex16(got) == h["hash"].s, name + ": hash of " + (h.has("string") ? '"' + h["string"].s + '"'
                                                                                    : h["uint64"].s) +
                                               " = " + hex16(got) + ", want " + h["hash"].s);
    }
  }
  int bad = 0, i = 0;
  for (const auto& op : v["ops"].a) {
    now = op["now_ms"].u64();
    Duration w = std::chrono::milliseconds(op["window_ms"].i64());
    Result r;
    const std::string& kind = op["op"].s;
    if (kind == "touch") {
      r = f.Touch(op["key"].s, w);
    } else if (kind == "count") {
      r = f.Count(op["key"].s, w);
    } else {
      REQUIRE(!"unknown op");
    }
    const json::Value& want = op["want"];
    Result wr;
    wr.Seen = want.flag("seen");
    wr.Count = want["count"].i64();
    wr.Upper = want["upper"].i64();
    wr.Saturated = want.flag("saturated");
    wr.Restored = want.flag("restored");
    wr.Degraded = want.flag("degraded");
    if (!(r == wr)) {
      if (++bad <= 5)
        check::fail(__FILE__, __LINE__,
                    name + ": op " + std::to_string(i) + " " + kind + " " + op["key"].s + " window_ms " +
                        op["window_ms"].s + " now_ms " + op["now_ms"].s + ": got " + show(r) + " want " +
                        show(wr));
    }
    i++;
  }
  if (bad > 5) check::fail(__FILE__, __LINE__, name + ": ... " + std::to_string(bad) + " ops differ in all");
  CHECK_EQ(f.Evictions(), end["evictions"].u64());
  CHECK_EQ(f.Relocations(), end["relocations"].u64());
  CHECK_EQ(f.Lost(), end["lost"].u64());
  CHECK_EQ(f.Shed(), end["shed"].u64());
  CHECK_EQ(f.Restored(), end["restored"].u64());
  CHECK_EQ(f.Occupancy(now), end["occupancy"].i64());
  CHECK_EQ(f.Slots(), end["slots"].i64());
  check::log(name + ": " + std::to_string(i) + " ops, " + std::to_string(bad) + " differ");
}

}  // namespace

TEST(TestConformance) {
  for (const char* name : {"basic", "burst", "churn", "overflow"}) runScenario(name);
}
