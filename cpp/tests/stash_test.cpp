// go/stash_race_test.go, go/restore_test.go, go/lost_test.go,
// go/hashbatch_test.go and go/mass_exp_test.go.
#include <atomic>
#include <cstdlib>
#include <thread>
#include <vector>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

// TestStashConcurrentEvictions: 8 workers touch 32 hot keys in a table of ~76
// slots while flooding it with heavier keys, so hot keys are evicted to the
// stash and restored over and over, concurrently with their own touches. With
// a stash big enough to drop nothing, every hot key must end with its exact
// count.
TEST(TestStashConcurrentEvictions) {
  const int workers = 8, hotN = 32, perKey = 20;
  int rounds = check::short_() ? 8 : 40;
  for (int round = 0; round < rounds; round++) {
    std::atomic<u64> clock{1};
    Config cfg = cfgOf(64, 1min, 1ms);
    cfg.Clock = [&clock] { return clock.load(); };
    cfg.Stash = 1 << 12;
    Filter f(cfg);
    std::vector<u64> keys(hotN);
    for (int i = 0; i < hotN; i++) keys[i] = mix64((static_cast<u64>(round) << 32) | static_cast<u64>(i));
    std::vector<std::thread> ts;
    for (int w = 0; w < workers; w++)
      ts.emplace_back([&, w] {
        gorand::Rand rng(static_cast<u64>(round), static_cast<u64>(w));
        for (int i = 0; i < hotN * perKey / workers; i++) {
          u64 now = clock.fetch_add(25) + 25;
          f.TouchAt(keys[static_cast<std::size_t>((i * workers + w) % hotN)], 0, now);
          if (i % 2 == 0) {
            u64 h = rng.Uint64();
            for (int k = 0; k < 25; k++) f.TouchAt(h, 0, now);
          }
        }
      });
    for (auto& t : ts) t.join();
    u64 now = clock.load();
    for (int i = 0; i < hotN; i++) {
      Result r = f.CountAt(keys[i], f.Ticks(1min), now);
      if (r.Count != perKey || r.Saturated)
        FATALF("round %d key %d: %+v, want count %d (evictions %d, restored %d, lost %d)", round, i, r, perKey,
               f.Evictions(), f.Restored(), f.Lost());
    }
    if (round == 0) LOGF("round 0: evictions %d, restored %d, lost %d", f.Evictions(), f.Restored(), f.Lost());
  }
}

// TestRestoreBehindLastTouch (go/restore_test.go): a key whose history comes
// back from the stash in a call with a clock behind the history's last touch
// keeps that last touch. Two paths: an evicted key restored into the table,
// and, in ConstantTime, a key waiting in the stash touched again there.
TEST(TestRestoreBehindLastTouch) {
  for (bool ct : {false, true}) {
    LOGF("-- %s", ct ? "ConstantTime waiting" : "evicted");
    if ((forceCT || forceBG) && !ct) {
      LOGF("ConstantTime (TF_CT=1) and Background (TF_BG=1) evict no key here: inserts wait");
      continue;
    }
    // A 64-entry stash: one waiting key doesn't force placement.
    Config cfg = cfgOf(4, 10s, 1ms);
    cfg.Clock = [] { return u64{1}; };
    cfg.Stash = 64;
    Filter f(cfg);
    Core& c = core(f);
    c.ct = ct;
    // Two lines, eight slots: touch keys at 1000 until one is in the stash.
    u64 k = 0;
    for (; k < 64; k++) {
      f.TouchAt(mix64(k), 0, 1000);
      if (f.Evictions() + f.Waited() > 0) break;
    }
    u64 v = 0;
    for (u64 i = 0; i < k + 1; i++) {
      std::string w = whereIs(c, mix64(i), 1000);
      if (w.find("stash") != std::string::npos && w.find("slot") == std::string::npos) v = mix64(i);
    }
    if (v == 0) FATALF("no key in the stash after %d touches (evictions %d, waited %d)", k + 1, f.Evictions(), f.Waited());
    f.TouchAt(v, 0, 990);  // a caller whose clock is 10 ticks behind v's last touch
    // v's events: 1000 and 990. In [995, 1010] one of them.
    if (Result r = f.CountAt(v, 15, 1010); r.Upper < 1 || r.Saturated)
      FATALF("Count after a touch behind the last one: %+v, want Upper >= 1 (events at 990 and 1000; %s)", r,
             whereIs(c, v, 1010));
    if (Result r = f.CountAt(v, 30, 1010); r.Upper < 2) FATALF("Count over both events: %+v, want Upper >= 2", r);
  }
}

// TestLostAtNewestEvent (go/lost_test.go): without an overflow table a
// history the stash drops is lost, and unseen keys read Degraded until a
// horizon after its newest event, even when the dropper's clock is behind it.
TEST(TestLostAtNewestEvent) {
  Config cfg = cfgOf(64, 1s, 1ms);
  cfg.Overflow = -1;
  Filter f(cfg);
  Core& c = core(f);
  u64 h = mix64(1);
  u64 fp = fingerprint(h), tg = tag(h);
  u64 w1 = c.nextReg(0, tg, 1000, 1000, true).first;
  c.spill(c.stashID(h, fp, tg), (fp << 48) | 1000, w1, 990);  // dropped by a clock 10 ticks behind its last touch
  if (f.Lost() != 1) FATALF("Lost() %d, want 1", f.Lost());
  if (Result r = f.CountAt(h, 1000, 1995); !r.Degraded)
    FATALF("at 1995, a horizon after the dropper's clock but not the event: %+v, want Degraded", r);
  if (Result r = f.CountAt(h, 1000, 2001); r.Degraded) FATALF("at 2001, past a horizon after the event: %+v, want not Degraded", r);
}

// TestHashUint64sMatchesFilter (go/hashbatch_test.go): HashUint64s equals
// HashUint64 with and without Config.Key, on every batch length 0-130.
TEST(TestHashUint64sMatchesFilter) {
  std::array<std::uint8_t, 16> key{9, 8, 7};
  for (bool keyed : {false, true}) {
    Config cfg = cfgOf(10, 1min);
    if (keyed) cfg.Key = key;
    Filter f(cfg);
    std::vector<u64> keys(130), out(130);
    for (std::size_t n = 0; n <= 130; n++) {
      for (std::size_t i = 0; i < n; i++) keys[i] = mix64(static_cast<u64>((n << 8) + i));
      f.HashUint64s(std::span<const u64>(keys.data(), n), out);
      for (std::size_t i = 0; i < n; i++) {
        if (u64 want = f.HashUint64(keys[i]); out[i] != want)
          FATALF("keyed %v, batch of %d, key %d: %#x, want %#x", keyed, n, i, out[i], want);
      }
    }
  }
}

// TestMassMatchesView (go/mass_exp_test.go; TF_EXP=1): mass, read from word1
// in place, equals the view's walk on 20 M random registers biased to the
// edges.
TEST(TestMassMatchesView) {
  if (const char* e = std::getenv("TF_EXP"); !e || !*e) SKIP("set TF_EXP=1");
  auto fp = newTest(1000, 1min);
  Core& f = core(*fp);
  auto viewMass = [&](u64 w1, u64 last, u64 now) -> u64 {
    if (age(now, last) > f.horizon || (w1 & regMask) == 0) return 0;
    View v = f.viewOf(0, w1, last).first;
    return f.viewMass(v, now);
  };
  u64 u = f.unit, h = f.horizon;
  u64 x = 7;
  auto next = [&] {
    x = mix64(x + 0x9e3779b97f4a7c15);
    return x;
  };
  const u64 offs[3] = {0, 1, u - 1};
  for (int i = 0; i < 20'000'000; i++) {
    u64 r = next();
    u64 reg = 0;
    if (r % 8 == 1) {  // bucket 0 alone
      reg = next() % 32;
    } else if (r % 8 != 0) {  // (0: empty)
      for (int j = 0; j < nBuckets; j++) {
        u64 mx = (u64{1} << bucketBits[j]) - 1;
        u64 c = 0;
        switch (next() % 4) {
          case 1:
            c = 1;
            break;
          case 2:
            c = mx;
            break;
          case 3:
            c = next() & mx;
            break;
        }
        reg |= c << bucketShift[j];
      }
    }
    u64 tgPart = next() % ((u64{1} << tagBits) - 1);  // Go evaluates the two draws left to right
    u64 onPart = next() & onsetMask;
    u64 w1 = (tgPart << tagShift) | (onPart << onsetShift) | reg;
    u64 last = 3 * h + next() % (10 * h);
    if (next() % 4 == 0) last = last / u * u + offs[next() % 3];  // on, just past, just before a unit boundary
    u64 now;
    switch (next() % 6) {
      case 0:
        now = last;  // an event at now
        break;
      case 1:
        now = last - next() % (2 * u);  // a caller behind the last touch
        break;
      case 2:
        now = last + h - 1 + next() % 3;  // at the horizon
        break;
      case 3: {
        u64 a = last / u + next() % 30;
        now = a * u + offs[next() % 3];  // unit boundaries
        break;
      }
      default:
        now = last + next() % (h + 2 * u);
    }
    if (u64 got = f.mass(0, w1, last, now), want = viewMass(w1, last, now); got != want)
      FATALF("case %d: w1 %#x last %d now %d: mass %d, view %d", i, w1, last, now, got, want);
  }
}
