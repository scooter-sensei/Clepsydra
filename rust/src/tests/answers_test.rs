//! answers_test.go.

use std::collections::HashMap;
use std::time::Duration;

use super::check::*;
use super::gorand::Rand;
use super::helpers::*;
use crate::{Config, Filter, Result};

struct DigestCell {
    mode: &'static str,
    seed: u64,
    repeat: usize,
    spill1: bool,
    cms: bool,
}

/// TestAnswerDigest replays a seeded single-thread workload across 30 cells
/// and prints a digest of every value every call returns, per call kind, plus
/// the counters at the end.
#[test]
fn TestAnswerDigest() {
    let t = T::new("TestAnswerDigest");
    if getenv("TF_EXP") != "1" {
        skip!(t, "skipping answer digest test; TF_EXP=1 not set");
    }
    let modes = ["", "bg", "ct"];
    let seeds = [1u64, 2, 3];
    let mut cells: Vec<DigestCell> = Vec::new();
    // 1. seeds 1, 2, 3 × modes default / bg / ct × 64 repeat keys
    // 2. the same × 6,000 repeat keys
    // 3. the same × 6,000 with SpillMin: 1
    for (repeat, spill1) in [(64, false), (6000, false), (6000, true)] {
        for mode in modes {
            for seed in seeds {
                cells.push(DigestCell { mode, seed, repeat, spill1, cms: false });
            }
        }
    }
    // 4. seeds 1–3, default mode, 6,000 with SpillMin: 1, and f.cms = true
    for seed in seeds {
        cells.push(DigestCell { mode: "", seed, repeat: 6000, spill1: true, cms: true });
    }
    if cells.len() != 30 {
        fatalf!(t, "expected 30 cells, got {}", cells.len());
    }
    let key: [u8; 16] = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16];
    for c in &cells {
        let mut cfg = Config { capacity: 2048, horizon: MINUTE, resolution: ms(1), key: Some(key), ..Config::default() };
        if c.spill1 {
            cfg.spill_min = 1;
        }
        match c.mode {
            "ct" => cfg.constant_time = Duration::from_nanos(200),
            "bg" => cfg.background = true,
            _ => {}
        }
        let clock = TestClock::new(1);
        let mut now = 1u64;
        cfg.clock = clock.func();
        let f = match Filter::with(cfg, |core| {
            if c.mode == "bg" {
                core.bg_manual = true;
            }
            if c.cms {
                core.cms = true;
            }
        }) {
            Ok(f) => f,
            Err(e) => fatalf!(t, "{e}"),
        };
        let mut r = Rand::new(c.seed, 99);
        let mut hs: HashMap<&str, Fnv64a> = HashMap::new();
        // put is fmt.Fprint(h, vals...) and a ";": the values as Go prints
        // them, a space between two that aren't strings.
        let mut put = |kind: &'static str, vals: &str| {
            let h = hs.entry(kind).or_insert_with(Fnv64a::new);
            h.write(vals);
            h.write(";");
        };
        let res = |x: Result| format!("{}/{}/{}/{}/{}/{}", x.seen, x.count, x.upper, x.saturated, x.restored, x.degraded);
        let repeat: Vec<u64> = (0..c.repeat as u64).map(|i| f.hash_uint64(i + (1 << 40))).collect();
        let mut oneoff = 0u64;
        let mut live_seen_checks = 0;
        let wins = [1u64, 500, 1_000, 5_000, 15_000, 30_000, 60_000];
        let mut batch = [0u64; 40];
        let mut out = [false; 40];
        let mut counts = [0i64; 40];
        for op in 0..400_000i64 {
            let phase = (op / 25_000) % 5;
            if r.int_n(1000) < 3 + phase * 4 {
                now += 1;
                clock.set(now);
            }
            let w = wins[r.int_n(wins.len() as i64) as usize];

            let h = if r.int_n(100) < 30 {
                repeat[r.int_n(repeat.len() as i64) as usize]
            } else if r.int_n(100) < 10 && oneoff > 0 {
                f.hash_uint64(oneoff - r.int_n(oneoff.min(5000) as i64) as u64)
            } else {
                oneoff += 1;
                f.hash_uint64(oneoff)
            };
            let k = r.int_n(100);
            if k < 40 {
                let live = f.sketch_live(now);
                let seen = f.check_and_touch_at(h, w, now);
                if live && seen {
                    live_seen_checks += 1;
                }
                put("check", &seen.to_string());
            } else if k < 70 {
                put("touch", &res(f.touch_at(h, w, now)));
            } else if k < 85 {
                put("count", &res(f.count_at(h, w, now)));
            } else if k < 90 {
                put("contains", &f.contains_at(h, w, now).to_string());
            } else if k < 95 {
                for b in &mut batch {
                    *b = repeat[r.int_n(repeat.len() as i64) as usize];
                    if r.int_n(3) == 0 {
                        oneoff += 1;
                        *b = f.hash_uint64(oneoff);
                    }
                }
                f.check_and_touch_batch(&batch, w, now, &mut out);
                put("cbatch", &gov(&out));
            } else {
                for b in &mut batch {
                    *b = repeat[r.int_n(repeat.len() as i64) as usize];
                }
                f.touch_and_count_batch(&batch, w, now, &mut out, &mut counts);
                put("tcbatch", &format!("{} {}", gov(&out), gov(&counts)));
            }
            if c.mode == "bg" && r.int_n(50) == 0 {
                while f.room.get().is_some() && f.place_next(f.room.get(), now) {}
            }
            if r.int_n(500) == 0 {
                put("decay", &f.decay_step(now, 4).to_string());
            }
        }
        put(
            "end",
            &format!(
                "{} {} {} {} {} {} {} {} {} {} {} {} {} {} {}",
                f.evictions(),
                f.relocations(),
                f.restored(),
                f.folded(),
                f.shed(),
                f.floored(),
                f.lost(),
                f.promoted(),
                f.hot_refused(),
                f.demoted(),
                f.hot_in_use(),
                f.waited(),
                f.waiting(),
                f.occupancy(now),
                f.bytes()
            ),
        );
        if c.repeat == 6000 && live_seen_checks == 0 {
            fatalf!(
                t,
                "overflow cell {{mode:{} seed:{} repeat:{} spill1:{} cms:{}}} read 0 seen checks with overflow live",
                c.mode,
                c.seed,
                c.repeat,
                c.spill1,
                c.cms
            );
        }
        let mut line = format!("cell mode={:?} seed={} repeat={} spill1={} cms={}", c.mode, c.seed, c.repeat, c.spill1, c.cms);
        for k in ["check", "touch", "count", "contains", "cbatch", "tcbatch", "decay", "end"] {
            line += &format!(" {k}={:016x}", hs.get(k).map_or(0, Fnv64a::sum64));
        }
        line += &format!(
            " (evictions {}, folded {}, floored {}, promoted {}, waited {}, seen checks with overflow live {live_seen_checks})",
            f.evictions(),
            f.folded(),
            f.floored(),
            f.promoted(),
            f.waited()
        );
        t.print(&line);
        f.close();
    }
}
