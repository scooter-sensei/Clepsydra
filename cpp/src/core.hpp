// detail::Core is the Go reference's Filter (go/filter.go) and every method
// it has, with the reference's names, so each function here can be read
// beside its Go original and the claim tags in comments point to the same
// code. Go's package-private visibility maps to this internal header: every
// member is public to the library and its white-box tests, and the public
// clepsydra::Filter holds a Core.
//
// Porting rules this file keeps (cpp/README.md has them all):
//   - Go's sync/atomic is sequentially consistent: every atomic operation here
//     is std::memory_order_seq_cst (the default), and a plain Go access to a
//     word others access atomically is a relaxed atomic access.
//   - Go's operator precedence differs from C++'s (& and << bind tighter than
//     + and - in Go): every mixed expression is fully parenthesized.
//   - Go's defer runs at function return: lock guards are declared at
//     function scope, never inside a block a Go defer sat in.
//   - Go's unsigned shifts by 64 or more give 0; no shift here can reach 64.
#pragma once

#include <algorithm>
#include <array>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <functional>
#include <limits>
#include <memory>
#include <mutex>
#include <new>
#include <stdexcept>
#include <thread>
#include <tuple>
#include <type_traits>
#include <utility>
#include <vector>

#include "bits.hpp"
#include "clepsydra/clepsydra.hpp"
#include "cpu.hpp"
#include "siphash.hpp"

namespace clepsydra::detail {

using u64 = std::uint64_t;
using i64 = std::int64_t;
using AU64 = std::atomic<std::uint64_t>;
using Clock = std::chrono::steady_clock;

// ---- slots.go ----
inline constexpr int slotsPerLine = 4;
inline constexpr int wordsPerLine = 2 * slotsPerLine;
// dispBit marks an entry living in its alternate line ("displaced").
inline constexpr u64 dispBit = u64{1} << 47;
// satBit marks a slot whose register overflowed within its span.
inline constexpr u64 satBit = u64{1} << 46;
inline constexpr u64 tsMask = satBit - 1;
inline constexpr unsigned tagBits = 12;
inline constexpr unsigned tagShift = 64 - tagBits;
// word1's bits 48-51: the onset of the register's newest bucket.
inline constexpr unsigned onsetShift = 48;
inline constexpr u64 onsetMask = 0xf;
// busy is word1 while a slot is being handed over.
inline constexpr u64 tagBusy = (u64{1} << tagBits) - 1;
inline constexpr u64 busy = tagBusy << tagShift;
inline constexpr unsigned claimStripeBits = 8;

// ---- register.go ----
inline constexpr int nBuckets = 7;
inline constexpr i64 geomUnits = 24;
inline constexpr i64 unitsPerHorizon = geomUnits;
inline constexpr u64 regMask = (u64{1} << 48) - 1;
inline constexpr unsigned bucketBits0 = 5;
inline constexpr u64 satPerUnit = (u64{1} << bucketBits0) - 1;
inline constexpr std::array<unsigned, nBuckets> bucketLogStep{0, 0, 1, 2, 3, 3, 3};
inline constexpr std::array<i64, nBuckets> bucketOff{0, 1, 2, 4, 8, 16, 24};
inline constexpr std::array<unsigned, nBuckets> bucketBits{bucketBits0, 5, 6, 7, 8, 8, 9};
inline constexpr std::array<unsigned, nBuckets> bucketShift = [] {
  std::array<unsigned, nBuckets> sh{};
  for (int j = 1; j < nBuckets; j++) sh[j] = sh[j - 1] + bucketBits[j - 1];
  return sh;
}();
// bucketBase maps a register bit to the shift of the bucket holding it.
inline constexpr std::array<std::uint8_t, 48> bucketBase = [] {
  std::array<std::uint8_t, 48> b{};
  for (int j = 0; j < nBuckets; j++)
    for (unsigned k = bucketShift[j]; k < bucketShift[j] + bucketBits[j]; k++)
      b[k] = static_cast<std::uint8_t>(bucketShift[j]);
  return b;
}();
inline constexpr std::array<i64, nBuckets> bucketStepMask = [] {
  std::array<i64, nBuckets> m{};
  for (int j = 0; j < nBuckets; j++) m[j] = ~((i64{1} << bucketLogStep[j]) - 1);
  return m;
}();

// ---- hot.go ----
inline constexpr unsigned hotIdxShift = bucketBits0;
inline constexpr u64 hotIdxMask = (u64{1} << (48 - hotIdxShift)) - 1;
inline constexpr i64 satNone = std::numeric_limits<i64>::min();
inline constexpr u64 hotCount = (u64{1} << 32) - 1;
inline constexpr u64 hotClosed = u64{1} << 32;
inline constexpr u64 hotGen = u64{1} << 33;
inline constexpr unsigned hotOnShift = 60;
inline constexpr u64 hotOnMask = onsetMask << hotOnShift;

// ---- cuckoo.go ----
inline constexpr int relocDepth = 3;
inline constexpr int relocBudget = 500;
inline constexpr unsigned pressureShift = 16;
inline constexpr u64 pressureOne = u64{1} << pressureShift;
inline constexpr u64 noStripe = u64{1} << claimStripeBits;
inline constexpr int claimFull = 0;
inline constexpr int claimDefer = 1;
inline constexpr i64 claimWait = -2;

// ---- stash.go ----
inline constexpr int srcTable = 1;
inline constexpr int srcStash = 2;
inline constexpr int srcRetry = 3;
inline constexpr int srcRoom = 4;
inline constexpr u64 stashStripes = 64;
inline constexpr u64 stashWays = 4;
inline constexpr u64 deferMark = dispBit;
inline constexpr auto bgNap = std::chrono::microseconds(50);
inline constexpr int bgLinger = 20;

// ---- decay.go, batch.go, touch.go ----
inline constexpr u64 decayEvery = 63;
inline constexpr int batchChunk = 32;
inline constexpr i64 ctInside = -2;

// ---- ct.go ----
inline constexpr int opTouch = 0;
inline constexpr int opCount = 1;
inline constexpr i64 autoUp = 99'500;
inline constexpr i64 autoDown = 500;
inline constexpr Duration autoStart = std::chrono::microseconds(4);
inline constexpr Duration autoMax = std::chrono::microseconds(50);
inline constexpr Duration padDither = std::chrono::nanoseconds(128);

// ---- buzz.go ----
inline constexpr int buzzCap = 16;
inline constexpr int buzzWays = 8;
inline constexpr u64 buzzStripes = 64;
inline constexpr unsigned buzzNShift = 46;
inline constexpr unsigned buzzHShift = 51;
inline constexpr unsigned buzzUShift = 55;

// ---- free functions of slots.go and register.go ----

inline u64 fingerprint(u64 h) {
  u64 fp = h >> 48;
  return fp != 0 ? fp : 1;
}

// tag is 12 more fingerprint bits, kept in word1; tagBusy is reserved, and
// keys that would have it share the tag below.
inline u64 tag(u64 h) {
  u64 t = (h >> 36) & ((u64{1} << tagBits) - 1);
  return t ^ ((t + 1) >> tagBits);
}

// age is now-ts, or 0 if ts is ahead.
inline u64 age(u64 now, u64 ts) { return now < ts ? 0 : now - ts; }

inline u64 mix64(u64 x) {
  x ^= x >> 30;
  x *= 0xbf58476d1ce4e5b9;
  x ^= x >> 27;
  x *= 0x94d049bb133111eb;
  return x ^ (x >> 31);
}

// cas is Go's atomic CompareAndSwap: old is taken by value (C++'s
// compare_exchange writes the current value back into its first argument).
template <class T, class V>
inline bool cas(std::atomic<T>& a, V old, V nw) {
  T o = static_cast<T>(old);
  return a.compare_exchange_strong(o, static_cast<T>(nw));
}

// Defer runs f when it goes out of scope: Go's defer, when declared at the
// function's own scope (Go runs every defer at function return, never at the
// end of a block).
template <class F>
struct Defer {
  F f;
  ~Defer() { f(); }
};
template <class F>
Defer(F) -> Defer<F>;

// checkTick throws TickError if now is past MaxTick.
inline void checkTick(u64 now) {
  if (now > MaxTick) throw TickError();
}

// bucketStart is b_j in units (may be negative near u = 0).
inline i64 bucketStart(i64 u, int j) { return (u & bucketStepMask[j]) - bucketOff[j]; }

// isHot reports whether word1 points at a hot entry instead of holding a
// register.
inline bool isHot(u64 w1) { return (w1 & ((u64{1} << bucketBits0) - 1)) == 0 && (w1 & regMask) != 0; }
inline u64 hotIdx(u64 w1) { return (w1 >> hotIdxShift) & hotIdxMask; }

// hot reports whether some bucket of w1 holds satPerUnit or more events.
inline bool hot(u64 w1) {
  if (isHot(w1)) return false;
  return (w1 & 0x1F) >= satPerUnit || ((w1 >> 5) & 0x1F) >= satPerUnit || ((w1 >> 10) & 0x3F) >= satPerUnit ||
         ((w1 >> 16) & 0x7F) >= satPerUnit || ((w1 >> 23) & 0xFF) >= satPerUnit || ((w1 >> 31) & 0xFF) >= satPerUnit ||
         ((w1 >> 39) & 0x1FF) >= satPerUnit;
}

// lowerOnset is w1 with its onset lowered to on, if on is earlier.
inline u64 lowerOnset(u64 w1, u64 on) {
  if (on < ((w1 >> onsetShift) & onsetMask)) w1 = (w1 & ~(onsetMask << onsetShift)) | (on << onsetShift);
  return w1;
}

// Register is the velocity register's 7 counters.
struct Register {
  std::array<u64, nBuckets> c{};
  u64& operator[](int j) { return c[j]; }
  u64 operator[](int j) const { return c[j]; }
  // add puts n events into bucket j, spilling into older buckets when it is
  // full; reports whether any of the n didn't fit in bucket j.
  bool add(int j, u64 n);
  // advance re-buckets from unit u0 to unit u1 >= u0, reporting whether a
  // merge overflowed its new bucket.
  bool advance(i64 u0, i64 u1);
  u64 pack() const;
};
Register unpack(u64 w1);

// advanceWide is Register::advance for a hot entry's uncapped counts.
void advanceWide(std::array<u64, nBuckets>& r, i64 u0, i64 u1);

// View is a register's state, narrow or hot.
struct View {
  i64 e = 0;
  std::array<u64, nBuckets> c{};
  u64 on = 0;
  i64 sat = 0;
  bool hot = false;
};

// demotable: v fits a register at half of every bucket's width, and no event
// of the register's span is missing.
bool demotable(const View& v, i64 u);

// AlignedBuf is the reference's alignedWords: an array of T aligned to a
// cache line, every element value-initialized (zero), which also takes the
// pages' faults at allocation (the reference's prefault). A size whose bytes
// overflow std::size_t throws std::length_error, as std::vector's does (Go's
// make panics); the elements' construction can't throw.
template <class T>
class AlignedBuf {
  static_assert(std::is_nothrow_default_constructible_v<T>);

 public:
  AlignedBuf() = default;
  explicit AlignedBuf(std::size_t n) : n_(n) {
    if (n > std::numeric_limits<std::size_t>::max() / sizeof(T))
      throw std::length_error("temporalfilter: table larger than the address space");
    p_ = static_cast<T*>(::operator new(std::max<std::size_t>(n, 1) * sizeof(T), std::align_val_t{64}));
    for (std::size_t i = 0; i < n; i++) new (p_ + i) T();
  }
  ~AlignedBuf() { reset(); }
  AlignedBuf(AlignedBuf&& o) noexcept : p_(std::exchange(o.p_, nullptr)), n_(std::exchange(o.n_, 0)) {}
  AlignedBuf& operator=(AlignedBuf&& o) noexcept {
    if (this != &o) {
      reset();
      p_ = std::exchange(o.p_, nullptr);
      n_ = std::exchange(o.n_, 0);
    }
    return *this;
  }
  AlignedBuf(const AlignedBuf&) = delete;
  AlignedBuf& operator=(const AlignedBuf&) = delete;
  T& operator[](std::size_t i) const { return p_[i]; }
  T* data() const { return p_; }
  std::size_t size() const { return n_; }

 private:
  void reset() {
    if (p_) {
      for (std::size_t i = n_; i-- > 0;) p_[i].~T();
      ::operator delete(p_, std::align_val_t{64});
      p_ = nullptr;
      n_ = 0;
    }
  }
  T* p_ = nullptr;
  std::size_t n_ = 0;
};

struct alignas(64) PaddedMutex {
  std::mutex mu;
  void lock() { mu.lock(); }
  void unlock() { mu.unlock(); }
  bool try_lock() { return mu.try_lock(); }
};

// MoveCount: moves of a stripe's keys begun and done, and evictions of them
// under way (slots.go).
struct MoveCount {
  AU64 begun{0}, done{0};
  std::atomic<i64> evicting{0};
};

struct StashEntry {
  u64 id = 0, w0 = 0, w1 = 0;  // id 0 = empty; w0's displaced bit is clear
};
static_assert(sizeof(StashEntry) == 24);

// StashTable: sets of stashWays entries, and a bit per set that holds a key
// waiting for a slot (stash.go).
struct StashTable {
  StashTable(u64 sets, PaddedMutex* mus);
  std::pair<StashEntry*, PaddedMutex*> set(u64 id) const;
  bool waiting(u64 id) const;

  AlignedBuf<StashEntry> e;
  AlignedBuf<AU64> wait;
  u64 n;            // sets
  PaddedMutex* mu;  // the filter's stashStripes mutexes, which exist before the table
  AU64 at{0};       // the next set to place waiting keys from
};

// HotEntry is one hot entry (hot.go), a cache line.
struct alignas(64) HotEntry {
  AU64 ver{0};                // odd while a writer holds the entry
  AU64 id{0};                 // owner's identity; 0: free
  std::atomic<i64> epoch{0};  // unit of bucket 0
  std::atomic<i64> sat{0};    // newest unit whose events may be missing; satNone
  AU64 c0{0};                 // bucket 0's count | hotClosed | generation | onset
  std::array<std::atomic<std::uint32_t>, nBuckets - 1> c{};

  u64 lock();
  void unlock(u64 ver);
  View load() const;
  void store(View& v);
};
static_assert(sizeof(HotEntry) == 64);

struct Core;

struct HotTable {
  // The free list never holds more than the table's entries: reserved up
  // front, putFree never allocates, so a slot drop (a sweep, a relocation)
  // can't throw between beginning and ending its move.
  explicit HotTable(std::size_t n) : e(n) { free.reserve(n); }
  std::pair<View, bool> read(u64 idx, u64 id) const;
  bool add(Core& f, u64 idx, u64 id, u64 n, i64 u, u64 now, AU64* slot, u64 old);
  void putFree(u64 idx);
  u64 reclaim(i64 u);

  AlignedBuf<HotEntry> e;  // index 0 unused
  std::mutex mu;
  std::vector<std::uint32_t> free;
  u64 next = 1;  // entries never handed out: next..len(e)-1
  u64 hand = 0;  // reclaim scan position
  std::atomic<i64> full{satNone};
  std::atomic<i64> inUse{0};
  AU64 demoted{0};
};

// ---- ghost.go ----
inline constexpr unsigned ghFPBits = 14;
inline constexpr unsigned ghCntBits = 5;
inline constexpr unsigned ghSlBits = 2;
inline constexpr unsigned ghEntryBits = ghFPBits + ghCntBits + ghSlBits;  // 21
inline constexpr int ghPerWord = 3;
inline constexpr int ghWords = 8;
inline constexpr int ghEntries = ghWords * ghPerWord;
inline constexpr u64 ghCntMax = (u64{1} << ghCntBits) - 1;
inline constexpr u64 ghEntryMask = (u64{1} << ghEntryBits) - 1;
inline constexpr unsigned ghSubBits = ghCntBits + ghSlBits;
inline constexpr u64 ghSubMask = (u64{1} << ghSubBits) - 1;
inline constexpr int ghSubMax = ghEntryBits / ghSubBits;

inline u64 ghEntry(u64 fp, u64 cnt, u64 sl) { return (fp << (ghCntBits + ghSlBits)) | (cnt << ghSlBits) | (sl & 3); }
inline u64 ghFP(u64 e) { return e >> (ghCntBits + ghSlBits); }
inline u64 ghCnt(u64 e) { return (e >> ghSlBits) & ghCntMax; }

struct Ghost {
  Ghost(u64 bytes, u64 horizon, int choices, int sub);
  void slot(u64 id, std::array<i64, 2>& base, int& nb, u64& fp) const;
  u64 real(u64 e, u64 ep) const;
  bool live(u64 sl, u64 window, u64 now) const;
  AU64* at(i64 base, int k, unsigned& sh) const;
  unsigned floorShift(u64 fp) const;
  u64 floorOf(i64 base, u64 fp, u64 ep, u64 now) const;
  void add(u64 id, u64 n, u64 last, bool sat, u64 now);
  void raiseFloor(i64 base, u64 fp, u64 n, u64 sl, u64 now);
  void advance(u64 ns);
  std::pair<Result, bool> estimate(u64 id, u64 window, u64 now) const;

  AlignedBuf<AU64> words;
  u64 nb;  // buckets
  int choices;
  int sub;
  u64 sliceLen;
  u64 horizon;
  std::mutex mu;
  AU64 epoch{0};  // newest slice + 1 seen by an add (0: none)
  AU64 until{0};  // tick before which an entry may be live
  std::atomic<bool> sat{false};
  AU64 floored{0};
};

// ---- overflow.go: the count-min sketch (experiments: Core::cms) ----
inline constexpr int maxRows = 4;
inline constexpr int maxSlices = 6;
// sigmas: a slice's count-mean-min estimate counts only above this many
// standard deviations of its noise.
inline constexpr double sigmas = 3;

// SketchShape is the sketch's geometry: rows, counter bits (8 or 16), and
// slices per horizon (the ring holds parts+2).
struct SketchShape {
  int rows = 0, bits = 0, parts = 0;
  // bytes is the memory of a sketch of width w; 0 for the zero shape, which a
  // filter with the overflow table off has (its snapshot's shape says 0).
  u64 bytes(u64 w) const {
    if (bits == 0) return 0;
    u64 per = static_cast<u64>(32 / bits);
    return static_cast<u64>(rows) * w * ((static_cast<u64>(parts) + 2 + per - 1) / per) * 4;
  }
};
inline constexpr SketchShape defaultShape{4, 8, 2};

struct Sketch {
  Sketch(u64 w, u64 horizon, SketchShape sh);
  u64 col(u64 id, int r) const;
  std::uint32_t cell(u64 base, u64 p) const;
  void add(u64 id, u64 t, u64 n, bool sat, u64 now);
  void renew(u64 p, u64 sl);
  std::pair<Result, bool> estimate(u64 id, u64 window, u64 now, bool full) const;

  std::vector<std::atomic<std::uint32_t>> cells;  // [row][column][slice / per], bits per slice
  u64 w;                                          // columns per row
  int rows;
  u64 bits, per, nsl, cw;  // counter bits, counters per word, ring slices, words per column
  std::uint32_t cmask, csat;
  u64 sliceLen, horizon;
  std::mutex mu;
  std::array<AU64, maxSlices> epochs{}, totals{}, squares{};
  AU64 until{0};
};

// ---- buzz.go ----
struct BuzzEntry {
  u64 w0 = 0, w1 = 0;
  u64 meta = 0;
  std::array<u64, 4> ev{};

  u64 before() const { return meta & tsMask; }
  int n() const { return static_cast<int>((meta >> buzzNShift) & 31); }
  int head() const { return static_cast<int>((meta >> buzzHShift) & 15); }
  u64 units() const { return (meta >> buzzUShift) & 31; }
  u64 step(int k) const { return (ev[k / 4] >> (16 * (k % 4))) & 0xFFFF; }
  u64 stepAt(int k, u64 rq) const;
  int count(u64 now, u64 window, u64 q) const;
  bool covers(u64 now, u64 window) const;
  void log(u64 t, u64 q, u64 horizon);
};
static_assert(sizeof(BuzzEntry) == 56);

struct BuzzBlock {
  std::array<u64, buzzWays> ids{};
  std::array<BuzzEntry, buzzWays> e{};
};
static_assert(sizeof(BuzzBlock) == 512);

struct BuzzTable {
  BuzzTable(u64 blocks, u64 q);
  std::pair<u64, PaddedMutex*> block(u64 h, u64 id);
  bool mayHold(u64 k) const { return ((used[k >> 6].load() >> (k & 63)) & 1) != 0; }
  void leave(u64 k, int i);
  void mark(u64 k);

  AlignedBuf<BuzzBlock> b;
  AlignedBuf<AU64> used;
  u64 q;  // ticks per step
  std::array<PaddedMutex, buzzStripes> mu;
  AU64 given{0}, dropped{0}, decided{0};
};

// QuantilePad is AutoPad's running estimate (ct.go), in ps.
struct alignas(64) QuantilePad {
  std::atomic<i64> q{0};
};

// ---- test hooks and test-wide switches (Go: package variables) ----
// Each hook, if set, runs at the point the Go reference's hook of the same
// name runs (go/filter.go lists them).
inline std::function<void()> testHookCopied, testHookVictim, testHookTaken, testHookStamped, testHookStamping,
    testHookPlacing, testHookSweeping, testHookGhostRead, testHookGhostAdding, testHookGhostFloor;
inline std::function<void(i64)> testHookPicked, testHookInstalling, testHookBumping, testHookBumped, testHookOpenSeen,
    testHookSweepSeen, testHookSweepRead, testHookHotAdding;
// forceCT turns ConstantTime on in every filter (tests: TF_CT=1).
inline bool forceCT = false;
// forceBG turns Background on in every filter (tests: TF_BG=1).
inline bool forceBG = false;
// buzzOff turns buzz entries off in every filter (tests).
inline bool buzzOff = false;

// condMu serializes conditional touches of keys sharing its stripe (the
// hash's top 6 bits), across filters (touchif.go).
std::array<PaddedMutex, 64>& condMu();

// Result of read and recall: a slot's words and where they came from.
struct Found {
  u64 w0 = 0, w1 = 0;
  int src = 0;
};

// Core is Go's Filter.
struct Core {
  explicit Core(const Config& cfg);
  ~Core();
  Core(const Core&) = delete;
  Core& operator=(const Core&) = delete;

  // ---- filter.go ----
  void close();
  u64 now() const { return clock(); }
  u64 ticks(Duration d) const;
  u64 hash(std::string_view key) const;
  u64 hashUint64(u64 k) const;
  void hashUint64s(std::span<const u64> keys, std::span<u64> out) const;
  bool containsAt(u64 h, u64 window, u64 now);
  i64 occupancy(u64 now) const;
  i64 slots() const { return static_cast<i64>(nwords / 2); }
  i64 bytes() const;
  u64 floored() const;
  u64 demoted() const;
  i64 hotInUse() const;

  // ---- slots.go ----
  MoveCount& moveOf(u64 tg) { return moves[tg & ((u64{1} << claimStripeBits) - 1)]; }
  bool slotWords(i64 i, u64 fpd, u64 tg, u64& w0, u64& w1) const;
  bool markSat(i64 i, u64 fpd, bool on);
  i64 locate(u64 h, u64 fp, u64 tg) const;
  std::pair<i64, u64> locateAt(u64 h, u64 fp, u64 tg) const;
  std::pair<i64, u64> locateBoth(u64 h, u64 fp, u64 tg) const;
  i64 find(u64 line, u64 fpd, u64 tg) const;
  u64 primary(u64 h) const { return mulHi(h << 28, lines); }
  u64 alt(u64 line, u64 fp) const;
  u64 stashID(u64 h, u64 fp, u64 tg) const { return (primary(h) << 28) | (fp << tagBits) | tg; }
  u64 slotID(u64 line, u64 w0, u64 w1) const;

  // ---- register.go ----
  u64 onsetOf(u64 now, i64 u) const;
  std::pair<u64, bool> nextReg(u64 old, u64 tg, u64 last, u64 now, bool reset) const;
  std::pair<u64, bool> nextRegU(u64 old, u64 tg, u64 last, u64 now, i64 lastU, i64 nowU, bool reset) const;
  // bump: ok is false if the slot was handed to another key meanwhile.
  bool bump(i64 i, u64 fpd, u64 tg, u64 now, i64 nu, u64 last, i64 lu, bool reset, u64& w1, u64& ref, bool& overflow);
  bool unpoint(i64 i, u64 w0, u64 old, u64 fpd, u64 tg, u64 now, i64 nu, u64& w1, u64& ref);
  bool strayed(i64 i, u64 tg) const;
  u64 mass(u64 id, u64 w1, u64 last, u64 now) const;
  u64 viewMass(const View& v, u64 now) const;
  std::pair<Result, bool> estimate(u64 id, u64 w1, u64 last, u64 now, u64 window, bool sat) const;
  i64 unitOf(u64 t) const;

  // ---- touch.go ----
  Result count(u64 h, u64 window, u64 now, i64 i0);
  Result estimateAt(u64 h, u64 w1, u64 last, u64 now, u64 window, bool sat);
  Found read(u64 h, u64 now, i64 i0);
  i64 lookupStable(u64 h, u64 fp, u64 tg, bool both, u64& w0, u64& w1);
  bool sketchLive(u64 now) const;
  void addSketch(Result& r, u64 h, u64 window, u64 now) const;
  Result touch(u64 h, u64 window, u64 now, bool wantCount, i64 i0);
  void farAhead(i64 i, i64 nu, i64 lu, bool stale, u64& old, bool& far, bool& wait) const;
  bool stamp(i64 i, u64 w0, u64 p, u64 tg, u64 now);
  bool realign(u64 h, i64 i, u64 w0, u64 old, u64 tg, u64 now, i64 nu, i64 lu, u64& w1, bool& over);
  bool realign2U(i64 i, u64 w0, u64 old, u64 tg, u64 now, i64 nu, i64 lu, u64& w1, bool& over);

  // ---- cuckoo.go ----
  using PutFunc = std::function<bool(i64 i, u64 w0, u64 w1, u64 mine0, u64 mine1)>;
  Result miss(u64 h, u64 fp, u64 tg, u64 window, u64 now, bool wantCount, u64 s0, u64 s1, int src);
  i64 claim(u64 h, u64 fp, u64 w1, u64 now, u64 stamp, u64 sat, int mode);
  i64 claimAt(std::array<u64, 2> lns, std::array<u64, 2> mine, u64 w1, u64 now, u64 stripe, int mode,
              const PutFunc* put);
  i64 evict(u64 line, u64 mine0, u64 mine1, u64 now, u64 stripe, const PutFunc* put, i64 victim, u64 victimW0,
            u64 victimW1);
  bool putNew(const PutFunc* put, i64 i, u64 w0, u64 w1, u64 mine0, u64 mine1);
  bool putOver(const PutFunc* put, i64 i, u64 w0, u64 w1, u64 mine0, u64 mine1);
  void warmAlts(std::array<u64, 2> lns);
  void warmLine(u64 line);
  bool install(i64 i, u64 w0, u64 w1, u64 mine0, u64 mine1);
  bool relocate(i64 i, u64 line, u64 mine0, u64 mine1, u64 now, int depth, int& budget, MoveCount* parent,
                const PutFunc* put);
  void abandon(i64 d);

  // ---- stash.go ----
  Found recall(u64 h, u64 fp, u64 tg, u64 now, bool take);
  bool stashMay(u64 h, u64 fp, u64 tg, u64 now) const;
  bool settled(MoveCount& m, u64 b, u64 d) const;
  bool roomGet(StashTable* p, u64 id, u64 now, MoveCount* mv, u64& w0, u64& w1, bool& retry);
  bool lostRecently(u64 now) const;
  void placeRoomKey(StashTable* p, StashEntry e);
  void bgKick();
  void bgLoop();
  void bgPlace();
  void allocStash();
  StashTable* waitTable();
  void stashPut(u64 id, u64 w0, u64 w1, u64 now) { stashPutAs(id, w0, w1, now, false); }
  void raiseStashUntil(u64 until);
  void stashPutAs(u64 id, u64 w0, u64 w1, u64 now, bool wait);
  bool stashWait(u64 id, u64 w0, u64 w1, u64 now);
  void putBack(u64 id, u64 w0, u64 w1, u64 now);
  void stashWrite(StashTable* p, StashEntry* e, StashEntry ne);
  bool stashGet(u64 id, u64 now, bool take, u64& w0, u64& w1) { return tableGet(stash.load(), id, now, take, w0, w1); }
  bool tableGet(StashTable* p, u64 id, u64 now, bool take, u64& w0, u64& w1);
  void dropped(u64 now);

  // ---- ct.go ----
  std::pair<Clock::time_point, Duration> ctBegin(int op, u64 now);
  void ctEnd(int op, u64 h, Clock::time_point t0, Clock::time_point s, Duration pad);
  void place(Clock::time_point t0, Duration budget, u64 now);
  bool placeNext(StashTable* p, u64 now);
  void placeSet(StashTable* p, u64 i, u64 now);
  void placeRoomSet(StashTable* p, u64 i);
  Result touchCT(u64 h, u64 window, u64 now, bool wantCount);

  // ---- batch.go ----
  void checkAndTouchBatch(std::span<const u64> hs, u64 window, u64 now, std::span<bool> out);
  void touchAndCountBatch(std::span<const u64> hs, u64 window, u64 now, std::span<bool> seen, std::span<i64> counts);
  void velocityCountBatch(std::span<const u64> hs, u64 window, u64 now, std::span<i64> counts);
  void warm(std::span<const u64> hs, std::array<i64, batchChunk>& at);

  // ---- decay.go ----
  i64 continuousDecay() { return decayStep(clock(), 4); }
  i64 decayStep(u64 now, u64 budget);
  void sweepLine(u64 line, u64 now);

  // ---- hot.go ----
  HotTable* hotTable();
  bool demoteSlot(AU64* slot, u64 p, u64 nw, i64 u);
  u64 hotAlloc(u64 id, View& v, u64 now);
  std::pair<View, bool> hotClose(u64 idx, u64 id);
  bool promote(i64 i, u64 w0, u64 old, u64 tg, u64 last, u64 now, u64& w1);
  bool promoteHeld(i64 i, u64 w0, u64 old, u64 tg, u64 last, u64 now, u64& w1);
  u64 hotEntryFor(i64 i, u64 w0, u64 old, u64 tg, u64 last, u64 now, u64& idx, u64& id);
  std::pair<View, bool> viewOf(u64 id, u64 w1, u64 last) const;
  bool hotGone(u64 id, u64 w1) const;
  void slotDrop(u64 id, u64 w1);
  bool hotAdd(u64 w1, u64 id, u64 now, AU64* slot);

  // ---- overflow.go ----
  void spill(u64 id, u64 w0, u64 w1, u64 now);
  Ghost* ghostTable();
  Sketch* overflow();
  u64 skBytes() const { return skShape.bytes(skW); }

  // ---- buzz.go, touchif.go ----
  BuzzTable* buzzTable();
  i64 certain(u64 w1, u64 last, u64 now, u64 window) const;
  std::pair<Result, bool> touchIfBuzz(u64 h, u64 window, u64 limit, u64 now);
  bool uncertain(bool plain, const Result& r, u64 w1, u64 last, u64 now, u64 win, i64 over, u64 limit) const;
  std::pair<Result, bool> touchIfUnderAt(u64 h, u64 window, u64 limit, u64 now);

  // ---- merge.go ----
  void merge(Core& g);

  // ---- snapshot.go ----
  std::vector<u64> snapShape() const;
  i64 writeTo(std::ostream& w);
  i64 readFrom(std::istream& r);
  std::string snapSane() const;
  void snapScrub();
  u64 continued(u64 oldNow, i64 wall) const;
  std::pair<u64, u64> fromV2(u64 w0, u64 w1) const;

  // ---- fields (go/filter.go's Filter) ----
  AlignedBuf<AU64> words;  // 64-byte aligned, lines*wordsPerLine
  std::size_t nwords = 0;
  u64 lines = 0;    // any size; indexed by multiply-shift, not masking
  u64 horizon = 0;  // ticks
  u64 unit = 0;     // ticks per velocity register unit
  u64 unitInv = 0;  // floor(2^64 / unit) + 1, for unitOf
  Duration res{0};
  std::function<u64()> clock;
  // Read by calls, written only by the constructor (hot: by the first
  // promotion): kept apart from the counters below.
  bool ct = false;
  Duration pad{0};
  std::atomic<HotTable*> hot{nullptr};
  i64 hotN = 0;
  bool bg = false;

  alignas(64) AU64 cursor{0};  // ContinuousDecay progress, in lines
  alignas(64) AU64 coarse{0};  // cached tick for CoarseClock
  std::thread coarseThread;
  bool coarseOn = false;  // CoarseClock: the clock is the cached tick (Go: f.stop != nil), even after close
  std::mutex stopMu;
  std::condition_variable stopCv;
  bool stopping = false;
  void* coarseStop = nullptr;  // Windows: the event that ends the coarse thread's timer wait (src/filter.cpp)
  AU64 evictions{0};
  AU64 relocations{0};
  alignas(64) std::atomic<std::uint32_t> pressure{0};  // EWMA of relocation-search failure
  alignas(64) std::array<PaddedMutex, u64{1} << claimStripeBits> claimMu;
  std::array<MoveCount, u64{1} << claimStripeBits> moves;

  siphash::Key sip;  // Config::Key, or a random per-filter key
  std::atomic<StashTable*> stash{nullptr};
  u64 stashN = 0;  // sets of stashWays entries
  alignas(64) AU64 stashUntil{0};
  AU64 lostAt{0};
  AU64 lost{0};
  AU64 restored{0};
  AU64 folded{0};
  std::array<PaddedMutex, stashStripes> stashMu;
  std::atomic<Sketch*> sk{nullptr};  // count-min overflow (cms), nullptr until the first spill
  std::atomic<Ghost*> gh{nullptr};
  bool cms = false;  // overflow to the count-min sketch, not the ghost table (experiments)
  SketchShape skShape{};
  int ghChoices = 0;
  int ghSub = 0;
  bool autoPad = false;
  std::array<QuantilePad, 2> padQ;
  std::mutex skMu;
  u64 skW = 0;  // overflow size in 16 B units; 0: no overflow
  u64 spillMin = 0;
  AU64 shed{0};
  std::atomic<i64> deferred{0};  // keys waiting for a slot
  AU64 waited{0};
  std::atomic<i64> placeNs{0};
  AU64 promoted{0};
  AU64 hotRefused{0};
  std::atomic<bool> placing{false};
  bool bgManual = false;  // tests: Background without the placer (the test places)
  alignas(64) std::atomic<StashTable*> room{nullptr};
  u64 roomN = 0;
  bool ownClock = false;
  AU64 clockAdd{0};
  std::array<PaddedMutex, stashStripes> roomMu;
  std::atomic<BuzzTable*> buzz{nullptr};

  // Owners of the tables the atomic pointers above point to (Go's garbage
  // collector): freed with the filter.
  std::vector<std::unique_ptr<StashTable>> ownStash;
  std::unique_ptr<HotTable> ownHot;
  std::unique_ptr<Ghost> ownGhost;
  std::unique_ptr<Sketch> ownSketch;
  std::unique_ptr<BuzzTable> ownBuzz;

  // The Background placer (Go: a goroutine started on demand that exits
  // after bgLinger idle naps): one thread, started on the first wait and
  // parked on a condition variable while it would have exited.
  std::thread bgThread;
  std::mutex bgMu;
  std::condition_variable bgCv;
  bool bgWake = false;
  bool bgStop = false;
};

}  // namespace clepsydra::detail
