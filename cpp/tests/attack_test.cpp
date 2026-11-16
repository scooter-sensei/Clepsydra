// go/attack_test.go: attacks on the filter (docs/paper/06-security.md), each
// measured against the defence: forged identities and lock-stripe floods need
// the hash to be unkeyed; eviction floods need the stash to be off or overrun;
// heavy hitters need the count to read low without saying so. The regression
// tier compares the log with go/verify/expected/attacks.txt.
#include <algorithm>
#include <functional>
#include <set>
#include <string>
#include <vector>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

namespace {

// unmix64 inverts mix64: the unkeyed hash is a bijection anyone can run
// backwards.
u64 unmix64(u64 y) {
  auto inv = [](u64 c) {
    u64 x = c;
    for (int i = 0; i < 6; i++) x *= 2 - c * x;
    return x;
  };
  y ^= (y >> 31) ^ (y >> 62);
  y *= inv(0x94d049bb133111ebULL);
  y ^= (y >> 27) ^ (y >> 54);
  y *= inv(0xbf58476d1ce4e5b9ULL);
  return y ^ (y >> 30) ^ (y >> 60);
}

// Evicting is evictionSetup's result.
struct Evicting {
  Attacked a;
  gorand::Rand rng;
  u64 off;
};

// evictionSetup fills a table to its design load with one-touch keys and adds
// an offender with k events.
Evicting evictionSetup(i64 stash, int k, const Opts& opts) {
  Evicting e{attackFilter(4096, stash, opts), gorand::Rand(7, static_cast<u64>(stash + 1)), 0};
  Filter& f = *e.a.f;
  u64& now = *e.a.now;
  for (i64 i = 0, n = f.Slots() * 85 / 100; i < n; i++) {
    now++;
    f.TouchAt(e.rng.Uint64(), 0, now);
  }
  e.off = e.rng.Uint64();
  for (int i = 0; i < k; i++) {
    now++;
    f.TouchAt(e.off, 0, now);
  }
  return e;
}

// sprayUntilEvicted touches each key from next k+1 times (so it outranks the
// offender for eviction) until the offender is out of the table, then returns
// the attacker's event count.
int sprayUntilEvicted(Filter& f, u64& now, u64 off, int k, const std::function<u64()>& next) {
  u64 fp = fingerprint(off), tg = tag(off);
  int events = 0;
  while (core(f).locate(off, fp, tg) >= 0 && events < (1 << 24)) {
    u64 h = next();
    for (int i = 0; i < k + 1; i++) {
      f.TouchAt(h, 0, now);
      events++;
    }
    now++;
  }
  return events;
}

}  // namespace

// TestAttackForgedIdentity: with the old unkeyed integer hash an attacker
// computes ids whose hashes differ from a victim's only in bit 0..3. They land
// on the victim's line with its identity, so the attacker's events count as
// the victim's (framing it) and the victim reads as seen. Keyed, the same ids
// land elsewhere.
TEST(TestAttackForgedIdentity) {
  const u64 victim = 4242;
  const int perForgery = 25;
  auto run = [&](const std::function<u64(u64)>& hash) {
    Attacked a = attackFilter(1 << 14, 0);
    Filter& f = *a.f;
    u64& now = *a.now;
    u64 hv = hash(victim);
    for (int b = 0; b < 4; b++) {
      u64 forged = unmix64(mix64(victim) ^ (u64{1} << b));  // chosen against the unkeyed hash
      for (int i = 0; i < perForgery; i++) {
        now++;
        f.TouchAt(hash(forged), 0, now);
      }
    }
    Result r = f.CountAt(hv, f.Ticks(1min), now);
    return std::pair<i64, bool>{r.Count, r.Seen};
  };
  auto [before, seenBefore] = run([](u64 x) { return mix64(x); });
  Attacked keyed = attackFilter(1, 0);
  auto [after, seenAfter] = run([&](u64 x) { return keyed.f->HashUint64(x); });
  LOGF("victim never touched; attacker sent 4 forged ids x %d: unkeyed reads %d (seen %v), keyed reads %d (seen %v)",
       perForgery, before, seenBefore, after, seenAfter);
  if (before != 4 * perForgery || !seenBefore) ERRORF("the forgery should work on the unkeyed hash: read %d", before);
  if (after != 0 || seenAfter) ERRORF("keyed hash: victim reads %d, seen %v; want 0, false", after, seenAfter);
}

// TestAttackStripeFlood: unkeyed, an attacker picks ids whose hashes share the
// top 8 bits, so every first touch queues on one of the 256 claim locks.
// Keyed, the same ids spread over the stripes.
TEST(TestAttackStripeFlood) {
  Attacked a = attackFilter(1, 0);
  std::set<u64> unkeyed, keyed;
  for (u64 i = 0; i < 4096; i++) {
    u64 id = unmix64((u64{0xAB} << 56) | ((i * 0x9e3779b97f4a7c15ULL) >> 8));
    unkeyed.insert(mix64(id) >> (64 - claimStripeBits));
    keyed.insert(a.f->HashUint64(id) >> (64 - claimStripeBits));
  }
  LOGF("4096 crafted ids hit %d stripe(s) unkeyed, %d keyed", unkeyed.size(), keyed.size());
  if (unkeyed.size() != 1 || keyed.size() < 240)
    ERRORF("stripes: unkeyed %d (want 1), keyed %d (want ~256)", unkeyed.size(), keyed.size());
}

// TestAttackEvictionReset: an offender with k events gets evicted so that it
// comes back as a first-time key. With the unkeyed hash the attacker builds
// keys on the offender's two lines (same primary line and fingerprint, other
// tags): a few hundred events. Keyed, the spray lands at random and needs
// about a table's worth of keys, each outranking the offender. The stash then
// brings the offender back with its history. A flood that also overruns the
// stash with heavier keys pushes the history into the overflow table: Upper
// still covers it, and Count does when the history stands out from the
// flood's noise (then Degraded). Without the overflow table the history is
// lost, and every new key reads Degraded.
TEST(TestAttackEvictionReset) {
  if (forceCT || forceBG)
    SKIP(
        "ConstantTime evicts only when a waiting key's stash set is full; Background evicts in its placer "
        "goroutine, so the spray can't tell when");
  const int k = 20;
  const u64 window = 60'000;
  auto report = [&](const std::string& name, Filter& f, int events, const Result& r) {
    LOGF(
        "%-30s attacker events %7d; offender returns: count %2d upper %3d (true %d) seen %-5v restored %-5v degraded "
        "%-5v; evictions %d folded %d lost %d",
        name, events, r.Count, r.Upper, k + 1, r.Seen, r.Restored, r.Degraded, f.Evictions(), f.Folded(), f.Lost());
  };
  int targeted = 0, sprayed = 0;
  for (i64 stash : {i64{-1}, i64{0}}) {
    Evicting e = evictionSetup(stash, k, {noSketch});
    Filter& f = *e.a.f;
    u64& now = *e.a.now;
    const u64 off = e.off;
    u64 tagN = 0;
    int events = sprayUntilEvicted(f, now, off, k, [&] {  // forge: flip tag bits only
      tagN++;
      return off ^ (tagN << 36);
    });
    now++;
    Result r = f.TouchAt(off, window, now);
    report(check::format("unkeyed targeted, stash %v", stash >= 0), f, events, r);
    targeted = events;
    if (stash < 0 && (r.Count != 1 || !r.Degraded))
      ERRORF("no stash: expected a reset reported as Degraded, got %+v", r);
    if (stash >= 0 && (!r.Restored || r.Count != k + 1))
      ERRORF("stash: offender not restored after a targeted eviction: %+v", r);
  }
  struct Mode {
    std::string name;
    i64 stash;
    Opts opts;
  };
  const std::vector<Mode> modes{
      {"keyed spray, no stash/sketch", -1, {noSketch}},
      {"keyed spray, stash only", 0, {noSketch}},
      {"keyed spray, stash + sketch", 0, {}},
  };
  for (const Mode& m : modes) {
    Evicting e = evictionSetup(m.stash, k, m.opts);
    Filter& f = *e.a.f;
    u64& now = *e.a.now;
    int events = sprayUntilEvicted(f, now, e.off, k, [&] { return e.rng.Uint64(); });
    now++;
    Result r = f.TouchAt(e.off, window, now);
    report(m.name, f, events, r);
    sprayed = std::max(sprayed, events);
    if (!r.Restored && !r.Degraded && r.Upper < k + 1) ERRORF("%s: history lost silently: %+v", m.name, r);
    if (m.opts.empty() && (f.Lost() != 0 || r.Upper < k + 1))
      ERRORF("%s: lost %d, Upper %d < %d", m.name, f.Lost(), r.Upper, k + 1);
  }
  LOGF("keyed hashing raises the cost of evicting a %d-event key from %d to %d attacker events (%.0fx; table %d slots)",
       k, targeted, sprayed, static_cast<double>(sprayed) / static_cast<double>(targeted), 4096 * 100 / 85);
}

// TestAttackFloodCollateral: offenders with k events each, then a flood of
// keys sent k+1 times each (heavier than the offenders, so they evict them) at
// 1x to 16x the table's slots, or a cheap flood of one-event keys, all within
// 30 s. Afterwards: how the offenders read (Upper never below the truth;
// Count's shortfall), and how keys never sent read (Degraded, Seen, Upper).
// Without the overflow table every new key reads Degraded once any history is
// lost.
TEST(TestAttackFloodCollateral) {
  const int k = 20, offenders = 200, probes = 20'000;
  const u64 window = 60'000;
  struct Flood {
    i64 mult;
    int weight;
  };
  const std::vector<Flood> floods{{1, k + 1}, {2, k + 1}, {3, k + 1}, {4, k + 1}, {8, k + 1}, {16, k + 1}, {16, 1}};
  for (bool sk : {false, true}) {
    for (const Flood& fl : floods) {
      const i64 mult = fl.mult;
      Opts opts;
      if (!sk) opts.push_back(noSketch);
      Attacked a = attackFilter(4096, 0, opts);
      Filter& f = *a.f;
      u64& now = *a.now;
      gorand::Rand rng(11, static_cast<u64>(mult * 100 + fl.weight));
      for (i64 i = 0, n = f.Slots() * 85 / 100; i < n; i++) {
        now++;
        f.TouchAt(rng.Uint64(), 0, now);
      }
      std::vector<u64> offs(offenders);
      for (u64& o : offs) {
        o = rng.Uint64();
        for (int i = 0; i < k; i++) f.TouchAt(o, 0, now);
        now++;
      }
      const i64 per = std::max<i64>(1, f.Slots() * mult / 30'000);
      for (i64 i = 0, n = f.Slots() * mult; i < n; i++) {
        u64 h = rng.Uint64();
        for (int j = 0; j < fl.weight; j++) f.TouchAt(h, 0, now);
        if (i % per == 0) now++;  // the flood spans at most 30 s of the 60 s horizon
      }
      now++;
      int low = 0, below = 0;
      i64 shortfall = 0;
      for (u64 h : offs) {
        Result r = f.CountAt(h, window, now);
        if (r.Upper < k) below++;
        if (r.Count < k) {
          low++;
          shortfall += k - r.Count;
        }
      }
      int deg = 0, seen = 0, strict = 0;
      i64 upper = 0;
      for (int i = 0; i < probes; i++) {
        Result r = f.CountAt(rng.Uint64(), window, now);
        if (r.Degraded) deg++;
        if (r.Degraded || r.Upper > k) strict++;  // a strict limit of k turns it away
        if (r.Seen) seen++;
        upper += r.Upper;
      }
      LOGF(
          "sketch %-5v flood %2dx slots x %2d (%7d events): offenders Upper<true %d/%d, Count low %3d/%d (mean "
          "shortfall %4.1f); new keys Degraded %5.1f%% Seen %5.2f%% mean Upper %6.1f, denied by a strict limit of %d: "
          "%5.1f%%; folded %d shed %d lost %d",
          sk, mult, fl.weight, f.Slots() * mult * fl.weight, below, offenders, low, offenders,
          static_cast<double>(shortfall) / static_cast<double>(std::max(low, 1)),
          100 * static_cast<double>(deg) / probes, 100 * static_cast<double>(seen) / probes,
          static_cast<double>(upper) / probes, k, 100 * static_cast<double>(strict) / probes, f.Folded(), f.Shed(),
          f.Lost());
      if (sk && (below > 0 || f.Lost() > 0))
        ERRORF("sketch: %d offenders read Upper below the truth, %d lost", below, f.Lost());
      if (sk && deg * 100 > probes) ERRORF("sketch: %d of %d new keys read Degraded", deg, probes);
    }
  }
}

// TestAttackHeavyHitter: a key sending faster than the register holds reads
// low (docs/paper/03-design.md, count limits). That must never happen
// silently: whenever Saturated is false, Upper must be >= the exact count.
// Steady and bursty senders from 1 to 500 events/s, queried for 1-60 s
// windows at random times, against an exact log of their events.
TEST(TestAttackHeavyHitter) {
  struct Stats {
    i64 queries = 0, sat = 0, silentLow = 0, upperLow = 0, countLow = 0;
  };
  const std::vector<double> rates{1, 5, 12, 13, 20, 31, 50, 100, 500};
  Stats all;
  for (bool burst : {false, true}) {
    for (double rate : rates) {
      Stats s;
      for (int key = 0; key < 8; key++) {
        Attacked a = attackFilter(1024, 0);
        Filter& f = *a.f;
        u64& now = *a.now;
        gorand::Rand rng(static_cast<u64>(rate * 1000), static_cast<u64>(key));
        u64 h = rng.Uint64();
        std::vector<u64> log;
        u64 end = u64{180'000} + rng.Uint64N(60'000);
        while (now < end) {
          double gap = rng.ExpFloat64() * 1000 / rate;
          if (burst && rng.IntN(20) == 0) gap *= 20;  // quiet spells, then catch-up bursts
          now += std::max<u64>(1, static_cast<u64>(gap));
          int n = 1;
          if (burst) n = 1 + static_cast<int>(rng.IntN(4));
          for (int i = 0; i < n; i++) {
            f.TouchAt(h, 0, now);
            log.push_back(now);
          }
          if (rng.IntN(25) == 0) {
            u64 w = static_cast<u64>(1000 * (1 + rng.IntN(60)));
            i64 exact = 0;
            for (u64 ts : log)
              if (now - ts <= w) exact++;
            Result r = f.CountAt(h, w, now);
            s.queries++;
            if (r.Saturated) {
              s.sat++;
              continue;
            }
            if (static_cast<double>(r.Count) < 0.9 * static_cast<double>(exact) - 1) s.countLow++;
            if (r.Upper < exact) {
              s.upperLow++;
              if (s.upperLow <= 3)
                LOGF("rate %v burst %v: window %ds exact %d, Count %d, Upper %d, not Saturated", rate, burst, w / 1000,
                     exact, r.Count, r.Upper);
            }
          }
        }
      }
      LOGF("burst %-5v %4.0f ev/s: %5d queries, %5.1f%% Saturated; unflagged: Upper < exact %d, Count >10%% low %d",
           burst, rate, s.queries, 100 * static_cast<double>(s.sat) / static_cast<double>(std::max<i64>(1, s.queries)),
           s.upperLow, s.countLow);
      all.queries += s.queries;
      all.sat += s.sat;
      all.upperLow += s.upperLow;
      all.countLow += s.countLow;
    }
  }
  check::print(check::format(
      "heavy hitter: %d queries, %d Saturated, %d unflagged with Upper < exact, %d unflagged with Count >10%% low",
      all.queries, all.sat, all.upperLow, all.countLow));
  if (all.upperLow > 0) ERRORF("%d unflagged queries had Upper below the exact count", all.upperLow);
}

// TestUpperBoundRandom is TestAttackHeavyHitter over random senders: each key
// switches between random rates (0.2-60 events/s), silences and bursts of up
// to 200 events in one tick, with horizons of 10 s and 60 s. It checks the
// same contract (unflagged Upper >= exact) and reports how loose Upper is.
TEST(TestUpperBoundRandom) {
  int keys = check::short_() ? 300 : 3000;
  i64 queries = 0, sat = 0, low = 0;
  double slack = 0, exactSum = 0;
  const std::vector<double> rateChoices{0.2, 1, 3, 8, 12, 20, 40, 60};
  for (int key = 0; key < keys; key++) {
    gorand::Rand rng(99, static_cast<u64>(key));
    const Duration hz = key % 2 == 0 ? Duration(10s) : Duration(1min);
    u64 now = 1;
    Config cfg = cfgOf(64, hz, 1ms);
    cfg.Clock = [&now] { return now; };
    Filter f(cfg);
    const u64 hMs = static_cast<u64>(hz / 1ms);
    u64 h = rng.Uint64();
    std::vector<u64> log;
    double rate = 0;
    while (now < 6 * hMs) {
      if (rng.IntN(200) == 0 || rate == 0) rate = rateChoices[static_cast<std::size_t>(rng.IntN(8))];
      now += std::max<u64>(1, static_cast<u64>(rng.ExpFloat64() * 1000 / rate));
      if (rng.IntN(500) == 0) now += static_cast<u64>(rng.IntN(static_cast<i64>(hMs)));  // silence
      int n = 1;
      if (rng.IntN(100) == 0) n = 1 + static_cast<int>(rng.IntN(200));  // burst
      for (int i = 0; i < n; i++) {
        f.TouchAt(h, 0, now);
        log.push_back(now);
      }
      if (rng.IntN(10) == 0) {
        u64 w = 1 + rng.Uint64N(hMs);
        i64 exact = 0;
        for (u64 ts : log)
          if (now - ts <= w) exact++;
        Result r = f.CountAt(h, w, now);
        queries++;
        if (r.Saturated) {
          sat++;
          continue;
        }
        if (r.Upper < exact) {
          low++;
          if (low <= 5)
            LOGF("key %d horizon %v window %dms: exact %d Count %d Upper %d unflagged", key, hz, w, exact, r.Count,
                 r.Upper);
        }
        slack += static_cast<double>(r.Upper - exact);
        exactSum += static_cast<double>(exact);
      }
    }
  }
  check::print(check::format(
      "upper bound: %d queries, %d Saturated, %d unflagged with Upper < exact; unflagged Upper exceeds exact by %.1f%% "
      "of events",
      queries, sat, low, 100 * slack / std::max(1.0, exactSum)));
  if (low > 0) ERRORF("%d unflagged queries had Upper below the exact count", low);
}
