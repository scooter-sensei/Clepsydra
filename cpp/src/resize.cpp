// go/resize.go: Resizable, a filter whose Capacity changes, the old filter
// read beside the new for a horizon, keys moved on their next touch (claims
// D28).
#include "core.hpp"

namespace clepsydra {

using detail::u64;

Resizable::Resizable(const Config& cfg) : cfg_(cfg) {
  if (cfg.Hot >= 0) throw Error("temporalfilter: Resizable needs hot entries off (Config.Hot < 0)");
  auto f = std::make_unique<Filter>(cfg);
  if (f->c_->ct || f->c_->bg) throw Error("temporalfilter: Resizable of a ConstantTime or Background filter");
  cur_ = std::move(f);
}

Resizable::~Resizable() = default;

// Resize starts a transition to a filter of the given Capacity at now.
void Resizable::Resize(std::int64_t capacity, std::uint64_t now) {
  std::lock_guard<std::mutex> lk(mu_);
  settle(now);
  if (old_) throw Error("temporalfilter: Resize during a transition");
  Config cfg = cfg_;
  cfg.Capacity = capacity;
  auto f = std::make_unique<Filter>(cfg);
  until_ = now + cur_->c_->horizon;
  old_ = std::move(cur_);
  cur_ = std::move(f);
}

void Resizable::settle(std::uint64_t now) {
  if (old_ && now > until_) old_.reset();
}

// move takes h's slot, live in the old table and absent from the new one,
// into the new table (as Merge installs a key), and clears it in the old.
void Resizable::move(std::uint64_t h, std::uint64_t now) {
  using namespace detail;
  u64 fp = fingerprint(h), tg = tag(h);
  Core& f = *cur_->c_;
  Core& o = *old_->c_;
  u64 w0, w1;
  if (f.lookupStable(h, fp, tg, false, w0, w1) >= 0) return;
  i64 i = o.lookupStable(h, fp, tg, false, w0, w1);
  if (i < 0 || (w1 >> tagShift) == tagBusy || isHot(w1) || age(now, w0 & tsMask) > o.horizon) return;
  bool ok = false;
  {
    std::lock_guard<PaddedMutex> lk(f.claimMu[h >> (64 - claimStripeBits)]);
    for (int k = 0; k < 8; k++) {
      if (f.claim(h, fp, w1, now, w0 & tsMask, w0 & satBit, claimFull) >= 0) {
        ok = true;
        break;
      }
    }
  }
  if (ok) {
    o.words[static_cast<std::size_t>(i) + 1].store(0);
    o.words[static_cast<std::size_t>(i)].store(0);
    moved_++;
  }
}

namespace {
Result sumResult(Result a, const Result& b) {
  a.Count += b.Count;
  a.Upper += b.Upper;
  a.Seen = a.Seen || b.Seen;
  a.Saturated = a.Saturated || b.Saturated;
  a.Restored = a.Restored || b.Restored;
  a.Degraded = a.Degraded || b.Degraded;
  return a;
}
}  // namespace

Result Resizable::TouchAt(std::uint64_t h, std::uint64_t window, std::uint64_t now) {
  std::lock_guard<std::mutex> lk(mu_);
  settle(now);
  if (!old_) return cur_->TouchAt(h, window, now);
  move(h, now);
  Result a = cur_->TouchAt(h, window, now);
  return sumResult(a, old_->CountAt(h, window, now));
}

Result Resizable::CountAt(std::uint64_t h, std::uint64_t window, std::uint64_t now) {
  std::lock_guard<std::mutex> lk(mu_);
  settle(now);
  Result r = cur_->CountAt(h, window, now);
  if (old_) r = sumResult(r, old_->CountAt(h, window, now));
  return r;
}

std::int64_t Resizable::VelocityCountAt(std::uint64_t h, std::uint64_t window, std::uint64_t now) {
  return CountAt(h, window, now).Count;
}

bool Resizable::ContainsAt(std::uint64_t h, std::uint64_t window, std::uint64_t now) {
  std::lock_guard<std::mutex> lk(mu_);
  settle(now);
  return cur_->ContainsAt(h, window, now) || (old_ && old_->ContainsAt(h, window, now));
}

std::int64_t Resizable::Bytes() {
  std::lock_guard<std::mutex> lk(mu_);
  std::int64_t n = cur_->Bytes();
  if (old_) n += old_->Bytes();
  return n;
}

std::uint64_t Resizable::Evictions() {
  std::lock_guard<std::mutex> lk(mu_);
  std::uint64_t n = cur_->Evictions();
  if (old_) n += old_->Evictions();
  return n;
}

}  // namespace clepsydra
