// go/slots.go: the slot words' layout, lookups, fingerprint and line
// arithmetic.
#include "core.hpp"

namespace clepsydra::detail {

// slotWords loads slot i's words if it still holds fpd/tg: the fingerprint
// and displaced bit (word0>>47) and the tag the lookup matched there. word0 is
// read again after word1: a realign between the two loads would pair the old
// last touch with the new register.
bool Core::slotWords(i64 i, u64 fpd, u64 tg, u64& w0, u64& w1) const {
  if (i < 0) {
    w0 = w1 = 0;
    return false;
  }
  AU64* w = &words[static_cast<std::size_t>(i)];
  w0 = w[0].load();
  w1 = w[1].load();
  if ((w0 >> 47) != fpd || (w1 >> tagShift) != tg) return false;
  if (u64 a = w[0].load(); a != w0 && unitOf(a & tsMask) != unitOf(w0 & tsMask)) return false;
  return true;
}

// markSat sets (on) or clears the saturated bit of the slot at word index i if
// it still holds fpd, and returns the bit as left. It clears only while no
// bucket is hot, reading word1 after word0.
bool Core::markSat(i64 i, u64 fpd, bool on) {
  AU64& a0 = words[static_cast<std::size_t>(i)];
  AU64& a1 = words[static_cast<std::size_t>(i) + 1];
  for (;;) {
    u64 w0 = a0.load();
    if ((w0 >> 47) != fpd) return on;
    if (((w0 & satBit) != 0) == on) return on;
    if (u64 w1 = a1.load(); !on && ((w1 >> tagShift) == tagBusy || detail::hot(w1)))
      return true;  // a busy word1 (realign) may come back with a hot bucket
    if (a0.compare_exchange_strong(w0, w0 ^ satBit)) return on;
  }
}

i64 Core::locate(u64 h, u64 fp, u64 tg) const { return locateAt(h, fp, tg).first; }

// locateAt is locate, with the fingerprint and displaced bit it matched.
std::pair<i64, u64> Core::locateAt(u64 h, u64 fp, u64 tg) const {
  u64 p = primary(h);
  if (i64 i = find(p, fp << 1, tg); i >= 0) return {i, fp << 1};
  return {find(alt(p, fp), (fp << 1) | 1, tg), (fp << 1) | 1};
}

// locateBoth is locateAt reading both lines every time (ConstantTime).
std::pair<i64, u64> Core::locateBoth(u64 h, u64 fp, u64 tg) const {
  u64 p = primary(h);
  i64 i = find(p, fp << 1, tg);
  i64 j = find(alt(p, fp), (fp << 1) | 1, tg);
  if (i >= 0) return {i, fp << 1};
  return {j, (fp << 1) | 1};
}

// find returns the word index of the slot in line whose fingerprint and
// displaced bit equal fpd and whose tag is tg, or -1.
i64 Core::find(u64 line, u64 fpd, u64 tg) const {
  const std::size_t base = static_cast<std::size_t>(line * wordsPerLine);
  AU64* w = &words[base];
  for (int s = 0; s < wordsPerLine; s += 2) {
    if ((w[s].load() >> 47) == fpd && (w[s + 1].load() >> tagShift) == tg) return static_cast<i64>(base) + s;
  }
  return -1;
}

// alt is the other candidate line for fp stored in line: alt(l) = (x - l) mod
// lines, its own inverse.
u64 Core::alt(u64 line, u64 fp) const {
  u64 x = mulHi(mix64(fp * 0x9e3779b97f4a7c15), lines);
  if (x >= line) return x - line;
  return x + lines - line;
}

// slotID is stashID for the entry in a slot of line, without its key.
u64 Core::slotID(u64 line, u64 w0, u64 w1) const {
  u64 fp = w0 >> 48;
  if ((w0 & dispBit) != 0) line = alt(line, fp);
  return (line << 28) | (fp << tagBits) | (w1 >> tagShift);
}

}  // namespace clepsydra::detail
