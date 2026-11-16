// go/hot.go: hot entries: promotion, the lock-free add, demotion pinned to
// word0's unit (demoteSlot), reclaim (claims A15, A31, A32, D29).
#include "core.hpp"

namespace clepsydra::detail {

u64 HotEntry::lock() {
  for (int spins = 0;; spins++) {
    u64 v = ver.load();
    if ((v & 1) == 0 && cas(ver, v, v + 1)) {
      c0.fetch_or(hotClosed);
      return v + 1;
    }
    if ((spins & 63) == 63) gosched();
  }
}

void HotEntry::unlock(u64 v) {
  u64 cv = c0.load();
  c0.store((((cv & ~hotClosed) + hotGen) & ~hotOnMask) | (cv & hotOnMask));  // the generation wraps inside its bits
  ver.store(v + 1);
}

// load reads the entry's state; the caller holds it or validates the version
// around the call.
View HotEntry::load() const {
  View v;
  v.e = epoch.load();
  v.sat = sat.load();
  v.hot = true;
  u64 x = c0.load();
  v.c[0] = x & hotCount;
  v.on = x >> hotOnShift;
  for (int j = 1; j < nBuckets; j++) v.c[j] = c[j - 1].load();
  return v;
}

// store writes v into a held entry. A count past 32 bits is kept as a floor.
void HotEntry::store(View& v) {
  for (int j = 0; j < nBuckets; j++) {
    if (v.c[j] > 0xFFFFFFFFu) {
      v.c[j] = 0xFFFFFFFFu;
      v.sat = std::max(v.sat, v.e);
    }
    if (j == 0) {
      c0.store((c0.load() & ~(hotCount | hotOnMask)) | v.c[0] | (v.on << hotOnShift));
    } else {
      c[j - 1].store(static_cast<std::uint32_t>(v.c[j]));
    }
  }
  epoch.store(v.e);
  sat.store(v.sat);
}

// read returns entry idx's state, and whether it belongs to id.
std::pair<View, bool> HotTable::read(u64 idx, u64 id) const {
  const HotEntry& x = e[static_cast<std::size_t>(idx)];
  for (int spins = 0;; spins++) {
    if (u64 v1 = x.ver.load(); (v1 & 1) == 0) {
      u64 owner = x.id.load();
      View v = x.load();
      if (x.ver.load() == v1) return {v, owner == id};
    }
    if ((spins & 63) == 63) gosched();
  }
}

// add counts n events at now (unit u) into entry idx if it belongs to id. If
// slot is the word1 that points here (old), a touch that starts a new unit
// with every bucket at most half the register's width hands the counts back
// to the slot as a register and frees the entry (demote).
bool HotTable::add(Core& f, u64 idx, u64 id, u64 n, i64 u, u64 now, AU64* slot, u64 old) {
  HotEntry& x = e[static_cast<std::size_t>(idx)];
  for (;;) {  // the usual case, a touch in the entry's own unit: one CAS
    u64 c = x.c0.load();
    if ((c & hotClosed) != 0 || x.id.load() != id || x.epoch.load() != u || (c & hotCount) + n > hotCount) break;
    if (cas(x.c0, c, c + n)) return true;
  }
  u64 ver = x.lock();
  if (x.id.load() != id) {
    x.unlock(ver);
    return false;
  }
  if (u == x.epoch.load()) {
    if (u64 c = (x.c0.load() & hotCount) + n; c <= hotCount) {
      x.c0.store((x.c0.load() & ~hotCount) | c);
      x.unlock(ver);
      return true;
    }
  }
  View v = x.load();
  if (u > v.e) {
    advanceWide(v.c, v.e, u);
    v.e = u;
    v.on = f.onsetOf(now, u);  // these events start bucket 0
    v.c[0] += n;
  } else if (u == v.e) {
    v.c[0] += n;
    v.on = std::min(v.on, f.onsetOf(now, u));  // a caller behind the onset
  } else {
    std::array<u64, nBuckets> y{};
    y[0] = n;
    advanceWide(y, u, v.e);
    for (int j = 0; j < nBuckets; j++) v.c[j] += y[j];
  }
  if (slot && v.e == u && demotable(v, u)) {
    Register r;
    for (int j = 0; j < nBuckets; j++) r[j] = v.c[j];
    u64 nw = (old & ~((u64{1} << tagShift) - 1)) | (v.on << onsetShift) | r.pack();
    if (f.demoteSlot(slot, old, nw, u)) {
      x.id.store(0);
      x.unlock(ver);
      putFree(idx);
      demoted.fetch_add(1);
      return true;
    }
  }
  x.store(v);
  x.unlock(ver);
  return true;
}

// demoteSlot is a demotion's swap of word1 (slot) from the hot pointer p to
// the register nw, at unit u, taken only while word0's last touch is in u.
bool Core::demoteSlot(AU64* slot, u64 p, u64 nw, i64 u) {
  AU64* p0 = slot - 1;
  if (cpu::HasCAS2) {
    u64 w0 = p0->load();
    return unitOf(w0 & tsMask) == u && cpu::CAS2(p0, w0, p, w0, nw);
  }
  MoveCount& m = moveOf(p >> tagShift);
  m.begun.fetch_add(1);
  Defer done{[&m] { m.done.fetch_add(1); }};
  if (!cas(*slot, p, busy)) return false;
  if (unitOf(p0->load() & tsMask) != u) {
    slot->store(p);
    return false;
  }
  slot->store(nw);
  return true;
}

bool demotable(const View& v, i64 u) {
  if (v.sat != satNone && v.sat + 2 * unitsPerHorizon >= u) return false;
  for (int j = 0; j < nBuckets; j++) {
    if (v.c[j] > ((u64{1} << bucketBits[j]) - 1) / 2) return false;
  }
  return true;
}

// putFree puts a closed entry (its owner 0) on the free list.
void HotTable::putFree(u64 idx) {
  inUse.fetch_sub(1);
  std::lock_guard<std::mutex> lk(mu);
  free.push_back(static_cast<std::uint32_t>(idx));
  full.store(satNone);
}

HotTable* Core::hotTable() {
  if (HotTable* t = hot.load()) return t;
  std::lock_guard<std::mutex> lk(skMu);
  if (HotTable* t = hot.load()) return t;
  ownHot = std::make_unique<HotTable>(static_cast<std::size_t>(hotN + 1));
  hot.store(ownHot.get());
  return ownHot.get();
}

// hotAlloc takes a free entry for id, holding v, or returns 0 if none is free.
u64 Core::hotAlloc(u64 id, View& v, u64 now) {
  HotTable* t = hotTable();
  i64 u = unitOf(now);
  if (t->full.load() == u) {  // found none in this unit already: don't search again
    hotRefused.fetch_add(1);
    return 0;
  }
  u64 idx = 0;
  {
    std::lock_guard<std::mutex> lk(t->mu);
    if (!t->free.empty()) {
      idx = t->free.back();
      t->free.pop_back();
    } else if (t->next < t->e.size()) {
      idx = t->next++;
    } else {
      idx = t->reclaim(u);
    }
    if (idx == 0) t->full.store(u);
  }
  if (idx == 0) {
    hotRefused.fetch_add(1);
    return 0;
  }
  HotEntry& x = t->e[static_cast<std::size_t>(idx)];
  u64 ver = x.lock();
  x.id.store(id);
  x.store(v);
  x.unlock(ver);
  t->inUse.fetch_add(1);
  return idx;
}

// reclaim takes an entry whose newest event is past the horizon at unit u
// (caller holds mu).
u64 HotTable::reclaim(i64 u) {
  i64 dead = u - unitsPerHorizon - 2;
  u64 n = e.size() - 1;
  for (u64 k = 0; k < std::min<u64>(n, 256); k++) {
    hand = hand % n + 1;
    HotEntry& x = e[static_cast<std::size_t>(hand)];
    if (x.id.load() == 0 || x.epoch.load() >= dead) continue;  // free ones are on the free list
    u64 ver = x.lock();
    bool ok = x.id.load() != 0 && x.epoch.load() < dead;
    if (ok) x.id.store(0);
    x.unlock(ver);
    if (ok) {
      inUse.fetch_sub(1);
      return hand;
    }
  }
  return 0;
}

// hotClose gives up entry idx if it belongs to id and returns its last state.
std::pair<View, bool> Core::hotClose(u64 idx, u64 id) {
  HotTable* t = hot.load();
  if (!t) return {View{}, false};
  HotEntry& x = t->e[static_cast<std::size_t>(idx)];
  View v;
  bool ok = false;
  u64 ver = x.lock();
  if (x.id.load() == id) {
    v = x.load();
    ok = true;
    x.id.store(0);
  }
  x.unlock(ver);
  if (ok) t->putFree(idx);
  return {v, ok};
}

// promote moves the register of the key in slot i to a hot entry with this
// touch counted, published under a hold (claims A29).
bool Core::promote(i64 i, u64 w0, u64 old, u64 tg, u64 last, u64 now, u64& w1) {
  u64 idx = 0, id = 0;
  w1 = hotEntryFor(i, w0, old, tg, last, now, idx, id);
  if (w1 == 0) return false;
  AU64* w = &words[static_cast<std::size_t>(i)];
  MoveCount& m = moveOf(tg);
  m.begun.fetch_add(1);
  Defer done{[&m] { m.done.fetch_add(1); }};
  if (!cas(w[1], old, busy)) {
    hotClose(idx, id);  // never published
    w1 = 0;
    return false;
  }
  if ((w[0].load() >> 47) != (w0 >> 47)) {
    w[1].store(old);
    hotClose(idx, id);
    w1 = 0;
    return false;
  }
  w[1].store(w1);
  promoted.fetch_add(1);
  return true;
}

// promoteHeld is promote for a touch that holds slot i's word1 busy (realign),
// which publishes w1 itself.
bool Core::promoteHeld(i64 i, u64 w0, u64 old, u64 tg, u64 last, u64 now, u64& w1) {
  u64 idx = 0, id = 0;
  if (w1 = hotEntryFor(i, w0, old, tg, last, now, idx, id); w1 == 0) return false;
  promoted.fetch_add(1);
  return true;
}

// hotEntryFor takes a free hot entry for the key in slot i holding old's
// counts and one event at now: word1 pointing to it (0 if none is free), its
// index and identity.
u64 Core::hotEntryFor(i64 i, u64 w0, u64 old, u64 tg, u64 last, u64 now, u64& idx, u64& id) {
  idx = id = 0;
  if (hotN == 0) return 0;
  if (HotTable* t = hot.load(); t && t->full.load() == unitOf(now)) {
    hotRefused.fetch_add(1);  // every entry was in use a moment ago
    return 0;
  }
  id = slotID(static_cast<u64>(i / wordsPerLine), w0, old);
  View v;
  v.e = unitOf(last);
  v.on = (old >> onsetShift) & onsetMask;
  v.sat = satNone;
  v.hot = true;
  Register r = unpack(old & regMask);
  for (int j = 0; j < nBuckets; j++) v.c[j] = r[j];
  if ((w0 & satBit) != 0) v.sat = v.e;  // it overflowed before: its counts up to now are floors
  if (i64 u = unitOf(now); u > v.e) {
    advanceWide(v.c, v.e, u);
    v.e = u;
    v.on = onsetOf(now, u);  // this touch starts bucket 0
  } else if (u == v.e) {
    v.on = std::min(v.on, onsetOf(now, u));
  } else {
    v.on = 0;  // an event from an earlier unit, counted at v.e
  }
  v.c[0]++;
  if (idx = hotAlloc(id, v, now); idx == 0) {
    id = 0;
    return 0;
  }
  return (tg << tagShift) | (idx << hotIdxShift);
}

// viewOf returns the register state that word1 holds or points to, for the
// key with identity id and lastTouch last; false if w1 points to an entry no
// longer the key's.
std::pair<View, bool> Core::viewOf(u64 id, u64 w1, u64 last) const {
  if (isHot(w1)) {
    if (HotTable* t = hot.load()) return t->read(hotIdx(w1), id);
    return {View{}, false};
  }
  View v;
  v.e = unitOf(last);
  v.on = (w1 >> onsetShift) & onsetMask;
  v.c[0] = w1 & 0x1F;
  v.c[1] = (w1 >> 5) & 0x1F;
  v.c[2] = (w1 >> 10) & 0x3F;
  v.c[3] = (w1 >> 16) & 0x7F;
  v.c[4] = (w1 >> 23) & 0xFF;
  v.c[5] = (w1 >> 31) & 0xFF;
  v.c[6] = (w1 >> 39) & 0x1FF;
  v.sat = satNone;
  return {v, true};
}

// hotGone: w1 points to a hot entry that is no longer the key id's (claims
// A25).
bool Core::hotGone(u64 id, u64 w1) const {
  if (!isHot(w1)) return false;
  return !viewOf(id, w1, 0).second;
}

// slotDrop gives up the hot entry, if any, of a slot's history that the slot
// itself discards past the horizon (claims A40).
void Core::slotDrop(u64 id, u64 w1) {
  if (isHot(w1)) hotClose(hotIdx(w1), id);
}

// hotAdd counts one event at now into the entry word1 points to; slot, if not
// nullptr, is the word1 the touch found the key by.
bool Core::hotAdd(u64 w1, u64 id, u64 now, AU64* slot) {
  HotTable* t = hot.load();
  return t && t->add(*this, hotIdx(w1), id, 1, unitOf(now), now, slot, w1);
}

void advanceWide(std::array<u64, nBuckets>& r, i64 u0, i64 u1) {
  if (u1 <= u0) return;
  if (u1 - u0 > 2 * unitsPerHorizon) {
    r = {};
    return;
  }
  std::array<i64, nBuckets> starts{};
  for (int j = 0; j < nBuckets; j++) starts[j] = bucketStart(u1, j);
  std::array<u64, nBuckets> out{};
  int j = 0;
  for (int k = 0; k < nBuckets; k++) {
    if (r[k] == 0) continue;
    i64 start = bucketStart(u0, k);
    while (j < nBuckets && start < starts[j]) j++;
    if (j == nBuckets) break;  // older buckets only start earlier: all dropped
    out[j] += r[k];
  }
  r = out;
}

}  // namespace clepsydra::detail
