// go/register.go: the velocity register (packing, advance, spill, the onset,
// the estimate) and the count of one event in a slot (bump).
#include "core.hpp"

namespace clepsydra::detail {

Register unpack(u64 w1) {
  Register r;
  r[0] = w1 & 0x1F;
  r[1] = (w1 >> 5) & 0x1F;
  r[2] = (w1 >> 10) & 0x3F;
  r[3] = (w1 >> 16) & 0x7F;
  r[4] = (w1 >> 23) & 0xFF;
  r[5] = (w1 >> 31) & 0xFF;
  r[6] = (w1 >> 39) & 0x1FF;
  return r;
}

u64 Register::pack() const {
  return c[0] | (c[1] << 5) | (c[2] << 10) | (c[3] << 16) | (c[4] << 23) | (c[5] << 31) | (c[6] << 39);
}

// add puts n events into bucket j, spilling into older buckets when it is
// full: the count is preserved and the event is aged at most one bucket.
bool Register::add(int j, u64 n) {
  bool overflow = n > ((u64{1} << bucketBits[j]) - 1 - c[j]);
  for (; j < nBuckets && n > 0; j++) {
    u64 room = (u64{1} << bucketBits[j]) - 1 - c[j];
    u64 take = std::min(room, n);
    c[j] += take;
    n -= take;
  }
  return overflow;
}

// advance re-buckets from unit u0 to unit u1 >= u0, reporting whether a merge
// overflowed its new bucket.
bool Register::advance(i64 u0, i64 u1) {
  if (u1 <= u0) return false;
  if (u1 - u0 > 2 * unitsPerHorizon) {
    c = {};
    return false;
  }
  std::array<i64, nBuckets> starts{};
  for (int j = 0; j < nBuckets; j++) starts[j] = bucketStart(u1, j);
  Register out;
  bool overflow = false;
  int j = 0;
  for (int k = 0; k < nBuckets; k++) {
    if (c[k] == 0) continue;
    i64 start = bucketStart(u0, k);
    while (j < nBuckets && start < starts[j]) j++;
    if (j == nBuckets) break;  // older buckets only start earlier: all dropped
    overflow = out.add(j, c[k]) || overflow;
  }
  *this = out;
  return overflow;
}

// onsetOf is where the tick now falls in its unit u, in sixteenths.
u64 Core::onsetOf(u64 now, i64 u) const { return static_cast<u64>(unitOf(16 * (now - static_cast<u64>(u) * unit))); }

// nextReg returns word1 after counting one event at now in old, the register
// of key tg last touched at last; the result is at the unit of max(now, last).
std::pair<u64, bool> Core::nextReg(u64 old, u64 tg, u64 last, u64 now, bool reset) const {
  return nextRegU(old, tg, last, now, unitOf(last), unitOf(now), reset);
}

// nextRegU is nextReg with last's and now's units, lastU and nowU.
std::pair<u64, bool> Core::nextRegU(u64 old, u64 tg, u64 last, u64 now, i64 lastU, i64 nowU, bool reset) const {
  if (!reset && (old >> tagShift) == tg && lastU == nowU) {
    // Fast path, same unit: increment the first bucket with room in place.
    u64 free = ~old & regMask;
    if (free == 0) return {old, true};  // every bucket full
    int b = std::countr_zero(free);
    u64 w1 = old + (u64{1} << bucketBase[b]);
    if (now < last) w1 = lowerOnset(w1, onsetOf(now, nowU));  // a caller behind the last touch
    return {w1, b >= static_cast<int>(bucketBits0)};
  }
  Register r;
  bool overflow = false;
  i64 e = std::max(nowU, lastU);
  bool keep = !reset && (old >> tagShift) == tg;
  if (keep) {
    r = unpack(old & regMask);
    if (nowU > lastU) overflow = r.advance(lastU, nowU);  // else a caller with a stale clock: count at e
  }
  u64 on = 0;  // an event from a unit before e: its bucket's onset says nothing
  if (nowU == e && (!keep || nowU > lastU)) on = onsetOf(now, nowU);  // the event starts the newest bucket
  overflow = r.add(0, 1) || overflow;
  return {(tg << tagShift) | (on << onsetShift) | r.pack(), overflow};
}

// bump counts one event at now in the register of the slot at word index i,
// which must still hold the caller's key (fpd, tg). It reports false if the
// slot has been handed to another key since the caller found it (claims A29).
bool Core::bump(i64 i, u64 fpd, u64 tg, u64 now, i64 nu, u64 last, i64 lu, bool reset, u64& w1, u64& ref,
                bool& overflow) {
  AU64& a0 = words[static_cast<std::size_t>(i)];
  AU64& a1 = words[static_cast<std::size_t>(i) + 1];
  for (;;) {
    u64 old = a1.load();
    u64 w0 = a0.load();
    if ((old >> tagShift) != tg || (w0 >> 47) != fpd) {
      w1 = ref = 0;
      overflow = false;
      return false;
    }
    ref = w0 & tsMask;
    if (isHot(old)) {
      u64 id = slotID(static_cast<u64>(i / wordsPerLine), w0, old);
      if (!reset) {
        if (testHookHotAdding) testHookHotAdding(i);
        if (hotAdd(old, id, now, &a1)) {
          w1 = old;
          overflow = false;
          return true;
        }
        if (a1.load() != old) continue;  // word1 changed (a demotion, or the slot changed hands): look again
      }
      // The entry aged out with the key's history (or the caller resets it):
      // start afresh, at the unit word0 has when the register replaces it.
      u64 nw, nref;
      if (unpoint(i, w0, old, fpd, tg, now, nu, nw, nref)) {
        if (reset) slotDrop(id, old);
        w1 = nw;
        ref = nref;
        overflow = false;
        return true;
      }
      reset = false;  // a word changed: a concurrent touch may have reset it; merge
      continue;
    }
    i64 refU = lu;
    if (ref == last) {
    } else if (ref == now) {  // the caller's own CAS
      refU = nu;
    } else {
      refU = unitOf(ref);
    }
    auto [nw, over] = nextRegU(old, tg, ref, now, refU, nu, reset);
    if (over) {
      u64 pw;
      if (promote(i, w0, old, tg, ref, now, pw)) {
        w1 = pw;
        overflow = false;
        return true;
      }
    }
    if (testHookBumping) testHookBumping(i);
    if (nw == old) {
      w1 = nw;
      overflow = over;
      return true;
    }
    if (cas(a1, old, nw)) {
      if (testHookBumped) testHookBumped(i);
      if ((a0.load() >> 47) != fpd && strayed(i, tg)) {
        w1 = ref = 0;
        overflow = false;
        return false;  // the count went to another key: count afresh
      }
      w1 = nw;
      overflow = over;
      return true;
    }
    reset = false;  // a concurrent touch already reset it; merge
  }
}

// unpoint replaces slot i's word1, old, a hot pointer whose entry is gone, by
// a fresh register holding the event at now (unit nu), at the unit of the
// last touch word0 has when it goes in: by one 16-byte CAS from w0 and old, or
// else under a hold of word1, word0 read under it.
bool Core::unpoint(i64 i, u64 w0, u64 old, u64 fpd, u64 tg, u64 now, i64 nu, u64& w1, u64& ref) {
  AU64* p = &words[static_cast<std::size_t>(i)];
  if (cpu::HasCAS2) {
    ref = w0 & tsMask;
    w1 = nextRegU(old, tg, ref, now, unitOf(ref), nu, true).first;
    return cpu::CAS2(p, w0, old, w0, w1);
  }
  MoveCount& m = moveOf(tg);
  m.begun.fetch_add(1);
  Defer done{[&m] { m.done.fetch_add(1); }};
  if (!cas(p[1], old, busy)) {
    w1 = ref = 0;
    return false;
  }
  if (w0 = p[0].load(); (w0 >> 47) != fpd) {
    p[1].store(old);
    w1 = ref = 0;
    return false;
  }
  ref = w0 & tsMask;
  w1 = nextRegU(old, tg, ref, now, unitOf(ref), nu, true).first;
  p[1].store(w1);
  return true;
}

// strayed reports whether bump's count may have gone to another key: word1
// tells once any hold of the slot is over (claims A29).
bool Core::strayed(i64 i, u64 tg) const {
  for (;;) {
    u64 w1 = words[static_cast<std::size_t>(i) + 1].load();
    if ((w1 >> tagShift) != tagBusy) return w1 != 0 && (w1 >> tagShift) == tg;  // 0: a free slot
    gosched();
  }
}

// mass is the event count of the key with identity id in buckets that still
// reach inside the horizon (for eviction scoring).
u64 Core::mass(u64 id, u64 w1, u64 last, u64 now) const {
  if (age(now, last) > horizon) return 0;
  if (isHot(w1)) {
    auto [v, ok] = viewOf(id, w1, last);
    if (!ok) return 0;
    return viewMass(v, now);
  }
  u64 reg = w1 & regMask;
  if (reg == 0) return 0;
  i64 e = unitOf(last);
  i64 oldest = static_cast<i64>(now - std::min(now, horizon)) / static_cast<i64>(unit);  // as viewMass
  if (e + 1 <= oldest) return 0;
  if ((reg >> bucketBits0) == 0) return reg;
  u64 n = reg & ((u64{1} << bucketBits0) - 1);
  i64 top = bucketStart(e, 0);
  for (int j = 1; j < nBuckets && top > oldest; j++) {
    n += (reg >> bucketShift[j]) & ((u64{1} << bucketBits[j]) - 1);
    top = bucketStart(e, j);
  }
  return n;
}

// viewMass is mass for a register state already read.
u64 Core::viewMass(const View& v, u64 now) const {
  i64 oldest = static_cast<i64>(now - std::min(now, horizon)) / static_cast<i64>(unit);
  u64 n = 0;
  i64 top = v.e + 1;
  for (int j = 0; j < nBuckets; j++) {
    if (top <= oldest) break;
    n += v.c[j];
    top = bucketStart(v.e, j);
  }
  return n;
}

// estimate interpolates the count in (now-window, now] straight from the
// buckets as laid out at the register's own unit (go/register.go has the
// full account). ok is false if w1 points to an entry no longer the key's.
std::pair<Result, bool> Core::estimate(u64 id, u64 w1, u64 last, u64 now, u64 window, bool sat) const {
  window = std::min(window, horizon);
  if (age(now, last) > window) return {Result{}, true};
  if (!isHot(w1)) {
    u64 reg = w1 & regMask;
    if ((reg >> bucketBits0) == 0) {
      u64 c0 = reg & ((u64{1} << bucketBits0) - 1);
      if (c0 == 1 && now == last && window > 0) {
        Result r;
        r.Count = 1;
        r.Upper = 1;
        return {r, true};
      }
      i64 e = unitOf(last);
      if (static_cast<i64>(now - window) <= e * static_cast<i64>(unit)) {
        i64 n = std::max<i64>(1, static_cast<i64>(c0));
        Result r;
        r.Count = n;
        r.Upper = n;
        r.Saturated = sat && c0 >= satPerUnit;
        return {r, true};
      }
    }
  }
  auto [v, ok] = viewOf(id, w1, last);
  if (!ok) return {Result{}, false};
  const i64 e = v.e;
  const double fu = static_cast<double>(unit);
  const double from = static_cast<double>(now) - static_cast<double>(window);
  double top = static_cast<double>(e + 1) * fu;
  bool newest = true;
  double est = 0.0;
  u64 upper = 0;
  bool hit = false;
  for (int j = 0; j < nBuckets; j++) {
    double lo = static_cast<double>(bucketStart(e, j)) * fu;
    u64 c = v.c[j];
    double hi = top;
    top = lo;
    if (c == 0) {
      if (from >= lo) break;
      continue;
    }
    hit = hit || c >= satPerUnit;
    upper += c;
    double a = 0.0;  // events known to lie inside the window, counted whole
    if (newest) {
      hi = std::max(lo, std::min(static_cast<double>(last), hi));
      newest = false;
      if (from < hi) a = 1;  // one of them lies exactly at last
    }
    if (from <= lo) {
      est += static_cast<double>(c);
      continue;
    }
    if (j == 0 && v.on != 0 && static_cast<double>(c) > a) {
      // The newest bucket's events lie from its onset to the last touch (hi),
      // not from the unit's start (claims D29).
      double on = lo + static_cast<double>(v.on) * fu / 16;
      if (from <= on) {
        est += static_cast<double>(c);
      } else if (hi > on) {
        est += a + (static_cast<double>(c) - a) * std::max(0.0, std::min(1.0, (hi - from) / (hi - on)));
      } else {
        est += a;
      }
      break;
    }
    if (from < hi) est += a + (static_cast<double>(c) - a) * (hi - from) / (hi - lo);
    break;
  }
  i64 n = std::max<i64>(1, static_cast<i64>(est + 0.5));
  Result r;
  r.Count = n;
  r.Upper = std::max(n, static_cast<i64>(upper));
  r.Saturated = sat && hit;
  if (v.hot) r.Saturated = v.sat != satNone && from < static_cast<double>(v.sat + 1) * fu;
  return {r, true};
}

// unitOf is t / unit via a precomputed reciprocal.
i64 Core::unitOf(u64 t) const {
  u64 q = mulHi(t, unitInv);
  if (q * unit > t) q--;
  return static_cast<i64>(q);
}

}  // namespace clepsydra::detail
