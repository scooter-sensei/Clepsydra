//! snapshot_test.go, and the port's TestSnapshotRefusesWildIndices.
//!
//! Snapshots (claims D27): answers across a restore, the format's refusals,
//! its continuity across builds (toolchains, architectures, languages) and
//! its size.

use std::cell::{Cell, RefCell};
use std::sync::atomic::Ordering::SeqCst;
use std::time::Duration;

use super::check::*;
use super::gorand::Rand;
use super::helpers::*;
use crate::crc32c;
use crate::hot::{HOT_IDX_MASK, HOT_IDX_SHIFT, hot_idx, is_hot};
use crate::slots::*;
use crate::{Config, Filter, MAX_TICK, Result};

pub(crate) fn snap_cfg() -> Config {
    Config { capacity: 512, horizon: MINUTE, resolution: ms(1), ..Config::default() }
}

pub(crate) fn snap_key(k: usize) -> u64 {
    mix64(k as u64 + 1)
}

fn snap_modes() -> bool {
    force_ct() || force_bg()
}

const SKIP_MODES: &str = "snapshots refuse ConstantTime and Background filters";

/// WriteTo into a buffer.
#[track_caller]
fn snapshot_of(f: &Filter) -> Vec<u8> {
    let mut buf = Vec::new();
    if let Err(e) = f.write_to(&mut buf) {
        panic!("WriteTo: {e}");
    }
    buf
}

fn restore(g: &Filter, b: &[u8]) -> std::result::Result<i64, crate::Error> {
    g.read_from(&mut &b[..])
}

/// TestSnapshot runs tfbench's velocity workload once uninterrupted and once
/// snapshotted at 150 s and restored into a new filter, at Capacity 512 so
/// the stash, the overflow table and hot entries are all in use across the
/// snapshot: every answer must be the same. Then the built-in clock: a filter
/// restored 20 ms after its snapshot continues the old timeline.
#[test]
fn TestSnapshot() {
    let t = T::new("TestSnapshot");
    if snap_modes() {
        skip!(t, "{SKIP_MODES}");
    }
    let wins = [1_000u64, 5_000, 15_000, 30_000, 60_000];
    let run = |snap_at: u64| -> (Vec<Result>, Filter, usize, i64) {
        let f = RefCell::new(must(snap_cfg()));
        let ans: RefCell<Vec<Result>> = RefCell::new(Vec::new());
        let (size, bytes_at) = (Cell::new(0usize), Cell::new(0i64));
        vel_workload(
            |k, at| {
                let r = f.borrow().touch_at(snap_key(k), 60_000, at);
                ans.borrow_mut().push(r);
            },
            |k, _, wi, now| {
                if now == snap_at {
                    let buf = snapshot_of(&f.borrow());
                    size.set(buf.len());
                    bytes_at.set(f.borrow().bytes());
                    let g = must(snap_cfg());
                    if let Err(e) = restore(&g, &buf) {
                        fatalf!(t, "{e}");
                    }
                    *f.borrow_mut() = g;
                }
                let r = f.borrow().count_at(snap_key(k), wins[wi], now);
                ans.borrow_mut().push(r);
            },
        );
        (ans.into_inner(), f.into_inner(), size.get(), bytes_at.get())
    };
    let (u, fu, _, _) = run(0);
    let (r, fr, size, bytes_at) = run(150_000);
    let (mut diff, mut first) = (0, -1i64);
    for i in 0..u.len().min(r.len()) {
        if u[i] != r[i] {
            diff += 1;
            if first < 0 {
                first = i as i64;
            }
        }
    }
    logf!(
        t,
        "answers {} / {}, differing {diff} (first at {first}); end: evictions {} / {}, folded {} / {}, promoted {} / {}, floored {} / {}",
        u.len(),
        r.len(),
        fu.evictions(),
        fr.evictions(),
        fu.folded(),
        fr.folded(),
        fu.promoted(),
        fr.promoted(),
        fu.floored(),
        fr.floored()
    );
    logf!(t, "snapshot {size} bytes, Bytes() {bytes_at} at the snapshot ({:.3}%)", 100.0 * (size as i64 - bytes_at) as f64 / bytes_at as f64);
    if diff != 0 || u.len() != r.len() {
        errorf!(t, "a restore changed answers");
    }
    if fr.evictions() == 0 || fr.folded() == 0 || fr.promoted() == 0 {
        errorf!(t, "the stash, overflow table or hot entries were not in use");
    }
    let c = must(Config { capacity: 512, horizon: MINUTE, ..Config::default() });
    c.touch_at(1, 0, c.clock());
    let buf = snapshot_of(&c);
    let old = c.clock();
    std::thread::sleep(ms(20));
    let d = must(Config { capacity: 512, horizon: MINUTE, ..Config::default() });
    if let Err(e) = restore(&d, &buf) {
        fatalf!(t, "{e}");
    }
    let now = d.clock();
    logf!(t, "clock: old {old}, restored {now} (+{} ticks, want >= {})", now.wrapping_sub(old), c.ticks(ms(20)));
    if now < old + c.ticks(ms(20)) {
        errorf!(t, "the restored clock did not continue the old timeline");
    }
}

/// snapHalf runs the velocity workload's first half (to 150 s) into a new
/// filter and returns it.
pub(crate) fn snap_half() -> Filter {
    let f = must(snap_cfg());
    vel_workload(
        |k, at| {
            if at < 150_000 {
                f.touch_at(snap_key(k), 60_000, at);
            }
        },
        |_, _, _, _| {},
    );
    f
}

/// snapRest runs the second half on f (from 150 s) and hashes every answer.
fn snap_rest(f: &Filter) -> (u64, i64) {
    snap_rest_but(f, true)
}

/// snapRestBut is snapRest, Count left out of the hash unless count.
fn snap_rest_but(f: &Filter, count: bool) -> (u64, i64) {
    let h = RefCell::new(Fnv64a::new());
    let n = Cell::new(0i64);
    let put = |mut r: Result| {
        if !count {
            r.count = 0;
        }
        h.borrow_mut().write(&gov_result(&r));
        n.set(n.get() + 1);
    };
    vel_workload(
        |k, at| {
            if at >= 150_000 {
                put(f.touch_at(snap_key(k), 60_000, at));
            }
        },
        |k, _, wi, now| {
            if now >= 150_000 {
                put(f.count_at(snap_key(k), VEL_WINS[wi], now));
            }
        },
    );
    (h.borrow().sum64(), n.get())
}

fn crc32c_of(b: &[u8]) -> u32 {
    crc32c::update(0, b)
}

fn word(b: &[u8], i: usize) -> u64 {
    u64::from_le_bytes(b[i..i + 8].try_into().unwrap())
}

/// The sections' ends: each a word holding the section's number above the
/// CRC32C of the bytes since the previous end.
fn section_ends(snap: &[u8]) -> Vec<usize> {
    let mut ends: Vec<usize> = Vec::new();
    let mut i = 8;
    while i + 8 <= snap.len() {
        let w = word(snap, i);
        if (w >> 32) == ends.len() as u64 {
            let from = ends.last().copied().unwrap_or(0);
            if crc32c_of(&snap[from..i]) as u64 == (w & 0xffffffff) {
                ends.push(i + 8);
            }
        }
        i += 8;
    }
    ends
}

/// TestSnapshotCorrupt: a snapshot cut at every length to 512, every 997th
/// and the last 16, one with a trailing byte, and one bit flipped at 2,000
/// seeded positions and at the first and last byte of every section: every
/// one is refused.
#[test]
fn TestSnapshotCorrupt() {
    let t = T::new("TestSnapshotCorrupt");
    if snap_modes() {
        skip!(t, "{SKIP_MODES}");
    }
    let f = snap_half();
    let snap = snapshot_of(&f);
    let g = must(snap_cfg());
    if let Err(e) = restore(&g, &snap) {
        fatalf!(t, "intact snapshot refused: {e}");
    }
    let ends = section_ends(&snap);
    let mut accepted = 0;
    let mut try_ = |b: &[u8]| {
        let h = must(snap_cfg());
        if restore(&h, b).is_ok() {
            accepted += 1;
        }
    };
    let mut cuts = 0;
    for n in 0..snap.len() {
        if n <= 512 || n % 997 == 0 || n >= snap.len() - 16 {
            try_(&snap[..n]);
            cuts += 1;
        }
    }
    let mut longer = snap.clone();
    longer.push(0);
    try_(&longer);
    cuts += 1;
    let mut flips = 0;
    let mut r = Rand::new(16, 16);
    let mut pos: Vec<usize> = Vec::new();
    for _ in 0..2000 {
        pos.push(r.int_n(snap.len() as i64) as usize);
    }
    let mut prev = 0;
    for &e in &ends {
        pos.push(prev);
        pos.push(e - 1);
        prev = e;
    }
    for &p in &pos {
        let mut b = snap.clone();
        b[p] ^= 1 << (p % 8);
        try_(&b);
        flips += 1;
    }
    logf!(t, "snapshot {} bytes, sections ending at {}; truncations {cuts} and bit flips {flips} tried, accepted {accepted}", snap.len(), gov(&ends));
    if accepted != 0 || ends.len() != 7 {
        errorf!(t, "a corrupted or truncated snapshot was accepted ({accepted}), or the sections were not found ({})", ends.len());
    }
}

/// TestSnapshotRefusesWildIndices (the C++ port's, cpp/README.md,
/// Deviations): a stream whose check words are right but which holds values
/// no quiescent filter writes, each of which a later call would use as an
/// index or wait on: a slot or a stash entry pointing past the hot entries,
/// the next hot entry out of range, a hot entry held (an odd version, which
/// readers wait out forever), a wait bit past the stash's last set, and a
/// stash section in a filter without a stash. Check words catch damage, not
/// a crafted stream (a snapshot is trusted input, docs/paper/06-security.md);
/// where the reference panics or hangs at the first use of such a value,
/// ReadFrom here refuses each, and the refused filter holds nothing and
/// keeps answering.
#[test]
fn TestSnapshotRefusesWildIndices() {
    let t = T::new("TestSnapshotRefusesWildIndices");
    if snap_modes() {
        skip!(t, "{SKIP_MODES}");
    }
    let f = snap_half();
    let snap = snapshot_of(&f);
    let Some(hot) = f.hot.get() else {
        fatalf!(t, "the workload allocated no hot table");
    };
    if f.stash.get().is_none() {
        fatalf!(t, "the workload allocated no stash");
    }
    let ends = section_ends(&snap);
    if ends.len() != 7 {
        fatalf!(t, "found {} sections, want 7", ends.len());
    }
    // edit sets the word at byte offset at, in section sec, and reseals sec.
    let edit = |snap: &[u8], ends: &[usize], sec: usize, at: usize, x: u64| -> Vec<u8> {
        let mut b = snap.to_vec();
        b[at..at + 8].copy_from_slice(&x.to_le_bytes());
        let (from, end) = (if sec == 0 { 0 } else { ends[sec - 1] }, ends[sec] - 8);
        let check = crc32c_of(&b[from..end]) as u64 | ((sec as u64) << 32);
        b[end..end + 8].copy_from_slice(&check.to_le_bytes());
        b
    };
    let wild = HOT_IDX_MASK << HOT_IDX_SHIFT; // a hot pointer to the last index the field holds
    if !is_hot(wild) || hot_idx(wild) < hot.e.len() as u64 {
        fatalf!(t, "{wild:#x} is no out-of-range hot pointer");
    }
    // Sections: 0 header, 1 table, 2 counters, 3 stash, 4 hot entries, 5 overflow, 6 buzz.
    let mut cases: Vec<(&str, Vec<u8>, Config)> = vec![
        ("a slot's word1 pointing past the hot entries", edit(&snap, &ends, 1, ends[0] + 8, wild), snap_cfg()),
        ("a stash entry's word1 pointing past the hot entries", edit(&snap, &ends, 3, ends[2] + 8 + 16, wild), snap_cfg()),
        ("a hot entry held (odd version)", edit(&snap, &ends, 4, ends[3] + 8, word(&snap, ends[3] + 8) | 1), snap_cfg()),
        ("the next hot entry out of range", edit(&snap, &ends, 4, ends[4] - 48, 1 << 40), snap_cfg()),
    ];
    if f.stash_n % 64 != 0 {
        cases.push(("a wait bit past the stash's last set", edit(&snap, &ends, 3, ends[3] - 24, word(&snap, ends[3] - 24) | (1 << 63)), snap_cfg()));
    }
    // A filter built without a stash, its snapshot's stash section marked present.
    let bare = Config { stash: -1, ..snap_cfg() };
    let b = must(bare.clone());
    for k in 0..100 {
        b.touch_at(snap_key(k), 60_000, 1_000);
    }
    let bsnap = snapshot_of(&b);
    let bends = section_ends(&bsnap);
    if bends.len() != 7 {
        fatalf!(t, "found {} sections in the stashless snapshot, want 7", bends.len());
    }
    cases.push(("a stash section in a filter without a stash", edit(&bsnap, &bends, 3, bends[2], 1), bare));
    for (what, b, cfg) in &cases {
        let h = must(cfg.clone());
        match restore(&h, b) {
            Ok(_) => {
                errorf!(t, "{what}: restored");
                continue;
            }
            Err(e) => logf!(t, "{what}: {e}"),
        }
        if let Some(i) = h.words.iter().position(|w| ld(w) != 0) {
            errorf!(t, "{what}: the refused filter kept word {i}");
        }
        for i in 0..1000 {
            h.touch_at(snap_key(i), 60_000, 150_000); // answers, without reading past a table
        }
    }
    let ok = must(snap_cfg());
    if let Err(e) = restore(&ok, &snap) {
        errorf!(t, "the intact snapshot was refused: {e}");
    }
}

/// TestSnapshotCross: SNAP_OUT=<file> writes the first half's snapshot;
/// SNAP_IN=<file> restores it, finishes the workload and compares the answers
/// with this build's uninterrupted run (another build, the C++ port or the Go
/// reference wrote the file).
#[test]
fn TestSnapshotCross() {
    let t = T::new("TestSnapshotCross");
    if snap_modes() {
        skip!(t, "{SKIP_MODES}");
    }
    let out = getenv("SNAP_OUT");
    if !out.is_empty() {
        let buf = snapshot_of(&snap_half());
        if let Err(e) = std::fs::write(&out, &buf) {
            fatalf!(t, "{e}");
        }
        logf!(t, "wrote {} bytes", buf.len());
        return;
    }
    let inp = getenv("SNAP_IN");
    if inp.is_empty() {
        skip!(t, "set SNAP_OUT or SNAP_IN");
    }
    let data = match std::fs::read(&inp) {
        Ok(d) => d,
        Err(e) => fatalf!(t, "{e}"),
    };
    let g = must(snap_cfg());
    if let Err(e) = restore(&g, &data) {
        fatalf!(t, "{e}");
    }
    let (hr, nr) = snap_rest(&g);
    let (hu, nu) = snap_rest(&snap_half());
    logf!(t, "restored from {inp}: {nr} answers, hash {hr:x}; uninterrupted here: {nu} answers, hash {hu:x}");
    if hr != hu || nr != nu {
        errorf!(t, "answers differ after a restore across builds");
    }
}

/// TestSnapshotV2: a version 2 snapshot, before claims D29 (snapHalf's,
/// written by 0df8d1d into testdata/snapshot-v2.bin, with the stash, the
/// overflow table and hot entries in use), restores. Its registers kept 4
/// bits of their unit; each is moved to its last touch's unit with an onset
/// of 0, which says nothing, so the restored filter answers as the version 2
/// filter did: v2Answers' hash is the one that build printed for its own
/// filter. Then the second half: every answer's fields but Count are this
/// build's uninterrupted run's (Count reads a bucket open at the snapshot
/// without its onset).
#[test]
fn TestSnapshotV2() {
    let t = T::new("TestSnapshotV2");
    if snap_modes() {
        skip!(t, "{SKIP_MODES}");
    }
    let data = match std::fs::read(format!("{TESTDATA_DIR}/snapshot-v2.bin")) {
        Ok(d) => d,
        Err(e) => fatalf!(t, "{e}"),
    };
    let g = must(snap_cfg());
    if let Err(e) = restore(&g, &data) {
        fatalf!(t, "{e}");
    }
    const V2_HASH: u64 = 0x164ff3f1b71acfa5; // 0df8d1d's v2Answers of snapHalf()
    const V2_N: i64 = 30000;
    let (h, n) = v2_answers(&g);
    if h != V2_HASH || n != V2_N {
        errorf!(t, "restored from version 2: answers {n}, hash {h:#x}; version 2's own: {V2_N}, {V2_HASH:#x}");
    }
    if g.promoted() == 0 || g.evictions() == 0 || g.folded() == 0 {
        errorf!(t, "the fixture's hot entries, stash or overflow table are not in use");
    }
    let (hr, nr) = snap_rest_but(&g, false);
    let (hu, nu) = snap_rest_but(&snap_half(), false);
    if hr != hu || nr != nu {
        errorf!(t, "after a version 2 restore, answers but Count: {nr} {hr:#x}; uninterrupted: {nu} {hu:#x}");
    }
}

/// v2Answers hashes CountAt of every workload key at every window, at three
/// clocks after snapHalf's half (TestSnapshotV2).
fn v2_answers(f: &Filter) -> (u64, i64) {
    let mut h = Fnv64a::new();
    let mut n = 0;
    for now in [149_999u64, 150_700, 152_400] {
        for k in 0..2000 {
            for w in VEL_WINS {
                h.write(&format!("{};", gov_result(&f.count_at(snap_key(k), w, now))));
                n += 1;
            }
        }
    }
    (h.sum64(), n)
}

/// TestSnapshotRefusesClockPastMaxTick: a restore continues the built-in
/// clock by the wall time since the snapshot, so a lineage of snapshots can
/// carry it past MaxTick, where a slot can no longer hold a tick and hits
/// read wrong until an insert panics (A35). A filter snapshotted 5 ticks
/// before MaxTick and restored 20 ms later (20 ticks at 1 ms) must be refused
/// before anything is written into the target; one 100,000 ticks before
/// MaxTick restores, its clock continued.
#[test]
fn TestSnapshotRefusesClockPastMaxTick() {
    let t = T::new("TestSnapshotRefusesClockPastMaxTick");
    if snap_modes() {
        skip!(t, "{SKIP_MODES}");
    }
    let snap_at = |before: u64| -> Vec<u8> {
        let f = must(snap_cfg());
        for k in 0..100 {
            f.touch_at(snap_key(k), 60_000, f.now());
        }
        f.clock_add.fetch_add(MAX_TICK - before - f.clock(), SeqCst);
        snapshot_of(&f)
    };

    let late = snap_at(5);
    std::thread::sleep(ms(20));
    let g = must(snap_cfg());
    if restore(&g, &late).is_ok() {
        fatalf!(t, "a restore continuing the clock past MaxTick was accepted: clock {}, MaxTick {MAX_TICK}", g.clock());
    }
    let c = g.clock();
    if c > MAX_TICK / 2 {
        errorf!(t, "a refused restore moved the clock to {c}");
    }
    for (i, w) in g.words.iter().enumerate() {
        if ld(w) != 0 {
            fatalf!(t, "a refused restore wrote the table (word {i} = {:#x})", ld(w));
        }
    }

    let h = must(snap_cfg());
    if let Err(e) = restore(&h, &snap_at(100_000)) {
        fatalf!(t, "a restore 100,000 ticks before MaxTick was refused: {e}");
    }
    let c = h.clock();
    if !(MAX_TICK - 100_000..=MAX_TICK).contains(&c) {
        errorf!(t, "restored clock {c}, want within [MaxTick-100000, MaxTick]");
    }
}

/// TestSnapshotNoOverflow: a filter with the overflow table off (Overflow < 0)
/// snapshots and restores like any other. A seeded stream over 6x Capacity
/// keys loses histories from the stash before the snapshot; after it, the
/// filter and its restored copy take the same calls and give the same
/// answers, Degraded ones included, and a filter with the overflow table on
/// refuses the snapshot. Before, the reference's WriteTo and ReadFrom both
/// panicked: the Config's shape includes the count-min bytes, computed from
/// the sketch shape, which is zero with the overflow table off (a division by
/// zero).
#[test]
fn TestSnapshotNoOverflow() {
    let t = T::new("TestSnapshotNoOverflow");
    if snap_modes() {
        skip!(t, "{SKIP_MODES}");
    }
    let cfg = Config { capacity: 64, horizon: Duration::from_secs(1), resolution: ms(1), overflow: -1, ..Config::default() };
    let mut r = Rand::new(1, 2);
    let mut ops = vec![(0u64, 0u64); 20_000]; // k, at
    let mut at = 1u64;
    for o in ops.iter_mut() {
        at += r.int_n(3) as u64;
        *o = (snap_key(r.int_n(400) as usize), at);
    }
    let half = ops.len() / 2;
    let f = must(cfg.clone());
    for &(k, at) in &ops[..half] {
        f.touch_at(k, 1_000, at);
    }
    let lost_at = f.lost();
    if lost_at == 0 {
        fatalf!(t, "no history was lost before the snapshot");
    }
    let buf = snapshot_of(&f);
    let g = must(cfg);
    if let Err(e) = restore(&g, &buf) {
        fatalf!(t, "{e}");
    }
    let (mut diff, mut degraded) = (0, 0);
    for (i, &(k, at)) in ops[half..].iter().enumerate() {
        let (a, b) = (f.touch_at(k, 1_000, at), g.touch_at(k, 1_000, at));
        let other = ops[(i * 7919) % ops.len()].0;
        let (c, d) = (f.count_at(other, 500, at), g.count_at(other, 500, at));
        if a != b || c != d {
            diff += 1;
        }
        if a.degraded || c.degraded {
            degraded += 1;
        }
    }
    logf!(
        t,
        "lost {lost_at} histories by the snapshot, {} by the end (restored copy {}); {degraded} of {} steps Degraded, {diff} differing",
        f.lost(),
        g.lost(),
        ops.len() - half
    );
    if diff != 0 {
        errorf!(t, "{diff} answers differ after the restore");
    }
    if degraded == 0 {
        errorf!(t, "no answer read Degraded: the lost histories went untested");
    }
    let on = must(Config { capacity: 64, horizon: Duration::from_secs(1), resolution: ms(1), ..Config::default() });
    if restore(&on, &buf).is_ok() {
        errorf!(t, "a filter with the overflow table on restored a snapshot of one with it off");
    }
}

/// TestSnapshotSize (TF_EXP=1): a snapshot is within 1% of Bytes() at
/// Capacity 200,000 and 1,000,000 (3 x Capacity keys over 30 s: the stash
/// allocated), and at 200,000 with the stash, the hot table and the overflow
/// table all allocated (a 64-entry stash and SpillMin 1, so every dropped
/// history folds; 100 keys touched 64 times in one tick take hot entries).
#[test]
fn TestSnapshotSize() {
    let t = T::new("TestSnapshotSize");
    if snap_modes() {
        skip!(t, "{SKIP_MODES}");
    }
    if !env_on("TF_EXP") {
        skip!(t, "set TF_EXP=1");
    }
    let check = |name: &str, f: &Filter, all: bool| {
        let buf = snapshot_of(f);
        let (st, hot, gh) = (f.stash.get().is_some(), f.hot.get().is_some(), f.gh.get().is_some());
        let over = (buf.len() as i64 - f.bytes()) as f64 / f.bytes() as f64;
        logf!(
            t,
            "{name}: snapshot {} bytes, Bytes() {} ({:+.3}%); stash {st}, hot {hot}, overflow {gh}; evictions {}, folded {}, promoted {}",
            buf.len(),
            f.bytes(),
            100.0 * over,
            f.evictions(),
            f.folded(),
            f.promoted()
        );
        if over > 0.01 || over < -0.01 || !st || (all && (!hot || !gh)) {
            errorf!(t, "{name}: not within 1% of Bytes(), or a section not allocated");
        }
    };
    for c in [200_000i64, 1_000_000] {
        let f = must(Config { capacity: c, horizon: MINUTE, resolution: ms(1), ..Config::default() });
        for i in 0..3 * c as u64 {
            f.touch_at(mix64(i), 0, 1 + i * 30_000 / (3 * c as u64));
        }
        check(&format!("Capacity {c}"), &f, false);
    }
    const C: u64 = 200_000;
    let f = must(Config { capacity: C as i64, horizon: MINUTE, resolution: ms(1), stash: 64, spill_min: 1, ..Config::default() });
    for i in 0..3 * C {
        f.touch_at(mix64(i), 0, 1 + i * 30_000 / (3 * C));
    }
    for k in 0..100u64 {
        for _ in 0..64 {
            f.touch_at(mix64(k + (1 << 45)), 0, 30_001);
        }
    }
    check("Capacity 200000, Stash 64, SpillMin 1", &f, true);
}
