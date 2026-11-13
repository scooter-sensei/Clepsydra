// go/ct.go: ConstantTime's pads, and the placement steps the pads and the
// Background placer both run (claims S28, S36, S41).
#include <random>

#include "core.hpp"

namespace clepsydra::detail {

namespace {
// ditherNs is rand.Uint64N(padDither): a uniform draw in [0, 128) ns.
u64 ditherNs() {
  thread_local std::mt19937_64 rng{std::random_device{}()};
  return rng() % static_cast<u64>(padDither.count());
}
}  // namespace

// ctBegin starts a ConstantTime call of kind op and returns its start and pad.
// Before the call's own work it spends what the pad leaves beyond its own
// estimate placing keys that wait in the stash.
std::pair<Clock::time_point, Duration> Core::ctBegin(int op, u64 now) {
  auto t0 = Clock::now();
  Duration own{padQ[op].q.load() / 1000};
  Duration p;
  if (op == opTouch && !autoPad) {
    p = pad;
  } else if (autoPad) {
    p = own;
  } else {
    p = std::min(own, pad);
  }
  place(t0, p - own, now);
  return {t0, p};
}

// ctEnd moves op's own-work estimate with the work done since s (1 call in 4
// by key moves it) and spins until the pad, plus a random dither, has passed
// since t0.
void Core::ctEnd(int op, u64 h, Clock::time_point t0, Clock::time_point s, Duration p) {
  if (((h >> 8) & 3) == 0) {
    auto& qp = padQ[op].q;
    if (Duration est{qp.load() / 1000}; Clock::now() - s > est) {
      if (est < autoMax) qp.fetch_add(autoUp);
    } else if (est > Duration::zero()) {
      qp.fetch_sub(autoDown);
    }
  }
  p += Duration(static_cast<i64>(ditherNs()));
  while (Clock::now() - t0 < p) {
  }
}

// place spends up to budget since t0 on keys that wait in the stash for a
// slot. If more than one key per 16 stash entries waits, every call places
// one set regardless.
void Core::place(Clock::time_point t0, Duration budget, u64 now) {
  StashTable* p = stash.load();
  if (!p) return;
  bool forced = deferred.load() > static_cast<i64>(p->e.size() / 16);
  while (deferred.load() > 0) {
    Duration est{placeNs.load()};
    if (est == Duration::zero()) est = std::chrono::microseconds(1);
    if (!forced && budget - (Clock::now() - t0) < est) {
      placeNs.store((est - est / 64).count());  // a step that ran long once mustn't stop placing for good
      return;
    }
    forced = false;
    auto s = Clock::now();
    if (!placeNext(p, now)) return;
    placeNs.store((est + (std::chrono::duration_cast<Duration>(Clock::now() - s) - est) / 8).count());
  }
}

// placeNext places the waiting keys of table p's next set (after the last one
// visited) whose wait bit is set; false if no bit is set.
bool Core::placeNext(StashTable* p, u64 now) {
  if (!p) return false;
  u64 nw = p->wait.size();
  u64 start = p->at.load();
  for (u64 k = 0; k < nw + 1; k++) {
    u64 wi = (start / 64 + k) % nw;
    u64 b = p->wait[static_cast<std::size_t>(wi)].load();
    if (k == 0) b &= ~((u64{1} << (start % 64)) - 1);  // bits before the cursor wait for the wrap-around
    if (b == 0) continue;
    u64 set = wi * 64 + static_cast<u64>(std::countr_zero(b));
    p->at.store(set + 1);
    if (p == room.load()) {
      placeRoomSet(p, set);
    } else {
      placeSet(p, set, now);
    }
    return true;
  }
  return false;
}

// placeSet moves the waiting keys of table p's set i into the table. A key
// whose stripe is busy, or that loses a race for its slot, is left for a
// later step.
void Core::placeSet(StashTable* p, u64 i, u64 now) {
  StashEntry* set = &p->e[static_cast<std::size_t>(i * stashWays)];
  PaddedMutex& mu = p->mu[i % stashStripes];
  std::array<u64, stashWays> ids{};
  int n = 0;
  mu.lock();
  for (u64 k = 0; k < stashWays; k++) {
    if (StashEntry* e = &set[k]; e->id != 0 && (e->w0 & deferMark) != 0) {
      if (age(now, e->w0 & tsMask) > horizon) {
        stashWrite(p, e, StashEntry{});  // expired while it waited
        continue;
      }
      ids[n++] = e->id;
    }
  }
  if (n == 0) p->wait[static_cast<std::size_t>(i / 64)].fetch_and(~(u64{1} << (i % 64)));
  mu.unlock();
  for (int k = 0; k < n; k++) {
    u64 id = ids[k];
    u64 fp = (id >> tagBits) & ((u64{1} << 16) - 1), tg = id & ((u64{1} << tagBits) - 1);
    u64 stripe = fp >> (16 - claimStripeBits);
    PaddedMutex& cm = claimMu[stripe];
    if (!cm.try_lock()) continue;
    u64 w0, w1;
    if (tableGet(p, id, now, true, w0, w1)) {
      u64 pl = id >> 28;
      u64 al = alt(pl, fp);
      if (find(pl, fp << 1, tg) >= 0 || find(al, (fp << 1) | 1, tg) >= 0) {
        stashPut(id, w0, w1, now);  // the table holds the key: no longer waiting
      } else if (claimAt({pl, al}, {w0 & ~dispBit, w0 | dispBit}, w1, now, stripe, claimFull, nullptr) < 0) {
        stashPutAs(id, w0, w1, now, true);
      }
    }
    cm.unlock();
  }
}

// placeRoomSet places the waiting keys of the wait room's set i (Background).
void Core::placeRoomSet(StashTable* p, u64 i) {
  StashEntry* set = &p->e[static_cast<std::size_t>(i * stashWays)];
  PaddedMutex& mu = p->mu[i % stashStripes];
  std::array<StashEntry, stashWays> waitingKeys{};
  int n = 0;
  mu.lock();
  for (u64 k = 0; k < stashWays; k++) {
    if (StashEntry e = set[k]; e.id != 0 && (e.w0 & deferMark) != 0) waitingKeys[n++] = e;
  }
  if (n == 0) p->wait[static_cast<std::size_t>(i / 64)].fetch_and(~(u64{1} << (i % 64)));
  mu.unlock();
  for (int k = 0; k < n; k++) placeRoomKey(p, waitingKeys[k]);
}

// touchCT is touch in ConstantTime mode, padded.
Result Core::touchCT(u64 h, u64 window, u64 now, bool wantCount) {
  checkTick(now);  // before the stripe: realign's check would throw holding it
  auto [t0, p] = ctBegin(opTouch, now);
  auto s = Clock::now();
  Result r = touch(h, window, now, wantCount, ctInside);
  ctEnd(opTouch, h, t0, s, p);
  return r;
}

}  // namespace clepsydra::detail
