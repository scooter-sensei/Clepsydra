// go/touchif_test.go: TouchIfUnder (claims A45).
#include <algorithm>
#include <atomic>
#include <barrier>
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

// TestTouchIfBurst (claims A45): G workers at one tick, each calling until
// refused, on fresh keys: admitted per key must equal the limit.
TEST(TestTouchIfBurst) {
  const int G = 8;
  for (int nk : {1, 1000}) {
    for (u64 limit : {u64{10}, u64{100}, u64{1000}}) {
      Filter f(cfgOf(200'000, 1min, 1ms));
      std::vector<std::atomic<i64>> adm(static_cast<std::size_t>(nk));
      std::vector<std::thread> ts;
      for (int g = 0; g < G; g++)
        ts.emplace_back([&, g] {
          gorand::Rand r(static_cast<u64>(g), 1);
          // until every key has refused this worker once
          std::vector<bool> refused(static_cast<std::size_t>(nk));
          int left = nk;
          while (left > 0) {
            auto k = static_cast<std::size_t>(r.IntN(nk));
            if (f.TouchIfUnderAt(mix64(static_cast<u64>(k) + (u64{1} << 45)), 60'000, limit, 1000).second) {
              adm[k].fetch_add(1);
            } else if (!refused[k]) {
              refused[k] = true;
              left--;
            }
          }
        });
      for (auto& t : ts) t.join();
      i64 lo = i64{1} << 62, hi = 0;
      for (auto& a : adm) {
        lo = std::min(lo, a.load());
        hi = std::max(hi, a.load());
      }
      LOGF("burst keys %4d limit %4d: admitted per key min %d max %d; evictions %d, promoted %d", nk, limit, lo, hi,
           f.Evictions(), f.Promoted());
      if (lo != static_cast<i64>(limit) || hi != static_cast<i64>(limit))
        ERRORF("burst keys %d limit %d: admitted %d to %d per key", nk, limit, lo, hi);
    }
  }
}

// TestTouchIfSustained (claims A45): G workers over three horizons on a shared
// clock (1 ms steps, 4 calls each per step); admitted per key at every window
// position must stay within the limit.
TEST(TestTouchIfSustained) {
  const int G = 8;
  const u64 hTicks = 60'000;
  struct C {
    u64 w, limit;
  };
  for (C c : {C{1'000, 100}, C{5'000, 500}}) {
    for (int nk : {1, 1000}) {
      Filter f(cfgOf(200'000, 1min, 1ms));
      std::vector<std::vector<u64>> logs(G);
      std::atomic<i64> sat{0};
      u64 ticks = 3 * hTicks;
      if (nk > 1 || check::short_()) ticks = hTicks / 2;
      std::vector<gorand::Rand> rs;
      for (int g = 0; g < G; g++) rs.emplace_back(static_cast<u64>(g), 2);
      SpinBarrier sync(G + 1);
      std::atomic<u64> now{0};
      std::vector<std::thread> ts;
      for (int g = 0; g < G; g++)
        ts.emplace_back([&, g] {
          gorand::Rand& r = rs[static_cast<std::size_t>(g)];
          for (;;) {
            sync.arrive_and_wait();
            u64 n = now.load();
            if (n == 0) return;
            for (int j = 0; j < 4; j++) {
              i64 k = r.IntN(nk);
              auto [res, ok] = f.TouchIfUnderAt(mix64(static_cast<u64>(k) + (u64{1} << 44)), c.w, c.limit, n);
              if (ok) logs[static_cast<std::size_t>(g)].push_back((static_cast<u64>(k) << 32) | n);
              if (res.Saturated) sat.fetch_add(1);
            }
            sync.arrive_and_wait();
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
      std::vector<std::vector<u64>> per(static_cast<std::size_t>(nk));
      for (auto& l : logs)
        for (u64 x : l) per[x >> 32].push_back(x & ((u64{1} << 32) - 1));
      i64 over = 0, positions = 0, sum = 0, minW = i64{1} << 30;
      for (auto& tsk : per) {
        std::sort(tsk.begin(), tsk.end());
        std::size_t lo = 0;
        for (std::size_t hi = 0; hi < tsk.size(); hi++) {  // window ending at each admission's tick
          while (tsk[lo] + c.w < tsk[hi]) lo++;
          std::size_t j = hi;
          while (j + 1 < tsk.size() && tsk[j + 1] == tsk[hi]) j++;
          i64 n = static_cast<i64>(j - lo + 1);
          positions++;
          sum += n;
          minW = std::min(minW, n);
          if (n > static_cast<i64>(c.limit)) over++;
        }
      }
      LOGF("sustained keys %4d W=%ds limit %d: positions %d, over the limit %d, mean admitted per window %.1f (min "
           "%d); evictions %d, saturated answers %d",
           nk, c.w / 1000, c.limit, positions, over, static_cast<double>(sum) / static_cast<double>(std::max<i64>(positions, 1)),
           minW, f.Evictions(), sat.load());
      if (over != 0)
        ERRORF("sustained keys %d W=%ds limit %d: over the limit at %d window positions", nk, c.w / 1000, c.limit, over);
    }
  }
}

// TestTouchIfVelocity (claims A45): one thread, tfbench's velocity workload's
// events as requests, against the filter's own admitted log and an exact
// sliding-log limiter fed the same requests.
TEST(TestTouchIfVelocity) {
  struct C {
    u64 w, limit;
  };
  for (C c : {C{1'000, 5}, C{5'000, 20}}) {
    Filter f(cfgOf(200'000, 1min, 1ms));
    std::vector<std::vector<u64>> own(2000), ex(2000);
    auto inWin = [&](const std::vector<u64>& ts, u64 now) {
      u64 n = 0;
      for (i64 i = static_cast<i64>(ts.size()) - 1; i >= 0 && ts[static_cast<std::size_t>(i)] + c.w >= now; i--) n++;
      return n;
    };
    i64 reqs = 0, agree = 0, admitOver = 0, refusedUnder = 0, refusedUnderUpper = 0, bothAdmit = 0, exAdmit = 0;
    workload::velWorkload(
        [&](int k, u64 tk) {
          reqs++;
          auto ki = static_cast<std::size_t>(k);
          u64 h = mix64(static_cast<u64>(k) + (u64{1} << 50));
          u64 truth = inWin(own[ki], tk);
          auto [r, ok] = f.TouchIfUnderAt(h, c.w, c.limit, tk);
          if (ok) {
            if (truth >= c.limit) admitOver++;
            own[ki].push_back(tk);
          } else if (truth < c.limit) {
            refusedUnder++;
            if (static_cast<u64>(r.Upper) > truth || r.Saturated) refusedUnderUpper++;
          }
          bool eok = inWin(ex[ki], tk) < c.limit;
          if (eok) {
            ex[ki].push_back(tk);
            exAdmit++;
          }
          if (eok == ok) {
            agree++;
            if (ok) bothAdmit++;
          }
        },
        [](int, const std::vector<u64>&, int, u64) {});
    LOGF("velocity W=%ds limit %d: requests %d, agreement with exact %.4f (exact admits %d, both %d); admitted at/over "
         "own limit %d; refused under own limit %d, of which Upper > truth %d",
         c.w / 1000, c.limit, reqs, static_cast<double>(agree) / static_cast<double>(reqs), exAdmit, bothAdmit,
         admitOver, refusedUnder, refusedUnderUpper);
    if (admitOver != 0 || refusedUnderUpper != refusedUnder)
      ERRORF("velocity W=%ds limit %d: %d admitted at or over the limit; %d refused under it without Upper > truth",
             c.w / 1000, c.limit, admitOver, refusedUnder - refusedUnderUpper);
  }
}

// TestTouchIfUnderLostHistory: without an overflow table a dropped history is
// lost, and an unseen key reads Degraded while its events may still be in a
// window: TouchIfUnderAt refuses such a key; once the lost events are past a
// horizon it admits again.
TEST(TestTouchIfUnderLostHistory) {
  Config cfg = cfgOf(64, 1s, 1ms);
  cfg.Overflow = -1;
  Filter f(cfg);
  Core& c = core(f);
  u64 h = mix64(1);
  u64 fp = fingerprint(h), tg = tag(h);
  u64 w1 = c.nextReg(0, tg, 1000, 1000, true).first;
  c.spill(c.stashID(h, fp, tg), (fp << 48) | 1000, w1, 1000);
  if (auto [r, ok] = f.TouchIfUnderAt(h, 1000, 5, 1500); ok || !r.Degraded || r.Seen)
    ERRORF("an unseen key read Degraded after a lost history: %+v, admitted %v; want refused", r, ok);
  if (auto [r, ok] = f.TouchIfUnderAt(h, 1000, 5, 2001); !ok) ERRORF("past a horizon after the lost event: %+v, refused", r);
}
