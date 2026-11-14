// go/snapshot_test.go: snapshots (claims D27): answers across a restore, the
// format's refusals, its continuity across builds (toolchains, architectures,
// languages: a Go snapshot restores here) and its size.
#include <cstdlib>
#include <fstream>
#include <iterator>
#include <sstream>
#include <thread>

#include "check.hpp"
#include "crc32c.hpp"
#include "gorand.hpp"
#include "helpers.hpp"
#include "workload.hpp"

using namespace clepsydra;
using namespace clepsydra::detail;
using namespace helpers;
using namespace workload;
using namespace std::chrono_literals;

namespace {
Config snapCfg() { return cfgOf(512, 1min, 1ms); }
u64 snapKey(int k) { return mix64(static_cast<u64>(k) + 1); }

bool snapModes() { return forceCT || forceBG; }

std::string snapshotOf(Filter& f) {
  std::ostringstream o;
  f.WriteTo(o);
  return o.str();
}

// restore reads b into a new snapCfg filter; false (and err) if refused.
bool restore(Filter& g, const std::string& b, std::string* err = nullptr) {
  std::istringstream in(b);
  try {
    g.ReadFrom(in);
  } catch (const Error& e) {
    if (err) *err = e.what();
    return false;
  }
  return true;
}

// snapHalf runs the velocity workload's first half (to 150 s) into a new
// filter and returns it.
std::unique_ptr<Filter> snapHalf() {
  auto f = std::make_unique<Filter>(snapCfg());
  velWorkload(
      [&](int k, u64 at) {
        if (at < 150'000) f->TouchAt(snapKey(k), 60'000, at);
      },
      [](int, const std::vector<u64>&, int, u64) {});
  return f;
}

// snapRestBut runs the second half on f (from 150 s) and hashes every answer,
// Count left out of the hash unless count.
std::pair<u64, int> snapRestBut(Filter& f, bool count) {
  Fnv64a h;
  int n = 0;
  auto put = [&](Result r) {
    if (!count) r.Count = 0;
    h.write(goV(r));
    n++;
  };
  velWorkload(
      [&](int k, u64 at) {
        if (at >= 150'000) put(f.TouchAt(snapKey(k), 60'000, at));
      },
      [&](int k, const std::vector<u64>&, int wi, u64 now) {
        if (now >= 150'000) put(f.CountAt(snapKey(k), velWins[static_cast<std::size_t>(wi)], now));
      });
  return {h.sum(), n};
}

std::pair<u64, int> snapRest(Filter& f) { return snapRestBut(f, true); }

// v2Answers hashes CountAt of every workload key at every window, at three
// clocks after snapHalf's half (TestSnapshotV2).
std::pair<u64, int> v2Answers(Filter& f) {
  Fnv64a h;
  int n = 0;
  for (u64 now : {u64{149'999}, u64{150'700}, u64{152'400}}) {
    for (int k = 0; k < 2000; k++) {
      for (u64 w : velWins) {
        h.write(goV(f.CountAt(snapKey(k), w, now)) + ";");
        n++;
      }
    }
  }
  return {h.sum(), n};
}

std::string readFile(const std::string& path, bool& ok) {
  std::ifstream in(path, std::ios::binary);
  ok = static_cast<bool>(in);
  return std::string(std::istreambuf_iterator<char>(in), std::istreambuf_iterator<char>());
}
}  // namespace

// TestSnapshot runs tfbench's velocity workload once uninterrupted and once
// snapshotted at 150 s and restored into a new filter, at Capacity 512 so the
// stash, the overflow table and hot entries are all in use across the
// snapshot: every answer must be the same. Then the built-in clock: a filter
// restored 20 ms after its snapshot continues the old timeline.
TEST(TestSnapshot) {
  if (snapModes()) SKIP("snapshots refuse ConstantTime and Background filters");
  const std::vector<u64> wins{1'000, 5'000, 15'000, 30'000, 60'000};
  struct Run {
    std::vector<Result> ans;
    std::unique_ptr<Filter> f;
    std::size_t size = 0;
    i64 bytesAt = 0;
  };
  auto run = [&](u64 snapAt) {
    Run out;
    out.f = std::make_unique<Filter>(snapCfg());
    velWorkload([&](int k, u64 at) { out.ans.push_back(out.f->TouchAt(snapKey(k), 60'000, at)); },
                [&](int k, const std::vector<u64>&, int wi, u64 now) {
                  if (now == snapAt) {
                    std::string b = snapshotOf(*out.f);
                    out.size = b.size();
                    out.bytesAt = out.f->Bytes();
                    auto g = std::make_unique<Filter>(snapCfg());
                    std::string err;
                    if (!restore(*g, b, &err)) FATALF("%s", err);
                    out.f = std::move(g);
                  }
                  out.ans.push_back(out.f->CountAt(snapKey(k), wins[static_cast<std::size_t>(wi)], now));
                });
    return out;
  };
  Run u = run(0), r = run(150'000);
  int diff = 0;
  i64 first = -1;
  for (std::size_t i = 0; i < std::min(u.ans.size(), r.ans.size()); i++) {
    if (!(u.ans[i] == r.ans[i])) {
      diff++;
      if (first < 0) first = static_cast<i64>(i);
    }
  }
  Core &cu = core(*u.f), &cr = core(*r.f);
  LOGF("answers %d / %d, differing %d (first at %d); end: evictions %d / %d, folded %d / %d, promoted %d / %d, "
       "floored %d / %d",
       u.ans.size(), r.ans.size(), diff, first, u.f->Evictions(), r.f->Evictions(), cu.folded.load(),
       cr.folded.load(), cu.promoted.load(), cr.promoted.load(), u.f->Floored(), r.f->Floored());
  LOGF("snapshot %d bytes, Bytes() %d at the snapshot (%.3f%%)", r.size, r.bytesAt,
       100 * (static_cast<double>(r.size) - static_cast<double>(r.bytesAt)) / static_cast<double>(r.bytesAt));
  if (diff != 0 || u.ans.size() != r.ans.size()) ERRORF("a restore changed answers");
  if (r.f->Evictions() == 0 || cr.folded.load() == 0 || cr.promoted.load() == 0)
    ERRORF("the stash, overflow table or hot entries were not in use");
  Filter c(cfgOf(512, 1min));
  c.TouchAt(1, 0, c.Now());
  std::string b = snapshotOf(c);
  u64 old = c.Now();
  std::this_thread::sleep_for(20ms);
  Filter d(cfgOf(512, 1min));
  std::string err;
  if (!restore(d, b, &err)) FATALF("%s", err);
  u64 now = d.Now();
  LOGF("clock: old %d, restored %d (+%d ticks, want >= %d)", old, now, now - old, c.Ticks(20ms));
  if (now < old + c.Ticks(20ms)) ERRORF("the restored clock did not continue the old timeline");
}

// TestSnapshotCorrupt: a snapshot cut at every length to 512, every 997th and
// the last 16, one with a trailing byte, and one bit flipped at 2,000 seeded
// positions and at the first and last byte of every section: every one is
// refused.
TEST(TestSnapshotCorrupt) {
  if (snapModes()) SKIP("snapshots refuse ConstantTime and Background filters");
  auto f = snapHalf();
  std::string snap = snapshotOf(*f);
  {
    Filter g(snapCfg());
    std::string err;
    if (!restore(g, snap, &err)) FATALF("intact snapshot refused: %s", err);
  }
  auto word = [&](std::size_t i) {
    u64 x = 0;
    for (int k = 0; k < 8; k++) x |= static_cast<u64>(static_cast<std::uint8_t>(snap[i + static_cast<std::size_t>(k)])) << (8 * k);
    return x;
  };
  // The sections' ends: each a word holding the section's number above the
  // CRC32C of the bytes since the previous end.
  std::vector<std::size_t> ends;
  for (std::size_t i = 8; i + 8 <= snap.size(); i += 8) {
    u64 w = word(i);
    if ((w >> 32) == ends.size()) {
      std::size_t from = ends.empty() ? 0 : ends.back();
      if (static_cast<u64>(crc32cUpdate(0, reinterpret_cast<const std::uint8_t*>(snap.data()) + from, i - from)) ==
          (w & 0xffffffff))
        ends.push_back(i + 8);
    }
  }
  int accepted = 0;
  auto tryB = [&](const std::string& b) {
    Filter h(snapCfg());
    if (restore(h, b)) accepted++;
  };
  int cuts = 0;
  for (std::size_t n = 0; n < snap.size(); n++) {
    if (n <= 512 || n % 997 == 0 || n >= snap.size() - 16) {
      tryB(snap.substr(0, n));
      cuts++;
    }
  }
  tryB(snap + std::string(1, '\0'));
  cuts++;
  int flips = 0;
  gorand::Rand r(16, 16);
  std::vector<std::size_t> pos;
  for (int k = 0; k < 2000; k++) pos.push_back(static_cast<std::size_t>(r.IntN(static_cast<i64>(snap.size()))));
  std::size_t prev = 0;
  for (std::size_t e : ends) {
    pos.push_back(prev);
    pos.push_back(e - 1);
    prev = e;
  }
  for (std::size_t p : pos) {
    std::string b = snap;
    b[p] = static_cast<char>(static_cast<std::uint8_t>(b[p]) ^ (1u << (p % 8)));
    tryB(b);
    flips++;
  }
  LOGF("snapshot %d bytes, sections ending at %v; truncations %d and bit flips %d tried, accepted %d", snap.size(),
       ends, cuts, flips, accepted);
  if (accepted != 0 || ends.size() != 7)
    ERRORF("a corrupted or truncated snapshot was accepted (%d), or the sections were not found (%d)", accepted,
           ends.size());
}

// TestSnapshotRefusesWildIndices: a stream whose check words are right but
// whose words no quiescent filter writes, where a restored value is later an
// index or a lock: a slot or a stash entry pointing past the hot entries, a
// free hot entry or the next one out of range, a hot entry held (an odd
// version, which readers wait out forever), a wait bit past the stash's last
// set. Check words catch damage, not a crafted stream (a snapshot is trusted
// input, docs/paper/06-security.md), and where the reference panics at the
// first use of such an index, C++ would index past its tables: ReadFrom
// refuses each, and the refused filter holds nothing (it is unspecified, but
// memory-safe) and keeps answering. Not in the reference (cpp/README.md,
// Deviations).
TEST(TestSnapshotRefusesWildIndices) {
  if (snapModes()) SKIP("snapshots refuse ConstantTime and Background filters");
  auto f = snapHalf();
  const std::string snap = snapshotOf(*f);
  Core& c = core(*f);
  if (!c.hot.load() || !c.stash.load()) FATALF("the workload allocated no hot table or no stash");
  auto word = [](const std::string& b, std::size_t i) {
    u64 x = 0;
    for (int k = 0; k < 8; k++) x |= static_cast<u64>(static_cast<std::uint8_t>(b[i + static_cast<std::size_t>(k)])) << (8 * k);
    return x;
  };
  auto put = [](std::string& b, std::size_t i, u64 x) {
    for (int k = 0; k < 8; k++) b[i + static_cast<std::size_t>(k)] = static_cast<char>(x >> (8 * k));
  };
  // The sections' ends, as TestSnapshotCorrupt finds them.
  std::vector<std::size_t> ends;
  for (std::size_t i = 8; i + 8 <= snap.size(); i += 8) {
    if (u64 w = word(snap, i); (w >> 32) == ends.size()) {
      std::size_t from = ends.empty() ? 0 : ends.back();
      if (static_cast<u64>(crc32cUpdate(0, reinterpret_cast<const std::uint8_t*>(snap.data()) + from, i - from)) ==
          (w & 0xffffffff))
        ends.push_back(i + 8);
    }
  }
  if (ends.size() != 7) FATALF("found %d sections, want 7", ends.size());
  // edit sets the word at byte offset at, in section sec, and reseals sec.
  auto edit = [&](std::size_t sec, std::size_t at, u64 x) {
    std::string b = snap;
    put(b, at, x);
    const std::size_t from = sec == 0 ? 0 : ends[sec - 1], end = ends[sec] - 8;
    put(b, end,
        static_cast<u64>(crc32cUpdate(0, reinterpret_cast<const std::uint8_t*>(b.data()) + from, end - from)) |
            (static_cast<u64>(sec) << 32));
    return b;
  };
  const u64 wild = hotIdxMask << hotIdxShift;  // a hot pointer to the last index the field holds
  if (!isHot(wild) || hotIdx(wild) < c.hot.load()->e.size()) FATALF("%#x is no out-of-range hot pointer", wild);
  // Sections: 0 header, 1 table, 2 counters, 3 stash, 4 hot entries, 5 overflow, 6 buzz.
  struct Case {
    const char* what;
    std::string b;
  };
  std::vector<Case> cases = {
      {"a slot's word1 pointing past the hot entries", edit(1, ends[0] + 8, wild)},
      {"a stash entry's word1 pointing past the hot entries", edit(3, ends[2] + 8 + 16, wild)},
      {"a hot entry held (odd version)", edit(4, ends[3] + 8, word(snap, ends[3] + 8) | 1)},
      {"the next hot entry out of range", edit(4, ends[4] - 48, u64{1} << 40)},
  };
  if (u64 tail = c.stashN % 64; tail != 0)
    cases.push_back({"a wait bit past the stash's last set", edit(3, ends[3] - 24, word(snap, ends[3] - 24) | (u64{1} << 63))});
  for (const Case& k : cases) {
    Filter h(snapCfg());
    std::string err;
    if (restore(h, k.b, &err)) {
      ERRORF("%s: restored", k.what);
      continue;
    }
    LOGF("%s: %s", k.what, err);
    Core& hc = core(h);
    for (std::size_t i = 0; i < hc.nwords; i++) {
      if (hc.words[i].load() != 0) {
        ERRORF("%s: the refused filter kept word %d", k.what, i);
        break;
      }
    }
    for (int i = 0; i < 1000; i++) h.TouchAt(snapKey(i), 60'000, 150'000);  // answers, without reading past a table
  }
  Filter ok(snapCfg());
  if (std::string err; !restore(ok, snap, &err)) ERRORF("the intact snapshot was refused: %s", err);
}

// TestSnapshotCross: SNAP_OUT=<file> writes the first half's snapshot;
// SNAP_IN=<file> restores it, finishes the workload and compares the answers
// with this build's uninterrupted run (another build, or the Go reference,
// wrote the file).
TEST(TestSnapshotCross) {
  if (snapModes()) SKIP("snapshots refuse ConstantTime and Background filters");
  if (const char* out = std::getenv("SNAP_OUT"); out && *out) {
    std::string b = snapshotOf(*snapHalf());
    std::ofstream o(out, std::ios::binary);
    o.write(b.data(), static_cast<std::streamsize>(b.size()));
    if (!o) FATALF("writing %s failed", out);
    LOGF("wrote %d bytes", b.size());
    return;
  }
  const char* in = std::getenv("SNAP_IN");
  if (!in || !*in) SKIP("set SNAP_OUT or SNAP_IN");
  bool ok;
  std::string data = readFile(in, ok);
  if (!ok) FATALF("reading %s failed", in);
  Filter g(snapCfg());
  std::string err;
  if (!restore(g, data, &err)) FATALF("%s", err);
  auto [hr, nr] = snapRest(g);
  auto [hu, nu] = snapRest(*snapHalf());
  LOGF("restored from %s: %d answers, hash %x; uninterrupted here: %d answers, hash %x", in, nr, hr, nu, hu);
  if (hr != hu || nr != nu) ERRORF("answers differ after a restore across builds");
}

// TestSnapshotV2: a version 2 snapshot, before claims D29 (snapHalf's, written
// by the Go reference at 0df8d1d into go/testdata/snapshot-v2.bin, with the
// stash, the overflow table and hot entries in use), restores. The restored
// filter answers as the version 2 filter did: v2Answers' hash is the one that
// build printed for its own filter. Then the second half: every answer's
// fields but Count are this build's uninterrupted run's.
TEST(TestSnapshotV2) {
  if (snapModes()) SKIP("snapshots refuse ConstantTime and Background filters");
  bool ok;
  std::string data = readFile(std::string(CLEPSYDRA_GO_DIR) + "/testdata/snapshot-v2.bin", ok);
  if (!ok) FATALF("go/testdata/snapshot-v2.bin not found");
  Filter g(snapCfg());
  std::string err;
  if (!restore(g, data, &err)) FATALF("%s", err);
  const u64 v2Hash = 0x164ff3f1b71acfa5;  // 0df8d1d's v2Answers of snapHalf()
  const int v2N = 30000;
  if (auto [h, n] = v2Answers(g); h != v2Hash || n != v2N)
    ERRORF("restored from version 2: answers %d, hash %#x; version 2's own: %d, %#x", n, h, v2N, v2Hash);
  if (g.Promoted() == 0 || g.Evictions() == 0 || core(g).folded.load() == 0)
    ERRORF("the fixture's hot entries, stash or overflow table are not in use");
  auto [hr, nr] = snapRestBut(g, false);
  auto [hu, nu] = snapRestBut(*snapHalf(), false);
  if (hr != hu || nr != nu)
    ERRORF("after a version 2 restore, answers but Count: %d %#x; uninterrupted: %d %#x", nr, hr, nu, hu);
}

// TestSnapshotRefusesClockPastMaxTick: a restore continues the built-in clock
// by the wall time since the snapshot, so a lineage of snapshots can carry it
// past MaxTick (claims A35). A filter snapshotted 5 ticks before MaxTick and
// restored 20 ms later must be refused before anything is written into the
// target; one 100,000 ticks before MaxTick restores, its clock continued.
TEST(TestSnapshotRefusesClockPastMaxTick) {
  if (snapModes()) SKIP("snapshots refuse ConstantTime and Background filters");
  auto snapAt = [](u64 before) {
    Filter f(snapCfg());
    for (int k = 0; k < 100; k++) f.TouchAt(snapKey(k), 60'000, f.Now());
    Core& c = core(f);
    c.clockAdd.fetch_add(MaxTick - before - c.clock());
    return snapshotOf(f);
  };
  std::string late = snapAt(5);
  std::this_thread::sleep_for(20ms);
  Filter g(snapCfg());
  if (restore(g, late))
    FATALF("a restore continuing the clock past MaxTick was accepted: clock %d, MaxTick %d", g.Now(), MaxTick);
  if (u64 c = g.Now(); c > MaxTick / 2) ERRORF("a refused restore moved the clock to %d", c);
  Core& gc = core(g);
  for (std::size_t i = 0; i < gc.nwords; i++) {
    if (u64 w = gc.words[i].load(); w != 0) FATALF("a refused restore wrote the table (word %d = %#x)", i, w);
  }
  Filter h(snapCfg());
  std::string err;
  if (!restore(h, snapAt(100'000), &err)) FATALF("a restore 100,000 ticks before MaxTick was refused: %s", err);
  if (u64 c = h.Now(); c < MaxTick - 100'000 || c > MaxTick)
    ERRORF("restored clock %d, want within [MaxTick-100000, MaxTick]", c);
}

// TestSnapshotNoOverflow: a filter with the overflow table off (Overflow < 0)
// snapshots and restores like any other. A seeded stream over 6x Capacity
// keys loses histories from the stash before the snapshot; after it, the
// filter and its restored copy take the same calls and give the same
// answers, Degraded ones included, and a filter with the overflow table on
// refuses the snapshot. (The reference divided by zero here before changes
// 102; the port wrote 0 for the zero shape from the start.)
TEST(TestSnapshotNoOverflow) {
  if (snapModes()) SKIP("snapshots refuse ConstantTime and Background filters");
  Config cfg = cfgOf(64, 1s, 1ms);
  cfg.Overflow = -1;
  struct Op {
    u64 k, at;
  };
  gorand::Rand r(1, 2);
  std::vector<Op> ops(20'000);
  u64 at = 1;
  for (auto& o : ops) {
    at += static_cast<u64>(r.IntN(3));
    o = Op{snapKey(static_cast<int>(r.IntN(400))), at};
  }
  const std::size_t half = ops.size() / 2;
  Filter f(cfg);
  for (std::size_t i = 0; i < half; i++) f.TouchAt(ops[i].k, 1'000, ops[i].at);
  const u64 lostAt = f.Lost();
  if (lostAt == 0) FATALF("no history was lost before the snapshot");
  const std::string buf = snapshotOf(f);
  Filter g(cfg);
  if (std::string err; !restore(g, buf, &err)) FATALF("%s", err);
  int diff = 0, degraded = 0;
  for (std::size_t j = 0; j < ops.size() - half; j++) {
    const Op& o = ops[half + j];
    Result a = f.TouchAt(o.k, 1'000, o.at);
    Result b = g.TouchAt(o.k, 1'000, o.at);
    const u64 other = ops[(j * 7919) % ops.size()].k;
    Result c = f.CountAt(other, 500, o.at);
    Result d = g.CountAt(other, 500, o.at);
    if (!(a == b) || !(c == d)) diff++;
    if (a.Degraded || c.Degraded) degraded++;
  }
  LOGF("lost %d histories by the snapshot, %d by the end (restored copy %d); %d of %d steps Degraded, %d differing",
       lostAt, f.Lost(), g.Lost(), degraded, ops.size() - half, diff);
  if (diff != 0) ERRORF("%d answers differ after the restore", diff);
  if (degraded == 0) ERRORF("no answer read Degraded: the lost histories went untested");
  Filter on(cfgOf(64, 1s, 1ms));
  if (restore(on, buf)) ERRORF("a filter with the overflow table on restored a snapshot of one with it off");
}

// TestSnapshotSize (TF_EXP=1): a snapshot is within 1% of Bytes() at Capacity
// 200,000 and 1,000,000, and at 200,000 with the stash, the hot table and the
// overflow table all allocated.
TEST(TestSnapshotSize) {
  if (snapModes()) SKIP("snapshots refuse ConstantTime and Background filters");
  if (const char* e = std::getenv("TF_EXP"); !e || !*e) SKIP("set TF_EXP=1");
  auto checkF = [](const std::string& name, Filter& f, bool all) {
    std::string b = snapshotOf(f);
    Core& c = core(f);
    bool st = c.stash.load() != nullptr, hot = c.hot.load() != nullptr, gh = c.gh.load() != nullptr;
    double over = (static_cast<double>(b.size()) - static_cast<double>(f.Bytes())) / static_cast<double>(f.Bytes());
    LOGF("%s: snapshot %d bytes, Bytes() %d (%+.3f%%); stash %v, hot %v, overflow %v; evictions %d, folded %d, "
         "promoted %d",
         name, b.size(), f.Bytes(), 100 * over, st, hot, gh, f.Evictions(), f.Folded(), f.Promoted());
    if (over > 0.01 || over < -0.01 || !st || (all && (!hot || !gh)))
      ERRORF("%s: not within 1%% of Bytes(), or a section not allocated", name);
  };
  for (i64 c : {i64{200'000}, i64{1'000'000}}) {
    Filter f(cfgOf(c, 1min, 1ms));
    for (i64 i = 0; i < 3 * c; i++)
      f.TouchAt(mix64(static_cast<u64>(i)), 0, 1 + static_cast<u64>(i) * 30'000 / static_cast<u64>(3 * c));
    checkF("Capacity " + std::to_string(c), f, false);
  }
  const i64 c = 200'000;
  Config cfg = cfgOf(c, 1min, 1ms);
  cfg.Stash = 64;
  cfg.SpillMin = 1;
  Filter f(cfg);
  for (i64 i = 0; i < 3 * c; i++)
    f.TouchAt(mix64(static_cast<u64>(i)), 0, 1 + static_cast<u64>(i) * 30'000 / static_cast<u64>(3 * c));
  for (u64 k = 0; k < 100; k++)
    for (int j = 0; j < 64; j++) f.TouchAt(mix64(k + (u64{1} << 45)), 0, 30'001);
  checkF("Capacity 200000, Stash 64, SpillMin 1", f, true);
}
