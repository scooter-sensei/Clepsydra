// The lab's copy of TestGhostEpochRace (setup.sh builds it), its arrangement
// from the environment: LAB_PIN 0 (every core), 1 (every round's threads on
// the first 2 CPUs, as GOMAXPROCS(2)), 2 (rounds 0-1, 4-5, ... on 2 CPUs);
// LAB_INJECT 0/1 (the stops in the window); LAB_K (a stop waits for 1..K
// epoch moves; the suite's test: 3); LAB_VARY 0/1 (histories of 1-9 events
// instead of 5); LAB_ROUNDS (default as the suite: 8 short, 40 long). Prints
// one LAB line per run. The suite's TestGhostEpochRace is LAB_PIN=0
// LAB_INJECT=1 LAB_K=3; the reference's arrangement ported is LAB_PIN=0
// LAB_INJECT=0. Linux only (the pinning).
#include <pthread.h>
#include <sched.h>

#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <thread>
#include <vector>

#include "check.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;

namespace clepsydra::detail {
extern std::atomic<long long> labMiss[5];
}

// pinTwo confines the calling thread to the first two CPUs it may use.
static void pinTwo() {
  cpu_set_t all, two;
  CPU_ZERO(&all);
  CPU_ZERO(&two);
  if (sched_getaffinity(0, sizeof all, &all) != 0) return;
  for (int i = 0, k = 0; i < CPU_SETSIZE && k < 2; i++)
    if (CPU_ISSET(i, &all)) CPU_SET(i, &two), k++;
  pthread_setaffinity_np(pthread_self(), sizeof two, &two);
}

static int labEnv(const char* k, int def) {
  const char* v = std::getenv(k);
  return v && *v ? std::atoi(v) : def;
}

TEST(TestGhostLab) {
  Hooks hooks;
  const int pinMode = labEnv("LAB_PIN", 0), inject = labEnv("LAB_INJECT", 1);
  const u64 kMax = static_cast<u64>(labEnv("LAB_K", 3)), vary = static_cast<u64>(labEnv("LAB_VARY", 0));
  int rounds = labEnv("LAB_ROUNDS", check::short_() ? 8 : 40);
  const u64 horizon = 10'000, n = 5;
  const int workers = 8, per = 4000;
  std::atomic<i64> lost{0}, stops{0}, stale{0}, pinnedRounds{0};
  std::atomic<u64> seeds{0x853c49e6748fea9b};
  for (auto& m : labMiss) m = 0;
  auto t0 = std::chrono::steady_clock::now();
  for (int round = 0; round < rounds; round++) {
    Ghost g(4 * 64, horizon, 1 + round % 2, 1 + round % 3);
    const bool pinned = (pinMode == 1 || (pinMode == 2 && (round / 2) % 2 == 0));
    pinnedRounds += pinned;
    auto stop = [&](u64 every) {
      thread_local u64 x = 0;
      if (x == 0) x = seeds.fetch_add(0x9e3779b97f4a7c15) | 1;
      x ^= x << 13;
      x ^= x >> 7;
      x ^= x << 17;
      if (x % every != 0) return;
      stops.fetch_add(1);
      const u64 ep = g.epoch.load(), k = 1 + (x >> 32) % kMax;
      const auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(2);
      while (g.epoch.load() < ep + k) {
        if (std::chrono::steady_clock::now() > deadline) return;
        std::this_thread::yield();
      }
      for (int i = 0; i < 3; i++) std::this_thread::yield();
      stale.fetch_add(1);
    };
    if (inject) {
      testHookGhostAdding = [&] { stop(16); };
      testHookGhostFloor = [&] { stop(4); };
    }
    std::atomic<u64> clock{horizon};
    std::vector<std::thread> ts;
    for (int w = 0; w < workers; w++) {
      ts.emplace_back([&, w] {
        if (pinned) pinTwo();
        struct Rec {
          u64 id = 0, last = 0, n = 0;
        };
        Rec ring[3];
        for (u64 i = 0; i < static_cast<u64>(per); i++) {
          u64 now = clock.fetch_add(200) + 200;
          u64 id = (static_cast<u64>(round) << 48) | (static_cast<u64>(w) << 32) | (i + 1);
          const u64 cnt = vary ? 1 + mix64(id) % 9 : n;
          g.add(id, cnt, now, false, now);
          ring[i % 3] = Rec{id, now, cnt};
          u64 at = clock.load();
          for (const Rec& r : ring) {
            if (r.id == 0 || at - r.last > horizon - g.sliceLen) continue;
            Result e = g.estimate(r.id, horizon, at).first;
            if (u64 after = clock.load(); after / g.sliceLen >= r.last / g.sliceLen + 3) continue;
            if (e.Upper < static_cast<i64>(r.n)) lost.fetch_add(1);
          }
        }
      });
    }
    for (auto& t : ts) t.join();
    testHookGhostAdding = nullptr;
    testHookGhostFloor = nullptr;
  }
  double secs = std::chrono::duration<double>(std::chrono::steady_clock::now() - t0).count();
  std::printf("LAB lost=%lld stops=%lld stale=%lld miss=%lld/%lld/%lld/%lld/%lld pinned=%lld secs=%.2f\n",
              static_cast<long long>(lost.load()), static_cast<long long>(stops.load()),
              static_cast<long long>(stale.load()), labMiss[0].load(), labMiss[1].load(), labMiss[2].load(),
              labMiss[3].load(), labMiss[4].load(), static_cast<long long>(pinnedRounds.load()), secs);
  if (lost.load() > 0) FATALF("%d reads found a live history below its count", lost.load());
}
