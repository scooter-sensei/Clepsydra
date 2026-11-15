//! conformance_test.go: the conformance vectors, language-neutral traces in
//! ../spec/vectors/ that pin the filter's observable behaviour bit for bit.
//! Every port must reproduce them; docs/specification/conformance.md
//! describes the format. The reference writes them (`go test -run
//! TestConformance -update`); this port only replays them.

use std::time::Duration;

use super::check::*;
use super::json::Json;
use crate::{Config, Filter, Result};

pub(crate) const VECTOR_DIR: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/../spec/vectors");

/// vecKey is the SipHash key every vector uses: bytes 0x00..0x0f.
pub(crate) fn vec_key() -> [u8; 16] {
    std::array::from_fn(|i| i as u8)
}

/// vecConfig.config: the vector's Config, on a clock the replay moves.
fn config(c: &Json, clock: &TestClock) -> Config {
    Config {
        capacity: c.get("capacity").i64(),
        horizon: Duration::from_millis(c.get("horizon_ms").u64()),
        resolution: Duration::from_millis(c.get("resolution_ms").u64()),
        load: c.get("load").f64(),
        stash: c.get("stash").i64(),
        overflow: c.get("overflow").i64(),
        spill_min: c.get("spill_min").i64(),
        key: Some(vec_key()),
        clock: clock.func(),
        ..Config::default()
    }
}

fn want_result(w: &Json) -> Result {
    Result {
        seen: w.get("seen").bool(),
        count: w.get("count").i64(),
        upper: w.get("upper").i64(),
        saturated: w.get("saturated").bool(),
        restored: w.get("restored").bool(),
        degraded: w.get("degraded").bool(),
    }
}

/// The end counters, as the vector's "end" object lists them.
fn end_counters(f: &Filter, now: u64) -> [(&'static str, u64); 7] {
    [
        ("evictions", f.evictions()),
        ("relocations", f.relocations()),
        ("lost", f.lost()),
        ("shed", f.shed()),
        ("restored", f.restored()),
        ("occupancy", f.occupancy(now) as u64),
        ("slots", f.slots() as u64),
    ]
}

/// Replays one vector file and checks its hashes, every operation's full
/// Result and the end counters.
fn run_vector(t: &T, name: &str) {
    let path = format!("{VECTOR_DIR}/{name}.json");
    let b = match std::fs::read_to_string(&path) {
        Ok(b) => b,
        Err(e) => fatalf!(t, "{path}: {e} (generate with the reference: go test -run TestConformance -update)"),
    };
    let want = match Json::parse(&b) {
        Ok(v) => v,
        Err(e) => fatalf!(t, "{path}: {e}"),
    };
    if (force_ct() || force_bg()) && want.get("end").get("evictions").u64() > 0 {
        logf!(t, "{name}: skipped: ConstantTime defers inserts that find no free slot, so which keys are evicted differs");
        return;
    }
    let clock = TestClock::new(0);
    let f = match Filter::new(config(want.get("config"), &clock)) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{name}: {e}"),
    };
    for h in want.get("hashes").arr() {
        let got = if h.has("string") { f.hash(h.get("string").str()) } else { f.hash_uint64(h.get("uint64").u64()) };
        let got = format!("{got:016x}");
        if got != h.get("hash").str() {
            errorf!(t, "{name}: hash of {:?}: got {got} want {}", h, h.get("hash").str());
        }
    }
    let mut bad = 0;
    for (i, op) in want.get("ops").arr().iter().enumerate() {
        clock.set(op.get("now_ms").u64());
        let w = Duration::from_millis(op.get("window_ms").u64());
        let r = match op.get("op").str() {
            "touch" => f.touch(op.get("key").str(), w),
            "count" => f.count(op.get("key").str(), w),
            other => fatalf!(t, "{name}: unknown op {other:?}"),
        };
        if r != want_result(op.get("want")) {
            bad += 1;
            if bad <= 5 {
                errorf!(t, "{name}: op {i} {op:?}: got {r:?}");
            }
        }
    }
    if bad > 5 {
        errorf!(t, "{name}: ... {bad} ops differ in all");
    }
    for (k, got) in end_counters(&f, clock.get()) {
        let w = want.get("end").get(k).u64();
        if got != w {
            errorf!(t, "{name}: end counters: {k}: got {got} want {w}");
        }
    }
    f.close();
    logf!(t, "{name}: {} ops reproduced", want.get("ops").arr().len());
}

#[test]
fn TestConformance() {
    let t = T::new("TestConformance");
    for name in ["basic", "burst", "churn", "overflow"] {
        run_vector(&t, name);
    }
}

/// One operation of a vector's stream: op, key, window (ms), clock (ms).
type VecOp = (&'static str, String, i64, u64);

/// vecScenarios' operation streams (conformance_test.go), from the seed the
/// reference writes the vectors with: rand.New(rand.NewPCG(7, 7)) per
/// scenario.
fn scenario_ops(name: &str) -> Vec<VecOp> {
    let mut rng = super::gorand::Rand::new(7, 7);
    let mut ops: Vec<VecOp> = Vec::new();
    match name {
        "basic" => {
            let mut now = 1000u64;
            for i in 0..400 {
                let k = format!("user:{}", rng.int_n(12));
                let w = [1i64, 250, 1000, 5000, 30_000, 60_000][rng.int_n(6) as usize];
                let op = if i % 3 == 2 { "count" } else { "touch" };
                ops.push((op, k.clone(), w, now));
                if i % 25 == 0 {
                    // an event exactly one window ago is inside it
                    ops.push(("count", k, 250, now + 250));
                }
                now += rng.int_n(400) as u64;
            }
        }
        "burst" => {
            let mut now = 5u64;
            for i in 0..600 {
                ops.push(("touch", "hot".into(), 1000, now));
                if i % 20 == 0 {
                    for w in [100i64, 2500, 10_000, 60_000] {
                        ops.push(("count", "hot".into(), w, now));
                    }
                }
                now += if i < 300 { rng.int_n(3) } else { rng.int_n(400) } as u64;
            }
            for _ in 0..40 {
                now += 2000;
                ops.push(("count", "hot".into(), 60_000, now));
            }
        }
        "churn" => {
            let mut now = 1u64;
            for i in 0..300 {
                ops.push(("touch", format!("rep:{}", i % 8), 60_000, now));
                now += rng.int_n(20) as u64;
            }
            for i in 0..4 * 64 * 4 {
                ops.push(("touch", format!("flood:{i}"), 60_000, now));
                if i % 16 == 0 {
                    ops.push(("touch", format!("rep:{}", rng.int_n(8)), 60_000, now));
                }
                now += rng.int_n(15) as u64;
            }
            for i in 0..8 {
                ops.push(("count", format!("rep:{i}"), 60_000, now));
            }
            for i in 0..64 {
                ops.push(("count", format!("never:{i}"), 60_000, now));
            }
        }
        _ => {
            // overflow
            let mut now = 1u64;
            let n = 3 * 64;
            for round in 0..8 {
                for i in 0..n {
                    if round < 4 || i % 2 == 0 {
                        ops.push(("touch", format!("mid:{i}"), 60_000, now));
                    }
                    now += rng.int_n(10) as u64;
                }
            }
            for i in 0..n {
                ops.push(("count", format!("mid:{i}"), [5000i64, 60_000][i % 2], now));
            }
            for i in 0..64 {
                ops.push(("count", format!("never:{i}"), 60_000, now));
            }
        }
    }
    ops
}

/// TestConformanceScenarios: the vectors' operation streams are what the
/// reference's generators (vecScenarios) draw from their seed with this
/// port's math/rand/v2, op for op: the vectors test the filter, and this
/// tests that the port reads Go's streams as Go does. (The C++ port's.)
#[test]
fn TestConformanceScenarios() {
    let t = T::new("TestConformanceScenarios");
    for name in ["basic", "burst", "churn", "overflow"] {
        let path = format!("{VECTOR_DIR}/{name}.json");
        let b = match std::fs::read_to_string(&path) {
            Ok(b) => b,
            Err(e) => fatalf!(t, "{path}: {e}"),
        };
        let want = match Json::parse(&b) {
            Ok(v) => v,
            Err(e) => fatalf!(t, "{path}: {e}"),
        };
        let (got, want) = (scenario_ops(name), want.get("ops").arr());
        if got.len() != want.len() {
            errorf!(t, "{name}: {} ops generated, the vector has {}", got.len(), want.len());
        }
        for (i, (g, w)) in got.iter().zip(want).enumerate() {
            if g.0 != w.get("op").str() || g.1 != w.get("key").str() || g.2 != w.get("window_ms").i64() || g.3 != w.get("now_ms").u64() {
                errorf!(t, "{name}: op {i} differs: generated {g:?}, the vector's {w:?}");
                break;
            }
        }
    }
}
