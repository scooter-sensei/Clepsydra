// go/merge.go: Merge, one quiescent filter's keys added to another (claims
// D24, D26, D29).
#include "core.hpp"

namespace clepsydra::detail {

namespace {
// sumRegisters re-buckets views a and b to the newer unit and adds them
// bucket by bucket: the merged register word for tag tg, and whether a bucket
// overflowed.
std::pair<u64, bool> sumRegisters(const View& a, const View& b, u64 tg) {
  i64 e = std::max(a.e, b.e);
  Register ra, rb;
  ra.c = a.c;
  rb.c = b.c;
  bool over = ra.advance(a.e, e);
  over = rb.advance(b.e, e) || over;
  for (int i = 0; i < nBuckets; i++) over = ra.add(i, rb[i]) || over;
  // Bucket 0 is the newer register's: its onset, or the earlier of two in
  // one unit (0 says nothing).
  u64 on = a.on;
  if (a.e == b.e) {
    on = std::min(a.on, b.on);
  } else if (b.e > a.e) {
    on = b.on;
  }
  return {(tg << tagShift) | (on << onsetShift) | ra.pack(), over};
}
}  // namespace

void Core::merge(Core& g) {
  if (this == &g) throw Error("temporalfilter: Merge with itself");
  if (lines != g.lines || horizon != g.horizon || unit != g.unit || res != g.res)
    throw Error("temporalfilter: Merge needs the same Config");
  if (ct || g.ct || bg || g.bg) throw Error("temporalfilter: Merge of ConstantTime or Background filters");
  if (hot.load() || g.hot.load()) throw Error("temporalfilter: Merge with hot entries in use");
  u64 now = 0;
  for (Core* x : {this, &g}) {
    for (std::size_t i = 0; i < x->nwords; i += 2) now = std::max(now, x->words[i].load() & tsMask);
  }
  for (std::size_t i = 0; i < g.nwords; i += 2) {
    u64 w0 = g.words[i].load(), w1 = g.words[i + 1].load();
    u64 last = w0 & tsMask;
    if (w0 == 0 || (w1 >> tagShift) == tagBusy || age(now, last) > g.horizon) continue;
    u64 id = g.slotID(static_cast<u64>(i / wordsPerLine), w0, w1);
    u64 line = id >> 28, fp = w0 >> 48, tg = w1 >> tagShift;
    u64 lo = div64(line >> 28, line << 36, lines);  // ceil(line * 2^36 / lines): primary(h) == line
    if (lo * lines < (line << 36)) lo++;
    u64 h = (fp << 48) | (tg << 36) | lo;
    if (primary(h) != line || fingerprint(h) != fp || tag(h) != tg)
      throw Error("temporalfilter: Merge: identity does not round-trip");
    View vg = g.viewOf(id, w1, last).first;
    std::unique_lock<PaddedMutex> lk(claimMu[h >> (64 - claimStripeBits)]);
    u64 fw0 = 0, fw1 = 0;
    if (i64 j = lookupStable(h, fp, tg, false, fw0, fw1); j >= 0) {
      View vf = viewOf(id, fw1, fw0 & tsMask).first;
      auto [nw1, over] = sumRegisters(vf, vg, tg);
      u64 sat = (fw0 | w0) & satBit;
      if (over) sat = satBit;
      words[static_cast<std::size_t>(j) + 1].store(nw1);
      words[static_cast<std::size_t>(j)].store((fw0 & ~(tsMask | satBit)) | sat | std::max(last, fw0 & tsMask));
    } else {
      // f may hold the key's evicted history in its stash: take it and sum it
      // in.
      u64 nw1 = w1, sat = w0 & satBit, st = last;
      Found s = recall(h, fp, tg, now, true);
      if (s.src == srcStash) {
        View vf = viewOf(id, s.w1, s.w0 & tsMask).first;
        bool over = false;
        std::tie(nw1, over) = sumRegisters(vf, vg, tg);
        sat = (s.w0 | w0) & satBit;
        if (over) sat = satBit;
        st = std::max(last, s.w0 & tsMask);
      }
      bool placed = false;
      for (int k = 0; k < 8; k++) {
        if (claim(h, fp, nw1, now, st, sat, claimFull) >= 0) {
          placed = true;
          break;
        }
      }
      if (!placed) {
        if (s.src == srcStash) putBack(stashID(h, fp, tg), s.w0, s.w1, now);  // as miss does for its retry
        throw Error("temporalfilter: Merge: a key found no slot in 8 tries (both filters must be quiescent)");
      }
    }
  }
}

}  // namespace clepsydra::detail
