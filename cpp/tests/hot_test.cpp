// go/hot_test.go: hot entries: promotion under races, demotion, exhaustion
// and reclaim.
#include <atomic>
#include <cstdlib>
#include <thread>
#include <vector>

#include "check.hpp"
#include "helpers.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace std::chrono_literals;

// TestHotPromotionUnderRaces: many workers send the same keys past the
// register's capacity at once, so promotions race each other, while a fill to
// 98% slot load relocates and evicts them: every key must end with its exact
// count, in one slot.
TEST(TestHotPromotionUnderRaces) {
  int rounds = check::short_() ? 6 : 40;
  const int keys = 128, workers = 8, per = 200;
  const u64 at = 60'000;  // a multiple of 8 units, where a 60 s window covers every bucket whole
  for (int round = 0; round < rounds; round++) {
    i64 hotN = 2 * keys;
    if (const char* e = std::getenv("TF_HOTOFF"); e && *e) hotN = -1;
    Config cfg = cfgOf(4000, 1min, 1ms);
    cfg.Hot = hotN;
    Filter f(cfg);
    Core& c = core(f);
    u64 salt = static_cast<u64>(round) << 40;
    std::vector<u64> hs(keys);
    for (int i = 0; i < keys; i++) {
      hs[i] = mix64(salt + static_cast<u64>(i));
      for (int k = 0; k < 3; k++) f.CheckAndTouchAt(hs[i], 0, at);  // heavier than the fill
    }
    std::atomic<i64> sent{0};
    std::vector<std::thread> ts;
    for (int w = 0; w < workers; w++)
      ts.emplace_back([&, w] {
        for (int n = 0; n < per * keys / 2; n++) {
          f.CheckAndTouchAt(hs[static_cast<std::size_t>((n + w * 7) % keys)], 0, at);
          sent.fetch_add(1);
        }
      });
    i64 fill = static_cast<i64>(static_cast<double>(f.Slots()) * 0.98) - keys;
    for (u64 cc = 0; cc < static_cast<u64>(fill); cc++) f.CheckAndTouchAt(mix64(salt + (u64{1} << 36) + cc), 0, at);
    for (auto& t : ts) t.join();
    i64 total = 0;
    for (int i = 0; i < keys; i++) {
      u64 h = hs[i];
      u64 fp = fingerprint(h), tg = tag(h);
      u64 p = c.primary(h);
      if (c.find(p, fp << 1, tg) >= 0 && c.find(c.alt(p, fp), (fp << 1) | 1, tg) >= 0)
        FATALF("round %d key %d in two slots", round, i);
      Result r = f.CountAt(h, 60'000, at);
      if (r.Saturated && hotN > 0)
        FATALF("round %d key %d Saturated with %d hot entries free", round, i, 2 * keys - f.HotInUse());
      total += r.Count;
    }
    if (i64 want = sent.load() + 3 * keys; total != want)
      FATALF("round %d: counted %d of %d events (promoted %d, refused %d, relocations %d, evictions %d)", round,
             total, want, f.Promoted(), f.HotRefused(), f.Relocations(), f.Evictions());
  }
}

// TestHotDemotion: a key that bursts past the register is promoted; once it
// slows to a trickle its entry is handed back as a register, and its counts
// stay exact (against an exact log) before, across and after both moves.
TEST(TestHotDemotion) {
  u64 now = 1;
  Config cfg = cfgOf(1000, 1min, 1ms);
  cfg.Clock = [&now] { return now; };
  Filter f(cfg);
  u64 h = mix64(7);
  std::vector<u64> log;
  auto checkAll = [&](const char* when) {
    for (u64 w : {u64{1'000}, u64{5'000}, u64{15'000}, u64{60'000}}) {
      i64 exact = 0;
      for (u64 ts : log)
        if (now - ts <= w) exact++;
      Result r = f.CountAt(h, w, now);
      if (!r.Saturated && r.Upper < exact) FATALF("%s, window %ds: exact %d, got %+v", when, w / 1000, exact, r);
    }
  };
  for (int k = 0; k < 200; k++) {  // a burst in one tick: promoted
    f.TouchAt(h, 0, now);
    log.push_back(now);
  }
  if (f.Promoted() != 1 || f.HotInUse() != 1) FATALF("burst: promoted %d, in use %d", f.Promoted(), f.HotInUse());
  checkAll("after the burst");
  for (; now < 3 * 60'000; now += 1000) {  // then one event a second
    f.TouchAt(h, 0, now);
    log.push_back(now);
    checkAll("trickle");
  }
  if (f.Demoted() != 1 || f.HotInUse() != 0) FATALF("trickle: demoted %d, in use %d", f.Demoted(), f.HotInUse());
  if (Result r = f.CountAt(h, 60'000, now); r.Count != 60 || r.Saturated)
    FATALF("after demotion, 60 s window: %+v, want Count 60", r);
}

// TestHotTableExhaustion: with fewer hot entries than heavy keys, the rest
// saturate as without hot entries (flagged), HotRefused counts them, and
// entries whose history has aged out are taken by new keys.
TEST(TestHotTableExhaustion) {
  u64 now = 1;
  Config cfg = cfgOf(1000, 1min, 1ms);
  cfg.Hot = 4;
  cfg.Clock = [&now] { return now; };
  Filter f(cfg);
  const int heavy = 10;
  std::vector<i64> sent(heavy);
  for (; now < 60'000; now += 10) {  // 100 events per second per key
    for (int k = 0; k < heavy; k++) {
      f.TouchAt(mix64(static_cast<u64>(k)), 0, now);
      sent[k]++;
    }
  }
  int exact = 0, sat = 0;
  for (int k = 0; k < heavy; k++) {
    Result r = f.CountAt(mix64(static_cast<u64>(k)), 60'000, now);
    if (r.Saturated) {
      sat++;
    } else if (r.Upper < sent[k] || r.Count < sent[k] * 99 / 100 || r.Count > sent[k] * 101 / 100) {
      ERRORF("key %d unflagged: sent %d, got %+v", k, sent[k], r);
    } else {
      exact++;
    }
  }
  if (exact != 4 || sat != heavy - 4 || f.HotRefused() == 0 || f.HotInUse() != 4)
    FATALF("exact %d saturated %d (want 4, %d); refused %d, in use %d", exact, sat, heavy - 4, f.HotRefused(),
           f.HotInUse());
  // A horizon and two units of silence later the entries are dead: new heavy
  // keys take them.
  now += 60'000 + 2 * 2500 + 1;
  u64 before = f.Promoted();
  for (int k = 0; k < 40; k++)
    for (int j = 0; j < 4; j++) f.TouchAt(mix64(static_cast<u64>(100 + j)), 0, now);
  if (u64 got = f.Promoted() - before; got != 4)
    FATALF("after the horizon %d of 4 new heavy keys were promoted (in use %d, refused %d)", got, f.HotInUse(),
           f.HotRefused());
  for (int k = 0; k < 4; k++) {
    if (Result r = f.CountAt(mix64(static_cast<u64>(100 + k)), 60'000, now); r.Count != 40 || r.Saturated)
      ERRORF("new heavy key %d: %+v, want Count 40", k, r);
  }
}

// TestRestoreAfterHotEntryReclaimed: a hot key K is evicted to the stash, its
// stash entry pointing at its hot entry. A promotion at a clock more than a
// horizon past K's newest event reclaims the entry for another key. K's next
// touch, at a clock behind that one but within a horizon of K's last touch,
// restores K's history: the entry is no longer K's. That touch must still be
// counted (claims A25).
TEST(TestRestoreAfterHotEntryReclaimed) {
  if (forceCT || forceBG) SKIP("ConstantTime and Background inserts don't install on the call path");
  Config cfg = cfgOf(1000, 1min, 1ms);
  cfg.Hot = 1;
  Filter f(cfg);
  Core& c = core(f);
  const u64 t0 = 60'000;
  u64 k = mix64(1), l = mix64(2);
  for (int i = 0; i < 32; i++) f.TouchAt(k, 0, t0);  // past the register's 31 events in a unit: promoted
  if (f.Promoted() != 1) FATALF("K not promoted (%d)", f.Promoted());
  // Evict K as claimAt does: its words to the stash, its slot freed.
  u64 fp = fingerprint(k), tg = tag(k);
  i64 i = c.locate(k, fp, tg);
  u64 w0 = c.words[static_cast<std::size_t>(i)].load(), w1 = c.words[static_cast<std::size_t>(i) + 1].load();
  if (!isHot(w1)) FATALF("K's slot holds no hot entry");
  c.stashPut(c.stashID(k, fp, tg), w0, w1, t0);
  c.words[static_cast<std::size_t>(i)].store(0);
  c.words[static_cast<std::size_t>(i) + 1].store(0);
  u64 tL = t0 + c.horizon + 3 * c.unit;  // K's entry is past the horizon here
  for (int j = 0; j < 32; j++) f.TouchAt(l, 0, tL);
  if (f.Promoted() != 2) FATALF("L not promoted (%d): K's entry not reclaimed", f.Promoted());
  u64 tK = t0 + c.horizon - 1;  // K's clock: behind L's, within a horizon of K's last touch
  f.TouchAt(k, 0, tK);
  f.TouchAt(k, 0, tK);
  if (Result r = f.CountAt(k, c.horizon, tK); r.Upper < 2 && !r.Saturated)
    FATALF("K reads %+v after two touches, want Upper >= 2", r);
}

namespace {
// hotReclaimSetup: a filter with one hot entry; K promoted at t0 and evicted as
// claimAt does (words to the stash, slot freed); both of K's lines then full of
// entries live until t0 + horizon. reclaim promotes another key at a clock
// more than a horizon past K's newest event, which takes K's entry. tK is a
// clock behind that one, within a horizon of K's last touch.
struct HotReclaim {
  std::unique_ptr<Filter> f;
  u64 k = 0, tK = 0;
  std::function<void()> reclaim;
};

HotReclaim hotReclaimSetup(bool bg) {
  HotReclaim s;
  Config cfg = cfgOf(1000, 1min, 1ms);
  cfg.Hot = 1;
  cfg.Clock = [] { return u64{1}; };
  cfg.Background = bg;
  s.f = std::make_unique<Filter>(cfg);
  Filter& f = *s.f;
  Core& c = core(f);
  c.bgManual = bg;
  const u64 t0 = 60'000;
  s.k = mix64(1);
  for (int i = 0; i < 32; i++) f.TouchAt(s.k, 0, t0);  // past the register's 31 events in a unit: promoted
  u64 fp = fingerprint(s.k), tg = tag(s.k);
  i64 i = c.locate(s.k, fp, tg);
  if (f.Promoted() != 1 || i < 0 || !isHot(c.words[static_cast<std::size_t>(i) + 1].load()))
    FATALF("K not promoted (%d) or not in the table", f.Promoted());
  c.stashPut(c.stashID(s.k, fp, tg), c.words[static_cast<std::size_t>(i)].load(),
             c.words[static_cast<std::size_t>(i) + 1].load(), t0);
  c.words[static_cast<std::size_t>(i)].store(0);
  c.words[static_cast<std::size_t>(i) + 1].store(0);
  u64 p = c.primary(s.k);
  for (u64 line : {p, c.alt(p, fp)}) {
    for (u64 j = u64{1} << 40; countLine(c, line, t0) < 4; j++) {
      if (u64 h = mix64(j); c.primary(h) == line) f.TouchAt(h, 0, t0);
    }
  }
  u64 l = mix64(2), tL = t0 + c.horizon + 3 * c.unit;
  Filter* fp2 = &f;
  s.reclaim = [fp2, l, tL] {
    for (int j = 0; j < 32; j++) fp2->TouchAt(l, 0, tL);
    if (fp2->Promoted() != 2) ERRORF("L not promoted (%d): K's entry not reclaimed", fp2->Promoted());
  };
  s.tK = t0 + c.horizon - 1;
  return s;
}
}  // namespace

// TestHotEntryReclaimedDuringRestore: as TestRestoreAfterHotEntryReclaimed,
// but the entry is reclaimed while K's restoring touch is in its claim
// (relocating an entry of K's full lines to make room), after the check that
// its entry is still K's. K's touch must still be counted.
TEST(TestHotEntryReclaimedDuringRestore) {
  if (forceCT || forceBG) SKIP("ConstantTime and Background inserts don't search on the call path");
  Hooks hooks;
  HotReclaim s = hotReclaimSetup(false);
  bool fired = false;
  auto hook = [&] {
    if (!fired) {
      fired = true;
      s.reclaim();
    }
  };
  testHookCopied = hook;
  testHookVictim = hook;
  s.f->TouchAt(s.k, 0, s.tK);
  testHookCopied = nullptr;
  testHookVictim = nullptr;
  if (!fired) FATALF("K's restore neither relocated nor evicted");
  s.f->TouchAt(s.k, 0, s.tK);
  if (Result r = s.f->CountAt(s.k, core(*s.f).horizon, s.tK); r.Upper < 2 && !r.Saturated)
    FATALF("K reads %+v after two touches, want Upper >= 2", r);
}

// TestHotEntryReclaimedBeforeWait: with Background, K's restoring touch finds
// its lines full and waits in the wait room, after its entry was reclaimed. It
// must wait with this touch counted, not the dead entry.
TEST(TestHotEntryReclaimedBeforeWait) {
  if (forceCT) SKIP("Background has no effect with ConstantTime");
  HotReclaim s = hotReclaimSetup(true);
  s.reclaim();
  Filter& f = *s.f;
  DonePtr done = go([&] {
    f.TouchAt(s.k, 0, s.tK);
    f.TouchAt(s.k, 0, s.tK);
  });
  if (!done->wait(5s)) FATALF("K's touches did not return");
  if (f.Waiting() != 1) ERRORF("%d keys waiting, want K", f.Waiting());
  if (Result r = f.CountAt(s.k, core(f).horizon, s.tK); r.Upper < 2 && !r.Saturated)
    FATALF("K reads %+v after two touches, want Upper >= 2", r);
}
