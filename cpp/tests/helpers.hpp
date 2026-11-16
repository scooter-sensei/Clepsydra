// White-box helpers the Go suite keeps beside its tests: reaching a Filter's
// Core (Go: the package's unexported fields), whereIs and floorLive
// (go/soak_test.go), the tag amplifier (go/tagamp_test.go), and Go's
// time.ParseDuration for the suite's environment switches.
#pragma once

#include <atomic>
#include <chrono>
#include <cinttypes>
#include <cstdio>
#include <cstdlib>
#include <functional>
#include <map>
#include <memory>
#include <mutex>
#include <ostream>
#include <sstream>
#include <string>
#include <thread>
#include <unordered_map>
#include <vector>

#include "check.hpp"
#include "clepsydra/clepsydra.hpp"
#include "core.hpp"

namespace clepsydra::detail {

struct TestAccess {
  static Core& core(Filter& f) { return *f.c_; }
  // A Resizable's internals (Go: z.old, z.moved, z.cur).
  static bool inTransition(Resizable& z) {
    std::lock_guard<std::mutex> lk(z.mu_);
    return z.old_ != nullptr;
  }
  static std::int64_t moved(Resizable& z) { return z.moved_; }
  static Filter& cur(Resizable& z) { return *z.cur_; }
};

}  // namespace clepsydra::detail

namespace helpers {

using namespace clepsydra;
using namespace clepsydra::detail;

inline Core& core(Filter& f) { return TestAccess::core(f); }

inline std::string show(const Result& r) {
  std::ostringstream o;
  o << "{Seen:" << r.Seen << " Count:" << r.Count << " Upper:" << r.Upper << " Saturated:" << r.Saturated
    << " Restored:" << r.Restored << " Degraded:" << r.Degraded << "}";
  return o.str();
}

inline std::string counts(const std::array<u64, nBuckets>& c) {
  std::ostringstream o;
  o << "[";
  for (int j = 0; j < nBuckets; j++) o << (j ? " " : "") << c[j];
  o << "]";
  return o.str();
}

// whereIs describes where h's history is: its slots, its stash entry, its
// wait-room entry and its overflow answer.
inline std::string whereIs(Core& f, u64 h, u64 now) {
  u64 fp = fingerprint(h), tg = tag(h);
  u64 id = f.stashID(h, fp, tg);
  u64 p = f.primary(h);
  std::ostringstream s;
  u64 ls[2] = {p, f.alt(p, fp)};
  for (int li = 0; li < 2; li++) {
    std::size_t base = static_cast<std::size_t>(ls[li] * wordsPerLine);
    for (int k = 0; k < wordsPerLine; k += 2) {
      u64 w0 = f.words[base + k].load(), w1 = f.words[base + k + 1].load();
      if ((w0 >> 48) == fp && (w1 >> tagShift) == tg) {
        auto [v, ok] = f.viewOf(id, w1, w0 & tsMask);
        s << "slot line" << li << "/" << k / 2 << " disp" << ((w0 & dispBit) != 0) << " last " << (w0 & tsMask)
          << " hot " << isHot(w1) << " ok " << ok << " epoch " << v.e << " counts " << counts(v.c) << "; ";
      }
    }
  }
  if (f.stashN > 0 && f.stash.load()) {
    auto [set, mu] = f.stash.load()->set(id);
    for (u64 i = 0; i < stashWays; i++) {
      if (set[i].id == id) {
        auto [v, ok] = f.viewOf(id, set[i].w1, set[i].w0 & tsMask);
        s << "stash last " << (set[i].w0 & tsMask) << " hot " << isHot(set[i].w1) << " ok " << ok << " epoch " << v.e
          << " counts " << counts(v.c) << "; ";
      }
    }
  }
  if (StashTable* rp = f.room.load()) {
    auto [set, mu] = rp->set(id);
    for (u64 i = 0; i < stashWays; i++) {
      if (set[i].id == id) {
        auto [v, ok] = f.viewOf(id, set[i].w1, set[i].w0 & tsMask);
        s << "room last " << (set[i].w0 & tsMask) << " busy " << ((set[i].w1 >> tagShift) == tagBusy) << " hot "
          << isHot(set[i].w1) << " ok " << ok << " epoch " << v.e << " counts " << counts(v.c) << " (wait bit "
          << rp->waiting(id) << "); ";
      }
    }
  }
  if (Ghost* g = f.gh.load()) s << "ghost " << show(g->estimate(id, f.horizon, now).first);
  return s.str();
}

// floorLive: one of h's overflow buckets has a floor live within window that
// covers h's fingerprint.
inline bool floorLive(Core& f, u64 h, u64 window, u64 now) {
  Ghost* g = f.gh.load();
  if (!g) return false;
  u64 ep = g->epoch.load();
  std::array<i64, 2> bases;
  int nb;
  u64 fp;
  g->slot(f.stashID(h, fingerprint(h), tag(h)), bases, nb, fp);
  for (int i = 0; i < nb; i++) {
    u64 e = (g->words[static_cast<std::size_t>(bases[i])].load() >> g->floorShift(fp)) & ghSubMask;
    if (ep != 0 && e != 0 && g->live(g->real(e, ep), std::min(window, g->horizon), now)) return true;
  }
  return false;
}

// TagAmp remaps each key's hash so its 12-bit tag is one of n values and its
// fingerprint is unique in the run, its primary line kept (TF_CFUZZ_TAGS,
// TF_STRESS_TAGS).
class TagAmp {
 public:
  static std::unique_ptr<TagAmp> fromEnv(const char* env) {
    const char* v = std::getenv(env);
    int n = v ? std::atoi(v) : 0;
    if (n <= 0) return nullptr;
    auto a = std::make_unique<TagAmp>();
    a->n_ = static_cast<u64>(std::min(n, 14));
    return a;
  }
  // h is x remapped; cache, if not null, is the caller's own copy.
  u64 h(u64 x, std::unordered_map<u64, u64>* cache) {
    if (cache) {
      if (auto it = cache->find(x); it != cache->end()) return it->second;
    }
    u64 fp;
    {
      std::lock_guard<std::mutex> lk(mu_);
      auto it = fp_.find(x);
      if (it == fp_.end()) {
        next_++;
        fp = (next_ * 0x9e37) & 0xffff;  // a bijection of 1..65535
        fp_[x] = fp;
      } else {
        fp = it->second;
      }
    }
    u64 tg = 0x010 + ((x >> 36) % n_) * 0x011;
    u64 y = (fp << 48) | (tg << 36) | (x & ((u64{1} << 36) - 1));
    if (cache) (*cache)[x] = y;
    return y;
  }

 private:
  u64 n_ = 0;
  std::mutex mu_;
  std::unordered_map<u64, u64> fp_;
  u64 next_ = 0;
};

inline u64 amp(TagAmp* a, u64 x, std::unordered_map<u64, u64>* cache) { return a ? a->h(x, cache) : x; }

// parseDuration is time.ParseDuration for the forms the suite uses ("60s",
// "5m", "1m30s", "500ms", "2h"); 0 for anything else.
inline Duration parseDuration(const char* s) {
  if (!s || !*s) return Duration::zero();
  double total = 0;
  std::string str(s);
  std::size_t i = 0;
  while (i < str.size()) {
    std::size_t j = i;
    while (j < str.size() && (std::isdigit(static_cast<unsigned char>(str[j])) || str[j] == '.')) j++;
    if (j == i) return Duration::zero();
    double v = std::stod(str.substr(i, j - i));
    std::size_t k = j;
    while (k < str.size() && std::isalpha(static_cast<unsigned char>(str[k]))) k++;
    std::string u = str.substr(j, k - j);
    double ns;
    if (u == "ns")
      ns = 1;
    else if (u == "us" || u == "µs")
      ns = 1e3;
    else if (u == "ms")
      ns = 1e6;
    else if (u == "s")
      ns = 1e9;
    else if (u == "m")
      ns = 60e9;
    else if (u == "h")
      ns = 3600e9;
    else
      return Duration::zero();
    total += v * ns;
    i = k;
  }
  return Duration(static_cast<std::int64_t>(total));
}

inline std::string hex(u64 x) {
  char b[17];
  std::snprintf(b, sizeof b, "%" PRIx64, x);
  return b;
}

}  // namespace helpers

// Result prints as Go's %+v does.
namespace clepsydra {
inline std::ostream& operator<<(std::ostream& o, const Result& r) { return o << helpers::show(r); }
}  // namespace clepsydra

namespace helpers {

// ---- the Go suite's shared helpers (go/filter_test.go, go/tagstripe_test.go,
// go/split_race_test.go, go/install_race_test.go) ----

inline std::array<std::uint8_t, 16> testKey() {
  std::array<std::uint8_t, 16> k{};
  for (int i = 0; i < 16; i++) k[i] = static_cast<std::uint8_t>(i);
  return k;
}

// newTest is the Go suite's newTest: Capacity and Horizon, 1 ms ticks.
inline std::unique_ptr<Filter> newTest(std::int64_t capacity, Duration horizon) {
  Config cfg;
  cfg.Capacity = capacity;
  cfg.Horizon = horizon;
  cfg.Resolution = std::chrono::milliseconds(1);
  return std::make_unique<Filter>(cfg);
}

// settle waits until a Background filter's placer has placed every waiting
// key and stopped, for tests that read the table's words directly.
inline void settle(Filter& f) {
  Core& c = core(f);
  while (c.placing.load() || (c.bg && !c.ct && c.deferred.load() > 0))
    std::this_thread::sleep_for(std::chrono::microseconds(100));
}

// countLine is the number of entries in line l live at now.
inline int countLine(Core& f, u64 l, u64 now) {
  int n = 0;
  for (int s = 0; s < wordsPerLine; s += 2) {
    u64 w0 = f.words[static_cast<std::size_t>(l * wordsPerLine) + static_cast<std::size_t>(s)].load();
    if (w0 != 0 && age(now, w0 & tsMask) <= f.horizon) n++;
  }
  return n;
}

// keyAt: a hash with primary line line, fingerprint fp and tag tg, whose
// touches don't run the inline decay step.
inline u64 keyAt(Core& f, u64 line, u64 fp, u64 tg) {
  for (u64 lo = ((line << 36) + f.lines - 1) / f.lines;; lo++) {
    u64 h = (fp << 48) | (tg << 36) | (lo & ((u64{1} << 36) - 1));
    if (f.primary(h) == line && (h & decayEvery) != 0) return h;
  }
}

// twoLineFPs: n fingerprints from start on whose alternate line from line is
// another line.
inline std::vector<u64> twoLineFPs(Core& f, u64 line, u64 start, int n) {
  std::vector<u64> out;
  for (u64 fp = start; static_cast<int>(out.size()) < n; fp++)
    if (f.alt(line, fp) != line) out.push_back(fp);
  return out;
}

// slotsOf: the word indexes of the slots where h is visible.
inline std::vector<i64> slotsOf(Core& f, u64 h) {
  u64 fp = fingerprint(h), tg = tag(h);
  u64 p = f.primary(h);
  std::vector<i64> out;
  u64 ls[2] = {p, f.alt(p, fp)};
  for (int li = 0; li < 2; li++) {
    if (li == 1 && ls[1] == p) break;
    i64 base = static_cast<i64>(ls[li] * wordsPerLine);
    for (int s = 0; s < wordsPerLine; s += 2) {
      u64 w0 = f.words[static_cast<std::size_t>(base + s)].load();
      u64 w1 = f.words[static_cast<std::size_t>(base + s + 1)].load();
      if ((w0 >> 48) == fp && ((w0 & dispBit) != 0) == (li == 1) && (w1 >> tagShift) == tg) out.push_back(base + s);
    }
  }
  return out;
}

// keySlots counts the slots of k's two lines holding k (its fingerprint and
// tag).
inline int keySlots(Core& f, u64 k) {
  u64 fp = fingerprint(k), tg = tag(k);
  u64 p = f.primary(k);
  std::vector<u64> ls{p};
  if (u64 a = f.alt(p, fp); a != p) ls.push_back(a);
  int n = 0;
  for (u64 l : ls) {
    for (int s = 0; s < wordsPerLine; s += 2) {
      std::size_t j = static_cast<std::size_t>(l * wordsPerLine) + static_cast<std::size_t>(s);
      if ((f.words[j].load() >> 48) == fp && (f.words[j + 1].load() >> tagShift) == tg) n++;
    }
  }
  return n;
}

// newMinuteFilter: Capacity capacity, a minute's horizon in 1 ms ticks, a
// Clock that reads 1, and the plain paths whatever TF_CT and TF_BG say (the
// races these tests drive are in the plain paths' order of steps).
inline std::unique_ptr<Filter> newMinuteFilter(std::int64_t capacity) {
  Config cfg;
  cfg.Capacity = capacity;
  cfg.Horizon = std::chrono::minutes(1);
  cfg.Resolution = std::chrono::milliseconds(1);
  cfg.Clock = [] { return u64{1}; };
  auto f = std::make_unique<Filter>(cfg);
  core(*f).ct = false;
  core(*f).bg = false;
  return f;
}

// Done is a goroutine's completion (Go: a channel closed when it returns).
struct Done {
  std::thread th;
  std::shared_ptr<std::atomic<bool>> fin = std::make_shared<std::atomic<bool>>(false);
  bool closed() const { return fin->load(); }
  // wait waits up to d for the goroutine to return; true if it has.
  bool wait(Duration d) {
    auto end = std::chrono::steady_clock::now() + d;
    while (!fin->load() && std::chrono::steady_clock::now() < end)
      std::this_thread::sleep_for(std::chrono::microseconds(50));
    if (fin->load() && th.joinable()) th.join();
    return fin->load();
  }
  ~Done() {
    if (th.joinable() && !wait(std::chrono::seconds(10))) th.detach();  // a failed test's stuck goroutine
  }
};
using DonePtr = std::shared_ptr<Done>;

// go runs fn on a thread of its own.
inline DonePtr go(std::function<void()> fn) {
  auto d = std::make_shared<Done>();
  auto fin = d->fin;
  d->th = std::thread([fn = std::move(fn), fin] {
    fn();
    fin->store(true);
  });
  return d;
}

// during runs fn on a thread and waits for it up to 20 ms: it may wait for a
// hold its caller has.
inline DonePtr during(std::function<void()> fn) {
  auto d = go(std::move(fn));
  d->wait(std::chrono::milliseconds(20));
  return d;
}

// awaitDone is waitFor's check: whether done returned within 5 s.
inline bool awaitDone(const DonePtr& d) { return d->wait(std::chrono::seconds(5)); }

// panics reports whether call throws TickError (Go: panics).
inline bool panics(const std::function<void()>& call) {
  try {
    call();
  } catch (const TickError&) {
    return true;
  }
  return false;
}

// Hooks clears the test hooks that are set when it goes out of scope (the Go
// tests' defer that resets the hooks they set). A hook that isn't set is left
// alone, not written: a goroutine the test started may still be running past
// the scope and reading it (TestTornOpenScanHoldHidesKey's K finishes its
// touch, through bump's hooks, after tornOpenHold returns), and Go's defers
// never write the hooks a test didn't set.
struct Hooks {
  template <class H>
  static void reset(H& h) {
    if (h) h = nullptr;
  }
  ~Hooks() {
    reset(testHookCopied);
    reset(testHookVictim);
    reset(testHookTaken);
    reset(testHookStamped);
    reset(testHookStamping);
    reset(testHookPlacing);
    reset(testHookSweeping);
    reset(testHookGhostRead);
    reset(testHookGhostAdding);
    reset(testHookGhostFloor);
    reset(testHookPicked);
    reset(testHookInstalling);
    reset(testHookBumping);
    reset(testHookBumped);
    reset(testHookOpenSeen);
    reset(testHookSweepSeen);
    reset(testHookSweepRead);
    reset(testHookHotAdding);
  }
};

// cfgOf builds a Config: capacity, horizon, resolution and the rest left at
// their defaults, for the tests' New(Config{...}) literals.
inline Config cfgOf(std::int64_t capacity, Duration horizon, Duration res = Duration::zero()) {
  Config c;
  c.Capacity = capacity;
  c.Horizon = horizon;
  c.Resolution = res;
  return c;
}

}  // namespace helpers

// waitFor is the Go suite's waitFor: the goroutine must return within 5 s.
#define WAIT_FOR(done, what)                                              \
  do {                                                                    \
    if (!::helpers::awaitDone(done)) FATALF("%s did not return", (what)); \
  } while (0)

namespace helpers {
// realignModes runs fn with realign's 16-byte CAS, when the CPU has it, and
// then with its hold (go/cas2_amd64_test.go).
inline void realignModes(const std::function<void()>& fn) {
  if (cpu::HasCAS2) {
    check::log("-- cas2");
    fn();
    cpu::HasCAS2 = false;
    Defer back{[] { cpu::HasCAS2 = true; }};
    check::log("-- hold");
    fn();
    return;
  }
  check::log("-- hold");
  fn();
}
}  // namespace helpers

namespace helpers {
// SpinBarrier is a reusable barrier for n threads that spins (yielding)
// instead of sleeping: the lock-step tests (TestTouchIfSustained,
// TestBuzzConcurrent) cross it twice per tick, where Go's channels cost a
// few µs and a sleeping barrier tens.
class SpinBarrier {
 public:
  explicit SpinBarrier(int n) : n_(n) {}
  void arrive_and_wait() {
    int gen = gen_.load();
    if (left_.fetch_sub(1) == 1) {
      left_.store(n_);
      gen_.fetch_add(1);
      return;
    }
    for (int spins = 0; gen_.load() == gen; spins++)
      if (spins > 64) std::this_thread::yield();
  }

 private:
  const int n_;
  std::atomic<int> left_{n_};
  std::atomic<int> gen_{0};
};
}  // namespace helpers

namespace helpers {
// ---- go/attack_test.go's filter, shared with the experiments that reuse
// its workload (go/floodexp_test.go) ----

using Opts = std::vector<std::function<void(Config&)>>;

// Attacked is attackFilter's result: the filter and its clock, which the
// test moves (the filter's Clock reads *now).
struct Attacked {
  std::unique_ptr<std::uint64_t> now;
  std::unique_ptr<Filter> f;
};

// attackFilter: Capacity, Stash, 1 ms ticks over a minute, the clock *now
// (starting at 1).
inline Attacked attackFilter(std::int64_t capacity, std::int64_t stash, const Opts& opts = {}) {
  Attacked a;
  a.now = std::make_unique<std::uint64_t>(1);
  std::uint64_t* now = a.now.get();
  Config cfg;
  cfg.Capacity = capacity;
  cfg.Horizon = std::chrono::minutes(1);
  cfg.Resolution = std::chrono::milliseconds(1);
  cfg.Clock = [now] { return *now; };
  cfg.Stash = stash;
  for (const auto& o : opts) o(cfg);
  a.f = std::make_unique<Filter>(cfg);
  return a;
}

// noSketch turns the overflow table off.
inline void noSketch(Config& c) { c.Overflow = -1; }
}  // namespace helpers
