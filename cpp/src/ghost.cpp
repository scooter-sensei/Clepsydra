// go/ghost.go: the overflow ghost table: two candidate buckets, three floors,
// epochs (claims A11, A14, A34).
#include "core.hpp"

namespace clepsydra::detail {

// Ghost sizes the table to bytes (rounded down to whole buckets).
Ghost::Ghost(u64 bytes, u64 hz, int ch, int sb)
    : words(static_cast<std::size_t>(std::max<u64>(1, bytes / (ghWords * 8)) * ghWords)),
      nb(std::max<u64>(1, bytes / (ghWords * 8))),
      choices(std::max(1, std::min(ch, 2))),
      sub(std::max(1, std::min(sb, ghSubMax))),
      sliceLen((hz + 1) / 2),
      horizon(hz) {}

// slot: id's candidate buckets (first word index of each), how many are
// distinct, and its fingerprint (never 0).
void Ghost::slot(u64 id, std::array<i64, 2>& base, int& n, u64& fp) const {
  u64 x = mix64(id ^ 0x5bd1e9955bd1e995);
  u64 b = mulHi(x, nb);
  fp = x & ((u64{1} << ghFPBits) - 1);
  if (fp == 0) fp = 1;
  base[0] = base[1] = static_cast<i64>(b * ghWords);
  n = 1;
  if (choices > 1) {
    if (u64 b2 = mulHi(mix64(id ^ 0x2545f4914f6cdd1d), nb); b2 != b) {
      base[1] = static_cast<i64>(b2 * ghWords);
      n = 2;
    }
  }
}

// real decodes an entry's slice against epoch ep (newest slice + 1).
u64 Ghost::real(u64 e, u64 ep) const {
  u64 cur = ep - 1;
  return cur - ((cur - e) & 3);
}

// live: slice sl's events can be within window of now.
bool Ghost::live(u64 sl, u64 window, u64 now) const {
  u64 end = sl * sliceLen + sliceLen - 1;
  return age(now, end) <= std::min(window, horizon);
}

// at returns entry k (0..23) of the bucket at base; k 0 holds the floors.
AU64* Ghost::at(i64 base, int k, unsigned& sh) const {
  sh = static_cast<unsigned>(k % ghPerWord) * ghEntryBits;
  return &words[static_cast<std::size_t>(base + k / ghPerWord)];
}

// floorShift is the bit offset, in a bucket's first word, of the floor that
// covers fingerprint fp.
unsigned Ghost::floorShift(u64 fp) const { return static_cast<unsigned>(fp % static_cast<u64>(sub)) * ghSubBits; }

// floorOf is the live floor covering fp in the bucket at base, at epoch ep.
u64 Ghost::floorOf(i64 base, u64 fp, u64 ep, u64 now) const {
  u64 e = (words[static_cast<std::size_t>(base)].load() >> floorShift(fp)) & ghSubMask;
  if (e == 0 || !live(real(e, ep), horizon, now)) return 0;
  return ghCnt(e);
}

// add records a dropped history: n events, the newest at tick last.
void Ghost::add(u64 id, u64 n, u64 last, bool isSat, u64 now) {
  if (n == 0 || age(now, last) > horizon) return;
  // The epoch must reach the history's own slice, even when the caller's
  // clock is behind its newest event.
  u64 sl = last / sliceLen;
  if (u64 ns = std::max(now, last) / sliceLen; epoch.load() < ns + 1) advance(ns);
  if (isSat || n >= ghCntMax) {
    n = ghCntMax;
    isSat = true;
    sat.store(true);
  }
  if (u64 u = sl * sliceLen + sliceLen - 1 + horizon; until.load() < u) {
    for (;;) {
      u64 cur = until.load();
      if (cur >= u || cas(until, cur, u)) break;
    }
  }
  std::array<i64, 2> bases{};
  int nbk = 0;
  u64 fp = 0;
  slot(id, bases, nbk, fp);
  for (;;) {  // Go: outer
    // A write is committed only if ep is still the epoch.
    u64 ep = epoch.load();
    if (sl + 3 < ep) return;  // older than the live slices
    if (testHookGhostAdding) testHookGhostAdding();
    std::array<int, 2> freeK{-1, -1}, liveN{0, 0};
    int victim = -1, vb = 0;
    u64 vc = ghCntMax + 1, vsl = 0;
    bool restart = false;
    for (int bi = 0; bi < nbk && !restart; bi++) {
      i64 base = bases[bi];
      freeK[bi] = -1;
      for (int wi = 0; wi < ghWords && !restart; wi++) {
        AU64& w = words[static_cast<std::size_t>(base + wi)];
        u64 old = w.load();
        int k = wi * ghPerWord;
        unsigned sh = 0;
        if (wi == 0) {
          k = 1;
          sh = ghEntryBits;
        }
        if ((old >> sh) == 0) {
          if (freeK[bi] < 0) freeK[bi] = k;
          continue;
        }
        for (; sh < ghPerWord * ghEntryBits; k++, sh += ghEntryBits) {
          u64 e = (old >> sh) & ghEntryMask;
          if (e == 0 || !live(real(e, ep), horizon, now)) {
            if (freeK[bi] < 0) freeK[bi] = k;
            continue;
          }
          liveN[bi]++;
          if (ghFP(e) == fp) {
            u64 esl = std::max(real(e, ep), sl);
            u64 ne = ghEntry(fp, std::min(ghCntMax, ghCnt(e) + n), esl);
            if (epoch.load() == ep && cas(w, old, (old & ~(ghEntryMask << sh)) | (ne << sh))) return;
            restart = true;  // Go: continue outer
            break;
          }
          if (u64 c = ghCnt(e); c < vc || (c == vc && real(e, ep) < vsl)) {
            victim = k;
            vb = bi;
            vc = c;
            vsl = real(e, ep);
          }
        }
      }
    }
    if (restart) continue;
    int bi = -1;
    for (int i = 0; i < nbk; i++) {
      if (freeK[i] >= 0 && (bi < 0 || liveN[i] < liveN[bi])) bi = i;
    }
    if (bi >= 0) {
      unsigned sh = 0;
      AU64* w = at(bases[bi], freeK[bi], sh);
      u64 old = w->load();
      u64 e = (old >> sh) & ghEntryMask;
      if (e != 0 && live(real(e, ep), horizon, now)) continue;  // taken meanwhile
      if (epoch.load() == ep && cas(*w, old, (old & ~(ghEntryMask << sh)) | (ghEntry(fp, n, sl) << sh))) return;
      continue;
    }
    if (epoch.load() != ep) continue;
    // Full: the lightest history leaves (the newcomer if it is the lightest)
    // and its bucket's floor rises to cover it. The floor goes up first.
    if (n <= vc) {
      // The newcomer may go to either bucket's floor: the one already higher.
      int fb = 0;
      if (nbk > 1 && floorOf(bases[1], fp, ep, now) > floorOf(bases[0], fp, ep, now)) fb = 1;
      raiseFloor(bases[fb], fp, n, sl, now);
      return;
    }
    unsigned sh = 0;
    AU64* w = at(bases[vb], victim, sh);
    u64 old = w->load();
    u64 e = (old >> sh) & ghEntryMask;
    if (ghCnt(e) != vc || ghFP(e) == fp) continue;
    raiseFloor(bases[vb], ghFP(e), vc, real(e, ep), now);
    if (epoch.load() == ep && cas(*w, old, (old & ~(ghEntryMask << sh)) | (ghEntry(fp, n, sl) << sh))) return;
  }
}

// raiseFloor lifts the bucket's floor covering fingerprint fp to at least n
// events, live until slice sl's events expire.
void Ghost::raiseFloor(i64 base, u64 fp, u64 n, u64 sl, u64 now) {
  floored.fetch_add(1);
  AU64& w = words[static_cast<std::size_t>(base)];
  unsigned sh = floorShift(fp);
  for (;;) {
    u64 ep = epoch.load();
    if (testHookGhostFloor) testHookGhostFloor();
    u64 old = w.load();
    u64 e = (old >> sh) & ghSubMask;
    u64 c = 0, fsl = sl;
    if (e != 0 && live(real(e, ep), horizon, now)) {
      c = ghCnt(e);
      fsl = std::max(sl, real(e, ep));
    }
    u64 ne = ghEntry(0, std::max(c, n), fsl);
    if (ne == e || (epoch.load() == ep && cas(w, old, (old & ~(ghSubMask << sh)) | (ne << sh)))) return;
  }
}

// advance moves the epoch to slice ns, first clearing every entry that is
// dead by then.
void Ghost::advance(u64 ns) {
  std::lock_guard<std::mutex> lk(mu);
  u64 ep = epoch.load();
  if (ep >= ns + 1) return;
  if (ep != 0) {
    for (std::size_t i = 0; i < words.size(); i++) {
      for (;;) {
        u64 old = words[i].load();
        u64 nw = old;
        for (int k = 0; k < ghPerWord; k++) {
          unsigned sh = static_cast<unsigned>(k) * ghEntryBits;
          int n = 1;
          u64 mask = ghEntryMask;
          if (k == 0 && i % ghWords == 0) {  // the floors, each with its own slice
            n = ghSubMax;
            mask = ghSubMask;
          }
          for (int r = 0; r < n; r++) {
            if (u64 e = (old >> sh) & mask; e != 0 && real(e, ep) + 2 < ns) nw &= ~(mask << sh);
            sh += ghSubBits;
          }
        }
        if (nw == old || cas(words[i], old, nw)) break;
      }
    }
  }
  epoch.store(ns + 1);
}

// estimate is id's dropped events within window: matching entries count in
// Count and Upper, the floors in Upper only. The lines are decoded against
// the epoch they were loaded under (claims A34).
std::pair<Result, bool> Ghost::estimate(u64 id, u64 window, u64 now) const {
  std::array<i64, 2> bases{};
  int nbk = 0;
  u64 fp = 0;
  slot(id, bases, nbk, fp);
  std::array<std::array<u64, ghWords>, 2> line{};
  u64 ep = 0;
  for (;;) {
    ep = epoch.load();
    if (ep == 0 || now > until.load()) return {Result{}, false};
    bool empty = true;
    for (int bi = 0; bi < nbk; bi++) {
      for (int i = 0; i < ghWords; i++) {
        u64 w = words[static_cast<std::size_t>(bases[bi] + i)].load();
        line[bi][i] = w;
        if (w != 0) empty = false;
      }
    }
    if (empty) return {Result{}, false};
    if (testHookGhostRead) testHookGhostRead();
    if (epoch.load() == ep) break;
  }
  Result r;
  auto entry = [&](u64 e, bool counts) {
    if (e != 0 && ghFP(e) == fp && live(real(e, ep), window, now)) {
      i64 c = static_cast<i64>(ghCnt(e));
      r.Upper += c;
      if (counts) r.Count += c;
      r.Saturated = r.Saturated || static_cast<u64>(c) == ghCntMax;
    }
  };
  // raiseFloor counts a floor before it writes it, and floored is loaded
  // after the words: a load that holds a floor sees floored > 0.
  bool hasFloor = floored.load() > 0;
  for (int bi = 0; bi < nbk; bi++) {
    if (hasFloor) {
      if (u64 e = (line[bi][0] >> floorShift(fp)) & ghSubMask; e != 0 && live(real(e, ep), window, now)) {
        i64 c = static_cast<i64>(ghCnt(e));
        r.Upper += c;
        r.Saturated = r.Saturated || static_cast<u64>(c) == ghCntMax;
      }
    }
    if (u64 w0 = line[bi][0]; (w0 >> ghEntryBits) != 0) {
      entry((w0 >> ghEntryBits) & ghEntryMask, true);
      entry((w0 >> (2 * ghEntryBits)) & ghEntryMask, true);
    }
    for (int w = 1; w < ghWords; w++) {
      u64 lw = line[bi][w];
      if (lw == 0) continue;
      entry(lw & ghEntryMask, true);
      entry((lw >> ghEntryBits) & ghEntryMask, true);
      entry((lw >> (2 * ghEntryBits)) & ghEntryMask, true);
    }
  }
  if (r.Upper == 0) return {Result{}, false};
  r.Seen = r.Count > 0;
  return {r, true};
}

}  // namespace clepsydra::detail
