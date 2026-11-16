// go/filter.go: Config, construction, the clock and the hashes, ContainsAt and
// the counters.
#include <cmath>
#include <random>

#include "core.hpp"

// After the port's headers, so no Windows macro reaches them.
#if defined(_WIN32)
#ifndef NOMINMAX
#define NOMINMAX
#endif
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>
#ifndef CREATE_WAITABLE_TIMER_HIGH_RESOLUTION
#define CREATE_WAITABLE_TIMER_HIGH_RESOLUTION 0x00000002
#endif
#endif

namespace clepsydra::detail {

namespace {
#if defined(_WIN32)
// coarseTimer refreshes c's cached tick every period until c->coarseStop is
// set, on a high-resolution waitable timer: Windows times a condition
// variable's wait on the system timer, 15.6 ms apart unless some process has
// asked for finer, so a 1 ms CoarseClock could lag 15 ms (TestCoarseClock
// failed 1 run in 100 that way); Go's runtime waits on the same kind of
// timer. False if there is none (before Windows 10 1803): the caller then
// waits on the condition variable.
template <class Read>
bool coarseTimer(Core* c, Duration period, const Read& read) {
  HANDLE t = CreateWaitableTimerExW(nullptr, nullptr, CREATE_WAITABLE_TIMER_HIGH_RESOLUTION, TIMER_ALL_ACCESS);
  if (!t) return false;
  LARGE_INTEGER due;
  due.QuadPart = -std::max<LONGLONG>(1, static_cast<LONGLONG>(period.count() / 100));  // relative, in 100 ns
  HANDLE hs[2] = {static_cast<HANDLE>(c->coarseStop), t};
  for (;;) {
    if (!SetWaitableTimer(t, &due, 0, nullptr, nullptr, FALSE)) break;
    if (WaitForMultipleObjects(2, hs, FALSE, INFINITE) != WAIT_OBJECT_0 + 1) break;  // stopped, or failed
    c->coarse.store(read());
  }
  CloseHandle(t);
  return true;
}
#endif
}  // namespace

Core::Core(const Config& in) {
  Config cfg = in;
  if (cfg.Capacity <= 0 || cfg.Horizon <= Duration::zero())
    throw Error("temporalfilter: Capacity and Horizon must be positive");
  if (cfg.Resolution <= Duration::zero()) cfg.Resolution = std::chrono::microseconds(1);
  const u64 hz = static_cast<u64>(cfg.Horizon / cfg.Resolution);
  if (hz < static_cast<u64>(2 * unitsPerHorizon))  // unit >= 2 ticks
    throw Error("temporalfilter: Horizon too small for Resolution");
  if (hz >= MaxTick)  // no tick could age a key past it
    throw Error("temporalfilter: Horizon too large for Resolution (MaxTick ticks or more)");
  if (cfg.Load <= 0) cfg.Load = 0.85;
  if (cfg.Load > 1) throw Error("temporalfilter: Load must be <= 1");
  if (cfg.Clock && cfg.Clock() > MaxTick) throw TickError();
  lines = std::max<u64>(2, static_cast<u64>(std::ceil(static_cast<double>(cfg.Capacity) / (cfg.Load * slotsPerLine))));
  horizon = hz;
  unit = (hz + unitsPerHorizon - 1) / unitsPerHorizon;
  res = cfg.Resolution;
  clock = cfg.Clock;
  if (cfg.Key) {
    sip = siphash::Key::New(*cfg.Key);
  } else {
    // Go: a random maphash seed. Here: SipHash-2-4 under a random secret key.
    std::random_device rd;
    auto word = [&rd] { return (static_cast<u64>(rd()) << 32) | rd(); };
    sip.k0 = word();
    sip.k1 = word();
  }
  if (cfg.Stash == 0) cfg.Stash = static_cast<i64>(std::max<u64>(1, lines * slotsPerLine / 16));
  if (cfg.Stash > 0) stashN = static_cast<u64>(cfg.Stash + static_cast<i64>(stashWays) - 1) / stashWays;
  roomN = std::max<u64>(16, lines * slotsPerLine / (128 * stashWays));  // Background: one entry per 128 slots
  if (cfg.Overflow == 0) cfg.Overflow = static_cast<i64>(std::max<u64>(64, lines * slotsPerLine * 5 / 9));
  if (cfg.Overflow > 0) {
    skW = static_cast<u64>(cfg.Overflow);
    skShape = defaultShape;
    ghChoices = 2;
    ghSub = 3;
  }
  if (cfg.SpillMin == 0) cfg.SpillMin = 4;
  spillMin = static_cast<u64>(std::max<i64>(cfg.SpillMin, 1));
  if (cfg.Hot == 0) cfg.Hot = static_cast<i64>(std::max<u64>(16, lines * slotsPerLine / 128));
  hotN = static_cast<i64>(std::min(static_cast<u64>(std::max<i64>(cfg.Hot, 0)), hotIdxMask));
  ct = cfg.ConstantTime > Duration::zero() || cfg.ConstantTime == AutoPad || forceCT;
  pad = cfg.ConstantTime;
  autoPad = cfg.ConstantTime == AutoPad;
  bg = (cfg.Background || forceBG) && !ct;
  for (auto& p : padQ) p.q.store(static_cast<i64>(autoStart.count()) * 1000);
  unitInv = ~u64{0} / unit + 1;
  nwords = static_cast<std::size_t>(lines * wordsPerLine);
  words = AlignedBuf<AU64>(nwords);
  if (!clock) {
    const auto base = Clock::now();
    const Duration r = cfg.Resolution;
    ownClock = true;
    auto read = [this, base, r]() -> u64 { return static_cast<u64>((Clock::now() - base) / r) + 1 + clockAdd.load(); };
    clock = read;
    if (cfg.CoarseClock > Duration::zero()) {
      coarse.store(read());
      coarseOn = true;
      const Duration period = std::max<Duration>(cfg.CoarseClock, std::chrono::microseconds(10));
#if defined(_WIN32)
      coarseStop = CreateEventW(nullptr, TRUE, FALSE, nullptr);  // manual reset: close sets it once
#endif
      coarseThread = std::thread([this, period, read] {
#if defined(_WIN32)
        if (coarseStop && coarseTimer(this, period, read)) return;
#endif
        std::unique_lock<std::mutex> lk(stopMu);
        while (!stopCv.wait_for(lk, period, [this] { return stopping; })) coarse.store(read());
      });
      clock = [this] { return coarse.load(); };
    }
  }
}

Core::~Core() {
  close();
  if (bgThread.joinable()) {
    {
      std::lock_guard<std::mutex> lk(bgMu);
      bgStop = true;
    }
    bgCv.notify_all();
    bgThread.join();
  }
}

// close stops the CoarseClock thread, if any. The filter stays usable; its
// clock just stops advancing.
void Core::close() {
  {
    std::lock_guard<std::mutex> lk(stopMu);
    if (stopping) return;
    stopping = true;
  }
  stopCv.notify_all();
#if defined(_WIN32)
  if (coarseStop) SetEvent(static_cast<HANDLE>(coarseStop));
#endif
  if (coarseThread.joinable()) coarseThread.join();
#if defined(_WIN32)
  if (coarseStop) CloseHandle(static_cast<HANDLE>(coarseStop));
  coarseStop = nullptr;
#endif
}

u64 Core::ticks(Duration d) const { return static_cast<u64>(std::max(d, Duration::zero()) / res); }

u64 Core::hash(std::string_view key) const { return sip.HashString(key); }

u64 Core::hashUint64(u64 k) const { return sip.HashUint64(k); }

void Core::hashUint64s(std::span<const u64> keys, std::span<u64> out) const {
  if (out.size() < keys.size()) throw std::out_of_range("temporalfilter: HashUint64s: out shorter than keys");
  sip.HashUint64s(keys, out);
}

// containsAt reports whether h was touched within window, without touching it.
bool Core::containsAt(u64 h, u64 window, u64 now) {
  if (ct) return count(h, window, now, -1).Seen;
  Found f = read(h, now, -1);
  if (f.src != 0 && age(now, f.w0 & tsMask) <= std::min(window, horizon)) return true;
  if (!sketchLive(now)) return false;
  Result r;
  addSketch(r, h, window, now);
  return r.Seen;
}

i64 Core::occupancy(u64 now) const {
  i64 n = 0;
  for (std::size_t i = 0; i < nwords; i += 2) {
    u64 w0 = words[i].load();
    if (w0 != 0 && age(now, w0 & tsMask) <= horizon) n++;
  }
  return n;
}

// bytes is the memory the filter holds now: its table, and the stash,
// overflow table, hot entries and buzz entries once allocated.
i64 Core::bytes() const {
  i64 n = static_cast<i64>(8 * nwords);
  for (StashTable* s : {stash.load(), room.load()}) {
    if (s) n += static_cast<i64>(s->e.size() * sizeof(StashEntry) + 8 * s->wait.size());
  }
  if (Ghost* g = gh.load()) n += static_cast<i64>(8 * g->words.size());
  if (Sketch* s = sk.load()) n += static_cast<i64>(4 * s->cells.size());
  if (HotTable* t = hot.load()) n += static_cast<i64>(t->e.size() * sizeof(HotEntry));
  if (BuzzTable* t = buzz.load()) n += static_cast<i64>(t->b.size() * sizeof(BuzzBlock) + 8 * t->used.size());
  return n;
}

u64 Core::floored() const {
  if (Ghost* g = gh.load()) return g->floored.load();
  return 0;
}

u64 Core::demoted() const {
  if (HotTable* t = hot.load()) return t->demoted.load();
  return 0;
}

i64 Core::hotInUse() const {
  if (HotTable* t = hot.load()) return t->inUse.load();
  return 0;
}

}  // namespace clepsydra::detail
