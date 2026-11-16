// go/overflowread_test.go and go/answers_test.go: the overflow table's reads,
// and the answer digest (every value every call returns, over 30 seeded
// cells, hashed as the Go suite hashes it: the regression tier compares the
// lines with go/verify/expected/answer-digest.txt).
#include <algorithm>
#include <cstdlib>
#include <map>
#include <sstream>
#include <vector>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"
#include "workload.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

// TestCheckAndTouchMatchesTouchSeen: CheckAndTouch's answer is Touch's Seen.
// Two filters with one Config are fed one seeded sequence, a flood of
// one-off keys against 6,000 repeat keys coming back at random gaps up to
// the horizon, with SpillMin 1 so every dropped history goes to the overflow
// table. Filter A calls CheckAndTouchAt, B TouchAt; their states stay equal
// and every answer must too. Run with the ghost table and the count-min
// overflow, each in the default mode and in Background, the placer run by
// hand at seeded points.
TEST(TestCheckAndTouchMatchesTouchSeen) {
  const u64 horizon = 60'000, span = 3 * 60'000;
  std::array<std::uint8_t, 16> key{9, 8, 7, 6, 5, 4, 3, 2, 1};
  const std::vector<u64> wins{1'000, 10'000, 60'000};
  for (bool cms : {false, true}) {
    for (bool bg : {false, true}) {
      u64 now = 1;
      Config cfg = cfgOf(2048, 1min, 1ms);
      cfg.SpillMin = 1;
      cfg.Key = key;
      cfg.Clock = [&now] { return now; };
      cfg.Background = bg;
      Filter a(cfg), b(cfg);
      for (Filter* f : {&a, &b}) {
        core(*f).bgManual = true;  // also under TF_BG=1: no placer, so the twins stay in step
        core(*f).cms = cms;
      }
      gorand::Rand rng(5, 1);
      struct Event {
        u64 at, h;
      };
      std::vector<Event> evs;
      for (u64 i = 0; i < 6'000; i++) {
        u64 h = a.HashUint64(i + (u64{1} << 40));
        for (u64 at = rng.Uint64N(horizon); at < span; at += 1 + rng.Uint64N(horizon)) evs.push_back(Event{1 + at, h});
      }
      for (i64 i = 0; i < 8 * a.Slots() * static_cast<i64>(span / horizon); i++) {
        u64 at = 1 + rng.Uint64N(span);
        evs.push_back(Event{at, a.HashUint64(static_cast<u64>(i))});
      }
      std::stable_sort(evs.begin(), evs.end(), [](const Event& x, const Event& y) { return x.at < y.at; });
      i64 seenDegraded = 0;
      for (std::size_t i = 0; i < evs.size(); i++) {
        const Event& e = evs[i];
        now = e.at;
        u64 w = wins[static_cast<std::size_t>(rng.IntN(static_cast<i64>(wins.size())))];
        bool ra = a.CheckAndTouchAt(e.h, w, now);
        Result rb = b.TouchAt(e.h, w, now);
        if (ra != rb.Seen) {
          ERRORF("cms %v, bg %v: call %d (key %x, window %d, at %d): CheckAndTouch %v, Touch %+v", cms, bg, i, e.h, w,
                 now, ra, rb);
          break;
        }
        if (rb.Seen && rb.Degraded) seenDegraded++;
        if (core(a).bg && rng.IntN(50) == 0) {
          for (Filter* f : {&a, &b}) {
            Core& c = core(*f);
            while (c.room.load() && c.placeNext(c.room.load(), now)) {
            }
          }
        }
      }
      if (a.Evictions() != b.Evictions() || a.Folded() != b.Folded() || a.Lost() != b.Lost() ||
          a.Occupancy(now) != b.Occupancy(now))
        ERRORF("cms %v, bg %v: the twins' states differ: evictions %d/%d, folded %d/%d, lost %d/%d, occupancy %d/%d",
               cms, bg, a.Evictions(), b.Evictions(), a.Folded(), b.Folded(), a.Lost(), b.Lost(), a.Occupancy(now),
               b.Occupancy(now));
      LOGF("cms %v, bg %v: %d calls, %d Seen with a dropped history behind it, folded %d, evictions %d", cms, bg,
           evs.size(), seenDegraded, b.Folded(), b.Evictions());
      if (seenDegraded == 0)
        ERRORF(
            "cms %v, bg %v: Touch never answered Seen with a dropped history behind it (folded %d): the overflow "
            "table never answered",
            cms, bg, b.Folded());
    }
  }
}

// TestConstantTimeReadsOverflowOnEveryHit: a ConstantTime filter reads the
// overflow table on every CheckAndTouch, Seen or not.
TEST(TestConstantTimeReadsOverflowOnEveryHit) {
  Hooks hooks;
  u64 now = 1;
  Config cfg = cfgOf(4096, 1min, 1ms);
  cfg.ConstantTime = 200ns;
  cfg.Clock = [&now] { return now; };
  Filter f(cfg);
  Core& c = core(f);
  i64 n = 8 * f.Slots();
  for (i64 i = 0; i < n; i++) {
    u64 h = f.HashUint64(static_cast<u64>(i));
    for (int k = 0; k < 4; k++) f.TouchAt(h, 60'000, now);
    if (i % 256 == 0) now++;
  }
  if (Ghost* g = c.gh.load(); !c.sketchLive(now) || !g || now > g->until.load() || f.Folded() == 0)
    FATALF("overflow table not live at %d (folded %d)", now, f.Folded());
  std::vector<u64> present;
  for (i64 i = n - 1; i >= 0 && present.size() < 1'000; i--) {
    if (u64 h = f.HashUint64(static_cast<u64>(i)); c.locate(h, fingerprint(h), tag(h)) >= 0) present.push_back(h);
  }
  if (present.size() < 1'000) FATALF("only %d flood keys left in the table", present.size());
  i64 reads = 0;
  testHookGhostRead = [&reads] { reads++; };
  for (u64 h : present) {
    if (!f.CheckAndTouchAt(h, 60'000, now)) FATALF("present key %x answered not Seen", h);
  }
  testHookGhostRead = nullptr;
  if (reads < static_cast<i64>(present.size()))
    FATALF("%d overflow reads for %d Seen hits: ConstantTime skipped the overflow table", reads, present.size());
}

namespace {
struct DigestCell {
  std::string mode;
  u64 seed;
  int repeat;
  bool spill1, cms;
};

// Joiner hashes values as Go's fmt.Fprint(h, vals...) then ";" does: operands
// separated by a space when neither is a string.
struct Digest {
  std::map<std::string, workload::Fnv64a> hs;
  void put(const std::string& kind, const std::string& text) { hs[kind].write(text + ";"); }
};

std::string boolStr(bool b) { return b ? "true" : "false"; }
}  // namespace

// TestAnswerDigest (go/answers_test.go; TF_EXP=1) replays a seeded
// single-thread workload across 30 cells and prints a digest of every value
// every call returns, per call kind, plus the counters at the end: one line
// per cell, as the Go test prints it.
TEST(TestAnswerDigest) {
  if (const char* e = std::getenv("TF_EXP"); !e || std::string(e) != "1") SKIP("set TF_EXP=1");
  const std::string modes[3] = {"", "bg", "ct"};
  const u64 seeds[3] = {1, 2, 3};
  std::vector<DigestCell> cells;
  for (const auto& m : modes)
    for (u64 s : seeds) cells.push_back({m, s, 64, false, false});
  for (const auto& m : modes)
    for (u64 s : seeds) cells.push_back({m, s, 6000, false, false});
  for (const auto& m : modes)
    for (u64 s : seeds) cells.push_back({m, s, 6000, true, false});
  for (u64 s : seeds) cells.push_back({"", s, 6000, true, true});
  if (cells.size() != 30) FATALF("expected 30 cells, got %d", cells.size());
  std::array<std::uint8_t, 16> key{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16};
  for (const DigestCell& cl : cells) {
    Config cfg = cfgOf(2048, 1min, 1ms);
    cfg.Key = key;
    if (cl.spill1) cfg.SpillMin = 1;
    if (cl.mode == "ct") cfg.ConstantTime = 200ns;
    if (cl.mode == "bg") cfg.Background = true;
    u64 now = 1;
    cfg.Clock = [&now] { return now; };
    Filter f(cfg);
    Core& c = core(f);
    if (cl.mode == "bg") c.bgManual = true;
    if (cl.cms) c.cms = true;
    gorand::Rand r(cl.seed, 99);
    Digest dg;
    auto res = [](const Result& x) {
      return boolStr(x.Seen) + "/" + std::to_string(x.Count) + "/" + std::to_string(x.Upper) + "/" +
             boolStr(x.Saturated) + "/" + boolStr(x.Restored) + "/" + boolStr(x.Degraded);
    };
    std::vector<u64> repeat(static_cast<std::size_t>(cl.repeat));
    for (std::size_t i = 0; i < repeat.size(); i++) repeat[i] = f.HashUint64(i + (u64{1} << 40));
    u64 oneoff = 0;
    i64 liveSeenChecks = 0;
    const std::vector<u64> wins{1, 500, 1'000, 5'000, 15'000, 30'000, 60'000};
    std::vector<u64> batch(40);
    bool out[40];
    std::vector<i64> counts(40);
    auto slice = [](auto&& seq, std::size_t n) {
      std::string s = "[";
      for (std::size_t i = 0; i < n; i++) {
        if (i) s += " ";
        if constexpr (std::is_same_v<std::decay_t<decltype(seq[0])>, bool>) {
          s += seq[i] ? "true" : "false";
        } else {
          s += std::to_string(seq[i]);
        }
      }
      return s + "]";
    };
    for (int op = 0; op < 400'000; op++) {
      int phase = (op / 25'000) % 5;
      if (r.IntN(1000) < 3 + phase * 4) now++;
      u64 w = wins[static_cast<std::size_t>(r.IntN(static_cast<i64>(wins.size())))];
      u64 h;
      if (r.IntN(100) < 30) {
        h = repeat[static_cast<std::size_t>(r.IntN(static_cast<i64>(repeat.size())))];
      } else if (r.IntN(100) < 10 && oneoff > 0) {
        h = f.HashUint64(oneoff - static_cast<u64>(r.IntN(static_cast<i64>(std::min<u64>(oneoff, 5000)))));
      } else {
        oneoff++;
        h = f.HashUint64(oneoff);
      }
      i64 k = r.IntN(100);
      if (k < 40) {
        bool live = c.sketchLive(now);
        bool seen = f.CheckAndTouchAt(h, w, now);
        if (live && seen) liveSeenChecks++;
        dg.put("check", boolStr(seen));
      } else if (k < 70) {
        dg.put("touch", res(f.TouchAt(h, w, now)));
      } else if (k < 85) {
        dg.put("count", res(f.CountAt(h, w, now)));
      } else if (k < 90) {
        dg.put("contains", boolStr(f.ContainsAt(h, w, now)));
      } else if (k < 95) {
        for (auto& x : batch) {
          x = repeat[static_cast<std::size_t>(r.IntN(static_cast<i64>(repeat.size())))];
          if (r.IntN(3) == 0) {
            oneoff++;
            x = f.HashUint64(oneoff);
          }
        }
        f.CheckAndTouchBatch(batch, w, now, out);
        dg.put("cbatch", slice(out, 40));
      } else {
        for (auto& x : batch) x = repeat[static_cast<std::size_t>(r.IntN(static_cast<i64>(repeat.size())))];
        f.TouchAndCountBatch(batch, w, now, out, counts);
        dg.put("tcbatch", slice(out, 40) + " " + slice(counts, 40));
      }
      if (cl.mode == "bg" && r.IntN(50) == 0) {
        while (c.room.load() && c.placeNext(c.room.load(), now)) {
        }
      }
      if (r.IntN(500) == 0) dg.put("decay", std::to_string(c.decayStep(now, 4)));
    }
    std::ostringstream end;
    end << f.Evictions() << " " << f.Relocations() << " " << f.Restored() << " " << f.Folded() << " " << f.Shed() << " "
        << f.Floored() << " " << f.Lost() << " " << f.Promoted() << " " << f.HotRefused() << " " << f.Demoted() << " "
        << f.HotInUse() << " " << f.Waited() << " " << f.Waiting() << " " << f.Occupancy(now) << " " << f.Bytes();
    dg.put("end", end.str());
    if (cl.repeat == 6000 && liveSeenChecks == 0)  // Go: the cell as %+v
      FATALF("overflow cell {mode:%s seed:%d repeat:%d spill1:%v cms:%v} read 0 seen checks with overflow live",
             cl.mode, cl.seed, cl.repeat, cl.spill1, cl.cms);
    std::string line = check::format("cell mode=\"%s\" seed=%d repeat=%d spill1=%v cms=%v", cl.mode, cl.seed, cl.repeat,
                                     cl.spill1, cl.cms);
    for (const char* kd : {"check", "touch", "count", "contains", "cbatch", "tcbatch", "decay", "end"}) {
      auto it = dg.hs.find(kd);
      u64 sum = it == dg.hs.end() ? 0 : it->second.sum();
      line += check::format(" %s=%016x", kd, sum);
    }
    line += check::format(
        " (evictions %d, folded %d, floored %d, promoted %d, waited %d, seen checks with overflow live %d)",
        f.Evictions(), f.Folded(), f.Floored(), f.Promoted(), f.Waited(), liveSeenChecks);
    check::print(line);
  }
}
