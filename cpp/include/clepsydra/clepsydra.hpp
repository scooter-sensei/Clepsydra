// Clepsydra: a fixed-memory temporal filter. "Seen key X in the last W?" and
// "how many times in the last W?", for any window W up to a configured
// horizon, chosen per query.
//
// This is the C++ port of the Go reference implementation (go/). It follows
// docs/specification/format.md and reproduces spec/vectors/ exactly; where the
// specification is silent, the Go code is normative, and this port keeps its
// names and structure (cpp/README.md has the type mapping). Every operation,
// field and counter below has the meaning docs/paper/04-operations.md gives
// it under the same name.
#pragma once

#include <array>
#include <chrono>
#include <cstdint>
#include <functional>
#include <iosfwd>
#include <memory>
#include <mutex>
#include <optional>
#include <span>
#include <stdexcept>
#include <string>
#include <string_view>
#include <utility>

namespace clepsydra {

// Duration is Go's time.Duration: a signed count of nanoseconds.
using Duration = std::chrono::nanoseconds;

// MaxTick is the latest tick a filter can record: a slot keeps its last touch
// in 46 bits, and a later tick would run into the slot's flag and fingerprint
// bits. The constructor rejects a Clock already past it, and a first touch (an
// insert), a touch that realigns its register and ContinuousDecay throw
// TickError past it, before taking any lock. A hit or a count doesn't check.
inline constexpr std::uint64_t MaxTick = (std::uint64_t{1} << 46) - 1;

// AutoPad as Config::ConstantTime pads each operation to a running estimate of
// its own work's 99.5th-percentile latency instead of a fixed duration.
inline constexpr Duration AutoPad{-1};

// Error is every error the Go reference returns as an error value: a refused
// Config, a refused Merge, a refused or damaged snapshot.
class Error : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};

// TickError is the Go reference's panic(errTick): a tick past MaxTick. It is
// thrown before any lock is taken, so a caller that catches it keeps a usable
// filter. (The constructor throws it too for a Clock already past MaxTick, as
// New returns errTick.)
class TickError : public Error {
 public:
  TickError() : Error("temporalfilter: tick past MaxTick (2^46-1): see Config.Resolution") {}
};

// Config sizes a Filter. Zero fields take their defaults
// (docs/specification/format.md, "Defaults at a glance").
struct Config {
  // Capacity is the expected number of distinct keys live within Horizon.
  std::int64_t Capacity = 0;
  // Horizon is the largest window any query may use.
  Duration Horizon{0};
  // Resolution is the clock tick. Default 1 µs. Ticks must stay at most
  // MaxTick: 2.2 years of the built-in clock at 1 µs, 19.5 hours at 1 ns.
  Duration Resolution{0};
  // Load is the target slot occupancy when Capacity keys are live. Default
  // 0.85, at most 1.
  double Load = 0;
  // Clock, if set, returns the current tick. It must be monotonic and stay at
  // most MaxTick. Default: monotonic time since construction, in Resolution
  // units, plus 1.
  std::function<std::uint64_t()> Clock;
  // CoarseClock, if > 0 and Clock is unset, makes the built-in clock a cached
  // value refreshed every CoarseClock by one background thread (an atomic
  // load instead of a clock read). Close stops it.
  Duration CoarseClock{0};
  // Key, if set, makes Hash and HashUint64 SipHash-2-4 under this 128-bit key:
  // a keyed PRF, the same in every process that uses it. Default: a random
  // per-filter secret key (cpp/README.md: SipHash-2-4 under a key drawn from
  // std::random_device, where Go uses maphash).
  std::optional<std::array<std::uint8_t, 16>> Key;
  // Stash is the number of eviction-stash entries (24 B each). Default (0):
  // one entry per 16 slots. Negative: none.
  std::int64_t Stash = 0;
  // Overflow sizes the overflow table in units of 16 B. Default (0): 5 units
  // per 9 slots. Negative: none (dropped histories are then lost).
  std::int64_t Overflow = 0;
  // SpillMin: a dropped history with fewer events than this in the horizon is
  // shed. Default (0): 4. 1 or less keeps every history.
  std::int64_t SpillMin = 0;
  // Hot is the number of hot entries (64 B each). Default (0): one per 128
  // slots, at least 16. Negative: none.
  std::int64_t Hot = 0;
  // ConstantTime, if > 0 or AutoPad, hides from response times whether a key
  // was seen (docs/paper/03-design.md §3.8).
  Duration ConstantTime{0};
  // Background, if set, keeps cuckoo searches off the call path
  // (docs/paper/03-design.md §3.4). No effect with ConstantTime.
  bool Background = false;
};

// Result is the full answer of Touch and Count, including how far it can be
// trusted (docs/paper/04-operations.md §4.2). Count and Upper are Go ints:
// 64-bit here.
struct Result {
  bool Seen = false;
  std::int64_t Count = 0;
  std::int64_t Upper = 0;
  bool Saturated = false;
  bool Restored = false;
  bool Degraded = false;

  friend bool operator==(const Result&, const Result&) = default;
};

namespace detail {
struct Core;
struct TestAccess;
}  // namespace detail

// Filter is safe for concurrent use. Hits never lock; first touches of a key
// serialize on a striped lock. It is neither copyable nor movable (its state
// is shared with its threads).
class Filter {
 public:
  // Go: New. Throws Error for a refused Config, TickError for a Clock already
  // past MaxTick.
  explicit Filter(const Config& cfg);
  ~Filter();
  Filter(const Filter&) = delete;
  Filter& operator=(const Filter&) = delete;

  // Close stops the CoarseClock thread, if any. The filter stays usable; its
  // clock just stops advancing. The destructor calls it.
  void Close();

  // Now returns the filter's current tick.
  std::uint64_t Now() const;
  // Ticks converts a duration to filter ticks (a negative one to 0).
  std::uint64_t Ticks(Duration d) const;

  // Hash hashes a string key for the *At methods, under the filter's secret key.
  std::uint64_t Hash(std::string_view key) const;
  // HashUint64 hashes an integer key for the *At methods. The *At methods
  // trust the hash they are given: pass them only Hash or HashUint64 values
  // of this filter.
  std::uint64_t HashUint64(std::uint64_t k) const;
  // HashUint64s sets out[i] = HashUint64(keys[i]) for every key (out must be
  // at least keys.size(); std::out_of_range otherwise).
  void HashUint64s(std::span<const std::uint64_t> keys, std::span<std::uint64_t> out) const;

  Result Touch(std::string_view key, Duration window);
  Result TouchAt(std::uint64_t h, std::uint64_t window, std::uint64_t now);
  Result Count(std::string_view key, Duration window);
  Result CountAt(std::uint64_t h, std::uint64_t window, std::uint64_t now);
  bool CheckAndTouch(std::string_view key, Duration window);
  bool CheckAndTouchAt(std::uint64_t h, std::uint64_t window, std::uint64_t now);
  std::int64_t VelocityCount(std::string_view key, Duration window);
  std::int64_t VelocityCountAt(std::uint64_t h, std::uint64_t window, std::uint64_t now);
  // TouchAndCount returns (seen within window before this touch, estimated
  // count within window including this touch).
  std::pair<bool, std::int64_t> TouchAndCount(std::string_view key, Duration window);
  std::pair<bool, std::int64_t> TouchAndCountAt(std::uint64_t h, std::uint64_t window, std::uint64_t now);
  bool ContainsAt(std::uint64_t h, std::uint64_t window, std::uint64_t now);

  // Batches: in the order of hs, a chunk at a time with the chunk's cache
  // misses overlapped. Each output span must be at least hs.size()
  // (std::out_of_range otherwise). Spans of bool: use a bool array, not
  // std::vector<bool>.
  void CheckAndTouchBatch(std::span<const std::uint64_t> hs, std::uint64_t window, std::uint64_t now,
                          std::span<bool> out);
  void TouchAndCountBatch(std::span<const std::uint64_t> hs, std::uint64_t window, std::uint64_t now,
                          std::span<bool> seen, std::span<std::int64_t> counts);
  void VelocityCountBatch(std::span<const std::uint64_t> hs, std::uint64_t window, std::uint64_t now,
                          std::span<std::int64_t> counts);

  // TouchIfUnder records an occurrence of key now only if the key is under
  // limit within window by the strict fail-closed recipe, and reports whether
  // it did (claims A45, A46).
  std::pair<Result, bool> TouchIfUnder(std::string_view key, Duration window, std::uint64_t limit);
  std::pair<Result, bool> TouchIfUnderAt(std::uint64_t h, std::uint64_t window, std::uint64_t limit,
                                         std::uint64_t now);

  // ContinuousDecay reclaims expired slots at a rate set by the clock (one
  // table pass per horizon, at most 4 lines per call). Returns lines swept.
  std::int64_t ContinuousDecay();

  // Merge adds g's keys to this filter (claims D24, D26, D29). Both filters
  // must be quiescent and built with the same Config. Throws Error when
  // refused (this filter unchanged) or when a key finds no slot (the keys
  // before it merged).
  void Merge(Filter& g);

  // WriteTo writes the filter's state (a quiescent filter) as a snapshot,
  // version 3 of the reference's format (claims D27, D29), and returns the
  // bytes written. ReadFrom restores one into a filter built with the same
  // Config that nothing has used yet, and returns the bytes read. Both throw
  // Error when refused; a ReadFrom that throws leaves the filter unspecified.
  std::int64_t WriteTo(std::ostream& w);
  std::int64_t ReadFrom(std::istream& r);

  // Counters for operators (docs/paper/04-operations.md §4.1).
  std::uint64_t Evictions() const;
  std::uint64_t Relocations() const;
  std::uint64_t Lost() const;
  std::uint64_t Folded() const;
  std::uint64_t Shed() const;
  std::uint64_t Waited() const;
  std::int64_t Waiting() const;
  std::uint64_t Restored() const;
  std::uint64_t Promoted() const;
  std::uint64_t HotRefused() const;
  std::uint64_t Demoted() const;
  std::int64_t HotInUse() const;
  std::uint64_t Floored() const;
  // Occupancy returns the number of slots holding a key live at now. O(table).
  std::int64_t Occupancy(std::uint64_t now) const;
  // Slots returns the table's slot count.
  std::int64_t Slots() const;
  // Bytes is the memory the filter holds now.
  std::int64_t Bytes() const;

 private:
  friend struct detail::TestAccess;
  friend class Resizable;
  std::unique_ptr<detail::Core> c_;
};

// Resizable is a filter that can change its Capacity without losing state
// (claims D28). Hot entries must be off (Config::Hot < 0); ConstantTime and
// Background are refused. Calls are serialized by one mutex.
class Resizable {
 public:
  explicit Resizable(const Config& cfg);
  ~Resizable();
  Resizable(const Resizable&) = delete;
  Resizable& operator=(const Resizable&) = delete;

  // Resize starts a transition to a filter of the given Capacity at now.
  void Resize(std::int64_t capacity, std::uint64_t now);
  Result TouchAt(std::uint64_t h, std::uint64_t window, std::uint64_t now);
  Result CountAt(std::uint64_t h, std::uint64_t window, std::uint64_t now);
  std::int64_t VelocityCountAt(std::uint64_t h, std::uint64_t window, std::uint64_t now);
  bool ContainsAt(std::uint64_t h, std::uint64_t window, std::uint64_t now);
  std::int64_t Bytes();
  std::uint64_t Evictions();

 private:
  friend struct detail::TestAccess;
  void settle(std::uint64_t now);
  void move(std::uint64_t h, std::uint64_t now);

  std::mutex mu_;
  Config cfg_;
  std::unique_ptr<Filter> cur_, old_;
  std::uint64_t until_ = 0;  // the transition ends after this tick
  std::int64_t moved_ = 0;   // keys moved to the new table
};

}  // namespace clepsydra
