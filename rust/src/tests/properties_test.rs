//! The C++ port's own tests (cpp/tests/properties_test.cpp): what the format
//! says each operation MUST keep (docs/specification/format.md), for the
//! operations the conformance vectors leave out (conformance.md: "an
//! implementation that offers them shows the properties of the format
//! instead").

use std::sync::{Barrier, Mutex};
use std::time::Duration;

use super::check::*;
use super::gorand::Rand;
use super::helpers::*;
use crate::slots::*;
use crate::{Config, Filter, Resizable};

fn test_key() -> [u8; 16] {
    std::array::from_fn(|i| i as u8)
}

fn modes() -> bool {
    force_ct() || force_bg()
}

/// Workload drives one or two filters with the same seeded stream of every
/// operation (touches, counts, conditional touches, bursts that promote, a
/// key pool past the table's capacity), so a pair's answers can be compared.
struct Workload {
    rng: Rand,
    clock: TestClock,
    now: u64,
    heavy: Vec<u64>,
}

impl Workload {
    fn new(seed: u64, clock: &TestClock) -> Workload {
        Workload { rng: Rand::new(seed, 0x5a7), clock: clock.clone(), now: clock.get(), heavy: (0..8u64).map(|i| mix64(0xbeef0000 + i)).collect() }
    }

    /// step runs one operation on every filter in fs and reports how their
    /// answers disagree, if they do.
    fn step(&mut self, fs: &[&Filter]) -> std::result::Result<(), String> {
        let rng = &mut self.rng;
        self.now += rng.int_n(40) as u64;
        if rng.int_n(500) == 0 {
            self.now += 1 + rng.uint64_n(20_000);
        }
        let now = self.now;
        self.clock.set(now);
        let h = if rng.int_n(10) == 0 { self.heavy[rng.int_n(8) as usize] } else { mix64(0x1000 + rng.uint64_n(700)) };
        let w = 1 + rng.uint64_n(12_000);
        let op = rng.int_n(100);
        let mut got: Vec<String> = Vec::new();
        for f in fs {
            let mut o = String::new();
            if op < 40 {
                o += &show(&f.touch_at(h, w, now));
            } else if op < 45 {
                for _ in 0..50 {
                    f.touch_at(h, 0, now);
                }
                o += &show(&f.count_at(h, w, now));
            } else if op < 70 {
                o += &show(&f.count_at(h, w, now));
            } else if op < 85 {
                let (r, ok) = f.touch_if_under_at(h, w, 1 + (h >> 7) % 20, now);
                o += &format!("{}{ok}", show(&r));
            } else if op < 90 {
                o += &format!("{}{}", f.contains_at(h, w, now), f.check_and_touch_at(h, w, now));
            } else if op < 95 {
                let hs: [u64; 8] = std::array::from_fn(|k| mix64(0x1000 + ((h.wrapping_add(k as u64 * 31)) % 700)));
                let (mut out, mut cnt) = ([false; 8], [0i64; 8]);
                f.touch_and_count_batch(&hs, w, now, &mut out, &mut cnt);
                for k in 0..8 {
                    o += &format!("{}{},", out[k], cnt[k]);
                }
            } else {
                f.decay_step(now, 4);
            }
            got.push(o);
        }
        for g in &got[1..] {
            if *g != got[0] {
                return Err(format!("op {op} key {h:x} window {w} now {now}: {} vs {g}", got[0]));
            }
        }
        Ok(())
    }
}

fn snap_config(clock: &TestClock, variant: i32) -> Config {
    let mut cfg = Config {
        capacity: 256,
        horizon: Duration::from_secs(10),
        resolution: ms(1),
        key: Some(test_key()),
        clock: clock.func(),
        ..Config::default()
    };
    if variant == 1 {
        cfg.stash = 8;
        cfg.spill_min = 1;
    }
    if variant == 2 {
        cfg.overflow = -1;
    }
    if variant == 3 {
        cfg.hot = 2;
    }
    cfg
}

/// TestSnapshotRoundTrip: a restore into a filter of the same configuration
/// answers every later call as the uninterrupted filter would, conditional
/// touches included (format.md, Snapshots; claims D27, A46). The stream fills
/// the stash, the overflow table, hot entries and buzz entries first.
#[test]
fn TestSnapshotRoundTrip() {
    let t = T::new("TestSnapshotRoundTrip");
    if modes() {
        skip!(t, "ConstantTime and Background filters refuse snapshots");
    }
    for variant in 0..4 {
        let clock = TestClock::new(1);
        let cfg = snap_config(&clock, variant);
        let f = must(cfg.clone());
        let mut wl = Workload::new(variant as u64 + 1, &clock);
        for _ in 0..30_000 {
            let _ = wl.step(&[&f]);
        }
        let mut buf: Vec<u8> = Vec::new();
        let n = match f.write_to(&mut buf) {
            Ok(n) => n,
            Err(e) => fatalf!(t, "variant {variant}: {e}"),
        };
        let g = must(cfg);
        match g.read_from(&mut &buf[..]) {
            Ok(m) if m == n && n == buf.len() as i64 => {}
            other => fatalf!(t, "variant {variant}: wrote {n} ({} bytes), read {other:?}", buf.len()),
        }
        if (g.bytes(), g.evictions(), g.promoted(), g.folded()) != (f.bytes(), f.evictions(), f.promoted(), f.folded()) {
            errorf!(t, "variant {variant}: the restored filter's Bytes, Evictions, Promoted or Folded differ");
        }
        for i in 0..30_000 {
            if let Err(why) = wl.step(&[&f, &g]) {
                errorf!(t, "variant {variant} step {i}: {why}");
                break;
            }
        }
        if g.evictions() != f.evictions() || g.occupancy(wl.now) != f.occupancy(wl.now) {
            errorf!(
                t,
                "variant {variant}: evictions {} / {}, occupancy {} / {}",
                f.evictions(),
                g.evictions(),
                f.occupancy(wl.now),
                g.occupancy(wl.now)
            );
        }
        logf!(t, "variant {variant}: {n} bytes; evictions {}, folded {}, promoted {}, lost {}", f.evictions(), f.folded(), f.promoted(), f.lost());
    }
}

/// TestSnapshotRefusals: a restore refuses another configuration, a truncated
/// stream, a damaged one and trailing bytes (format.md, Snapshots).
#[test]
fn TestSnapshotRefusals() {
    let t = T::new("TestSnapshotRefusals");
    if modes() {
        skip!(t, "ConstantTime and Background filters refuse snapshots");
    }
    let clock = TestClock::new(1);
    let cfg = snap_config(&clock, 0);
    let f = must(cfg.clone());
    let mut wl = Workload::new(9, &clock);
    for _ in 0..5_000 {
        let _ = wl.step(&[&f]);
    }
    let mut good: Vec<u8> = Vec::new();
    if let Err(e) = f.write_to(&mut good) {
        fatalf!(t, "{e}");
    }
    let refused = |bytes: &[u8], c: &Config| must(c.clone()).read_from(&mut &bytes[..]).is_err();
    if refused(&good, &cfg) {
        errorf!(t, "the intact snapshot was refused");
    }
    let other = Config { capacity: 512, ..cfg.clone() };
    if !refused(&good, &other) {
        errorf!(t, "another Config was restored");
    }
    for cut in [0, 1, 8, 100, good.len() / 2, good.len() - 8, good.len() - 1] {
        if !refused(&good[..cut], &cfg) {
            errorf!(t, "a stream cut at {cut} was restored");
        }
    }
    let mut longer = good.clone();
    longer.push(b'x');
    if !refused(&longer, &cfg) {
        errorf!(t, "trailing bytes were restored");
    }
    let (mut flips, mut missed) = (0, 0);
    let mut i = 0;
    while i < good.len() {
        let mut bad = good.clone();
        bad[i] ^= 1 << (i % 8);
        flips += 1;
        if !refused(&bad, &cfg) {
            missed += 1;
        }
        i += 61;
    }
    if missed != 0 {
        errorf!(t, "{missed} of {flips} single-bit flips were restored");
    }
}

/// TestMergeSums: a key in both filters gets the two registers summed and the
/// newer last touch; a key only the other holds is installed; Merge refuses
/// itself, another Config and hot entries in use (format.md, Merge; D24).
#[test]
fn TestMergeSums() {
    let t = T::new("TestMergeSums");
    if modes() {
        skip!(t, "ConstantTime and Background filters refuse Merge");
    }
    let clock = TestClock::new(1000);
    let cfg = Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), key: Some(test_key()), clock: clock.func(), ..Config::default() };
    let (f, g) = (must(cfg.clone()), must(cfg.clone()));
    for _ in 0..3 {
        f.touch("both", MINUTE);
    }
    for _ in 0..2 {
        g.touch("both", MINUTE);
    }
    f.touch("f-only", MINUTE);
    for _ in 0..4 {
        g.touch("g-only", MINUTE);
    }
    clock.add(30_000); // g's later touch of a key both hold sets the merged last touch
    g.touch("both", MINUTE);
    if let Err(e) = f.merge(&g) {
        fatalf!(t, "{e}");
    }
    clock.add(1);
    for (key, want) in [("both", 6i64), ("f-only", 1), ("g-only", 4), ("none", 0)] {
        let r = f.count(key, MINUTE);
        if r.count != want {
            errorf!(t, "{key}: {}, want Count {want}", show(&r));
        }
    }
    if !f.count("both", secs(1)).seen {
        errorf!(t, "both: not Seen within a second of g's last touch");
    }
    if f.merge(&f).is_ok() {
        errorf!(t, "Merge with itself: no error");
    }
    let o = must(Config { capacity: 2000, ..cfg.clone() });
    if f.merge(&o).is_ok() {
        errorf!(t, "Merge of another Config: no error");
    }
    let hot = must(cfg);
    for _ in 0..40 {
        hot.touch("burst", MINUTE); // past a register's 31 in one unit: promoted
    }
    if hot.promoted() != 1 {
        errorf!(t, "the burst promoted {} keys, want 1", hot.promoted());
    }
    if f.merge(&hot).is_ok() {
        errorf!(t, "Merge with hot entries in use: no error");
    }
}

/// mostInWindow: the most events of admitted inside any window [t-w, t].
fn most_in_window(mut admitted: Vec<u64>, w: u64) -> i64 {
    admitted.sort_unstable();
    let (mut most, mut lo) = (0i64, 0usize);
    for hi in 0..admitted.len() {
        while admitted[hi] - admitted[lo] > w {
            lo += 1;
        }
        most = most.max((hi - lo + 1) as i64);
    }
    most
}

/// TestTouchIfUnderNeverOverLimit: conditional touches admit no more than
/// limit events in any window, buzz entries (limits up to 16) or not, from
/// one thread and from eight racing on one key (format.md, Conditional touch;
/// claims A45, A46).
#[test]
fn TestTouchIfUnderNeverOverLimit() {
    let t = T::new("TestTouchIfUnderNeverOverLimit");
    for limit in [1u64, 3, 5, 16, 17, 40] {
        for w in [1_000u64, 5_000, 30_000] {
            for threads in [1usize, 8] {
                let mut now = 1000u64;
                let f = must(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), key: Some(test_key()), ..Config::default() });
                let h = f.hash("limited") ^ limit;
                let admitted: Mutex<Vec<u64>> = Mutex::new(Vec::new());
                let mut rng = Rand::new(limit * 1000 + w + threads as u64, 3);
                let gap = 1.max(w / (2 * limit)); // offered at twice the limit
                let nows: Vec<u64> = (0..400)
                    .map(|_| {
                        now += rng.uint64_n(2 * gap + 1);
                        now
                    })
                    .collect();
                // Every thread makes one request per round, all at the
                // round's clock; the barrier keeps rounds from overlapping.
                let sync = Barrier::new(threads);
                std::thread::scope(|s| {
                    for _ in 0..threads {
                        s.spawn(|| {
                            for &at in &nows {
                                sync.wait();
                                if f.touch_if_under_at(h, w, limit, at).1 {
                                    admitted.lock().unwrap().push(at);
                                }
                            }
                        });
                    }
                });
                let admitted = admitted.into_inner().unwrap();
                let none = admitted.is_empty();
                let most = most_in_window(admitted, w);
                if most > limit as i64 {
                    errorf!(t, "limit {limit} window {w} threads {threads}: {most} admitted in one window");
                }
                if none {
                    errorf!(t, "limit {limit} window {w} threads {threads}: nothing admitted");
                }
            }
        }
    }
}

/// TestHotKeepsBurstsExact: with hot entries a burst of 2,000 events in one
/// tick reads exactly in windows of 5 s and more, unsaturated; without them a
/// register saturates (claims A15, D7).
#[test]
fn TestHotKeepsBurstsExact() {
    let t = T::new("TestHotKeepsBurstsExact");
    for with_hot in [true, false] {
        let f = must(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), hot: if with_hot { 0 } else { -1 }, ..Config::default() });
        let h = mix64(42);
        for _ in 0..2000 {
            f.touch_at(h, 0, 1000);
        }
        for w in [5_000u64, 10_000, 60_000] {
            let r = f.count_at(h, w, 1001);
            if with_hot {
                if r.count != 2000 || r.upper < 2000 || r.saturated {
                    errorf!(t, "hot: {}", show(&r));
                }
            } else if !r.saturated {
                errorf!(t, "no hot entries: {}", show(&r));
            }
        }
        if f.promoted() != u64::from(with_hot) {
            errorf!(t, "hot {with_hot}: promoted {}", f.promoted());
        }
    }
}

/// TestResizeKeepsCounts: during a resize's transition a key keeps its events
/// (reads add the old filter's answer, a touch moves the key's slot), and
/// once a horizon has passed the new filter answers alone (format.md,
/// Resizing; claims D28).
#[test]
fn TestResizeKeepsCounts() {
    let t = T::new("TestResizeKeepsCounts");
    if modes() {
        skip!(t, "ConstantTime and Background filters can't be Resizable");
    }
    let cfg = Config { capacity: 500, horizon: MINUTE, resolution: ms(1), hot: -1, key: Some(test_key()), ..Config::default() };
    let z = match Resizable::new(cfg.clone()) {
        Ok(z) => z,
        Err(e) => fatalf!(t, "{e}"),
    };
    let hasher = must(cfg);
    let mut now = 1000u64;
    for k in 0..300u64 {
        for _ in 0..3 {
            z.touch_at(hasher.hash_uint64(k), 60_000, now);
        }
    }
    if let Err(e) = z.resize(4000, now) {
        fatalf!(t, "{e}");
    }
    now += 10;
    for k in (0..300u64).step_by(2) {
        z.touch_at(hasher.hash_uint64(k), 60_000, now);
    }
    now += 10;
    for k in 0..300u64 {
        let want = if k % 2 == 0 { 4 } else { 3 };
        let r = z.count_at(hasher.hash_uint64(k), 60_000, now);
        if r.count != want {
            errorf!(t, "key {k}: {}, want {want}", show(&r));
            break;
        }
    }
    now += 70_000; // past the transition: the old filter is gone with what it held
    z.touch_at(hasher.hash_uint64(0), 60_000, now);
    let r = z.count_at(hasher.hash_uint64(0), 60_000, now);
    if r.count != 1 {
        errorf!(t, "after the transition: {}, want Count 1", show(&r));
    }
}
