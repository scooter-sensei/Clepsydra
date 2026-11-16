// go/buzz.go and go/touchif.go: conditional touches (TouchIfUnder) and buzz
// entries, an exact log of a key's admitted conditional touches given on a
// refusal in doubt, for limits of 16 or less (claims A45, A46).
#include "core.hpp"

namespace clepsydra::detail {

std::array<PaddedMutex, 64>& condMu() {
  static std::array<PaddedMutex, 64> m;
  return m;
}

// stepAt is the step index of the step at ring position k, decoded against
// rq, the step of a tick no event of the log is after; 0 if it would be
// before tick 0.
u64 BuzzEntry::stepAt(int k, u64 rq) const {
  u64 d = (rq - step(k)) & 0xFFFF;
  if (d > rq) return 0;
  return rq - d;
}

// count is the log's Upper for [now-window, now]: an event counts unless its
// step ends before the window starts.
int BuzzEntry::count(u64 now, u64 window, u64 q) const {
  u64 rq = std::max(now, w0 & tsMask) / q;
  u64 fq = (now - std::min(now, window)) / q;
  int c = 0, hd = head();
  for (int k = 0; k < n(); k++) {
    if (stepAt((hd - 1 - k) & 15, rq) >= fq) c++;
  }
  return c;
}

// covers: every event the log may miss is outside [now-window, now].
bool BuzzEntry::covers(u64 now, u64 window) const { return now >= window && before() < now - window; }

// log records an admitted event at tick t. Events a horizon old leave the
// ring first; a full ring drops its oldest step.
void BuzzEntry::log(u64 t, u64 q, u64 horizon) {
  u64 bf = before();
  int cnt = n(), hd = head();
  u64 rq = std::max(t, w0 & tsMask) / q;
  if (t >= horizon + q) {  // else no step can be a horizon old
    for (u64 fq = (t - horizon) / q; cnt > 0; cnt--) {
      if (stepAt((hd - cnt) & 15, rq) >= fq) break;
    }
  }
  if (cnt == buzzCap) {
    bf = std::max(bf, (stepAt(hd, rq) + 1) * q - 1) & tsMask;
  } else {
    cnt++;
  }
  unsigned sh = 16 * static_cast<unsigned>(hd % 4);
  ev[hd / 4] = (ev[hd / 4] & ~(u64{0xFFFF} << sh)) | (((t / q) & 0xFFFF) << sh);
  meta = bf | (static_cast<u64>(cnt) << buzzNShift) | (static_cast<u64>((hd + 1) & 15) << buzzHShift) |
         (units() << buzzUShift);
}

BuzzTable::BuzzTable(u64 blocks, u64 qq)
    : b(static_cast<std::size_t>(blocks)), used(static_cast<std::size_t>((blocks + 63) / 64)), q(qq) {}

// block returns the index of the block of the key with hash h and identity
// id, and the lock that guards it, or nullptr (condMu's stripe guards it).
std::pair<u64, PaddedMutex*> BuzzTable::block(u64 h, u64 id) {
  u64 n = b.size();
  u64 x = mix64(id ^ 0x9e3779b97f4a7c15);
  if (n < buzzStripes) {
    u64 k = mulHi(x, n);
    return {k, &mu[k]};
  }
  u64 s = h >> (64 - 6);
  u64 lo = s * n / buzzStripes;
  u64 k = mulHi(x, (s + 1) * n / buzzStripes - lo);
  return {lo + k, nullptr};
}

// leave frees way i of block k, clearing the block's bit if it was the last.
void BuzzTable::leave(u64 k, int i) {
  BuzzBlock& blk = b[static_cast<std::size_t>(k)];
  blk.ids[i] = 0;
  for (u64 x : blk.ids)
    if (x != 0) return;
  used[static_cast<std::size_t>(k >> 6)].fetch_and(~(u64{1} << (k & 63)));
}

// mark sets block k's bit from its owners (a restore).
void BuzzTable::mark(u64 k) {
  for (u64 x : b[static_cast<std::size_t>(k)].ids) {
    if (x != 0) {
      used[static_cast<std::size_t>(k >> 6)].fetch_or(u64{1} << (k & 63));
      return;
    }
  }
}

// buzzTable returns the table, allocated on first use: one entry per 128
// slots.
BuzzTable* Core::buzzTable() {
  if (BuzzTable* t = buzz.load()) return t;
  std::lock_guard<std::mutex> lk(skMu);
  if (BuzzTable* t = buzz.load()) return t;
  u64 blocks = std::max<u64>(2, lines * slotsPerLine / (128 * buzzWays));
  ownBuzz = std::make_unique<BuzzTable>(blocks, std::max<u64>(1, (horizon + (u64{1} << 15) - 1) >> 15));
  buzz.store(ownBuzz.get());
  return ownBuzz.get();
}

// certain is the number of events of the register w1 that lie inside
// [now-window, now] wherever they fall in their buckets.
i64 Core::certain(u64 w1, u64 last, u64 now, u64 window) const {
  if (age(now, last) > window) return 0;
  View v = viewOf(0, w1, last).first;
  double fu = static_cast<double>(unit);
  double from = static_cast<double>(now) - static_cast<double>(window);
  i64 n = 0;
  for (int j = 0; j < nBuckets; j++) {
    if (from > static_cast<double>(bucketStart(v.e, j)) * fu) break;
    n += static_cast<i64>(v.c[j]);
  }
  return std::max<i64>(1, n);
}

// touchIfBuzz is TouchIfUnderAt for a limit of buzzCap or less, outside
// ConstantTime (the caller holds h's conditional stripe).
std::pair<Result, bool> Core::touchIfBuzz(u64 h, u64 window, u64 limit, u64 now) {
  u64 fp = fingerprint(h), tg = tag(h);
  u64 id = stashID(h, fp, tg);
  BuzzTable* t = buzz.load();
  u64 k = 0;
  PaddedMutex* mu = nullptr;
  bool held = false;  // h's block may hold an entry
  if (t) {
    std::tie(k, mu) = t->block(h, id);
    if (held = t->mayHold(k); held && mu == nullptr) {
      u64 line = 0;
      cpu::PrefetchLines(t->b[static_cast<std::size_t>(k)].ids.data(), &line, 1);
    }
  }
  Found f = read(h, now, -1);
  u64 w0 = f.w0, w1 = f.w1;
  u64 last = w0 & tsMask;
  bool live = f.src != 0 && age(now, last) <= horizon;
  Result r;
  if (live) {
    r = estimateAt(h, w1, last, now, window, (w0 & satBit) != 0);
    r.Seen = r.Count > 0;
  } else {
    r.Degraded = lostRecently(now);
  }
  i64 over = r.Upper;
  if (sketchLive(now)) addSketch(r, h, window, now);
  over = r.Upper - over;
  bool refuse =
      r.Saturated || (skW == 0 && r.Degraded && !r.Seen) || static_cast<u64>(std::max<i64>(r.Upper, 0)) >= limit;
  // A register the two words answer for: no hot entry, not saturated, not
  // full in every bucket.
  bool plain = live && !isHot(w1) && (w0 & satBit) == 0 && (~w1 & regMask) != 0;
  u64 win = std::min(window, horizon);
  if (!held) {  // no entry: as without entries, unless the refusal is in doubt
    if (!refuse) return {touch(h, window, now, true, -1), true};
    if (!uncertain(plain, r, w1, last, now, win, over, limit)) return {r, false};
    if (!t) {
      t = buzzTable();
      std::tie(k, mu) = t->block(h, id);
    }
  }
  std::unique_lock<PaddedMutex> lk;
  if (mu) lk = std::unique_lock<PaddedMutex>(*mu);
  BuzzBlock& b = t->b[static_cast<std::size_t>(k)];
  int way = -1;
  if (held) {
    for (int i = 0; i < buzzWays; i++) {
      if (b.ids[i] == id) {
        way = i;
        break;
      }
    }
  }
  BuzzEntry* e = nullptr;
  if (way >= 0) {
    if (e = &b.e[way]; !(plain && ((w0 ^ e->w0) & ~dispBit) == 0 && w1 == e->w1)) {
      t->leave(k, way);  // something else counted for the key, or its slot is gone
      e = nullptr;
      t->dropped.fetch_add(1);
    }
  }
  if (e && e->covers(now, win)) {
    r.Upper = e->count(now, win, t->q) + over;
    r.Count = std::min(r.Count, r.Upper);
    refuse = r.Saturated || static_cast<u64>(r.Upper) >= limit;
    t->decided.fetch_add(1);
  }
  if (refuse) {
    if (!e && uncertain(plain, r, w1, last, now, win, over, limit)) {
      for (int i = 0; i < buzzWays; i++) {
        if (b.ids[i] == 0 || age(now, b.e[i].w0 & tsMask) > b.e[i].units() * unit) {
          u64 units = std::min<u64>(31, (win + unit - 1) / unit + 8);
          t->used[static_cast<std::size_t>(k >> 6)].fetch_or(u64{1} << (k & 63));
          b.ids[i] = id;
          b.e[i] = BuzzEntry{w0, w1, std::max(now, last) | (units << buzzUShift), {}};
          t->given.fetch_add(1);
          break;
        }
      }
    }
    return {r, false};
  }
  u64 want = 0;
  bool ovf = false;
  if (e) std::tie(want, ovf) = nextReg(w1, tg, last, now, false);  // what the touch below makes of the words read
  Result got = touch(h, window, now, true, -1);
  if (e) {
    if (ovf) {  // the touch saturates or promotes the key: its words no longer answer for it
      t->leave(k, way);
      t->dropped.fetch_add(1);
    } else {
      u64 st = std::max(now, last);
      e->log(st, t->q, horizon);
      e->w0 = (w0 & ~tsMask) | st;
      e->w1 = want;  // checked at the key's next conditional touch
    }
  }
  return {got, true};
}

// uncertain: a refusal of a key whose words answer for it at a limit of
// buzzCap or less, though the events certainly inside the window, with the
// overflow table's part (over), are under the limit.
bool Core::uncertain(bool plain, const Result& r, u64 w1, u64 last, u64 now, u64 win, i64 over, u64 limit) const {
  return plain && !r.Saturated && static_cast<u64>(certain(w1, last, now, win) + over) < limit;
}

// touchIfUnderAt is TouchIfUnderAt: conditional touches of one key are
// serialized by one of 64 stripes, across filters (claims A45).
std::pair<Result, bool> Core::touchIfUnderAt(u64 h, u64 window, u64 limit, u64 now) {
  std::lock_guard<PaddedMutex> lk(condMu()[h >> (64 - 6)]);
  if (limit <= static_cast<u64>(buzzCap) && !ct && !buzzOff) return touchIfBuzz(h, window, limit, now);
  Result r = count(h, window, now, -1);
  if (r.Saturated || (skW == 0 && r.Degraded && !r.Seen) || static_cast<u64>(std::max<i64>(r.Upper, 0)) >= limit)
    return {r, false};
  return {touch(h, window, now, true, -1), true};
}

}  // namespace clepsydra::detail
