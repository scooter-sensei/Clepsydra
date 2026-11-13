// go/snapshot.go: snapshots, WriteTo and ReadFrom: the reference's format,
// version 3 (version 2 read and converted), seven sections each ended by its
// CRC32C check word, and the built-in clock's re-anchoring (claims D27, D29,
// A35, A46).
#include <cstdio>
#include <istream>
#include <ostream>
#include <string>

#include "core.hpp"
#include "crc32c.hpp"

namespace clepsydra::detail {

namespace {
constexpr u64 snapVersion = 3;
constexpr u64 snapMagic = 0x31504e5350454c43;  // "CLEPSNP1"

i64 unixNano() {
  return std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::system_clock::now().time_since_epoch())
      .count();
}

struct SnapW {
  explicit SnapW(std::ostream& o) : w(o) {}
  std::ostream& w;
  i64 n = 0;
  bool bad = false;
  std::uint32_t crc = 0, sec = 0;

  void u(u64 x) {
    if (bad) return;
    std::uint8_t b[8];
    for (int i = 0; i < 8; i++) b[i] = static_cast<std::uint8_t>(x >> (8 * i));
    crc = crc32cUpdate(crc, b, 8);
    w.write(reinterpret_cast<const char*>(b), 8);
    if (!w) {
      bad = true;
      return;
    }
    n += 8;
  }
  // end writes the section's check word.
  void end() {
    u(static_cast<u64>(crc) | (static_cast<u64>(sec) << 32));
    crc = 0;
    sec++;
  }
};

struct SnapR {
  explicit SnapR(std::istream& i) : r(i) {}
  std::istream& r;
  i64 n = 0;
  std::string err;
  std::uint32_t crc = 0, sec = 0;

  u64 u() {
    if (!err.empty()) return 0;
    std::uint8_t b[8];
    r.read(reinterpret_cast<char*>(b), 8);
    n += r.gcount();
    if (r.gcount() != 8) {
      err = "temporalfilter: snapshot: short stream";
      return 0;
    }
    crc = crc32cUpdate(crc, b, 8);
    u64 x = 0;
    for (int i = 0; i < 8; i++) x |= static_cast<u64>(b[i]) << (8 * i);
    return x;
  }
  void fail(const std::string& msg) {
    if (err.empty()) err = "temporalfilter: snapshot: " + msg;
  }
  // end reads and checks the section's check word.
  void end() {
    u64 want = static_cast<u64>(crc) | (static_cast<u64>(sec) << 32);
    if (u64 got = u(); err.empty() && got != want) {
      char buf[96];
      std::snprintf(buf, sizeof buf, "section %u: check word %#llx, want %#llx", sec,
                    static_cast<unsigned long long>(got), static_cast<unsigned long long>(want));
      fail(buf);
    }
    crc = 0;
    sec++;
  }
};

const char* errSnapMode = "temporalfilter: snapshots of ConstantTime, Background or count-min filters are not supported";
}  // namespace

std::vector<u64> Core::snapShape() const {
  // The last word is the count-min sketch's bytes for the overflow size, the
  // ghost table's own size: 16 B per unit. (The reference computes it from a
  // zero shape when Overflow < 0 and divides by zero there; 0 here.)
  return {lines,
          horizon,
          unit,
          static_cast<u64>(res.count()),
          stashN,
          static_cast<u64>(hotN),
          skW,
          static_cast<u64>(ghChoices),
          static_cast<u64>(ghSub),
          spillMin,
          skBytes()};
}

// writeTo writes the filter's state. The filter must be quiescent.
i64 Core::writeTo(std::ostream& w) {
  if (ct || bg || cms) throw Error(errSnapMode);
  SnapW s{w};
  s.u(snapMagic);
  s.u(snapVersion);
  for (u64 x : snapShape()) s.u(x);
  s.u(clock());
  s.u(static_cast<u64>(unixNano()));
  s.end();
  for (std::size_t i = 0; i < nwords; i++) s.u(words[i].load());
  s.end();
  for (u64 x : {cursor.load(), evictions.load(), relocations.load(), static_cast<u64>(pressure.load()),
                stashUntil.load(), lostAt.load(), lost.load(), restored.load(), folded.load(), shed.load(),
                waited.load(), promoted.load(), hotRefused.load()})
    s.u(x);
  s.end();
  if (StashTable* st = stash.load()) {
    s.u(1);
    for (std::size_t i = 0; i < st->e.size(); i++) {
      s.u(st->e[i].id);
      s.u(st->e[i].w0);
      s.u(st->e[i].w1);
    }
    for (std::size_t i = 0; i < st->wait.size(); i++) s.u(st->wait[i].load());
    s.u(st->at.load());
  } else {
    s.u(0);
  }
  s.end();
  if (HotTable* t = hot.load()) {
    s.u(1);
    for (std::size_t i = 0; i < t->e.size(); i++) {
      HotEntry& e = t->e[i];
      s.u(e.ver.load());
      s.u(e.id.load());
      s.u(static_cast<u64>(e.epoch.load()));
      s.u(static_cast<u64>(e.sat.load()));
      s.u(e.c0.load());
      for (std::size_t j = 0; j < e.c.size(); j += 2) s.u(static_cast<u64>(e.c[j].load()) | (static_cast<u64>(e.c[j + 1].load()) << 32));
    }
    s.u(t->free.size());
    for (std::uint32_t x : t->free) s.u(x);
    s.u(t->next);
    s.u(t->hand);
    s.u(static_cast<u64>(t->full.load()));
    s.u(static_cast<u64>(t->inUse.load()));
    s.u(t->demoted.load());
  } else {
    s.u(0);
  }
  s.end();
  if (Ghost* g = gh.load()) {
    s.u(1);
    for (std::size_t i = 0; i < g->words.size(); i++) s.u(g->words[i].load());
    s.u(g->epoch.load());
    s.u(g->until.load());
    s.u(g->sat.load() ? 1 : 0);
    s.u(g->floored.load());
  } else {
    s.u(0);
  }
  s.end();
  if (BuzzTable* t = buzz.load()) {
    s.u(1);
    for (std::size_t i = 0; i < t->b.size(); i++) {
      BuzzBlock& b = t->b[i];
      for (int j = 0; j < buzzWays; j++) {
        BuzzEntry& e = b.e[j];
        for (u64 x : {b.ids[j], e.w0, e.w1, e.meta, e.ev[0], e.ev[1], e.ev[2], e.ev[3]}) s.u(x);
      }
    }
  } else {
    s.u(0);
  }
  s.end();
  if (!s.bad) w.flush();
  if (s.bad || !w) throw Error("temporalfilter: snapshot: write failed");
  return s.n;
}

// continued is the built-in clock's tick for a snapshot written at tick oldNow
// and wall-clock wall (Unix ns): the old tick plus the wall time since.
u64 Core::continued(u64 oldNow, i64 wall) const {
  return oldNow + static_cast<u64>(std::max<i64>(0, unixNano() - wall)) / static_cast<u64>(res.count());
}

// readFrom restores a snapshot writeTo wrote into this filter, which was
// built with the same Config and nothing has used yet.
i64 Core::readFrom(std::istream& r) {
  if (ct || bg || cms) throw Error(errSnapMode);
  SnapR s{r};
  auto done = [&s]() -> i64 {
    if (!s.err.empty()) throw Error(s.err);
    return s.n;
  };
  if (s.u() != snapMagic) s.fail("bad magic");
  u64 v = s.u();
  if (v != snapVersion && v != 2)
    s.fail("version " + std::to_string(v) + ", want " + std::to_string(snapVersion) + " or 2");
  for (u64 x : snapShape()) {
    if (s.u() != x) s.fail("another Config");
  }
  u64 oldNow = s.u();
  i64 wall = static_cast<i64>(s.u());
  s.end();
  // A lineage of snapshots can carry the built-in clock past MaxTick (claims
  // A35): refuse it before anything is written.
  if (s.err.empty() && ownClock && continued(oldNow, wall) > MaxTick) s.fail("the continued clock is past MaxTick");
  if (!s.err.empty()) return done();
  for (std::size_t i = 0; i < nwords; i++) words[i].store(s.u());
  if (v == 2) {
    for (std::size_t i = 0; i < nwords; i += 2) {
      auto [a, b] = fromV2(words[i].load(), words[i + 1].load());
      words[i].store(a);
      words[i + 1].store(b);
    }
  }
  s.end();
  cursor.store(s.u());
  evictions.store(s.u());
  relocations.store(s.u());
  pressure.store(static_cast<std::uint32_t>(s.u()));
  stashUntil.store(s.u());
  lostAt.store(s.u());
  lost.store(s.u());
  restored.store(s.u());
  folded.store(s.u());
  shed.store(s.u());
  waited.store(s.u());
  promoted.store(s.u());
  hotRefused.store(s.u());
  s.end();
  if (s.u() == 1) {
    auto st = std::make_unique<StashTable>(stashN, stashMu.data());
    for (std::size_t i = 0; i < st->e.size(); i++) {
      st->e[i].id = s.u();
      st->e[i].w0 = s.u();
      st->e[i].w1 = s.u();
      if (v == 2) std::tie(st->e[i].w0, st->e[i].w1) = fromV2(st->e[i].w0, st->e[i].w1);
    }
    for (std::size_t i = 0; i < st->wait.size(); i++) st->wait[i].store(s.u());
    st->at.store(s.u());
    ownStash.push_back(std::move(st));
    stash.store(ownStash.back().get());
  }
  s.end();
  if (s.u() == 1) {
    if (hotN == 0) {
      s.fail("hot entries in a filter without them");
      return done();
    }
    HotTable* t = hotTable();
    for (std::size_t i = 0; i < t->e.size(); i++) {
      HotEntry& e = t->e[i];
      e.ver.store(s.u());
      e.id.store(s.u());
      e.epoch.store(static_cast<i64>(s.u()));
      e.sat.store(static_cast<i64>(s.u()));
      u64 c0 = s.u();
      if (v == 2) c0 &= ~hotOnMask;  // generation bits there; an onset of 0 says nothing
      e.c0.store(c0);
      for (std::size_t j = 0; j < e.c.size(); j += 2) {
        u64 x = s.u();
        e.c[j].store(static_cast<std::uint32_t>(x));
        e.c[j + 1].store(static_cast<std::uint32_t>(x >> 32));
      }
    }
    u64 n = s.u();
    if (n > t->e.size()) {
      s.fail("free list");
      return done();
    }
    t->free.assign(static_cast<std::size_t>(n), 0);
    for (auto& x : t->free) x = static_cast<std::uint32_t>(s.u());
    t->next = s.u();
    t->hand = s.u();
    t->full.store(static_cast<i64>(s.u()));
    t->inUse.store(static_cast<i64>(s.u()));
    t->demoted.store(s.u());
  }
  s.end();
  if (s.u() == 1) {
    Ghost* g = ghostTable();
    for (std::size_t i = 0; i < g->words.size(); i++) g->words[i].store(s.u());
    g->epoch.store(s.u());
    g->until.store(s.u());
    g->sat.store(s.u() == 1);
    g->floored.store(s.u());
  }
  s.end();
  if (v >= 3 && s.u() == 1) {
    BuzzTable* t = buzzTable();
    for (std::size_t i = 0; i < t->b.size(); i++) {
      BuzzBlock& b = t->b[i];
      for (int j = 0; j < buzzWays; j++) {
        BuzzEntry& e = b.e[j];
        b.ids[j] = s.u();
        e.w0 = s.u();
        e.w1 = s.u();
        e.meta = s.u();
        for (auto& x : e.ev) x = s.u();
      }
      t->mark(i);
    }
  }
  if (v >= 3) s.end();
  if (s.err.empty() && r.peek() != std::char_traits<char>::eof()) s.fail("trailing bytes");
  if (s.err.empty() && ownClock) {
    u64 target = continued(oldNow, wall);
    if (target > MaxTick) {  // crossed while the sections were read
      s.fail("the continued clock is past MaxTick");
    } else if (u64 cur = clock(); target > cur) {
      clockAdd.fetch_add(target - cur);
      if (coarseOn) coarse.fetch_add(target - cur);  // CoarseClock (closed or not): the cached tick moves on too
    }
  }
  return done();
}

// fromV2 is a version 2 snapshot's slot pair as version 3 holds it: the
// register moved to its last touch's unit, onset 0.
std::pair<u64, u64> Core::fromV2(u64 w0, u64 w1) const {
  if (w0 == 0 || w1 == 0 || (w1 >> tagShift) == tagBusy || isHot(w1)) return {w0, w1};
  i64 u = unitOf(w0 & tsMask);
  i64 d = (u - static_cast<i64>(w1 >> onsetShift)) & static_cast<i64>(onsetMask);
  if (d > static_cast<i64>(onsetMask / 2)) d -= static_cast<i64>(onsetMask + 1);
  Register r = unpack(w1 & regMask);
  if (i64 e = u - d; e < u) {
    if (r.advance(e, u)) w0 |= satBit;
  } else if (e > u) {
    w0 = (w0 & ~tsMask) | (static_cast<u64>(e) * unit);
  }
  return {w0, (w1 & ~((onsetMask << onsetShift) | regMask)) | r.pack()};
}

}  // namespace clepsydra::detail
