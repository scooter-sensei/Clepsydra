// go/overflow.go: spill (a history the stash drops goes to the overflow table),
// the overflow table's allocation, and the count-min sketch the ghost table
// replaced as the default, which the reference keeps for the experiments that
// compare the two (Core::cms; no Config reaches it).
#include <algorithm>
#include <cmath>

#include "core.hpp"

namespace clepsydra::detail {

// ---- the count-min sketch ----
//
// A count-min sketch over (key identity, time slice): every dropped event is
// added to one counter per row. Collisions only add other keys' events, so
// the smallest of a key's counters is never below its true count (Upper), and
// no flood can erase a history before it ages out. Count is the
// count-mean-min estimate (Deng & Rafiei 2007). Slices are Horizon/parts
// long; a ring of parts+2 covers the horizon plus the slice being written.

Sketch::Sketch(u64 width, u64 hz, SketchShape sh)
    : w(std::max<u64>(width, 2)),
      rows(sh.rows),
      bits(static_cast<u64>(sh.bits)),
      per(static_cast<u64>(32 / sh.bits)),
      nsl(static_cast<u64>(sh.parts + 2)),
      cw((nsl + per - 1) / per),
      cmask((std::uint32_t{1} << (sh.bits - 1)) - 1),
      csat(std::uint32_t{1} << (sh.bits - 1)),
      sliceLen((hz + static_cast<u64>(sh.parts) - 1) / static_cast<u64>(sh.parts)),
      horizon(hz) {
  cells = std::vector<std::atomic<std::uint32_t>>(static_cast<std::size_t>(static_cast<u64>(rows) * w * cw));
}

// col is id's counter group in row r.
u64 Sketch::col(u64 id, int r) const {
  u64 c = mulHi(mix64(id + static_cast<u64>(r) * 0x9e3779b97f4a7c15), w);
  return (static_cast<u64>(r) * w + c) * cw;
}

// cell loads the counter of ring position p in the group at base.
std::uint32_t Sketch::cell(u64 base, u64 p) const {
  return (cells[static_cast<std::size_t>(base + p / per)].load() >> (bits * (p % per))) & (csat | cmask);
}

// add counts n events of id at tick t (sat: the count is a floor).
void Sketch::add(u64 id, u64 t, u64 n, bool sat, u64 now) {
  if (n == 0 || age(now, t) > horizon) return;
  u64 sl = t / sliceLen;
  u64 p = sl % nsl;
  if (u64 e = epochs[p].load(); e != sl + 1) {
    if (e > sl + 1) return;  // a newer slice took this place: t is past the horizon
    renew(p, sl);
  }
  unsigned sh = static_cast<unsigned>(bits * (p % per));
  std::uint32_t full = csat | cmask;
  for (int r = 0; r < rows; r++) {
    std::atomic<std::uint32_t>& wd = cells[static_cast<std::size_t>(col(id, r) + p / per)];
    for (;;) {
      std::uint32_t old = wd.load();
      std::uint32_t c = (old >> sh) & full;
      u64 v = static_cast<u64>(c & cmask) + n;
      std::uint32_t nc = static_cast<std::uint32_t>(std::min(v, static_cast<u64>(cmask))) | (c & csat);
      if (sat || v >= static_cast<u64>(cmask)) nc |= csat;
      if (cas(wd, old, (old & ~(full << sh)) | (nc << sh))) break;
    }
  }
  totals[p].fetch_add(n);
  squares[p].fetch_add(n * n);
  if (u64 u = sl * sliceLen + sliceLen - 1 + horizon; until.load() < u) {
    for (;;) {
      u64 cur = until.load();
      if (cur >= u || cas(until, cur, u)) break;
    }
  }
}

// renew hands ring position p to slice sl, clearing it. Readers skip p
// meanwhile.
void Sketch::renew(u64 p, u64 sl) {
  std::lock_guard<std::mutex> lk(mu);
  if (epochs[p].load() >= sl + 1) return;
  epochs[p].store(0);
  std::uint32_t keep = ~((csat | cmask) << (bits * (p % per)));
  for (u64 i = p / per; i < cells.size(); i += cw) cells[static_cast<std::size_t>(i)].fetch_and(keep);
  totals[p].store(0);
  squares[p].store(0);
  epochs[p].store(sl + 1);
}

// estimate is id's events in the window ending at now: Upper is the count-min
// bound, with the slice at the window's edge whole; Count the count-mean-min
// estimate where it clears the noise, with that slice pro rata; Seen if Count
// rounds to at least 1. ok is false if no slice in the window has a count.
// full reads every row whatever it finds (ConstantTime).
std::pair<Result, bool> Sketch::estimate(u64 id, u64 window, u64 now, bool full) const {
  u64 cur = now / sliceLen;
  u64 oldest = 0;
  if (cur >= nsl - 1) oldest = cur - (nsl - 2);
  std::array<std::array<u64, maxRows>, maxSlices> c{};
  std::array<bool, maxSlices> sat;
  sat.fill(true);
  for (int row = 0; row < rows; row++) {
    u64 base = col(id, row);
    bool any = false;
    for (u64 sl = oldest; sl <= cur; sl++) {
      u64 p = sl % nsl;
      std::uint32_t v = cell(base, p);
      c[p][static_cast<std::size_t>(row)] = static_cast<u64>(v & cmask);
      sat[p] = sat[p] && (v & csat) != 0;
      any = any || (v != 0 && epochs[p].load() == sl + 1);
    }
    if (!any && !full) return {Result{}, false};
  }
  Result r;
  i64 from = static_cast<i64>(now) - static_cast<i64>(window);
  double est = 0.0;
  for (u64 sl = oldest; sl <= cur; sl++) {
    u64 p = sl % nsl;
    i64 start = static_cast<i64>(sl * sliceLen), end = static_cast<i64>(std::min(sl * sliceLen + sliceLen - 1, now));
    if (epochs[p].load() != sl + 1 || end < from || age(now, static_cast<u64>(end)) > horizon) continue;
    u64 lo = *std::min_element(c[p].begin(), c[p].begin() + rows);
    if (lo == 0) continue;
    r.Upper += static_cast<i64>(lo);
    r.Saturated = r.Saturated || sat[p];
    // Count-mean-min: each row less the noise expected in its counter.
    double total = static_cast<double>(totals[p].load());
    std::array<double, maxRows> d{};
    for (int row = 0; row < rows; row++) {
      double v = static_cast<double>(c[p][static_cast<std::size_t>(row)]);
      d[static_cast<std::size_t>(row)] = v - (total - v) / static_cast<double>(w - 1);
    }
    std::sort(d.begin(), d.begin() + rows);
    double m = std::min((d[static_cast<std::size_t>((rows - 1) / 2)] + d[static_cast<std::size_t>(rows / 2)]) / 2,
                        static_cast<double>(lo));
    // Other keys' events arrive in lumps, so the noise's variance is the sum
    // of the lumps' squares over w.
    if (m < sigmas * std::sqrt(static_cast<double>(squares[p].load()) / static_cast<double>(w)))
      m = 0;  // within the noise: other keys' events could explain it
    if (start >= from) {
      est += m;
    } else {
      est += m * static_cast<double>(end - from + 1) / static_cast<double>(end - start + 1);
    }
  }
  if (r.Upper == 0) return {Result{}, false};
  r.Count = static_cast<i64>(std::min(std::max(est, 0.0), static_cast<double>(r.Upper)) + 0.5);
  r.Seen = r.Count > 0;
  return {r, true};
}

// spill sends a history the stash is dropping (identity id, slot words w0,
// w1) to the overflow table, or, without one, records it as lost. A hot
// history's entry is closed and its last state spilled (claims A32). A live
// history with no events in the horizon, or one whose hot entry is no longer
// its own, is neither spilled nor lost.
void Core::spill(u64 id, u64 w0, u64 w1, u64 now) {
  u64 last = w0 & tsMask;
  if (age(now, last) > horizon) return;
  View v;
  if (isHot(w1)) {
    bool ok;
    std::tie(v, ok) = hotClose(hotIdx(w1), id);
    if (!ok) return;
  } else {
    v = viewOf(id, w1, last).first;
  }
  u64 m = viewMass(v, now);
  if (m == 0) return;  // no events in the horizon yet: the touch that moved word0 counts afresh
  if (skW == 0) {
    dropped(std::max(now, last));  // its events may be in a window until a horizon after its newest one
    return;
  }
  if (spillMin > 1 && m < spillMin) {
    shed.fetch_add(1);
    return;
  }
  bool sat = ((w0 & satBit) != 0 && detail::hot(w1)) || (v.hot && v.sat != satNone);
  if (!cms) {
    folded.fetch_add(1);
    ghostTable()->add(id, m, last, sat, now);
    return;
  }
  Sketch* s = overflow();
  folded.fetch_add(1);
  i64 e = v.e;
  u64 top = static_cast<u64>(e + 1) * unit;
  for (int j = 0; j < nBuckets; j++) {
    i64 lo = bucketStart(e, j);
    u64 c = v.c[static_cast<std::size_t>(j)];
    // The bucket's newest tick (lastTouch for the newest): placing its events
    // as late as possible only widens the windows that see them.
    if (c > 0 && top > 0) s->add(id, std::min(top - 1, last), c, sat, now);
    if (lo <= 0) break;
    top = static_cast<u64>(lo) * unit;
  }
}

// overflow returns the count-min sketch, allocating it on the first spill.
Sketch* Core::overflow() {
  if (Sketch* s = sk.load()) return s;
  std::lock_guard<std::mutex> lk(skMu);
  if (Sketch* s = sk.load()) return s;
  ownSketch = std::make_unique<Sketch>(skW, horizon, skShape);
  sk.store(ownSketch.get());
  return ownSketch.get();
}

// ghostTable returns the overflow table, allocating it on the first spill,
// with the bytes the sketch would take.
Ghost* Core::ghostTable() {
  if (Ghost* g = gh.load()) return g;
  std::lock_guard<std::mutex> lk(skMu);
  if (Ghost* g = gh.load()) return g;
  ownGhost = std::make_unique<Ghost>(skBytes(), horizon, ghChoices, ghSub);
  gh.store(ownGhost.get());
  return ownGhost.get();
}

}  // namespace clepsydra::detail
