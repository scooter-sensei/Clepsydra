// go/batch.go: the batches: prefetched chunks of keys, each touched from the
// slot found (claims S48).
#include "core.hpp"

namespace clepsydra::detail {

namespace {
void need(std::size_t have, std::size_t want, const char* what) {
  if (have < want) throw std::out_of_range(std::string("temporalfilter: ") + what + " shorter than hs");
}
}  // namespace

void Core::checkAndTouchBatch(std::span<const u64> hs, u64 window, u64 now, std::span<bool> out) {
  need(out.size(), hs.size(), "out");
  std::array<i64, batchChunk> at{};
  for (std::size_t lo = 0; lo < hs.size(); lo += batchChunk) {
    std::size_t hi = std::min(lo + batchChunk, hs.size());
    warm(hs.subspan(lo, hi - lo), at);
    for (std::size_t i = lo; i < hi; i++) out[i] = touch(hs[i], window, now, false, at[i - lo]).Seen;
  }
}

void Core::touchAndCountBatch(std::span<const u64> hs, u64 window, u64 now, std::span<bool> seen,
                              std::span<i64> counts) {
  need(seen.size(), hs.size(), "seen");
  need(counts.size(), hs.size(), "counts");
  std::array<i64, batchChunk> at{};
  for (std::size_t lo = 0; lo < hs.size(); lo += batchChunk) {
    std::size_t hi = std::min(lo + batchChunk, hs.size());
    warm(hs.subspan(lo, hi - lo), at);
    for (std::size_t i = lo; i < hi; i++) {
      Result r = touch(hs[i], window, now, true, at[i - lo]);
      seen[i] = r.Seen;
      counts[i] = r.Count;
    }
  }
}

void Core::velocityCountBatch(std::span<const u64> hs, u64 window, u64 now, std::span<i64> counts) {
  need(counts.size(), hs.size(), "counts");
  std::array<i64, batchChunk> at{};
  for (std::size_t lo = 0; lo < hs.size(); lo += batchChunk) {
    std::size_t hi = std::min(lo + batchChunk, hs.size());
    warm(hs.subspan(lo, hi - lo), at);
    for (std::size_t i = lo; i < hi; i++) counts[i] = count(hs[i], window, now, at[i - lo]).Count;
  }
}

// warm overlaps the cache misses of a chunk's keys: it prefetches every key's
// primary line, looks for each key there (at[k]: its slot, or -1), then
// prefetches the alternate lines of the keys it didn't find. In ConstantTime
// it looks nothing up and prefetches both lines of every key.
void Core::warm(std::span<const u64> hs, std::array<i64, batchChunk>& at) {
  std::array<u64, 2 * batchChunk> lns{};
  const void* w = words.data();
  const int n = static_cast<int>(hs.size());
  if (ct) {
    for (int k = 0; k < n; k++) {
      u64 p = primary(hs[k]);
      lns[2 * k] = p;
      lns[2 * k + 1] = alt(p, fingerprint(hs[k]));
      at[k] = -1;
    }
    cpu::PrefetchLines(w, lns.data(), 2 * n);
    return;
  }
  for (int k = 0; k < n; k++) lns[k] = primary(hs[k]);
  cpu::PrefetchLines(w, lns.data(), n);
  int m = 0;
  for (int k = 0; k < n; k++) {
    u64 p = lns[k], fp = fingerprint(hs[k]);
    if (at[k] = find(p, fp << 1, tag(hs[k])); at[k] < 0) lns[m++] = alt(p, fp);  // m <= k
  }
  cpu::PrefetchLines(w, lns.data(), m);
}

}  // namespace clepsydra::detail
