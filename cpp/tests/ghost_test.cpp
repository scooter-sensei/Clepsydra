// go/ghost_test.go: the overflow table's epochs and layout.
#include <atomic>
#include <chrono>
#include <thread>
#include <vector>

#include "check.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;

// TestGhostEpochRace: concurrent adds to a small ghost table across many slice
// boundaries. An add decodes the entries' 2-bit slices against the epoch it
// read; if another add moved the epoch on and wrote an entry for the new
// slice meanwhile, that entry decodes as four slices old, dead, and must not
// be taken for a free one and overwritten. Each worker reads back its last
// three histories after every add: each must keep its count in Upper, through
// its own entry or a floor, unless the shared clock has moved far enough
// during the read for the history to have rightly aged out.
//
// Go makes that interleaving likely by running its 8 goroutines on 2 Ps
// (GOMAXPROCS(2)), hoping the scheduler preempts an add between reading the
// epoch and scanning its bucket. Here it is made to happen: on one pass in 16
// a worker stops in the window (testHookGhostAdding: the add has loaded the
// epoch and not yet its bucket; testHookGhostFloor, one raise in 4: a floor
// raise likewise) and yields until other adds have moved the epoch on one to
// three slices, then a few times more so the mover can write its entry for
// the new slice. The scan that follows decodes against the stale epoch, the
// interleaving the test is for (an entry one slice newer reads as four slices
// old, two or three newer as live but old), thousands of times a run instead
// of whenever a preemption happens to land there. The 8 threads run on every
// core: on 2 CPUs, as Go's goroutines, the adds overlap less and the guards
// reject a third as often. The test fails if no stop ever saw the epoch move,
// since it would no longer test the race. (cpp/README.md, The overflow
// table's race tests, has the measurements.)
TEST(TestGhostEpochRace) {
  Hooks hooks;
  int rounds = check::short_() ? 8 : 40;
  const u64 horizon = 10'000, n = 5;
  const int workers = 8, per = 4000;
  std::atomic<i64> lost{0}, stops{0}, stale{0};
  std::atomic<u64> seeds{0x853c49e6748fea9b};
  for (int round = 0; round < rounds; round++) {
    Ghost g(4 * 64, horizon, 1 + round % 2, 1 + round % 3);  // one and two candidate buckets, one to three floors
    // stop holds the calling worker in its window on one pass in `every`,
    // until the epoch has moved on one to three slices (2 ms at most).
    auto stop = [&](u64 every) {
      thread_local u64 x = 0;  // xorshift64, a stream per thread
      if (x == 0) x = seeds.fetch_add(0x9e3779b97f4a7c15) | 1;
      x ^= x << 13;
      x ^= x >> 7;
      x ^= x << 17;
      if (x % every != 0) return;
      stops.fetch_add(1);
      const u64 ep = g.epoch.load(), moves = 1 + (x >> 32) % 3;
      const auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(2);
      while (g.epoch.load() < ep + moves) {
        if (std::chrono::steady_clock::now() > deadline) return;
        std::this_thread::yield();
      }
      for (int i = 0; i < 3; i++) std::this_thread::yield();  // the mover writes its entry for the new slice
      stale.fetch_add(1);
    };
    testHookGhostAdding = [&] { stop(16); };
    testHookGhostFloor = [&] { stop(4); };
    std::atomic<u64> clock{horizon};
    std::vector<std::thread> ts;
    for (int w = 0; w < workers; w++) {
      ts.emplace_back([&, w] {
        struct Rec {
          u64 id = 0, last = 0;
        };
        Rec ring[3];
        for (u64 i = 0; i < static_cast<u64>(per); i++) {
          u64 now = clock.fetch_add(200) + 200;
          u64 id = (static_cast<u64>(round) << 48) | (static_cast<u64>(w) << 32) | (i + 1);
          g.add(id, n, now, false, now);
          ring[i % 3] = Rec{id, now};
          u64 at = clock.load();
          for (const Rec& r : ring) {
            if (r.id == 0 || at - r.last > horizon - g.sliceLen) continue;
            Result e = g.estimate(r.id, horizon, at).first;
            // A slice's events are dead once an add has seen the slice three
            // later: the clock may have got there during the read.
            if (u64 after = clock.load(); after / g.sliceLen >= r.last / g.sliceLen + 3) continue;
            if (e.Upper < static_cast<i64>(n) && lost.fetch_add(1) + 1 <= 5)
              LOGF("round %d: history %x added at %d reads %+v at %d", round, r.id, r.last, e, at);
          }
        }
      });
    }
    for (auto& t : ts) t.join();
    testHookGhostAdding = nullptr;
    testHookGhostFloor = nullptr;
  }
  LOGF("%d stops in the window, %d across an epoch move", stops.load(), stale.load());
  if (lost.load() > 0) FATALF("%d reads found a live history below its count", lost.load());
  if (stale.load() == 0) FATALF("no worker stopped in its window saw the epoch move: the race went untested");
}

// TestGhostAddAcrossEpoch: TestGhostEpochRace's interleaving, made to happen
// once and checked exactly. An add loads the epoch; before it scans its
// bucket another add moves the epoch on a slice and writes an entry for the
// new slice there. Decoded against the epoch the first add loaded, that entry
// is four slices old, dead: the first add must not take its place (the
// defect of changes 28, an add that read the epoch once). Not in the
// reference, which finds it only through the race.
TEST(TestGhostAddAcrossEpoch) {
  Hooks hooks;
  const u64 horizon = 10'000;          // slices of 5,000 ticks
  Ghost g(64, horizon, 2, 3);          // one bucket: the histories meet in it
  g.add(1, 5, 52'000, false, 52'000);  // slice 10: the epoch is 11
  bool fired = false;
  testHookGhostAdding = [&] {
    if (fired) return;
    fired = true;
    g.add(2, 7, 55'000, false, 55'000);  // slice 11: the epoch moves on; history 2 takes the first free entry
  };
  g.add(3, 9, 54'999, false, 54'999);  // slice 10, the epoch loaded before the move
  testHookGhostAdding = nullptr;
  if (!fired) FATALF("the hook didn't run: the add's window is gone");
  for (auto [id, want] : {std::pair<u64, i64>{1, 5}, {2, 7}, {3, 9}}) {
    if (auto [r, ok] = g.estimate(id, horizon, 55'000); !ok || r.Count != want)
      ERRORF("history %d reads %+v, want %d events", id, r, want);
  }
}

// TestGhostFloorAcrossEpoch: the same for a floor. A raise loads the epoch;
// before it loads the floor another raise moves the epoch on a slice and
// lifts the floor in the new slice. Decoded against the stale epoch the
// higher floor is dead, and writing the first raise's count over it would
// lower it ("raiseFloor could lower a floor the same way", changes 28): every
// key reading the bucket must still see the higher floor in Upper.
TEST(TestGhostFloorAcrossEpoch) {
  Hooks hooks;
  const u64 horizon = 10'000;          // slices of 5,000 ticks
  Ghost g(64, horizon, 1, 1);          // one bucket, one floor that every fingerprint reads
  g.add(1, 5, 52'000, false, 52'000);  // slice 10: the epoch is 11
  bool fired = false;
  testHookGhostFloor = [&] {
    if (fired) return;
    fired = true;
    g.advance(11);                             // slice 11: the epoch moves on
    g.raiseFloor(0, 2, 20, 11, 55'000);        // a history of 20 events leaves to the floor
  };
  g.raiseFloor(0, 3, 4, 10, 54'999);  // a history of 4 events, slice 10, the epoch loaded before the move
  testHookGhostFloor = nullptr;
  if (!fired) FATALF("the hook didn't run: the raise's window is gone");
  for (u64 id : {u64{1}, u64{99}}) {
    if (auto [r, ok] = g.estimate(id, horizon, 55'000); !ok || r.Upper < 20)
      ERRORF("key %d reads %+v: the floor of 20 was lowered", id, r);
  }
}

// TestGhostAddAheadOfClock: an evictor whose clock is behind the evicted key's
// last touch spills a history whose newest event lies in a slice past the one
// its own clock is in. The entry's 2-bit slice must still decode to that
// slice.
TEST(TestGhostAddAheadOfClock) {
  const u64 horizon = 10'000;  // slices of 5,000 ticks
  Ghost g(4 * 64, horizon, 2, 3);
  g.add(1, 5, 80'008, false, 79'989);  // last touch 80,008; the evictor's clock reads 79,989
  for (u64 now : {u64{79'989}, u64{80'008}, u64{80'037}}) {
    if (Result r = g.estimate(1, horizon, now).first; r.Count != 5)
      FATALF("at %d the history reads %+v, want 5 events", now, r);
  }
  g.add(2, 5, 85'000, false, 85'000);  // a later slice: the epoch moves on, dead entries are swept
  if (Result r = g.estimate(1, horizon, 85'000).first; r.Count != 5)
    FATALF("after the next epoch the history reads %+v, want 5 events", r);
}

// TestGhostReadAcrossEpoch: a read loads its buckets, then an add moves the
// epoch on two slices and sweeps the entries dead by then, before the read
// decodes what it loaded. Against the epoch the lines were loaded under, the
// dead history is dead (claims A34).
TEST(TestGhostReadAcrossEpoch) {
  Hooks hooks;
  const u64 horizon = 10'000;  // slices of 5,000 ticks
  for (int choices : {1, 2}) {
    Ghost g(4 * 64, horizon, choices, 3);
    g.add(1, 5, 1'000, false, 1'000);    // slice 0
    g.add(2, 5, 10'500, false, 10'500);  // slice 2: the epoch moves on, nothing is dead yet
    // At 20,100 (slice 4) history 1's newest event is 19,100 old: dead.
    // History 2's (slice 2) is live, so the read gets past g.until.
    const u64 now = 20'100;
    if (auto [r, ok] = g.estimate(1, horizon, now); ok || !(r == Result{}))
      FATALF("choices %d: history 1 reads %+v before the add, want nothing", choices, r);
    bool fired = false;  // Go sets the hook to nil inside it: here that would destroy the running lambda
    testHookGhostRead = [&g, &fired] {
      if (fired) return;
      fired = true;
      g.add(3, 5, now - 50, false, now - 50);  // slice 4: the epoch moves on two, slice 0 is swept
    };
    auto [r, ok] = g.estimate(1, horizon, now);
    testHookGhostRead = nullptr;
    if (ok || !(r == Result{})) ERRORF("choices %d: history 1, dead, reads %+v across the epoch's move", choices, r);
    if (auto [r2, ok2] = g.estimate(2, horizon, now); !ok2 || r2.Count != 5)
      ERRORF("choices %d: history 2, live, reads %+v", choices, r2);
  }
}

// TestGhostScanLayout: estimate's scan decodes three entries per word, so the
// layout must keep three entries of ghEntryBits to a word.
TEST(TestGhostScanLayout) {
  if (ghPerWord != 3 || 3 * ghEntryBits > 64)
    FATALF("ghPerWord %d, ghEntryBits %d: estimate's scan decodes three entries per word", ghPerWord, ghEntryBits);
}
