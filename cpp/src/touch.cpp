// go/touch.go: the touch and count paths, the stable lookup, the overflow
// read, realign and its 16-byte CAS.
#include "core.hpp"

namespace clepsydra::detail {

// count is CountAt; i0, if not -1, is a slot where a lookup just found h.
Result Core::count(u64 h, u64 window, u64 now, i64 i0) {
  Clock::time_point t0, s;
  Duration p{0};
  if (ct) {
    std::tie(t0, p) = ctBegin(opCount, now);
    s = Clock::now();
  }
  Found f = read(h, now, i0);
  Result r;
  if (f.src != 0 && age(now, f.w0 & tsMask) <= horizon) {
    r = estimateAt(h, f.w1, f.w0 & tsMask, now, window, (f.w0 & satBit) != 0);
    r.Seen = r.Count > 0;
  } else {
    r.Degraded = lostRecently(now);
  }
  if (sketchLive(now)) addSketch(r, h, window, now);
  if (ct) ctEnd(opCount, h, t0, s, p);
  return r;
}

// estimateAt is estimate for key h. If w1 points to an entry the key gave up
// meanwhile, the key's slot is looked up again, without its stripe. If it
// can't be found, the answer fails closed.
Result Core::estimateAt(u64 h, u64 w1, u64 last, u64 now, u64 window, bool sat) {
  if (!isHot(w1)) return estimate(0, w1, last, now, window, sat).first;
  u64 fp = fingerprint(h), tg = tag(h);
  u64 id = stashID(h, fp, tg);
  for (int k = 0; k < 8; k++) {
    if (auto [r, ok] = estimate(id, w1, last, now, window, sat); ok) return r;
    u64 w0 = 0, nw1 = 0;
    i64 i = lookupStable(h, fp, tg, false, w0, nw1);
    if (i < 0) break;
    w1 = nw1;
    last = std::max(last, w0 & tsMask);
    sat = (w0 & satBit) != 0;
  }
  Result r;
  r.Count = 1;
  r.Upper = 1;
  r.Saturated = true;
  return r;
}

// read returns h's slot words from the table or, if it was evicted, the
// stash, and where they came from (0: neither).
Found Core::read(u64 h, u64 now, i64 i0) {
  u64 fp = fingerprint(h), tg = tag(h);
  if (!ct) {
    // lookupStable, inline: one scan whether the key is found or not.
    MoveCount& m = moveOf(tg);
    for (int t = 0;; t++) {
      u64 b = m.begun.load(), d = m.done.load();
      i64 i = i0;
      u64 fpd = fp << 1;  // warm finds a key undisplaced in its primary line
      if (t > 0 || i < 0) std::tie(i, fpd) = locateAt(h, fp, tg);
      u64 w0 = 0, w1 = 0;
      if (slotWords(i, fpd, tg, w0, w1)) return Found{w0, w1, srcTable};
      bool may = stashMay(h, fp, tg, now);
      if (i < 0 && b == d && m.begun.load() == b) {
        if (!may) return Found{};
        break;  // the key may be in the stash or the wait room: look under its stripe
      }
      if (t > 0) gosched();
    }
  }
  // Under the key's stripe: an eviction moves the key to the stash while
  // holding it, so the key is found in one of the two.
  std::lock_guard<PaddedMutex> lk(claimMu[h >> (64 - claimStripeBits)]);
  MoveCount& m = moveOf(tg);
  for (;;) {
    u64 b = m.begun.load(), d = m.done.load();
    u64 w0 = 0, w1 = 0;
    i64 i = lookupStable(h, fp, tg, true, w0, w1);
    Found s = recall(h, fp, tg, now, false);
    if (s.src == srcRetry) continue;
    if (i >= 0) return Found{w0, w1, srcTable};
    if (s.src == 0 && !settled(m, b, d)) continue;
    return s;
  }
}

// lookupStable returns the index and words of h's slot, or -1 if h has none,
// reading both lines if both (ConstantTime). A miss is trusted only if no
// move of a key on h's tag stripe began or ended around the scan.
i64 Core::lookupStable(u64 h, u64 fp, u64 tg, bool both, u64& w0, u64& w1) {
  MoveCount& m = moveOf(tg);
  for (;;) {
    u64 b = m.begun.load(), d = m.done.load();
    auto [i, fpd] = both ? locateBoth(h, fp, tg) : locateAt(h, fp, tg);
    if (i >= 0) {
      if (slotWords(i, fpd, tg, w0, w1)) return i;
    } else if (b == d && m.begun.load() == b) {
      w0 = w1 = 0;
      return -1;
    }
    gosched();
  }
}

// sketchLive: the overflow table may hold a count at now. ConstantTime reads
// it whatever it holds.
bool Core::sketchLive(u64 now) const {
  if (!cms) {
    Ghost* g = gh.load();
    return g != nullptr && (ct || now <= g->until.load());
  }
  Sketch* s = sk.load();
  return s != nullptr && (ct || now <= s->until.load());
}

// addSketch adds h's events in the overflow table to r, the answer from the
// table or stash.
void Core::addSketch(Result& r, u64 h, u64 window, u64 now) const {
  u64 id = stashID(h, fingerprint(h), tag(h));
  auto [s, ok] = !cms ? gh.load()->estimate(id, std::min(window, horizon), now)
                      : sk.load()->estimate(id, std::min(window, horizon), now, ct);
  if (!ok) return;
  r.Count += s.Count;
  r.Upper += s.Upper;
  r.Saturated = r.Saturated || s.Saturated;
  r.Seen = r.Seen || s.Seen;
  r.Degraded = r.Degraded || s.Count > 0;
}

// touch is TouchAt. i0, if not -1, is a slot where a lookup just found h
// (warm); ctInside is touchCT's call (go/touch.go has the full account of the
// slot protocol it follows, claims A21, A29, A30, A33, D29).
Result Core::touch(u64 h, u64 window, u64 now, bool wantCount, i64 i0) {
  const bool inside = i0 == ctInside;  // Go: ct
  if (!inside) {
    if ((h & decayEvery) == 0) decayStep(now, 1);
    if (ct) return touchCT(h, window, now, wantCount);
  }
  u64 fp = fingerprint(h), tg = tag(h);
  // The stripes are unlocked by hand where the reference unlocks them; the
  // guards release them only when a lazy table's allocation throws meanwhile
  // (std::bad_alloc, fatal in Go).
  std::unique_lock<PaddedMutex> held;  // ConstantTime: h's stripe, while held
  for (bool first = true;; first = false) {
    if (held) held.unlock();  // released between attempts
    i64 i = -1;
    u64 fpd = 0;  // what the lookup matched in the slot (word0>>47)
    if (inside) {
      held = std::unique_lock<PaddedMutex>(claimMu[h >> (64 - claimStripeBits)]);
      u64 w0 = 0, w1 = 0;
      i = lookupStable(h, fp, tg, true, w0, w1);
      Found s = recall(h, fp, tg, now, true);
      if (i < 0) {
        Result r = miss(h, fp, tg, window, now, wantCount, s.w0, s.w1, s.src);
        if (r.Count < 0) continue;
        held.unlock();
        if (sketchLive(now)) addSketch(r, h, window, now);
        return r;
      }
      if (s.src == srcStash) stashPut(stashID(h, fp, tg), s.w0, s.w1, now);  // a stale copy
      fpd = w0 >> 47;
    }
    if (first && !inside && i0 >= 0) {
      u64 a = 0, b = 0;
      if (slotWords(i0, fp << 1, tg, a, b)) {  // warm finds a key undisplaced in its primary line
        i = i0;
        fpd = fp << 1;
      }
    }
    if (i < 0) std::tie(i, fpd) = locateAt(h, fp, tg);
    if (i < 0) {
      // Miss: serialize first touches of the same key. A tick past MaxTick
      // throws before the lock.
      checkTick(now);
      std::unique_lock<PaddedMutex> mu(claimMu[h >> (64 - claimStripeBits)]);
      MoveCount& m = moveOf(tg);
      u64 b = m.begun.load(), d = m.done.load();
      u64 w0 = 0, w1 = 0;
      if (i = lookupStable(h, fp, tg, false, w0, w1); i < 0) {
        Found s = recall(h, fp, tg, now, true);
        if (s.src == srcRetry || (s.src == 0 && !settled(m, b, d))) {
          mu.unlock();
          continue;
        }
        Result r = miss(h, fp, tg, window, now, wantCount, s.w0, s.w1, s.src);
        mu.unlock();
        if (r.Count < 0) continue;
        if (sketchLive(now)) addSketch(r, h, window, now);
        return r;
      }
      mu.unlock();  // another first touch got there first: count as a hit
      fpd = w0 >> 47;
    }
    // The count: slot i held h when it was found.
    AU64& a0 = words[static_cast<std::size_t>(i)];
    u64 w0 = a0.load();
    if ((w0 >> 47) != fpd) continue;  // reclaimed or handed over between find and load
    u64 last = w0 & tsMask;
    bool stale = age(now, last) > horizon;
    u64 w1 = 0, ref = 0;
    bool over = false, ok = false;
    i64 nu = unitOf(now), lu = unitOf(last);
    u64 old = 0;
    bool far = false, wait = false;
    if (nu > lu) farAhead(i, nu, lu, stale, old, far, wait);
    if (wait) {
      gosched();
      continue;  // another touch is realigning the slot
    } else if (far) {
      if (cpu::HasCAS2 && (old >> tagShift) == tg && !isHot(old)) {  // realign's usual case
        checkTick(now);
        ok = realign2U(i, w0, old, tg, now, nu, lu, w1, over);
      } else {
        ok = realign(h, i, w0, old, tg, now, nu, lu, w1, over);
      }
      ref = now;
    } else {
      if (nu > lu) {  // a hot key into a new unit: word0 moves only while word1 still points to the entry
        if (testHookStamping) testHookStamping();
        if (!stamp(i, w0, old, tg, now)) continue;
      } else if (now > last && !cas(a0, w0, (w0 & ~tsMask) | now)) {
        continue;
      }
      if (nu > lu && testHookStamped) testHookStamped();
      ok = bump(i, fpd, tg, now, nu, last, lu, stale, w1, ref, over);
    }
    if (!ok) continue;  // handed to another key, or moved on, meanwhile; look again
    bool sat = (w0 & satBit) != 0;
    if (over != sat) sat = markSat(i, fpd, over);
    Result r;
    if (wantCount) r = estimateAt(h, w1, std::max(now, ref), now, window, sat);
    r.Seen = !stale && age(now, last) <= window;
    if (held) held.unlock();
    // CheckAndTouch asks only for Seen, which addSketch can set but never
    // clear: a hit already Seen skips the overflow read.
    if (sketchLive(now) && (wantCount || !r.Seen || ct)) addSketch(r, h, window, now);
    return r;
  }
}

// farAhead: a touch in unit nu would move slot i's last touch (in unit lu) to
// a later unit, and word1 (old) must move with it. wait: word1 is busy.
void Core::farAhead(i64 i, i64 nu, i64 lu, bool stale, u64& old, bool& far, bool& wait) const {
  old = 0;
  far = wait = false;
  if (nu <= lu) return;
  old = words[static_cast<std::size_t>(i) + 1].load();
  if ((old >> tagShift) == tagBusy) {
    wait = true;
    return;
  }
  if (isHot(old)) {
    far = stale;
    return;
  }
  far = true;
}

// stamp moves slot i's last touch (w0) to now, in a later unit, while word1
// still holds p, a hot entry's pointer of tag tg: by one 16-byte CAS of both
// words, or else under a hold of word1.
bool Core::stamp(i64 i, u64 w0, u64 p, u64 tg, u64 now) {
  AU64* w = &words[static_cast<std::size_t>(i)];
  u64 n0 = (w0 & ~tsMask) | now;
  if (cpu::HasCAS2) return cpu::CAS2(w, w0, p, n0, p);
  MoveCount& m = moveOf(tg);
  m.begun.fetch_add(1);
  Defer done{[&m] { m.done.fetch_add(1); }};
  if (!cas(w[1], p, busy)) return false;
  bool ok = cas(w[0], w0, n0);
  w[1].store(p);
  return ok;
}

// realign is touch's count of a touch at now in a later unit than slot i's
// last touch and register, or past the horizon of a hot key's slot. With
// CMPXCHG16B both words move by one CAS (realign2U); elsewhere under a hold.
bool Core::realign(u64 h, i64 i, u64 w0, u64 old, u64 tg, u64 now, i64 nu, i64 lu, u64& w1, bool& over) {
  (void)h;
  checkTick(now);
  w1 = 0;
  over = false;
  if ((old >> tagShift) != tg || (isHot(old) && age(now, w0 & tsMask) <= horizon)) return false;
  if (cpu::HasCAS2) return realign2U(i, w0, old, tg, now, nu, lu, w1, over);
  AU64* w = &words[static_cast<std::size_t>(i)];
  MoveCount& m = moveOf(tg);
  m.begun.fetch_add(1);
  Defer done{[&m] { m.done.fetch_add(1); }};
  if (!cas(w[1], old, busy)) return false;
  if (!cas(w[0], w0, (w0 & ~tsMask) | now)) {
    w[1].store(old);
    return false;
  }
  if (testHookStamped) testHookStamped();
  u64 last = w0 & tsMask;
  std::tie(w1, over) = nextRegU(old, tg, last, now, lu, nu, age(now, last) > horizon);
  if (over) {
    u64 pw = 0;
    if (promoteHeld(i, w0, old, tg, last, now, pw)) {
      w1 = pw;
      over = false;
    }
  }
  w[1].store(w1);
  if (isHot(old)) slotDrop(slotID(static_cast<u64>(i / wordsPerLine), w0, old), old);  // a stale hot key's reset
  return true;
}

// realign2U is realign by one 16-byte CAS of both words from w0, old (claims
// A33), with now's unit nu and the last touch's, lu.
bool Core::realign2U(i64 i, u64 w0, u64 old, u64 tg, u64 now, i64 nu, i64 lu, u64& w1, bool& over) {
  u64 last = w0 & tsMask;
  u64 n0 = (w0 & ~tsMask) | now;
  std::tie(w1, over) = nextRegU(old, tg, last, now, lu, nu, age(now, last) > horizon);
  u64 idx = 0, id = 0;
  if (over) {
    u64 x = 0, y = 0;
    if (u64 pw = hotEntryFor(i, w0, old, tg, last, now, x, y); pw != 0) {
      w1 = pw;
      over = false;
      idx = x;
      id = y;
    }
  }
  if (!cpu::CAS2(&words[static_cast<std::size_t>(i)], w0, old, n0, w1)) {
    if (idx != 0) hotClose(idx, id);  // never published
    w1 = 0;
    over = false;
    return false;
  }
  if (idx != 0) promoted.fetch_add(1);
  if (isHot(old)) slotDrop(slotID(static_cast<u64>(i / wordsPerLine), w0, old), old);  // a stale hot key's reset
  if (testHookStamped) testHookStamped();
  return true;
}

}  // namespace clepsydra::detail
