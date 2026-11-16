// go/batch_test.go: the batches against single calls, warm's hand-offs.
#include <vector>

#include "check.hpp"
#include "gorand.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

// TestBatchAtHighLoadMatchesSingle: at 90% load, where a quarter of the keys
// live in their alternate line, batches of repeated, present and new keys
// answer as the same calls made one at a time, in order.
TEST(TestBatchAtHighLoadMatchesSingle) {
  auto ap = newTest(1 << 14, 1min), bp = newTest(1 << 14, 1min);
  Filter &a = *ap, &b = *bp;
  Core& ca = core(a);
  std::vector<u64> pool(static_cast<std::size_t>(a.Slots() * 90 / 100));
  for (std::size_t i = 0; i < pool.size(); i++) {
    pool[i] = mix64(i + (u64{1} << 44));
    a.TouchAt(pool[i], 0, 1000);
    b.TouchAt(pool[i], 0, 1000);
  }
  gorand::Rand rng(3, 4);
  std::vector<u64> hs(4096);
  std::unique_ptr<bool[]> seen(new bool[hs.size()]());
  std::vector<i64> counts(hs.size());
  i64 missed = 0;
  u64 fresh = u64{1} << 50;
  for (int round = 0; round < 8; round++) {
    u64 now = 2000 + static_cast<u64>(round) * 500;
    for (std::size_t c = 0; c < hs.size(); c += 64) {  // 16 keys, 4 times each, a quarter of them new
      for (std::size_t i = 0; i < 16; i++) {
        u64 k = pool[static_cast<std::size_t>(rng.IntN(static_cast<i64>(pool.size())))];
        if (rng.IntN(4) == 0) {
          k = mix64(fresh);
          fresh++;
        }
        for (std::size_t j = 0; j < 4; j++) hs[c + i * 4 + j] = k;
      }
      rng.Shuffle(64, [&](i64 i, i64 j) {
        std::swap(hs[c + static_cast<std::size_t>(i)], hs[c + static_cast<std::size_t>(j)]);
      });
    }
    for (u64 h : hs)
      if (ca.find(ca.primary(h), fingerprint(h) << 1, tag(h)) < 0) missed++;
    std::span<bool> sv(seen.get(), hs.size());
    if (round % 2 == 0) {
      a.TouchAndCountBatch(hs, 3000, now, sv, counts);
    } else {
      a.CheckAndTouchBatch(hs, 3000, now, sv);
    }
    for (std::size_t i = 0; i < hs.size(); i++) {
      auto [s, c] = b.TouchAndCountAt(hs[i], 3000, now);
      if (seen[i] != s || (round % 2 == 0 && counts[i] != c))
        FATALF("round %d key %d: batch %v/%d single %v/%d", round, i, seen[i], counts[i], s, c);
    }
  }
  LOGF("%.1f%% of keys not in their primary line before their batch; evictions %d, relocations %d",
       100 * static_cast<double>(missed) / static_cast<double>(8 * hs.size()), a.Evictions(), a.Relocations());
}

// TestTouchStaleSlot: warm hands each touch the slot where it found the key,
// and the chunk's earlier touches can move or replace the entry meanwhile, so
// a touch first checks that the slot still holds its key.
TEST(TestTouchStaleSlot) {
  auto fp = newTest(1000, 1min);
  Filter& f = *fp;
  Core& c = core(f);
  u64 a = mix64(7);
  u64 b = a ^ (u64{1} << 36);  // another tag; the same fingerprint and lines
  if (fingerprint(a) != fingerprint(b) || tag(a) == tag(b) || c.primary(a) != c.primary(b))
    FATALF("b must share a's fingerprint and lines and differ in tag");
  f.TouchAt(a, 0, 1000);
  f.TouchAt(b, 0, 1000);
  i64 ib = c.locate(b, fingerprint(b), tag(b));
  i64 empty = -1;
  for (std::size_t i = 0; i < c.nwords && empty < 0; i += 2)
    if (c.words[i].load() == 0) empty = static_cast<i64>(i);
  u64 w0 = c.words[static_cast<std::size_t>(ib)].load(), w1 = c.words[static_cast<std::size_t>(ib) + 1].load();
  i64 i0s[2] = {ib, empty};
  for (int n = 0; n < 2; n++) {
    if (Result r = c.touch(a, 60'000, 2000, true, i0s[n]); r.Count != 2 + n || !r.Seen)
      FATALF("a handed slot %d: %+v, want count %d", i0s[n], r, 2 + n);
  }
  if (c.words[static_cast<std::size_t>(ib)].load() != w0 || c.words[static_cast<std::size_t>(ib) + 1].load() != w1)
    FATALF("a's touch changed b's slot");
  if (Result r = f.CountAt(b, 60'000, 2000); r.Count != 1) FATALF("b: %+v, want count 1", r);
}

// TestWarmConstantTime: in ConstantTime a batch's warm does the same work
// whatever the table holds: it hands every touch no slot. Outside
// ConstantTime it hands each present key the slot its primary line holds it
// in.
TEST(TestWarmConstantTime) {
  for (bool ct : {false, true}) {
    auto fp = newTest(1000, 1min);
    Filter& f = *fp;
    Core& c = core(f);
    c.ct = ct;
    std::vector<u64> hs(batchChunk);
    for (std::size_t i = 0; i < hs.size(); i++) {
      hs[i] = mix64(i);
      if (i % 2 == 0) f.TouchAt(hs[i], 0, 1000);
    }
    std::array<i64, batchChunk> at{};
    c.warm(hs, at);
    for (std::size_t i = 0; i < hs.size(); i++) {
      i64 want = -1;
      if (!ct && i % 2 == 0) want = c.find(c.primary(hs[i]), fingerprint(hs[i]) << 1, tag(hs[i]));
      if (at[i] != want) FATALF("ConstantTime %v key %d: warm handed slot %d, want %d", ct, i, at[i], want);
    }
  }
}
