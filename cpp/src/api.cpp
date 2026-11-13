// The public clepsydra::Filter: each method is the Go reference's exported
// method of the same name, forwarding to detail::Core.
#include "core.hpp"

namespace clepsydra {

Filter::Filter(const Config& cfg) : c_(std::make_unique<detail::Core>(cfg)) {}
Filter::~Filter() = default;

void Filter::Close() { c_->close(); }
std::uint64_t Filter::Now() const { return c_->now(); }
std::uint64_t Filter::Ticks(Duration d) const { return c_->ticks(d); }
std::uint64_t Filter::Hash(std::string_view key) const { return c_->hash(key); }
std::uint64_t Filter::HashUint64(std::uint64_t k) const { return c_->hashUint64(k); }
void Filter::HashUint64s(std::span<const std::uint64_t> keys, std::span<std::uint64_t> out) const {
  c_->hashUint64s(keys, out);
}

// Touch records an occurrence of key now and returns the full Result for
// window: TouchAndCount plus the trust flags.
Result Filter::Touch(std::string_view key, Duration window) {
  return c_->touch(c_->hash(key), c_->ticks(window), c_->now(), true, -1);
}
Result Filter::TouchAt(std::uint64_t h, std::uint64_t window, std::uint64_t now) {
  return c_->touch(h, window, now, true, -1);
}
// Count is VelocityCount with the trust flags of Touch. It does not record an
// occurrence.
Result Filter::Count(std::string_view key, Duration window) {
  return c_->count(c_->hash(key), c_->ticks(window), c_->now(), -1);
}
Result Filter::CountAt(std::uint64_t h, std::uint64_t window, std::uint64_t now) {
  return c_->count(h, window, now, -1);
}
bool Filter::CheckAndTouch(std::string_view key, Duration window) {
  return c_->touch(c_->hash(key), c_->ticks(window), c_->now(), false, -1).Seen;
}
bool Filter::CheckAndTouchAt(std::uint64_t h, std::uint64_t window, std::uint64_t now) {
  return c_->touch(h, window, now, false, -1).Seen;
}
std::int64_t Filter::VelocityCount(std::string_view key, Duration window) {
  return c_->count(c_->hash(key), c_->ticks(window), c_->now(), -1).Count;
}
std::int64_t Filter::VelocityCountAt(std::uint64_t h, std::uint64_t window, std::uint64_t now) {
  return c_->count(h, window, now, -1).Count;
}
std::pair<bool, std::int64_t> Filter::TouchAndCount(std::string_view key, Duration window) {
  Result r = c_->touch(c_->hash(key), c_->ticks(window), c_->now(), true, -1);
  return {r.Seen, r.Count};
}
std::pair<bool, std::int64_t> Filter::TouchAndCountAt(std::uint64_t h, std::uint64_t window, std::uint64_t now) {
  Result r = c_->touch(h, window, now, true, -1);
  return {r.Seen, r.Count};
}
bool Filter::ContainsAt(std::uint64_t h, std::uint64_t window, std::uint64_t now) {
  return c_->containsAt(h, window, now);
}

void Filter::CheckAndTouchBatch(std::span<const std::uint64_t> hs, std::uint64_t window, std::uint64_t now,
                                std::span<bool> out) {
  c_->checkAndTouchBatch(hs, window, now, out);
}
void Filter::TouchAndCountBatch(std::span<const std::uint64_t> hs, std::uint64_t window, std::uint64_t now,
                                std::span<bool> seen, std::span<std::int64_t> counts) {
  c_->touchAndCountBatch(hs, window, now, seen, counts);
}
void Filter::VelocityCountBatch(std::span<const std::uint64_t> hs, std::uint64_t window, std::uint64_t now,
                                std::span<std::int64_t> counts) {
  c_->velocityCountBatch(hs, window, now, counts);
}

std::pair<Result, bool> Filter::TouchIfUnder(std::string_view key, Duration window, std::uint64_t limit) {
  return c_->touchIfUnderAt(c_->hash(key), c_->ticks(window), limit, c_->now());
}
std::pair<Result, bool> Filter::TouchIfUnderAt(std::uint64_t h, std::uint64_t window, std::uint64_t limit,
                                               std::uint64_t now) {
  return c_->touchIfUnderAt(h, window, limit, now);
}

std::int64_t Filter::ContinuousDecay() { return c_->continuousDecay(); }
void Filter::Merge(Filter& g) { c_->merge(*g.c_); }
std::int64_t Filter::WriteTo(std::ostream& w) { return c_->writeTo(w); }
std::int64_t Filter::ReadFrom(std::istream& r) { return c_->readFrom(r); }

std::uint64_t Filter::Evictions() const { return c_->evictions.load(); }
std::uint64_t Filter::Relocations() const { return c_->relocations.load(); }
std::uint64_t Filter::Lost() const { return c_->lost.load(); }
std::uint64_t Filter::Folded() const { return c_->folded.load(); }
std::uint64_t Filter::Shed() const { return c_->shed.load(); }
std::uint64_t Filter::Waited() const { return c_->waited.load(); }
std::int64_t Filter::Waiting() const { return c_->deferred.load(); }
std::uint64_t Filter::Restored() const { return c_->restored.load(); }
std::uint64_t Filter::Promoted() const { return c_->promoted.load(); }
std::uint64_t Filter::HotRefused() const { return c_->hotRefused.load(); }
std::uint64_t Filter::Demoted() const { return c_->demoted(); }
std::int64_t Filter::HotInUse() const { return c_->hotInUse(); }
std::uint64_t Filter::Floored() const { return c_->floored(); }
std::int64_t Filter::Occupancy(std::uint64_t now) const { return c_->occupancy(now); }
std::int64_t Filter::Slots() const { return c_->slots(); }
std::int64_t Filter::Bytes() const { return c_->bytes(); }

}  // namespace clepsydra
