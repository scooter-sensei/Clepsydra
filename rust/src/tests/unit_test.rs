//! The port's own unit tests (the C++ port's unit_test.cpp): what the Go
//! suite checks elsewhere (TestTickRange, the conformance vectors), from the
//! Rust API's side.

use std::time::Duration;

use super::check::*;
use super::conformance_test::vec_key;
use crate::{Config, Filter, MAX_TICK};

/// TestExactBoundaryContains: lastTouch is exact, so W is checked to the tick
/// by ContainsAt and CountAt too (a companion of TestExactBoundary).
#[test]
fn TestExactBoundaryContains() {
    let t = T::new("TestExactBoundaryContains");
    let clock = TestClock::new(1000);
    let f = match Filter::new(Config {
        capacity: 100,
        horizon: MINUTE,
        resolution: ms(1),
        key: Some(vec_key()),
        clock: clock.func(),
        ..Config::default()
    }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    let h = f.hash("k");
    f.touch_at(h, 0, clock.get());
    let now = clock.add(500);
    if !f.contains_at(h, 500, now) {
        errorf!(t, "ContainsAt(500) after 500 ticks: false, want true");
    }
    if f.contains_at(h, 499, now) {
        errorf!(t, "ContainsAt(499) after 500 ticks: true, want false");
    }
    if !f.count_at(h, 500, now).seen {
        errorf!(t, "CountAt(500) after 500 ticks: not Seen");
    }
    if f.count_at(h, 499, now).seen {
        errorf!(t, "CountAt(499) after 500 ticks: Seen");
    }
    if !f.check_and_touch_at(h, 500, now) {
        errorf!(t, "CheckAndTouchAt(500) after 500 ticks: false, want true");
    }
}

/// TestMaxTick: a Clock past MaxTick is refused at construction; an insert
/// past it panics before any lock and leaves the filter usable (claims A35);
/// a horizon of MaxTick ticks is refused.
#[test]
fn TestMaxTick() {
    let t = T::new("TestMaxTick");
    let clock = TestClock::new(MAX_TICK + 1);
    let cfg = Config { capacity: 100, horizon: MINUTE, clock: clock.func(), ..Config::default() };
    match Filter::new(cfg.clone()) {
        Ok(_) => errorf!(t, "New accepted a Clock past MaxTick"),
        Err(e) if e.message() != crate::filter::ERR_TICK => errorf!(t, "New with a Clock past MaxTick: {e}"),
        Err(_) => {}
    }
    clock.set(5);
    let f = match Filter::new(cfg) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    if !panics(|| {
        f.touch_at(f.hash("x"), 10, MAX_TICK + 1);
    }) {
        errorf!(t, "an insert past MaxTick did not panic");
    }
    let r = f.touch_at(f.hash("x"), 10, 6);
    if r.count != 1 {
        errorf!(t, "after the refused insert: {r:?}, want Count 1");
    }
    let r = f.touch_at(f.hash("x"), 10, 7);
    if r.count != 2 {
        errorf!(t, "then: {r:?}, want Count 2");
    }
    let big = Filter::new(Config { capacity: 10, horizon: Duration::from_nanos(MAX_TICK), resolution: Duration::from_nanos(1), ..Config::default() });
    if big.is_ok() {
        errorf!(t, "a horizon of MaxTick ticks was accepted");
    }
}

/// TestTicksNegative: the reference converts a negative duration to 0 ticks
/// (max(d, 0)). A Duration has no negative value, so the type rules that case
/// out; the shortest, zero, converts to 0 ticks, and a duration to whole
/// ticks of the resolution (the default: a microsecond).
#[test]
fn TestTicksNegative() {
    let t = T::new("TestTicksNegative");
    let f = match Filter::new(Config { capacity: 10, horizon: MINUTE, ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    let got = f.ticks(Duration::ZERO);
    if got != 0 {
        errorf!(t, "Ticks(0) = {got}, want 0");
    }
    let got = f.ticks(ms(3));
    if got != 3000 {
        errorf!(t, "Ticks(3ms) = {got}, want 3000");
    }
}

/// TestConfigDefaults: the sizes the specification's defaults give.
#[test]
fn TestConfigDefaults() {
    let t = T::new("TestConfigDefaults");
    let f = match Filter::new(Config { capacity: 1000, horizon: MINUTE, ..Config::default() }) {
        Ok(f) => f,
        Err(e) => fatalf!(t, "{e}"),
    };
    // lines = max(2, ceil(1000 / (0.85 * 4))) = 295
    if f.slots() != 295 * 4 {
        errorf!(t, "Slots() = {}, want {}", f.slots(), 295 * 4);
    }
    if f.bytes() != 295 * 64 {
        errorf!(t, "Bytes() = {}, want {}", f.bytes(), 295 * 64);
    }
}
