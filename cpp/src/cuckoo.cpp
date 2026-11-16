// go/cuckoo.go: a first touch's placement: miss, the cuckoo search and the
// eviction policy (claimAt), install, relocation, abandon.
#include "core.hpp"

namespace clepsydra::detail {

// miss installs h, absent from the table, with this touch counted: a fresh
// register, or its evicted history from the stash. Count < 0 means it lost a
// race for a slot and the caller should look again. The caller holds h's
// claim stripe.
Result Core::miss(u64 h, u64 fp, u64 tg, u64 window, u64 now, bool wantCount, u64 s0, u64 s1, int src) {
  Result r;
  bool fromRoom = src == srcRoom;                 // taken out of the wait room: its move ends once it is placed
  u64 w1 = nextReg(0, tg, now, now, true).first;  // first event, published with the key
  u64 last = now, sat = 0;
  bool hotRestore = false;
  if (src != 0 && age(now, s0 & tsMask) <= horizon && !hotGone(stashID(h, fp, tg), s1)) {
    last = s0 & tsMask;
    if (isHot(s1)) {  // it keeps its hot entry; the touch is counted once it is installed
      w1 = s1;
      hotRestore = true;
    } else {
      bool over;
      std::tie(w1, over) = nextReg(s1, tg, last, now, false);
      if (over || ((s0 & satBit) != 0 && detail::hot(w1))) sat = satBit;
    }
    r.Seen = age(now, last) <= window;
    r.Restored = (s0 & deferMark) == 0;  // a key that waited for a slot was never evicted
  } else if (lostRecently(now)) {
    r.Degraded = true;
  }
  // A recalled history keeps its last touch even when this caller's clock is
  // behind it.
  u64 stampTick = std::max(now, last);
  int mode = claimFull;
  if (ct || bg) mode = claimDefer;
  i64 ci = claim(h, fp, w1, now, stampTick, sat, mode);
  if (ci == claimWait) {
    // No free slot: wait for one, in the stash (ConstantTime) or the wait
    // room (Background). If more than one key per 16 wait-room entries
    // waits, the placer isn't keeping up: search now.
    if ((ct || deferred.load() <= static_cast<i64>(roomN * stashWays / 16)) &&
        stashWait(stashID(h, fp, tg), (fp << 48) | sat | stampTick, w1, now)) {
      waited.fetch_add(1);
      if (bg) bgKick();
    } else {  // its set is full: search (and evict) now rather than drop a history
      ci = claim(h, fp, w1, now, stampTick, sat, claimFull);
    }
  }
  if (ci == -1 && (src == srcStash || fromRoom)) putBack(stashID(h, fp, tg), s0, s1, now);  // for the retry
  if (fromRoom) moveOf(tg).done.fetch_add(1);  // in the table, or back in the room or the stash
  if (ci == -1) {
    Result x;
    x.Count = -1;
    return x;
  }
  if (hotRestore && !hotAdd(w1, stashID(h, fp, tg), now, nullptr)) {
    // The entry was reclaimed since hotGone: look again.
    Result x;
    x.Count = -1;
    return x;
  }
  if (src == srcStash && r.Restored) restored.fetch_add(1);
  if (!wantCount) return r;
  Result e = estimateAt(h, w1, stampTick, now, window, sat != 0);
  e.Seen = r.Seen;
  e.Restored = r.Restored;
  e.Degraded = r.Degraded;
  return e;
}

// claim installs fp in an empty or expired slot of the primary line, then the
// alternate line; else a cuckoo path; else it evicts the slot with the fewest
// events in the horizon, oldest on ties (claims D4, E1). Returns the word0
// index, -1 (lost a race: look again) or claimWait (mode claimDefer).
i64 Core::claim(u64 h, u64 fp, u64 w1, u64 now, u64 stampTick, u64 sat, int mode) {
  u64 p = primary(h);
  return claimAt({p, alt(p, fp)}, {(fp << 48) | sat | stampTick, (fp << 48) | dispBit | sat | stampTick}, w1, now,
                 h >> (64 - claimStripeBits), mode, nullptr);
}

// claimAt is claim for an entry with lines and words mine (primary,
// alternate), on claim stripe stripe, which the caller holds (noStripe: none).
// put installs the entry (nullptr: install).
i64 Core::claimAt(std::array<u64, 2> lns, std::array<u64, 2> mine, u64 w1, u64 now, u64 stripe, int mode,
                  const PutFunc* put) {
  for (;;) {
    for (int li = 0; li < 2; li++) {
      const i64 base = static_cast<i64>(lns[li] * wordsPerLine);
      for (int s = 0; s < wordsPerLine; s += 2) {
        u64 w0 = words[static_cast<std::size_t>(base + s)].load();
        if (w0 == 0 || age(now, w0 & tsMask) > horizon) {
          if (testHookOpenSeen) testHookOpenSeen(base + s);
          if (u64 o1 = words[static_cast<std::size_t>(base + s + 1)].load();
              putOver(put, base + s, w0, o1, mine[li], w1)) {
            if (w0 != 0) slotDrop(slotID(lns[li], w0, o1), o1);  // the expired occupant's
            return base + s;
          }
          return -1;
        }
      }
    }
    if (mode == claimDefer) return claimWait;
    // Iterative deepening: all short paths before any long one. The deep
    // budget shrinks as recent searches fail.
    std::uint32_t p = pressure.load();
    int budget = 8 + static_cast<int>((static_cast<u64>(relocBudget - 8) * (pressureOne - p)) >> pressureShift);
    warmAlts(lns);
    for (int depth = 0; depth <= relocDepth; depth++) {
      for (int li = 0; li < 2; li++) {
        const i64 base = static_cast<i64>(lns[li] * wordsPerLine);
        for (int s = 0; s < wordsPerLine; s += 2) {
          if (relocate(base + s, lns[li], mine[li], w1, now, depth, budget, nullptr, put)) {
            pressure.store(p - (p >> 3));  // EWMA toward "searches succeed"
            return base + s;
          }
        }
      }
    }
    pressure.store(p - (p >> 3) + static_cast<std::uint32_t>(pressureOne >> 3));  // toward "searches fail"
    i64 victim = -1;
    int vli = 0;
    u64 victimScore = ~u64{0}, victimW0 = 0, victimW1 = 0;
    for (int li = 0; li < 2; li++) {
      const i64 base = static_cast<i64>(lns[li] * wordsPerLine);
      for (int s = 0; s < wordsPerLine; s += 2) {
        u64 w0 = words[static_cast<std::size_t>(base + s)].load();
        u64 ts = w0 & tsMask;
        u64 vw1 = words[static_cast<std::size_t>(base + s + 1)].load();
        // Read after the slot: a key whose move is under way is left alone,
        // and a busy word1 can't be taken.
        if (MoveCount& mv = moveOf(vw1 >> tagShift); (vw1 >> tagShift) == tagBusy || mv.begun.load() != mv.done.load())
          continue;
        u64 score = (std::min<u64>(mass(slotID(lns[li], w0, vw1), vw1, ts, now), (u64{1} << 16) - 1) << 48) | ts;
        if (score < victimScore) {
          victim = base + s;
          vli = li;
          victimScore = score;
          victimW0 = w0;
          victimW1 = vw1;
        }
      }
    }
    if (victim < 0) return -1;  // every candidate is mid-move: look again
    if (testHookVictim) testHookVictim();
    return evict(lns[vli], mine[vli], w1, now, stripe, put, victim, victimW0, victimW1);
  }
}

// evict is claimAt's eviction of the slot at word index victim (in line,
// holding victimW0/victimW1) for the new entry mine0/mine1.
i64 Core::evict(u64 line, u64 mine0, u64 mine1, u64 now, u64 stripe, const PutFunc* put, i64 victim, u64 victimW0,
                u64 victimW1) {
  // The victim's history goes to the stash under the victim's own claim
  // stripe. TryLock: the caller holds its own stripe.
  u64 vs = victimW0 >> (64 - claimStripeBits);
  std::unique_lock<PaddedMutex> lk(claimMu[vs], std::defer_lock);
  if (vs != stripe && !lk.try_lock()) return -1;
  // A move of the victim's stripe that began after the scan may have copied
  // the victim, and its hand-off would then fail: back off.
  MoveCount& m = moveOf(victimW1 >> tagShift);
  m.evicting.fetch_add(1);
  Defer unevict{[&m] { m.evicting.fetch_sub(1); }};
  if (m.begun.load() != m.done.load()) return -1;
  // Between the install and the stash a lock-free lookup finds the victim in
  // neither; the move count tells it to look again.
  m.begun.fetch_add(1);
  Defer done{[&m] { m.done.fetch_add(1); }};
  if (!putNew(put, victim, victimW0, victimW1, mine0, mine1)) return -1;
  evictions.fetch_add(1);
  stashPut(slotID(line, victimW0, victimW1), victimW0, victimW1, now);
  return victim;
}

// putNew installs a new key's entry with put, or install if put is nullptr.
bool Core::putNew(const PutFunc* put, i64 i, u64 w0, u64 w1, u64 mine0, u64 mine1) {
  if (put) return (*put)(i, w0, w1, mine0, mine1);
  return install(i, w0, w1, mine0, mine1);
}

// putOver is putNew over slot i's expired occupant w0 (0: none), whose word1
// read w1. The hold counts as a move of the stripe of w1's tag (claims A27,
// A28).
bool Core::putOver(const PutFunc* put, i64 i, u64 w0, u64 w1, u64 mine0, u64 mine1) {
  if (w1 == 0 || (w1 >> tagShift) == tagBusy) return putNew(put, i, w0, w1, mine0, mine1);  // free, or held
  MoveCount& m = moveOf(w1 >> tagShift);  // whoever holds w1 when the install takes it
  m.begun.fetch_add(1);
  bool ok = putNew(put, i, w0, w1, mine0, mine1);
  m.done.fetch_add(1);
  return ok;
}

// warmAlts loads one word of the alternate line of every entry in lns, so the
// misses are in flight together.
void Core::warmAlts(std::array<u64, 2> lns) {
  warmLine(lns[0]);
  warmLine(lns[1]);
}

// warmLine loads one word of the alternate line of each entry in line.
void Core::warmLine(u64 line) {
  u64 sink = 0;
  const std::size_t base = static_cast<std::size_t>(line * wordsPerLine);
  for (int s = 0; s < wordsPerLine; s += 2) {
    if (u64 w0 = words[base + s].load(); w0 != 0)
      sink += words[static_cast<std::size_t>(alt(line, w0 >> 48) * wordsPerLine)].load();
  }
  // runtime.KeepAlive(sink): the loads must happen.
  [[maybe_unused]] volatile u64 keep = sink;
}

// install hands slot i (word index), if it still holds w0/w1, to the entry
// mine0/mine1. word1 goes busy first.
bool Core::install(i64 i, u64 w0, u64 w1, u64 mine0, u64 mine1) {
  AU64* w = &words[static_cast<std::size_t>(i)];
  if ((w1 >> tagShift) == tagBusy || !cas(w[1], w1, busy)) return false;
  if (testHookInstalling) testHookInstalling(i);
  if (!cas(w[0], w0, mine0)) {
    w[1].store(w1);  // lost the slot; put word1 back
    return false;
  }
  w[1].store(mine1);
  return true;
}

// relocate moves the entry at word index i (in line) to its alternate line and
// installs mine0/mine1 in its place, first relocating one of that line's
// entries if it is full, up to depth more steps. The copy is hidden until the
// hand-off (claims A23).
bool Core::relocate(i64 i, u64 line, u64 mine0, u64 mine1, u64 now, int depth, int& budget, MoveCount* parent,
                    const PutFunc* put) {
  u64 w0 = words[static_cast<std::size_t>(i)].load();
  if (w0 == 0) return false;
  if (budget <= 0) return false;
  budget--;
  u64 w1 = words[static_cast<std::size_t>(i) + 1].load();
  if ((w1 >> tagShift) == tagBusy || words[static_cast<std::size_t>(i)].load() != w0)
    return false;  // being installed or realigned, or touched since
  if (testHookPicked) testHookPicked(i);
  u64 a = alt(line, w0 >> 48);
  const i64 base = static_cast<i64>(a * wordsPerLine);
  u64 moved = w0 ^ dispBit;               // displaced iff it wasn't
  MoveCount& m = moveOf(w1 >> tagShift);  // the entry's key's stripe
  auto replace = [&](i64 d) -> bool {     // the hidden copy is at d; now take the source
    Defer done{[&m] { m.done.fetch_add(1); }};
    if (parent) {
      parent->begun.fetch_add(1);  // the parent's copy goes in (hidden) with this install
      if (parent->evicting.load() != 0) {
        parent->done.fetch_add(1);
        abandon(d);
        return false;
      }
    }
    if (putNew(put, i, w0, w1, mine0, mine1)) {          // deeper levels pass no put: install
      words[static_cast<std::size_t>(d) + 1].store(w1);  // publish
      relocations.fetch_add(1);
      return true;
    }
    if (parent) parent->done.fetch_add(1);
    abandon(d);  // a touch reached the source first, or the key was evicted
    return false;
  };
  for (int s = 0; s < wordsPerLine; s += 2) {
    u64 d0 = words[static_cast<std::size_t>(base + s)].load();
    if (d0 != 0 && age(now, d0 & tsMask) <= horizon) continue;
    m.begun.fetch_add(1);
    if (m.evicting.load() != 0) {  // an eviction of this stripe is under way: it may take the source
      m.done.fetch_add(1);
      return false;
    }
    if (u64 d1 = words[static_cast<std::size_t>(base + s + 1)].load();
        putOver(nullptr, base + s, d0, d1, moved, busy)) {
      if (d0 != 0) slotDrop(slotID(a, d0, d1), d1);  // the expired occupant's
      if (testHookCopied) testHookCopied();
      return replace(base + s);
    }
    m.done.fetch_add(1);
  }
  if (depth > 0) {
    warmLine(a);
    for (int s = 0; s < wordsPerLine; s += 2) {
      if (relocate(base + s, a, moved, busy, now, depth - 1, budget, &m, nullptr)) return replace(base + s);
    }
  }
  return false;
}

// abandon clears the hidden copy at word index d, word0 first.
void Core::abandon(i64 d) {
  words[static_cast<std::size_t>(d)].store(0);
  words[static_cast<std::size_t>(d) + 1].store(0);
}

}  // namespace clepsydra::detail
