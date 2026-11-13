// go/merge_test.go and go/merge_stash_test.go: Merge (claims D24, D26, D29).
#include <map>
#include <vector>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"
#include "workload.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace workload;
using namespace std::chrono_literals;

// TestMerge splits tfbench's velocity workload at random between filters A
// and B, against S fed everything; every 5 s a fresh M = Merge(A) + Merge(B)
// must answer as S does: the same Seen everywhere (no filter evicts here), a
// count error per window at most 1.5x S's, and no Upper below the truth
// unless flagged Saturated.
TEST(TestMerge) {
  if (forceCT || forceBG) SKIP("Merge refuses ConstantTime and Background filters (TestMergeRefuses)");
  Config cfg = cfgOf(4096, 1min, 1ms);
  cfg.Hot = -1;
  auto mk = [&] { return std::make_unique<Filter>(cfg); };
  auto a = mk(), b = mk(), s = mk();
  auto key = [](int k) { return mix64(static_cast<u64>(k) + 1); };
  gorand::Rand rng(13, 13);
  std::vector<std::vector<u64>> times(2000);
  const std::vector<u64> wins{1'000, 5'000, 15'000, 30'000, 60'000};
  double errM[5]{}, errS[5]{}, sum[5]{};
  i64 seenDiff = 0, cmp = 0, lowUnflagged = 0, lowFlagged = 0, checkpoints = 0;
  std::unique_ptr<Filter> m;
  velWorkload(
      [&](int k, u64 at) {
        times[static_cast<std::size_t>(k)].push_back(at);
        s->CheckAndTouchAt(key(k), 0, at);
        if (rng.IntN(2) == 0) {
          a->CheckAndTouchAt(key(k), 0, at);
        } else {
          b->CheckAndTouchAt(key(k), 0, at);
        }
      },
      [&](int, const std::vector<u64>&, int, u64 now) {
        if (now % 5000 != 0 || now < 65'000) return;
        checkpoints++;
        m = mk();
        m->Merge(*a);
        m->Merge(*b);
        for (std::size_t k = 0; k < times.size(); k++) {
          for (std::size_t wi = 0; wi < wins.size(); wi++) {
            u64 w = wins[wi];
            i64 exact = 0;
            for (i64 i = static_cast<i64>(times[k].size()) - 1; i >= 0 && times[k][static_cast<std::size_t>(i)] > now - w; i--)
              exact++;
            Result rm = m->CountAt(key(static_cast<int>(k)), w, now), rs = s->CountAt(key(static_cast<int>(k)), w, now);
            cmp++;
            if (rm.Seen != rs.Seen) seenDiff++;
            errM[wi] += static_cast<double>(std::max(rm.Count - exact, exact - rm.Count));
            errS[wi] += static_cast<double>(std::max(rs.Count - exact, exact - rs.Count));
            sum[wi] += static_cast<double>(exact);
            if (rm.Upper < exact) {
              if (rm.Saturated) {
                lowFlagged++;
              } else {
                lowUnflagged++;
              }
            }
          }
        }
      });
  if (checkpoints == 0) FATALF("no checkpoint");
  LOGF("checkpoints %d, comparisons %d; Upper < exact flagged Saturated %d", checkpoints, cmp, lowFlagged);
  if (u64 ev = a->Evictions() + b->Evictions() + s->Evictions() + m->Evictions(); ev != 0)
    FATALF("%d evictions: the comparison needs none", ev);
  if (seenDiff != 0) ERRORF("Seen differs from the single filter's in %d of %d answers", seenDiff, cmp);
  if (lowUnflagged != 0) ERRORF("Upper below the truth, unflagged, in %d answers", lowUnflagged);
  for (std::size_t wi = 0; wi < wins.size(); wi++) {
    u64 w = wins[wi];
    LOGF("window %2ds  MAE merged %.4f  single %.4f", w / 1000, errM[wi] / sum[wi], errS[wi] / sum[wi]);
    if (errM[wi] > 1.5 * errS[wi])
      ERRORF("window %ds: merged count error %.0f over 1.5x the single filter's %.0f", w / 1000, errM[wi], errS[wi]);
  }
  const u64 end = 5 * 60'000;
  i64 fm = 0, fs = 0;
  for (u64 i = 0; i < 1'000'000; i++) {
    u64 h = mix64(i + (u64{1} << 40));
    if (m->ContainsAt(h, 60'000, end)) fm++;
    if (s->ContainsAt(h, 60'000, end)) fs++;
  }
  LOGF("absent probes 1e6: false Seen merged %d, single %d", fm, fs);
}

// TestMergeRefuses: Merge throws and leaves f as it was for f itself, another
// Config, ConstantTime, and hot entries in use.
TEST(TestMergeRefuses) {
  Config base = cfgOf(4096, 1min);
  Filter f(base);
  f.CheckAndTouchAt(mix64(1), 0, 1000);
  Filter hot(base);
  for (int i = 0; i < 100; i++) hot.CheckAndTouchAt(mix64(2), 0, 1000 + static_cast<u64>(i));
  if (hot.Promoted() == 0) FATALF("no key was promoted to a hot entry");
  Core& c = core(f);
  std::vector<u64> before(c.nwords);
  for (std::size_t i = 0; i < c.nwords; i++) before[i] = c.words[i].load();
  Filter otherCap(cfgOf(8192, 1min)), otherHz(cfgOf(4096, 2min));
  Config ctc = cfgOf(4096, 1min);
  ctc.ConstantTime = 1us;
  Filter ct(ctc);
  std::vector<std::pair<const char*, Filter*>> gs{
      {"itself", &f}, {"other Capacity", &otherCap}, {"other Horizon", &otherHz}, {"ConstantTime", &ct}, {"hot entries", &hot}};
  for (auto& [name, g] : gs) {
    bool threw = false;
    try {
      f.Merge(*g);
    } catch (const Error&) {
      threw = true;
    }
    if (!threw) ERRORF("Merge with %s: no error", name);
  }
  for (std::size_t i = 0; i < c.nwords; i++) {
    if (c.words[i].load() != before[i]) {
      ERRORF("a refused Merge changed f");
      break;
    }
  }
}

// TestMergeKeepsOwnStash (go/merge_stash_test.go): a key evicted to f's stash
// and live in g's table; after f.Merge(g), f must still count the stashed
// history, and still after the key's next eviction (claims D26).
TEST(TestMergeKeepsOwnStash) {
  if (forceCT || forceBG) SKIP("Merge refuses ConstantTime and Background filters (TestMergeRefuses)");
  Config cfg = cfgOf(1000, 1min, 1ms);
  Filter f(cfg), g(cfg);
  Core& c = core(f);
  u64 k = mix64(42);
  for (int i = 0; i < 5; i++) f.TouchAt(k, 0, static_cast<u64>(100 + i));
  for (int i = 0; i < 3; i++) g.TouchAt(k, 0, static_cast<u64>(110 + i));
  auto evict = [&](u64 now) {  // as claimAt's eviction does
    u64 w0, w1;
    i64 i = c.lookupStable(k, fingerprint(k), tag(k), false, w0, w1);
    if (i < 0) FATALF("not found");
    c.words[static_cast<std::size_t>(i) + 1].store(0);
    c.words[static_cast<std::size_t>(i)].store(0);
    c.stashPut(c.slotID(static_cast<u64>(i / wordsPerLine), w0, w1), w0, w1, now);
  };
  evict(120);
  f.Merge(g);
  if (Result r = f.CountAt(k, 60'000, 130); r.Upper < 8 && !r.Saturated)
    ERRORF("after Merge: %+v, truth 8 (5 in f's stash, 3 in g)", r);
  f.TouchAt(k, 0, 131);
  evict(132);
  if (Result r = f.CountAt(k, 60'000, 133); r.Upper < 9 && !r.Saturated) ERRORF("after the next eviction: %+v, truth 9", r);
}
