// go/stash.go: the eviction stash and the wait room (put, wait, recall) and
// the Background placer.
#include "core.hpp"

namespace clepsydra::detail {

StashTable::StashTable(u64 sets, PaddedMutex* mus)
    : e(static_cast<std::size_t>(sets * stashWays)), wait(static_cast<std::size_t>((sets + 63) / 64)), n(sets),
      mu(mus) {}

// set returns id's set (stashWays entries) and its lock.
std::pair<StashEntry*, PaddedMutex*> StashTable::set(u64 id) const {
  u64 i = mulHi(mix64(id), n);
  return {&e[static_cast<std::size_t>(i * stashWays)], &mu[i % stashStripes]};
}

// waiting: id's set may hold a key waiting for a slot.
bool StashTable::waiting(u64 id) const {
  u64 i = mulHi(mix64(id), n);
  return (wait[static_cast<std::size_t>(i / 64)].load() & (u64{1} << (i % 64))) != 0;
}

// recall finds h's evicted history in the stash, or (Background) h waiting in
// the wait room, removed if take. The caller holds h's claim stripe.
Found Core::recall(u64 h, u64 fp, u64 tg, u64 now, bool take) {
  u64 id = stashID(h, fp, tg);
  if (!ct) {
    if (StashTable* p = room.load(); p && p->waiting(id)) {
      MoveCount* mv = take ? &moveOf(tg) : nullptr;
      u64 w0, w1;
      bool retry;
      bool ok = roomGet(p, id, now, mv, w0, w1, retry);
      if (retry) return Found{0, 0, srcRetry};
      if (ok && take) return Found{w0, w1, srcRoom};
      if (ok) return Found{w0, w1, srcStash};
    }
    if (now > stashUntil.load()) return Found{};
  }
  u64 w0, w1;
  if (stashGet(id, now, take, w0, w1)) return Found{w0, w1, srcStash};
  return Found{};
}

// stashMay: h's history may be outside the table at now.
bool Core::stashMay(u64 h, u64 fp, u64 tg, u64 now) const {
  if (now <= stashUntil.load()) return true;
  StashTable* p = room.load();
  return p && p->waiting(stashID(h, fp, tg));
}

// settled: no move of the stripe whose counts are m overlapped a lookup that
// read them as b and d before its table scan and its recall.
bool Core::settled(MoveCount& m, u64 b, u64 d) const {
  return room.load() == nullptr || (b == d && m.begun.load() == b);
}

// roomGet is tableGet in the wait room, where an entry being committed to the
// table (w1 busy) is waited out; retry then tells the caller to look in the
// table again. With mv it takes the entry out, its move begun first.
bool Core::roomGet(StashTable* p, u64 id, u64 now, MoveCount* mv, u64& w0, u64& w1, bool& retry) {
  const bool take = mv != nullptr;
  auto [set, mu] = p->set(id);
  retry = false;
  for (;;) {
    bool busyNow = false;
    mu->lock();
    for (u64 i = 0; i < stashWays; i++) {
      StashEntry* e = &set[i];
      if (e->id != id) continue;
      if ((e->w1 >> tagShift) == tagBusy) {
        busyNow = true;
        break;
      }
      if (age(now, e->w0 & tsMask) > horizon) {
        if (take) stashWrite(p, e, StashEntry{});  // a fresh insert follows
        break;
      }
      w0 = e->w0;
      w1 = e->w1;
      if (take) {
        mv->begun.fetch_add(1);
        stashWrite(p, e, StashEntry{});
      }
      mu->unlock();
      if (take && testHookTaken) testHookTaken();
      retry = false;  // Go returns (w0, w1, true, false): a busy wait before doesn't make this a retry
      return true;
    }
    mu->unlock();
    if (!busyNow) {
      w0 = w1 = 0;
      return false;
    }
    retry = true;
    gosched();
  }
}

// lostRecently: without an overflow table, some history was lost within a
// horizon.
bool Core::lostRecently(u64 now) const {
  u64 la = lostAt.load();
  return la != 0 && age(now, la) <= horizon;
}

// placeRoomKey places waiting key e at the clock of its last touch, without
// its claim stripe; put commits it (claims A19, A24).
void Core::placeRoomKey(StashTable* p, StashEntry e) {
  const u64 id = e.id;
  const u64 fp = (id >> tagBits) & ((u64{1} << 16) - 1);
  MoveCount& m = moveOf(id & ((u64{1} << tagBits) - 1));
  const u64 pl = id >> 28;
  PutFunc put = [this, p, id, &m](i64 i, u64 w0, u64 w1, u64 mine0, u64) -> bool {
    auto [set, mu] = p->set(id);
    mu->lock();
    StashEntry* c = nullptr;
    for (u64 k = 0; k < stashWays; k++) {
      if (set[k].id == id && (set[k].w0 & deferMark) != 0 && (set[k].w1 >> tagShift) != tagBusy) c = &set[k];
    }
    if (!c) {
      mu->unlock();
      return false;
    }
    u64 c0 = c->w0, c1 = c->w1;
    c->w1 = busy;
    mu->unlock();
    if (testHookPlacing) testHookPlacing();
    m.begun.fetch_add(1);
    bool ok = install(i, w0, w1, (c0 & ~deferMark) | (mine0 & dispBit), c1);
    mu->lock();
    if (ok) {
      stashWrite(p, c, StashEntry{});
    } else {
      c->w1 = c1;
    }
    mu->unlock();
    m.done.fetch_add(1);
    return ok;
  };
  claimAt({pl, alt(pl, fp)}, {e.w0 & ~deferMark, e.w0 | dispBit}, e.w1, e.w0 & tsMask, noStripe, claimFull, &put);
}

// bgKick (Background) wakes the placer unless it is running. Go starts a
// goroutine; here one thread is started on the first kick and parked between
// runs.
void Core::bgKick() {
  bool f = false;
  if (!bgManual && !placing.load() && placing.compare_exchange_strong(f, true)) {
    {
      std::lock_guard<std::mutex> lk(bgMu);
      bgWake = true;
      if (!bgThread.joinable()) bgThread = std::thread(&Core::bgLoop, this);
    }
    bgCv.notify_one();
  }
}

// bgLoop parks the placer thread until a kick, then runs bgPlace (the Go
// goroutine's body).
void Core::bgLoop() {
  std::unique_lock<std::mutex> lk(bgMu);
  for (;;) {
    bgCv.wait(lk, [this] { return bgWake || bgStop; });
    if (bgStop) return;
    bgWake = false;
    lk.unlock();
    bgPlace();
    lk.lock();
  }
}

// bgPlace places waiting keys while any wait (it never reads the filter's
// clock), and returns after bgLinger naps with nothing to place.
void Core::bgPlace() {
  auto stopped = [this] {
    std::lock_guard<std::mutex> lk(bgMu);
    return bgStop;
  };
  for (;;) {
    for (int idle = 0; idle < bgLinger;) {
      if (deferred.load() > 0 && placeNext(room.load(), 0)) {
        idle = 0;
        continue;
      }
      idle++;
      std::this_thread::sleep_for(bgNap);
      if (stopped()) {
        placing.store(false);
        return;
      }
    }
    placing.store(false);
    bool f = false;
    if (deferred.load() == 0 || !placing.compare_exchange_strong(f, true)) return;
  }
}

// allocStash allocates the stash on the first eviction.
void Core::allocStash() {
  if (stash.load()) return;
  std::lock_guard<std::mutex> lk(skMu);
  if (!stash.load()) {
    ownStash.push_back(std::make_unique<StashTable>(stashN, stashMu.data()));
    stash.store(ownStash.back().get());
  }
}

// waitTable is where keys wait for a slot, allocated on first use (nullptr:
// nowhere). ConstantTime: the stash. Background: the wait room.
StashTable* Core::waitTable() {
  if (ct) {
    if (stashN == 0) return nullptr;
    allocStash();
    return stash.load();
  }
  if (StashTable* p = room.load()) return p;
  std::lock_guard<std::mutex> lk(skMu);
  if (!room.load()) {
    ownStash.push_back(std::make_unique<StashTable>(roomN, roomMu.data()));
    room.store(ownStash.back().get());
  }
  return room.load();
}

// stashPutAs keeps an evicted entry in its set: in its own old place, a free
// or expired way, or else in place of the way with the fewest events in the
// horizon if that has fewer than it; wait marks it as waiting for a slot.
void Core::stashPutAs(u64 id, u64 w0, u64 w1, u64 now, bool wait) {
  if (stashN == 0) {
    spill(id, w0, w1, now);
    return;
  }
  w0 &= ~dispBit;
  if (wait) w0 |= deferMark;
  allocStash();
  auto [set, mu] = stash.load()->set(id);
  std::unique_lock<PaddedMutex> lk(*mu);
  int way = -1;
  u64 least = ~u64{0};
  bool full = true;  // every way holds another live history
  for (int i = 0; i < static_cast<int>(stashWays); i++) {
    StashEntry* e = &set[i];
    if (e->id == id) {
      // An evictor holding an older snapshot of the same key must not
      // overwrite the newer one.
      if ((e->w0 & tsMask) > (w0 & tsMask) ||
          ((e->w0 & tsMask) == (w0 & tsMask) &&
           mass(e->id, e->w1, e->w0 & tsMask, now) >= mass(id, w1, w0 & tsMask, now)))
        return;
      way = i;
      least = 0;
      full = false;
      break;
    }
    if (e->id == 0 || age(now, e->w0 & tsMask) > horizon) {
      way = i;
      least = 0;
      full = false;
      break;
    }
    if (u64 m = mass(e->id, e->w1, e->w0 & tsMask, now); m < least) {
      way = i;
      least = m;
    }
  }
  StashEntry out;  // the history that leaves the stash, if any
  if (full) {
    if (least > mass(id, w1, w0 & tsMask, now)) {
      spill(id, w0, w1, now);
      return;
    }
    // The lightest leaves through spill even with no events in the horizon
    // (claims A32).
    out = set[way];
  }
  if (out.id != 0) spill(out.id, out.w0 & ~deferMark, out.w1, now);
  stashWrite(stash.load(), &set[way], StashEntry{id, w0, w1});
  if (!wait) raiseStashUntil((w0 & tsMask) + horizon);  // a waiting key moves no gate
}

// raiseStashUntil moves stashUntil up to until, if it is below.
void Core::raiseStashUntil(u64 until) {
  for (;;) {
    u64 u = stashUntil.load();
    if (u >= until || cas(stashUntil, u, until)) return;
  }
}

// stashWait puts a key that waits for a slot in a free or expired way of its
// set in the wait table, or in its own old place, and reports whether it did.
// A way being committed to the table (word1 busy) is never taken (claims A24).
bool Core::stashWait(u64 id, u64 w0, u64 w1, u64 now) {
  StashTable* p = waitTable();
  if (!p) return false;
  auto [set, mu] = p->set(id);
  std::lock_guard<PaddedMutex> lk(*mu);
  for (u64 i = 0; i < stashWays; i++) {
    if (StashEntry* e = &set[i];
        (e->w1 >> tagShift) != tagBusy && (e->id == 0 || e->id == id || age(now, e->w0 & tsMask) > horizon)) {
      stashWrite(p, e, StashEntry{id, (w0 & ~dispBit) | deferMark, w1});
      return true;
    }
  }
  return false;
}

// putBack returns a history recalled for an insert that lost a race for its
// slot: a key that waited back to wait, an evicted one to the stash.
void Core::putBack(u64 id, u64 w0, u64 w1, u64 now) {
  if ((w0 & deferMark) != 0 && stashWait(id, w0, w1, now)) return;
  stashPut(id, w0, w1, now);
}

// stashWrite replaces entry e of table p (its set's lock held), keeping the
// count of entries that wait for a slot.
void Core::stashWrite(StashTable* p, StashEntry* e, StashEntry ne) {
  if (e->id != 0 && (e->w0 & deferMark) != 0) deferred.fetch_sub(1);
  if (ne.id != 0 && (ne.w0 & deferMark) != 0) {
    deferred.fetch_add(1);
    u64 set = static_cast<u64>(e - p->e.data()) / stashWays;
    if (AU64& w = p->wait[static_cast<std::size_t>(set / 64)]; (w.load() & (u64{1} << (set % 64))) == 0)
      w.fetch_or(u64{1} << (set % 64));
  }
  *e = ne;
}

// tableGet is stashGet in table p (nullptr: not allocated, so empty).
bool Core::tableGet(StashTable* p, u64 id, u64 now, bool take, u64& w0, u64& w1) {
  w0 = w1 = 0;
  if (!p) return false;
  auto [set, mu] = p->set(id);
  std::lock_guard<PaddedMutex> lk(*mu);
  for (u64 i = 0; i < stashWays; i++) {
    StashEntry* e = &set[i];
    if (e->id != id) continue;
    if (age(now, e->w0 & tsMask) > horizon) return false;
    w0 = e->w0;
    w1 = e->w1;
    if (take) stashWrite(p, e, StashEntry{});
    return true;
  }
  return false;
}

// dropped records that a live key's history was lost at now.
void Core::dropped(u64 now) {
  lost.fetch_add(1);
  for (;;) {
    u64 la = lostAt.load();
    if (la >= now || cas(lostAt, la, now)) return;
  }
}

}  // namespace clepsydra::detail
