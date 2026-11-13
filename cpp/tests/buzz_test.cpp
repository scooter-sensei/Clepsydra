// go/buzz_test.go: buzz entries (claims A46): a conditional touch refused in
// doubt gives its key an exact log of admitted events.
#include <algorithm>
#include <barrier>
#include <cstdlib>
#include <sstream>
#include <thread>
#include <vector>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"
#include "workload.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

namespace {

struct BuzzRule {
  u64 w, limit;
  bool operator==(const BuzzRule&) const = default;
};
std::string str(const BuzzRule& ru) { return check::format("%vs/%d", static_cast<double>(ru.w) / 1000, ru.limit); }

// BuzzRow is one reading: the same requests offered to an exact sliding log,
// to TouchIfUnderAt without buzz entries (base) and with them.
struct BuzzRow {
  i64 events = 0;
  i64 admitted[3]{};   // exact, base, buzz
  i64 falseDeny[2]{};  // base, buzz
  u64 given = 0, dropped = 0, decided = 0, evictions = 0, relocations = 0;
  i64 bytesBase = 0, bytesBuzz = 0;
  double share(int a) const { return static_cast<double>(admitted[a]) / static_cast<double>(admitted[0]); }
  double denied(int a) const { return static_cast<double>(falseDeny[a]) / static_cast<double>(events); }
  std::string String() const {
    return check::format(
        "requests %7d exact admits %7d | admitted/exact base %.3f buzz %.3f | false denials base %.4f buzz %.4f | "
        "entries given %5d dropped %6d | decided by a log %7d",
        events, admitted[0], share(1), share(2), denied(0), denied(1), given, dropped, decided);
  }
};

// ownWithin counts the events of ts (sorted) inside [now-w, now].
int ownWithin(const std::vector<u64>& ts, u64 w, u64 now) {
  auto it = std::lower_bound(ts.begin(), ts.end(), now, [w](u64 t, u64 nw) { return !(t + w >= nw); });
  return static_cast<int>(ts.end() - it);
}

// buzzOffer offers nkeys keys' Poisson requests, at mult times the rule's
// rate, to the three limiters for the given horizons of 60 s, in filters of
// Capacity capN. One request in 1/plainFrac is a plain TouchAt in every arm.
// With churnLoad both filters also take tfbench's churn stream. It fails the
// test if an arm admits a request while the key's window already holds the
// limit.
BuzzRow buzzOffer(i64 capN, int nkeys, BuzzRule ru, double mult, int horizons, double plainFrac, double churnLoad,
                  u64 churnSeed) {
  const u64 hT = 60'000;
  bool saved = buzzOff;
  Defer restoreOff{[saved] { buzzOff = saved; }};
  BuzzRow row;
  Filter fb(cfgOf(capN, 1min, 1ms)), fz(cfgOf(capN, 1min, 1ms));
  gorand::Rand r(4, ru.w + ru.limit);
  struct Ev {
    u64 t;
    int k;
    bool plain;
  };
  std::vector<Ev> evs;
  double rate = mult * static_cast<double>(ru.limit) / static_cast<double>(ru.w);
  double end = static_cast<double>(horizons) * hT;
  for (int k = 0; k < nkeys; k++) {
    for (double x = r.ExpFloat64() / rate; x < end; x += r.ExpFloat64() / rate)
      evs.push_back(Ev{static_cast<u64>(x) + 1, k, plainFrac > 0 && r.Float64() < plainFrac});
  }
  std::stable_sort(evs.begin(), evs.end(), [](const Ev& a, const Ev& b) { return a.t < b.t; });
  std::vector<std::vector<u64>> own[3];
  for (auto& o : own) o.assign(static_cast<std::size_t>(nkeys), {});
  int limit = static_cast<int>(ru.limit);
  auto offer = [&](const Ev& e) {
    u64 h = mix64(static_cast<u64>(e.k) + (u64{1} << 50));
    auto ki = static_cast<std::size_t>(e.k);
    if (e.plain) {
      for (auto& o : own) o[ki].push_back(e.t);
      fb.TouchAt(h, ru.w, e.t);
      fz.TouchAt(h, ru.w, e.t);
      return;
    }
    row.events++;
    if (ownWithin(own[0][ki], ru.w, e.t) < limit) {
      own[0][ki].push_back(e.t);
      row.admitted[0]++;
    }
    Filter* fs[2] = {&fb, &fz};
    for (int a = 0; a < 2; a++) {
      buzzOff = a == 0;
      int in = ownWithin(own[a + 1][ki], ru.w, e.t);
      if (fs[a]->TouchIfUnderAt(h, ru.w, ru.limit, e.t).second) {
        if (in >= limit)
          FATALF("%s x%.1f arm %d: admitted with %d events in the window (key %d, tick %d)", str(ru), mult, a, in, e.k,
                 e.t);
        own[a + 1][ki].push_back(e.t);
        row.admitted[a + 1]++;
      } else if (in < limit) {
        row.falseDeny[a]++;
      }
    }
    buzzOff = false;
  };
  if (churnLoad > 0) {
    std::size_t i = 0;
    workload::Churn(churnLoad, churnSeed, capN, hT, mix64, [&](u64 h, u64 w, u64 now) {
      for (; i < evs.size() && evs[i].t <= now; i++) offer(evs[i]);
      fb.CheckAndTouchAt(h, w, now);
      fz.CheckAndTouchAt(h, w, now);
    });
    for (; i < evs.size(); i++) offer(evs[i]);
  } else {
    for (const Ev& e : evs) offer(e);
  }
  if (core(fb).buzz.load()) FATALF("the base arm's filter holds a buzz table");
  if (BuzzTable* tb = core(fz).buzz.load()) {
    row.given = tb->given.load();
    row.dropped = tb->dropped.load();
    row.decided = tb->decided.load();
  }
  row.evictions = fz.Evictions();
  row.relocations = fz.Relocations();
  row.bytesBase = fb.Bytes();
  row.bytesBuzz = fz.Bytes();
  return row;
}

// skipBuzzCT: ConstantTime filters keep TouchIfUnder as without buzz entries.
void skipBuzzCT() {
  if (forceCT) SKIP("ConstantTime filters keep TouchIfUnder as without buzz entries");
}

// steps runs G workers in lock step over ticks 1..ticks (Go: a channel per
// goroutine and a WaitGroup per tick): every worker gets each tick and all
// finish it before the next.
void steps(int G, u64 ticks, const std::function<void(int, u64)>& work) {
  SpinBarrier sync(G + 1);
  std::atomic<u64> now{0};
  std::vector<std::thread> ts;
  for (int g = 0; g < G; g++)
    ts.emplace_back([&, g] {
      for (;;) {
        sync.arrive_and_wait();  // a tick is set (or 0: stop)
        u64 n = now.load();
        if (n == 0) return;
        work(g, n);
        sync.arrive_and_wait();  // the tick is done
      }
    });
  for (u64 n = 1; n <= ticks; n++) {
    now.store(n);
    sync.arrive_and_wait();
    sync.arrive_and_wait();
  }
  now.store(0);
  sync.arrive_and_wait();
  for (auto& t : ts) t.join();
}

}  // namespace

// TestBuzzLimiter: 100 contested keys in a filter of Capacity 200,000, for 20
// horizons (4 for the 1 s rule). Offered 2x a rule a log decides, the buzz
// admits at least 0.97 of what an exact sliding log admits; offered 0.8x, its
// false denials are at most a quarter of the register's; no arm admits past a
// limit. A limit past the log (20) is decided as without entries.
TEST(TestBuzzLimiter) {
  skipBuzzCT();
  for (BuzzRule ru : {BuzzRule{1'000, 5}, BuzzRule{10'000, 5}, BuzzRule{60'000, 6}, BuzzRule{60'000, 16}}) {
    int horizons = ru.w == 1'000 ? 4 : 20;
    BuzzRow hi = buzzOffer(200'000, 100, ru, 2, horizons, 0, 0, 0);
    BuzzRow lo = buzzOffer(200'000, 100, ru, 0.8, horizons, 0, 0, 0);
    LOGF("%-7v x2.0 | %v", str(ru), hi.String());
    LOGF("%-7v x0.8 | %v", str(ru), lo.String());
    if (hi.share(2) < 0.97) ERRORF("%s offered 2x: the buzz admits %.3f of the exact log (0.97 wanted)", str(ru), hi.share(2));
    if (lo.denied(1) > lo.denied(0) / 4)
      ERRORF("%s offered 0.8x: false denials %.4f, the register's %.4f (a quarter wanted)", str(ru), lo.denied(1),
             lo.denied(0));
  }
  BuzzRow past = buzzOffer(200'000, 100, BuzzRule{2'500, 20}, 2, 2, 0, 0, 0);
  LOGF("2.5s/20 x2.0 | %v", past.String());
  if (past.admitted[1] != past.admitted[2] || past.given != 0)
    ERRORF("a limit of 20 was decided by a log: admitted %d, without entries %d, entries given %d", past.admitted[2],
           past.admitted[1], past.given);
}

// TestBuzzMemory: nothing is allocated before an uncertain refusal.
TEST(TestBuzzMemory) {
  Filter f(cfgOf(200'000, 1min, 1ms));
  Core& c = core(f);
  i64 b0 = f.Bytes();
  for (u64 k = 0; k < 10'000; k++)
    for (int j = 0; j < 4; j++) f.TouchIfUnderAt(mix64(k + (u64{1} << 51)), 60'000, 3, 1000);
  if (c.buzz.load() || f.Bytes() != b0)
    FATALF("memory before any uncertain refusal: table %v, Bytes %d -> %d", c.buzz.load() != nullptr, b0, f.Bytes());
  // An uncertain refusal: 5 events over 1.5 s, asked at 1 s / 5.
  u64 h = mix64(7);
  for (u64 i = 0; i < 5; i++) f.TouchIfUnderAt(h, 1'000, 5, 2000 + i * 200);
  if (forceCT) {  // ConstantTime keeps TouchIfUnder as without entries: no table, ever
    if (bool ok = f.TouchIfUnderAt(h, 1'000, 5, 3300).second; ok || c.buzz.load())
      FATALF("ConstantTime: admitted %v, table %v after an uncertain refusal", ok, c.buzz.load() != nullptr);
    return;
  }
  if (bool ok = f.TouchIfUnderAt(h, 1'000, 5, 3300).second; ok || !c.buzz.load())
    FATALF("no entry after an uncertain refusal");
  double per = static_cast<double>(f.Bytes() - b0) / 200'000;
  LOGF("Bytes %d -> %d once allocated: %.3f B per key of Capacity", b0, f.Bytes(), per);
  if (per > 0.6) ERRORF("%.3f B per key once allocated (0.6 at most)", per);
}

// TestBuzzPlainTouch: a plain touch of a key that holds an entry changes its
// words, so the entry is dropped and the next request is the register's.
TEST(TestBuzzPlainTouch) {
  skipBuzzCT();
  Filter f(cfgOf(1000, 1min, 1ms));
  u64 h = mix64(9);
  for (u64 i = 0; i < 5; i++) f.TouchIfUnderAt(h, 1'000, 5, 2000 + i * 200);
  f.TouchIfUnderAt(h, 1'000, 5, 3300);  // refused in doubt: an entry, its log from 3300
  BuzzTable* tb = core(f).buzz.load();
  if (!tb || tb->given.load() != 1) FATALF("no entry given");
  if (bool ok = f.TouchIfUnderAt(h, 1'000, 5, 4400).second; !ok || tb->decided.load() == 0)
    FATALF("the log decided nothing (admitted %v)", ok);
  f.TouchAt(h, 0, 4500);
  u64 d = tb->dropped.load();
  f.TouchIfUnderAt(h, 1'000, 5, 4600);
  if (tb->dropped.load() != d + 1)
    ERRORF("a plain touch did not drop the entry (dropped %d -> %d)", d, tb->dropped.load());
}

// TestBuzzConcurrent: G workers on a shared clock, as TestTouchIfSustained,
// with limits a log decides: the admitted events per key stay within the
// limit at every window position, and logs decide some of them.
TEST(TestBuzzConcurrent) {
  skipBuzzCT();
  const int G = 8;
  const u64 hTicks = 60'000;
  for (BuzzRule c : {BuzzRule{1'000, 5}, BuzzRule{5'000, 10}, BuzzRule{10'000, 5}}) {
    for (int nk : {1, 200}) {
      Filter f(cfgOf(200'000, 1min, 1ms));
      std::vector<std::vector<u64>> logs(G);  // per worker: key<<32 | tick
      std::vector<gorand::Rand> rs;
      for (int g = 0; g < G; g++) rs.emplace_back(static_cast<u64>(g), 3);
      u64 ticks = 2 * hTicks;
      if (check::short_()) ticks = std::max(hTicks / 4, 3 * c.w);  // a log decides once the window starts after it does
      steps(G, ticks, [&](int g, u64 now) {
        gorand::Rand& r = rs[static_cast<std::size_t>(g)];
        for (i64 n = 1 + r.IntN(2); n > 0; n--) {  // each key is asked about twice its limit per window
          i64 k = r.IntN(nk);
          if (r.Float64() < 2 * static_cast<double>(c.limit) * nk / (static_cast<double>(c.w) * 1.5 * G)) {
            if (f.TouchIfUnderAt(mix64(static_cast<u64>(k) + (u64{1} << 43)), c.w, c.limit, now).second)
              logs[static_cast<std::size_t>(g)].push_back((static_cast<u64>(k) << 32) | now);
          }
        }
      });
      std::vector<std::vector<u64>> per(static_cast<std::size_t>(nk));
      i64 total = 0;
      for (auto& l : logs)
        for (u64 x : l) {
          per[x >> 32].push_back(x & ((u64{1} << 32) - 1));
          total++;
        }
      i64 over = 0, positions = 0;
      for (auto& ts : per) {
        std::sort(ts.begin(), ts.end());
        std::size_t lo = 0;
        for (std::size_t hi = 0; hi < ts.size(); hi++) {
          while (ts[lo] + c.w < ts[hi]) lo++;
          std::size_t j = hi;
          while (j + 1 < ts.size() && ts[j + 1] == ts[hi]) j++;
          positions++;
          if (static_cast<i64>(j - lo + 1) > static_cast<i64>(c.limit)) over++;
        }
      }
      u64 decided = 0;
      if (BuzzTable* tb = core(f).buzz.load()) decided = tb->decided.load();
      LOGF("keys %3d %v: admitted %d (%.2f of limit x windows x keys), window positions %d, over the limit %d, "
           "decided by a log %d",
           nk, str(c), total,
           static_cast<double>(total) /
               (static_cast<double>(c.limit) * static_cast<double>(ticks) / static_cast<double>(c.w) * nk),
           positions, over, decided);
      if (over != 0) ERRORF("keys %d %s: over the limit at %d window positions", nk, str(c), over);
      if (decided == 0) ERRORF("keys %d %s: no decision was made by a log", nk, str(c));
    }
  }
}

// TestBuzzSnapshot: a strict limiter's decisions across a snapshot (version 3
// carries the buzz table): every decision and answer must be the same.
TEST(TestBuzzSnapshot) {
  if (forceCT || forceBG) SKIP("snapshots refuse ConstantTime and Background filters");
  Config cfg = cfgOf(20'000, 1min, 1ms);
  gorand::Rand r(5, 5);
  struct Ev {
    u64 t;
    int k;
  };
  std::vector<Ev> evs;
  for (int k = 0; k < 400; k++)
    for (double x = r.ExpFloat64() / 0.01; x < 240'000; x += r.ExpFloat64() / 0.01)
      evs.push_back(Ev{static_cast<u64>(x) + 1, k});
  std::stable_sort(evs.begin(), evs.end(), [](const Ev& a, const Ev& b) { return a.t < b.t; });
  auto run = [&](u64 snapAt, u64& decided) {
    std::vector<std::string> out;
    auto f = std::make_unique<Filter>(cfg);
    bool snapped = false;
    for (const Ev& e : evs) {
      if (!snapped && snapAt > 0 && e.t >= snapAt) {
        snapped = true;
        std::stringstream buf;
        f->WriteTo(buf);
        auto g = std::make_unique<Filter>(cfg);
        g->ReadFrom(buf);
        f = std::move(g);
      }
      auto [res, ok] = f->TouchIfUnderAt(mix64(static_cast<u64>(e.k) + (u64{1} << 49)), 1'000, 5, e.t);
      out.push_back((ok ? "true " : "false ") + workload::goV(res));
    }
    decided = 0;
    if (BuzzTable* tb = core(*f).buzz.load()) decided = tb->decided.load();
    return out;
  };
  u64 du, ds;
  auto u = run(0, du);
  auto s = run(120'000, ds);
  i64 diff = 0;
  for (std::size_t i = 0; i < u.size(); i++)
    if (u[i] != s[i]) diff++;
  LOGF("%d decisions, %d differ; decided by a log: %d uninterrupted, %d after the restore", u.size(), diff, du, ds);
  if (diff != 0 || du == 0) ERRORF("a restore changed %d decisions, or no log decided (%d)", diff, du);
}

// TestBuzzReading (TF_EXP=1) reads the buzz as the lab arm B12b read it
// (claims A46).
TEST(TestBuzzReading) {
  skipBuzzCT();
  if (const char* e = std::getenv("TF_EXP"); !e || !*e) SKIP("set TF_EXP=1");
  std::vector<BuzzRule> rules{{1'000, 5}, {5'000, 10}, {10'000, 3}, {10'000, 5}, {30'000, 10}, {60'000, 6}, {60'000, 16}};
  const double mults[] = {0.8, 1.2, 2};
  auto scen = [&](const char* name, i64 capN, double plain, const std::vector<BuzzRule>& rs) {
    for (const BuzzRule& ru : rs) {
      for (double mult : mults) {
        BuzzRow row = buzzOffer(capN, 400, ru, mult, 20, plain, 0, 0);
        LOGF("%s %-7v x%.1f | %v", name, str(ru), mult, row.String());
        if (std::string(name) == "A" && mult == 2 && ru == rs[0])
          LOGF("%s memory: Bytes without entries %d, with %d: %.3f B per key of Capacity", name, row.bytesBase,
               row.bytesBuzz, static_cast<double>(row.bytesBuzz - row.bytesBase) / static_cast<double>(capN));
      }
    }
  };
  scen("A", 200'000, 0, rules);
  scen("B", 12'800, 0, rules);
  scen("C", 200'000, 0.1, rules);
  scen("E", 200'000, 0, {{2'500, 20}, {5'000, 40}});
  struct L {
    double load;
    u64 seed;
  };
  for (L c : {L{1.0, 2}, L{1.15, 4}}) {
    for (BuzzRule ru : {BuzzRule{1'000, 5}, BuzzRule{10'000, 5}}) {
      for (double mult : {0.8, 2.0}) {
        BuzzRow row = buzzOffer(200'000, 1000, ru, mult, 4, 0, c.load, c.seed);
        LOGF("D load %.2f %-7v x%.1f | %v | evictions %d relocations %d", c.load, str(ru), mult, row.String(),
             row.evictions, row.relocations);
      }
    }
  }
}
