// go/decay.go: the decay sweep, ContinuousDecay and its inline steps.
#include "core.hpp"

namespace clepsydra::detail {

// decayStep sweeps up to budget lines, paced by the clock: one table pass per
// horizon. Returns lines swept.
i64 Core::decayStep(u64 now, u64 budget) {
  u64 hi, lo;
  mul64(now, lines, hi, lo);
  if (hi >= horizon) return 0;
  u64 target = div64(hi, lo, horizon);
  u64 c = cursor.load();
  if (c >= target) return 0;
  checkTick(now);  // past it, every slot would read expired
  u64 start = c;
  if (target - start > lines) start = target - lines;  // fell behind by over a lap: skip ahead
  u64 n = std::min(target - start, budget);
  if (!cas(cursor, c, start + n)) return 0;  // another caller took this step
  for (u64 l = start; l < start + n; l++) sweepLine(l % lines, now);
  return static_cast<i64>(n);
}

// sweepLine clears line's slots expired at now, taking each as install does,
// for no key, its hold counted as a move (claims A26, A28).
void Core::sweepLine(u64 line, u64 now) {
  const i64 base = static_cast<i64>(line * wordsPerLine);
  for (int s = 0; s < wordsPerLine; s += 2) {
    AU64* w = &words[static_cast<std::size_t>(base + s)];
    u64 w0 = w[0].load();
    if (w0 == 0 || age(now, w0 & tsMask) <= horizon) continue;
    if (testHookSweepSeen) testHookSweepSeen(base + s);
    u64 w1 = w[1].load();
    if ((w1 >> tagShift) == tagBusy) continue;  // being handed over
    if (testHookSweepRead) testHookSweepRead(base + s);
    MoveCount& m = moveOf(w1 >> tagShift);  // the stripe of whoever holds w1 when the CAS takes it
    m.begun.fetch_add(1);
    if (!cas(w[1], w1, busy)) {
      m.done.fetch_add(1);
      continue;  // touched since
    }
    if (testHookSweeping) testHookSweeping();
    if (cas(w[0], w0, u64{0})) {
      w[1].store(0);
      slotDrop(slotID(line, w0, w1), w1);
    } else {
      w[1].store(w1);  // touched since: put word1 back
    }
    m.done.fetch_add(1);
  }
}

}  // namespace clepsydra::detail
