// go/placement_test.go: the table layout's placement limit, against an exact
// placement oracle. The regression tier compares the lines starting with
// "placement" or "agreed on" with go/verify/expected/placement.txt.
#include <algorithm>
#include <array>
#include <cmath>
#include <functional>
#include <stdexcept>
#include <string>
#include <unordered_map>
#include <vector>

#include "check.hpp"
#include "helpers.hpp"
#include "workload.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

namespace {

using Cand = std::array<std::int32_t, 2>;

// Oracle is an exact placement model of the filter's table: each live key
// sits in one of its two candidate lines, and inserts take a shortest
// augmenting path when one exists. Keys expire Horizon after their last
// touch, as in the filter.
struct Oracle {
  struct Expiry {
    std::int32_t id;
    u64 t;
  };
  Core* f;
  u64 horizon;
  std::unordered_map<u64, std::int32_t> ids;  // live key hash -> id
  std::vector<u64> h, last;
  std::vector<Cand> cand;
  std::vector<std::int32_t> line;  // line holding the key
  std::vector<std::int32_t> free;
  std::vector<std::array<std::int32_t, slotsPerLine>> slots;  // key ids, -1 = empty
  std::vector<std::uint8_t> used;
  std::vector<Expiry> queue;  // touches in time order
  std::size_t qhead = 0;

  // BFS state, stamped to avoid clearing.
  std::vector<std::uint32_t> seen;
  std::uint32_t stamp = 0;
  std::vector<std::int32_t> from;  // line the key in via moves out of
  std::vector<std::int32_t> via;   // key that moves into this line
  std::vector<std::int32_t> depth;
  std::vector<std::int32_t> bfs;
  std::int32_t target = 0;  // line place freed a slot in
  i64 inserts = 0, drops = 0, live = 0, peak = 0;
  std::array<i64, 32> moves{};  // shortest augmenting path length (moves) per insert; 31 = 31+

  Oracle(Core& c, u64 hz) : f(&c), horizon(hz) {
    const std::size_t n = static_cast<std::size_t>(c.lines);
    slots.assign(n, {-1, -1, -1, -1});
    used.assign(n, 0);
    seen.assign(n, 0);
    from.assign(n, 0);
    via.assign(n, 0);
    depth.assign(n, 0);
  }

  // touch returns whether hk was seen within w, then records the touch.
  bool touch(u64 hk, u64 w, u64 now) {
    expire(now);
    if (auto it = ids.find(hk); it != ids.end()) {
      std::int32_t id = it->second;
      bool s = now - last[static_cast<std::size_t>(id)] <= w;
      last[static_cast<std::size_t>(id)] = now;
      queue.push_back({id, now});
      return s;
    }
    inserts++;
    u64 p = f->primary(hk);
    Cand c{static_cast<std::int32_t>(p), static_cast<std::int32_t>(f->alt(p, fingerprint(hk)))};
    auto [mv, ok] = place(c);
    if (!ok) {
      drops++;
      return false;
    }
    moves[static_cast<std::size_t>(std::min<i64>(mv, static_cast<i64>(moves.size()) - 1))]++;
    std::int32_t id;
    if (!free.empty()) {
      id = free.back();
      free.pop_back();
      h[static_cast<std::size_t>(id)] = hk;
      last[static_cast<std::size_t>(id)] = now;
      cand[static_cast<std::size_t>(id)] = c;
    } else {
      id = static_cast<std::int32_t>(h.size());
      h.push_back(hk);
      last.push_back(now);
      cand.push_back(c);
      line.push_back(-1);
    }
    ids[hk] = id;
    put(id, target);
    queue.push_back({id, now});
    live++;
    peak = std::max(peak, live);
    return false;
  }

  // expire removes keys whose last touch is more than horizon before now.
  void expire(u64 now) {
    while (qhead < queue.size() && now - queue[qhead].t > horizon) {
      Expiry e = queue[qhead];
      qhead++;
      if (last[static_cast<std::size_t>(e.id)] == e.t && line[static_cast<std::size_t>(e.id)] >= 0) remove(e.id);
    }
    if (qhead > (std::size_t{1} << 20)) {
      queue.erase(queue.begin(), queue.begin() + static_cast<std::ptrdiff_t>(qhead));
      qhead = 0;
    }
  }

  void put(std::int32_t id, std::int32_t l) {
    auto& sl = slots[static_cast<std::size_t>(l)];
    for (std::size_t s = 0; s < sl.size(); s++) {
      if (sl[s] < 0) {
        sl[s] = id;
        used[static_cast<std::size_t>(l)]++;
        line[static_cast<std::size_t>(id)] = l;
        return;
      }
    }
    throw std::logic_error("oracle: put into a full line");
  }

  void take(std::int32_t id) {
    std::int32_t l = line[static_cast<std::size_t>(id)];
    auto& sl = slots[static_cast<std::size_t>(l)];
    for (std::size_t s = 0; s < sl.size(); s++) {
      if (sl[s] == id) {
        sl[s] = -1;
        used[static_cast<std::size_t>(l)]--;
        line[static_cast<std::size_t>(id)] = -1;
        return;
      }
    }
    throw std::logic_error("oracle: key not in its line");
  }

  void remove(std::int32_t id) {
    take(id);
    ids.erase(h[static_cast<std::size_t>(id)]);
    free.push_back(id);
    live--;
  }

  // place frees a slot in one of the candidate lines c, moving keys along a
  // shortest augmenting path, and sets target to that line. It returns the
  // number of moves, or false (with a verified certificate) if no placement
  // exists.
  std::pair<i64, bool> place(const Cand& c) {
    for (std::int32_t l : c) {
      if (used[static_cast<std::size_t>(l)] < slotsPerLine) {
        target = l;
        return {0, true};
      }
    }
    stamp++;
    bfs.clear();
    for (std::int32_t l : c) {
      std::size_t ul = static_cast<std::size_t>(l);
      if (seen[ul] != stamp) {
        seen[ul] = stamp;
        from[ul] = -1;
        depth[ul] = 0;
        bfs.push_back(l);
      }
    }
    for (std::size_t i = 0; i < bfs.size(); i++) {
      std::int32_t m = bfs[i];
      for (std::int32_t k : slots[static_cast<std::size_t>(m)]) {
        const Cand& ck = cand[static_cast<std::size_t>(k)];
        std::int32_t other = ck[0];
        if (other == m) other = ck[1];
        std::size_t uo = static_cast<std::size_t>(other);
        if (other == m || seen[uo] == stamp) continue;
        seen[uo] = stamp;
        from[uo] = m;
        via[uo] = k;
        depth[uo] = depth[static_cast<std::size_t>(m)] + 1;
        if (used[uo] < slotsPerLine) {  // augment back along the path
          i64 mv = depth[uo];
          std::int32_t cur = other;
          while (from[static_cast<std::size_t>(cur)] >= 0) {
            std::int32_t kk = via[static_cast<std::size_t>(cur)], prev = from[static_cast<std::size_t>(cur)];
            take(kk);
            put(kk, cur);
            cur = prev;
          }
          target = cur;
          return {mv, true};
        }
        bfs.push_back(other);
      }
    }
    // Certificate: every line reached is full, and every key in them has both
    // candidates among them, so they hold 4|S| keys that need these 4|S|
    // slots, plus the new key.
    for (std::int32_t m : bfs) {
      if (used[static_cast<std::size_t>(m)] != slotsPerLine) FATALF("oracle: line %d reached but not full", m);
      for (std::int32_t k : slots[static_cast<std::size_t>(m)]) {
        for (std::int32_t l : cand[static_cast<std::size_t>(k)]) {
          if (seen[static_cast<std::size_t>(l)] != stamp)
            FATALF("oracle: key %d in line %d has candidate %d outside the closed set", k, m, l);
        }
      }
    }
    return {0, false};
  }
};

// kuhnPlacesAll reports whether every key can sit in one of its two lines
// with at most slotsPerLine keys per line.
bool kuhnPlacesAll(const std::vector<Cand>& keys, std::size_t lines) {
  std::vector<std::vector<std::size_t>> owner(lines);
  std::function<bool(std::size_t, std::vector<bool>&)> tryPlace = [&](std::size_t k, std::vector<bool>& seen) {
    for (std::int32_t li : keys[k]) {
      std::size_t l = static_cast<std::size_t>(li);
      if (seen[l]) continue;
      seen[l] = true;
      if (owner[l].size() < static_cast<std::size_t>(slotsPerLine)) {
        owner[l].push_back(k);
        return true;
      }
      for (std::size_t i = 0; i < owner[l].size(); i++) {
        if (tryPlace(owner[l][i], seen)) {
          owner[l][i] = k;
          return true;
        }
      }
    }
    return false;
  };
  for (std::size_t k = 0; k < keys.size(); k++) {
    std::vector<bool> seen(lines);
    if (!tryPlace(k, seen)) return false;
  }
  return true;
}

u64 mixHash(u64 x) { return mix64(x); }

}  // namespace

// TestPlacementLimit asks whether the scorecard's missed keys are forced by
// the table layout or caused by the filter's bounded cuckoo search. It
// replays the scorecard's churn stream (cmd/tfbench, same seeds) through an
// exact placement oracle with the filter's geometry: the same lines, 4 slots
// each, and the same two candidate lines per key. The oracle searches without
// bound (BFS for a shortest augmenting path), so it places a new key whenever
// any arrangement of the live keys plus that key exists, and drops the key
// only when none does; every drop is checked with a Hall-violation
// certificate. The real filter runs on the same stream. Output lines starting
// with "placement" are deterministic.
TEST(TestPlacementLimit) {
  if (check::short_()) SKIP("replays three 4-horizon churn streams");
  const i64 capN = 200'000;
  const u64 hTicks = 60'000;
  struct Case {
    double load;
    u64 seed;
  };
  for (const Case& c : {Case{1.0, 2}, Case{1.1, 3}, Case{1.15, 4}}) {
    auto fp = newTest(capN, 1min);
    Filter& f = *fp;
    Oracle o(core(f), hTicks);
    std::unordered_map<u64, u64> last;
    i64 pos = 0, filterFN = 0, oracleFN = 0;
    u64 pruned = 0;
    workload::Churn(c.load, c.seed, capN, hTicks, mixHash, [&](u64 h, u64 w, u64 now) {
      if (now % hTicks == 0 && now != pruned) {
        pruned = now;
        for (auto it = last.begin(); it != last.end();) {
          if (now - it->second > 2 * hTicks) {
            it = last.erase(it);
          } else {
            ++it;
          }
        }
      }
      bool got = f.CheckAndTouchAt(h, w, now);
      bool ogot = o.touch(h, w, now);
      auto it = last.find(h);
      bool want = it != last.end() && now - it->second <= w;
      last[h] = now;
      if (now < hTicks || !want) return;
      pos++;
      if (!got) filterFN++;
      if (!ogot) oracleFN++;
    });
    std::string hist;
    for (std::size_t d = 0; d < o.moves.size(); d++)
      if (o.moves[d] > 0) hist += check::format(" %d:%d", d, o.moves[d]);
    LOGF("placement load=%.2f seed=%d inserts=%d oracle_drops=%d oracle_peak_slot_load=%.4f oracle_missed=%d "
         "filter_evictions=%d filter_missed=%d positives=%d",
         c.load, c.seed, o.inserts, o.drops, static_cast<double>(o.peak) / static_cast<double>(4 * core(f).lines),
         oracleFN, f.Evictions(), filterFN, pos);
    LOGF("placement load=%.2f shortest_path_moves(moves:inserts)%s", c.load, hist);
  }
}

// TestOracleMatchesMaxFlow checks the oracle against an independent exact
// method on small overloaded tables: before every insert, a from-scratch
// maximum bipartite b-matching (Kuhn's augmenting paths) of the live keys plus
// the new key must place all of them exactly when the oracle can.
TEST(TestOracleMatchesMaxFlow) {
  const i64 capN = 170;  // 50 lines
  const u64 hTicks = 6'000;
  i64 feasible = 0, infeasible = 0;
  for (u64 seed = 1; seed <= 20; seed++) {
    auto fp = newTest(capN, 1min);
    Core& c = core(*fp);
    Oracle o(c, hTicks);
    workload::Churn(1.4, seed, capN, hTicks, mixHash, [&](u64 h, u64 w, u64 now) {
      o.expire(now);  // as touch does first; then ask max-flow
      const bool live = o.ids.count(h) != 0;
      bool want = false;
      if (!live) {
        u64 p = c.primary(h);
        std::vector<Cand> keys{{static_cast<std::int32_t>(p), static_cast<std::int32_t>(c.alt(p, fingerprint(h)))}};
        for (const auto& [_, id] : o.ids) keys.push_back(o.cand[static_cast<std::size_t>(id)]);
        want = kuhnPlacesAll(keys, static_cast<std::size_t>(c.lines));
      }
      i64 drops = o.drops;
      o.touch(h, w, now);
      if (!live) {
        bool got = o.drops == drops;
        if (got != want) FATALF("seed %d now %d: oracle placed=%v, max-flow placed=%v", seed, now, got, want);
        if (want) {
          feasible++;
        } else {
          infeasible++;
        }
      }
    });
  }
  LOGF("agreed on %d feasible and %d infeasible inserts", feasible, infeasible);
  if (infeasible == 0 || feasible == 0) FATALF("workload did not exercise both outcomes");
}

// TestLoadThreshold computes the load threshold of 2-choice cuckoo hashing
// with buckets of l slots: c*/l, where x solves 2l = x·Q(x,l)/Q(x,l+1),
// c* = x/(2·Q(x,l)) and Q(x,y) = P(Poisson(x) >= y) (Cain, Sanders and
// Wormald 2007; Fernholz and Ramachandran 2007). It checks the values
// tabulated in Walzer, "What if we tried Less Power?" (arXiv 2307.00644,
// §1.1.1) and the classic 1/2 for one slot.
TEST(TestLoadThreshold) {
  auto poissonTail = [](double x, int y) {  // P(Poisson(x) >= y)
    double term = std::exp(-x), cdf = 0;
    for (int j = 0; j < y; j++) {
      cdf += term;
      term *= x / static_cast<double>(j + 1);
    }
    return 1 - cdf;
  };
  auto threshold = [&](int l) {
    double lo = 1e-9, hi = 100.0;
    for (int i = 0; i < 200; i++) {
      double x = (lo + hi) / 2;
      if (x * poissonTail(x, l) / poissonTail(x, l + 1) < static_cast<double>(2 * l)) {
        lo = x;
      } else {
        hi = x;
      }
    }
    double x = (lo + hi) / 2;
    return x / (2 * poissonTail(x, l)) / static_cast<double>(l);
  };
  struct Case {
    int l;
    double want;
  };
  for (const Case& c : {Case{1, 0.5}, Case{2, 0.89701}, Case{3, 0.95915}, Case{4, 0.98037}}) {
    double got = threshold(c.l);
    LOGF("placement threshold choices=2 slots=%d load=%.5f", c.l, got);
    if (std::abs(got - c.want) > 5e-6) ERRORF("slots=%d: threshold %.6f, want %.5f", c.l, got, c.want);
  }
}

// TestComparedEntries measures how many stored entries an absent-key lookup
// compares its 28-bit identity with, at the scorecard's static-FPR setup
// (200k keys at the default 85% load). With the displaced bit it compares
// only entries homed at its primary line; without it, every entry in both
// candidate lines. Each comparison is a false positive with probability
// 2^-28, so the expected FPR is the mean count / 2^28.
TEST(TestComparedEntries) {
  const u64 capN = 200'000, probes = 1'000'000;
  auto fp = newTest(static_cast<i64>(capN), 1min);
  Filter& f = *fp;
  Core& c = core(f);
  for (u64 i = 0; i < capN; i++) f.CheckAndTouchAt(mix64(i), 0, 1);
  settle(f);
  i64 with = 0, without = 0;
  for (u64 i = 0; i < probes; i++) {
    u64 h = mix64(capN + i);
    u64 fpr = fingerprint(h);
    u64 p = c.primary(h);
    u64 a = c.alt(p, fpr);
    const u64 ls[2] = {p, a};
    for (int li = 0; li < 2; li++) {
      if (li == 1 && a == p) break;
      for (int s = 0; s < wordsPerLine; s += 2) {
        u64 w0 = c.words[static_cast<std::size_t>(ls[li] * wordsPerLine + static_cast<u64>(s))].load();
        if (w0 == 0) continue;
        without++;
        if (((w0 & dispBit) != 0) == (li == 1)) with++;
      }
    }
  }
  const double mw = static_cast<double>(with) / static_cast<double>(probes),
               mo = static_cast<double>(without) / static_cast<double>(probes);
  LOGF("placement compared_entries probes=%d with_displaced_bit=%.4f without=%.4f expected_fpr_with=%.3e without=%.3e",
       probes, mw, mo, mw / static_cast<double>(1 << 28), mo / static_cast<double>(1 << 28));
  i64 stored = 0, displaced = 0;
  for (u64 i = 0; i < c.lines * wordsPerLine; i += 2) {
    if (u64 w0 = c.words[static_cast<std::size_t>(i)].load(); w0 != 0) {
      stored++;
      if ((w0 & dispBit) != 0) displaced++;
    }
  }
  LOGF("placement displaced_entries stored=%d in_alternate_line=%d (%.1f%%)", stored, displaced,
       100 * static_cast<double>(displaced) / static_cast<double>(stored));
}
