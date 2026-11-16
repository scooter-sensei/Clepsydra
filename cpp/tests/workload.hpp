// Workloads the Go suite shares between tests: velWorkload (go/velinfo_test.go,
// tfbench's velocity workload) and FNV-1a over answers printed as Go's %v
// prints them, so a hash the Go suite pins reads the same here.
#pragma once

#include <cstdint>
#include <functional>
#include <string>
#include <vector>

#include "clepsydra/clepsydra.hpp"
#include "gorand.hpp"
#include "gosort.hpp"

namespace workload {

// velWins are the velocity workload's query windows, in 1 ms ticks.
inline const std::vector<std::uint64_t> velWins{1'000, 5'000, 15'000, 30'000, 60'000};

// velWorkload replays tfbench's velocity workload (cmd/tfbench/accuracy.go,
// velocityMAE): same seed, same order of draws, Go's sort of the events.
// visit is called once per query with the key's event times so far (sorted),
// the query's window index and now; touch once per event, before the queries
// that see it.
inline void velWorkload(const std::function<void(int, std::uint64_t)>& touch,
                        const std::function<void(int, const std::vector<std::uint64_t>&, int, std::uint64_t)>& visit) {
  const std::uint64_t hTicks = 60'000;
  gorand::Rand r(9, 9);
  const int nkeys = 2000;
  const double end = 5.0 * hTicks;
  struct Ev {
    std::uint64_t t;
    int k;
  };
  std::vector<Ev> evs;
  auto poisson = [&](double t0, double t1, double rate, int k) {
    for (double t = t0 + r.ExpFloat64() / rate; t < t1; t += r.ExpFloat64() / rate)
      evs.push_back(Ev{static_cast<std::uint64_t>(t) + 1, k});
  };
  for (int k = 0; k < nkeys; k++) {
    switch (k % 5) {
      case 0:
        poisson(0, end, (0.2 + r.Float64() * 4.8) / 1000, k);
        break;
      case 1:
        for (double t = r.ExpFloat64() * 10'000; t < end; t += r.ExpFloat64() * 10'000) {
          for (std::int64_t n = 5 + r.IntN(16); n > 0; n--)
            evs.push_back(Ev{static_cast<std::uint64_t>(t + r.Float64() * 200) + 1, k});
        }
        break;
      case 2:
        for (double t = 0.0; t < end;) {
          t += r.ExpFloat64() / ((0.1 + 9.9 * t / end) / 1000);
          evs.push_back(Ev{static_cast<std::uint64_t>(t) + 1, k});
        }
        break;
      case 3:
        poisson(0, end, 1.0 / 20'000, k);
        break;
      case 4:
        for (double t = 0.0; t < end; t += 10'000) poisson(t, t + 5000, 4.0 / 1000, k);
        break;
    }
  }
  gosort::Slice(evs, [](const Ev& a, const Ev& b) { return a.t < b.t; });
  std::vector<std::vector<std::uint64_t>> times(nkeys);
  std::size_t i = 0;
  for (std::uint64_t now = hTicks; now < static_cast<std::uint64_t>(end); now++) {
    while (i < evs.size() && evs[i].t <= now) {
      touch(evs[i].k, evs[i].t);
      times[static_cast<std::size_t>(evs[i].k)].push_back(evs[i].t);
      i++;
    }
    int k = static_cast<int>(r.IntN(nkeys));
    int wi = static_cast<int>(r.IntN(5));
    visit(k, times[static_cast<std::size_t>(k)], wi, now);
  }
}

// Windows are the query windows (ticks) the churn stream draws from
// (go/internal/workload).
inline const std::vector<std::uint64_t> Windows{1'000, 10'000, 60'000};

// Churn is go/internal/workload's Churn: a stream mixing new keys (60%) and
// repeats of keys born within the last 1.5 horizons, over 4 horizons of
// hTicks each, one window drawn from Windows per operation; load sets the
// arrival rate so that distinct keys live per horizon ~ load x capacity. The
// stream is a pure function of its arguments, as Go's.
inline void Churn(double load, std::uint64_t seed, std::int64_t capacity, std::uint64_t hTicks,
                  const std::function<std::uint64_t(std::uint64_t)>& hash,
                  const std::function<void(std::uint64_t, std::uint64_t, std::uint64_t)>& op) {
  gorand::Rand r(seed, 1);
  const double newFrac = 0.6;
  double ratePerMs = load * static_cast<double>(capacity) / (newFrac * static_cast<double>(hTicks)) / 1.375;
  struct Born {
    std::uint64_t h, t;
  };
  std::vector<Born> keys;
  std::size_t head = 0;  // keys[head:] is Go's keys slice
  std::uint64_t next = 0;
  for (std::uint64_t now = 1; now < 4 * hTicks; now++) {
    int n = static_cast<int>(ratePerMs);
    if (r.Float64() < ratePerMs - n) n++;
    for (int i = 0; i < n; i++) {
      std::uint64_t h;
      std::size_t live = keys.size() - head;
      if (live == 0 || r.Float64() < newFrac) {
        next++;
        h = hash(next);
        keys.push_back(Born{h, now});
      } else {                          // repeat a key born within the last 1.5 horizons
        std::size_t lo = 0, hi = live;  // sort.Search: the first i with keys[i].t+hTicks*3/2 > now
        while (lo < hi) {
          std::size_t mid = lo + (hi - lo) / 2;
          if (!(keys[head + mid].t + hTicks * 3 / 2 > now)) {
            lo = mid + 1;
          } else {
            hi = mid;
          }
        }
        h = keys[head + lo + static_cast<std::size_t>(r.IntN(static_cast<std::int64_t>(live - lo)))].h;
      }
      op(h, Windows[static_cast<std::size_t>(r.IntN(static_cast<std::int64_t>(Windows.size())))], now);
    }
    if (now % hTicks == 0) {  // drop keys too old to be repeated
      while (head < keys.size() && keys[head].t + 2 * hTicks < now) head++;
    }
  }
}

// goV is a Result as Go's %v prints it: {true 3 4 false false false}.
inline std::string goV(const clepsydra::Result& r) {
  auto b = [](bool x) { return x ? "true" : "false"; };
  return std::string("{") + b(r.Seen) + " " + std::to_string(r.Count) + " " + std::to_string(r.Upper) + " " +
         b(r.Saturated) + " " + b(r.Restored) + " " + b(r.Degraded) + "}";
}

// Fnv64a is hash/fnv's New64a.
struct Fnv64a {
  std::uint64_t h = 14695981039346656037ull;
  void write(const std::string& s) {
    for (unsigned char c : s) {
      h ^= c;
      h *= 1099511628211ull;
    }
  }
  std::uint64_t sum() const { return h; }
};

}  // namespace workload
