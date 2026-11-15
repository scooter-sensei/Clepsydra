//! smoke_test.go.

use std::time::Duration;

use super::check::*;
use super::conformance_test::vec_key;
use crate::{Config, Filter};

/// TestSmoke calls every public operation once on a default filter and
/// checks the obvious answers. It is the smoke tier (test.sh smoke): a build
/// that fails it is not worth testing further.
#[test]
fn TestSmoke() {
    let t = T::new("TestSmoke");
    let minute = Duration::from_secs(60);
    for cfg in [
        Config { capacity: 1000, horizon: minute, ..Config::default() },
        Config { capacity: 1000, horizon: minute, coarse_clock: Duration::from_millis(1), ..Config::default() },
        Config { capacity: 1000, horizon: minute, key: Some(vec_key()), constant_time: Duration::from_micros(1), ..Config::default() },
    ] {
        let f = match Filter::new(cfg) {
            Ok(f) => f,
            Err(e) => fatalf!(t, "{e}"),
        };
        let w = minute;
        if f.check_and_touch("a", w) {
            errorf!(t, "first CheckAndTouch reported seen");
        }
        if !f.check_and_touch("a", w) {
            errorf!(t, "second CheckAndTouch reported unseen");
        }
        let (seen, n) = f.touch_and_count("a", w);
        if !seen || n != 3 {
            errorf!(t, "TouchAndCount = {seen}, {n}; want true, 3");
        }
        let n = f.velocity_count("a", w);
        if n != 3 {
            errorf!(t, "VelocityCount = {n}; want 3");
        }
        let r = f.touch("a", w);
        if !r.seen || r.count != 4 || r.upper < 4 || r.saturated || r.degraded {
            errorf!(t, "Touch = {r:?}");
        }
        let r = f.count("b", w);
        if r.seen || r.count != 0 {
            errorf!(t, "Count of an unseen key = {r:?}");
        }
        let (r, ok) = f.touch_if_under("c", w, 1);
        if !ok || r.count != 1 {
            errorf!(t, "TouchIfUnder under the limit = {r:?}, {ok}");
        }
        let (r, ok) = f.touch_if_under("c", w, 1);
        if ok || r.upper < 1 {
            errorf!(t, "TouchIfUnder at the limit = {r:?}, {ok}");
        }
        let now = f.now();
        let hs = [f.hash_uint64(1), f.hash_uint64(2), f.hash_uint64(1)];
        let mut seen = vec![false; hs.len()];
        let mut counts = vec![0i64; hs.len()];
        f.check_and_touch_batch(&hs, f.ticks(w), now, &mut seen);
        f.touch_and_count_batch(&hs, f.ticks(w), now, &mut seen, &mut counts);
        if !seen[0] || counts[0] != 3 || counts[1] != 2 {
            errorf!(t, "batches: seen {seen:?} counts {counts:?}");
        }
        f.velocity_count_batch(&hs, f.ticks(w), now, &mut counts);
        if counts[0] != 4 || counts[1] != 2 {
            // key 1 four times, key 2 twice
            errorf!(t, "VelocityCountBatch = {counts:?}");
        }
        if f.contains_at(f.hash_uint64(3), f.ticks(w), now) {
            errorf!(t, "ContainsAt of an unseen key");
        }
        f.continuous_decay();
        let occ = f.occupancy(f.now());
        if occ < 3 || occ > f.slots() {
            errorf!(t, "Occupancy {occ} of {} slots", f.slots());
        }
        f.close();
    }
    if Filter::new(Config::default()).is_ok() {
        errorf!(t, "New accepted a zero Config");
    }
}

/// TestSmokeOptional calls the optional operations once (the smoke tier
/// calls every public operation): Merge, a snapshot round trip, and a
/// Resizable's transition. ConstantTime and Background filters refuse all
/// three. (The C++ port's; the reference's smoke test leaves them to their
/// own tests.)
#[test]
fn TestSmokeOptional() {
    let t = T::new("TestSmokeOptional");
    let modes = force_ct() || force_bg();
    let minute = Duration::from_secs(60);
    let clock = TestClock::new(1000);
    let cfg = Config { capacity: 1000, horizon: minute, key: Some(vec_key()), clock: clock.func(), ..Config::default() };
    let (f, g) = match (Filter::new(cfg.clone()), Filter::new(cfg.clone())) {
        (Ok(f), Ok(g)) => (f, g),
        (Err(e), _) | (_, Err(e)) => fatalf!(t, "{e}"),
    };
    f.touch("a", minute);
    g.touch("a", minute);
    g.touch("b", minute);
    match f.merge(&g) {
        Ok(()) => {
            if modes {
                errorf!(t, "Merge accepted a ConstantTime or Background filter");
            }
            let (a, b) = (f.count("a", minute).count, f.count("b", minute).count);
            if a != 2 || b != 1 {
                errorf!(t, "after Merge: a counts {a}, b {b}; want 2, 1");
            }
        }
        Err(e) if !modes => errorf!(t, "Merge refused: {e}"),
        Err(_) => {}
    }
    let mut buf = Vec::new();
    match f.write_to(&mut buf).and_then(|n| {
        let h = Filter::new(cfg.clone())?;
        let m = h.read_from(&mut buf.as_slice())?;
        Ok((n, m, h))
    }) {
        Ok((n, m, h)) => {
            if modes {
                errorf!(t, "the snapshot round trip accepted a ConstantTime or Background filter");
            }
            if n != m || n != buf.len() as i64 {
                errorf!(t, "WriteTo wrote {n} bytes ({} in the buffer), ReadFrom read {m}", buf.len());
            }
            for k in ["a", "b"] {
                let (got, want) = (h.count(k, minute), f.count(k, minute));
                if got != want {
                    errorf!(t, "restored {k}: {got:?}, want {want:?}");
                }
            }
        }
        Err(e) if !modes => errorf!(t, "snapshot refused: {e}"),
        Err(_) => {}
    }
    match crate::Resizable::new(Config { hot: -1, ..cfg }) {
        Ok(z) => {
            if modes {
                errorf!(t, "Resizable accepted ConstantTime or Background");
            }
            let (h1, win) = (f.hash_uint64(1), f.ticks(minute));
            z.touch_at(h1, win, clock.get());
            if let Err(e) = z.resize(4000, clock.get()) {
                errorf!(t, "Resize: {e}");
            }
            let now = clock.add(1);
            let r = z.touch_at(h1, win, now);
            if r.count != 2 {
                errorf!(t, "after Resize, TouchAt: {r:?}, want Count 2");
            }
            if !z.contains_at(h1, win, now) {
                errorf!(t, "after Resize, ContainsAt: false");
            }
            let v = z.velocity_count_at(h1, win, now);
            if v != 2 {
                errorf!(t, "after Resize, VelocityCountAt: {v}, want 2");
            }
            let r = z.count_at(h1, win, now);
            if r.upper != 2 {
                errorf!(t, "after Resize, CountAt: {r:?}, want Upper 2");
            }
            if z.bytes() <= 0 {
                errorf!(t, "Resizable's Bytes() = {}", z.bytes());
            }
            if z.evictions() != 0 {
                errorf!(t, "Resizable's Evictions() = {}, want 0", z.evictions());
            }
        }
        Err(e) if !modes => errorf!(t, "Resizable refused: {e}"),
        Err(_) => {}
    }
}
