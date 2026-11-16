//! The port's own unit tests (the C++ port's unit_test.cpp): what the Go
//! suite checks elsewhere (TestTickRange, the conformance vectors), from the
//! Rust API's side.

use std::time::Duration;

use std::sync::atomic::Ordering::SeqCst;

use super::check::*;
use super::conformance_test::vec_key;
use super::helpers::{closed_within, go};
use crate::hooks;
use crate::slots::{CLAIM_STRIPE_BITS, mix64, tag};
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

/// TestDebug: every public type prints something (the API Guidelines'
/// C-DEBUG, C-DEBUG-NONEMPTY), none prints the filter's secret key, and a
/// Resizable whose mutex is held prints without waiting for it.
#[test]
fn TestDebug() {
    let t = T::new("TestDebug");
    let cfg = Config { capacity: 1000, horizon: MINUTE, key: Some([0xa5; 16]), hot: -1, ..Config::default() };
    let f = must(cfg.clone());
    let mut printed = vec![
        ("Config", format!("{cfg:?}")),
        ("Filter", format!("{f:?}")),
        ("Result", format!("{:?}", f.count("k", MINUTE))),
        ("Error", format!("{:?}", Filter::new(Config::default()).unwrap_err())),
    ];
    let r = crate::Resizable::new(cfg);
    if let Ok(r) = &r {
        printed.push(("Resizable", format!("{r:?}")));
        let _held = r.guard();
        let s = format!("{r:?}");
        if !s.contains("<locked>") {
            errorf!(t, "Resizable with its mutex held: {s}, want <locked>");
        }
    } else if !force_ct() && !force_bg() {
        errorf!(t, "NewResizable: {:?}", r.err());
    }
    for (what, s) in printed {
        if s.is_empty() || s.contains("165") || s.contains("a5") {
            errorf!(t, "{what}: {s:?} (empty, or the key's bytes, 0xa5 = 165)");
        }
    }
}

/// TestConfigUnaddressable: a Config whose table no allocation can hold
/// (2^57 lines of 64 B or more: an allocation ends at `isize::MAX` bytes), or
/// one that no memory can back, or whose Load is NaN, is refused by `new`.
/// The reference wraps such a table's word count and fails at its make or
/// at its first index. Here the suite's overflow checks panicked on the way,
/// a release build wrapped the count and built the filter on a short table
/// whose later lines panic at their first index, and a table the allocator
/// refused aborted the process.
#[test]
fn TestConfigUnaddressable() {
    let t = T::new("TestConfigUnaddressable");
    let outcome = |capacity: i64, load: f64, stash: i64| -> String {
        let cfg = Config { capacity, horizon: MINUTE, load, stash, ..Config::default() };
        let mut got = String::from("a panic"); // what a panic out of new leaves
        panics(|| {
            got = match Filter::new(cfg) {
                Ok(_) => "accepted".into(),
                Err(e) => e.message().into(),
            };
        });
        got
    };
    const TOO_LARGE: &str = "temporalfilter: Capacity too large for Load";
    const NO_MEMORY: &str = "temporalfilter: no memory for the table (Capacity too large for Load)";
    for (capacity, load, stash, want, why) in [
        (1i64 << 62, 1.0, 0, TOO_LARGE, "2^60 lines: 2^66 bytes"),
        (i64::MAX, 0.85, 0, TOO_LARGE, "2^61.4 lines: the word count wraps"),
        (i64::MAX, 1e-300, 0, TOO_LARGE, "more lines than a 64-bit integer holds"),
        ((1 << 62) + 1024, 0.5, 0, TOO_LARGE, "2^61+512 lines: the word count wraps to 4096"),
        (1 << 56, 1.0, 0, NO_MEMORY, "2^54 lines: 2^60 bytes, past any address space"),
        (1000, f64::NAN, 0, "temporalfilter: Load must be <= 1", "a NaN Load"),
        (1000, 1.0, 0, "accepted", "a small table"),
        (1000, 1.0, i64::MAX, "accepted", "a stash allocated at first use"),
    ] {
        let got = outcome(capacity, load, stash);
        if got != want {
            errorf!(t, "Capacity {capacity}, Load {load:?}, Stash {stash} ({why}): {got}, want {want}");
        }
    }
}

/// TestPanicReleasesStripe: a panic out of a first touch releases the claim
/// stripe the touch held (its guard drops as the panic unwinds), so the next
/// first touch on that stripe goes ahead. The panic stands in for a lazy
/// table sized past what an allocation holds (the stash, or the overflow
/// table: evict's stashPut reaches them with the stripe held); in Go such a
/// failure is fatal instead. Plain paths hold the stripe from the lookup to
/// the install, ConstantTime's for the whole touch.
#[test]
fn TestPanicReleasesStripe() {
    let t = T::new("TestPanicReleasesStripe");
    for constant_time in [false, true] {
        let f = std::sync::Arc::new(must(Config {
            capacity: 1000,
            horizon: MINUTE,
            resolution: ms(1),
            key: Some(vec_key()),
            clock: TestClock::new(1).func(),
            constant_time: if constant_time { Duration::from_micros(1) } else { Duration::ZERO },
            ..Config::default()
        }));
        let h = f.hash("a");
        let hook = set_hook(&hooks::OPEN_SEEN, |_| panic!("an allocation failed")); // the first slot search of an empty table
        let panicked = panics(|| {
            f.touch_at(h, 10, 1);
        });
        drop(hook);
        if !panicked {
            errorf!(t, "constant_time={constant_time}: the hook's panic didn't reach the caller");
            continue;
        }
        let mut h2 = 0; // another key on h's stripe
        let mut i = 0;
        while h2 == 0 || (h2 >> (64 - CLAIM_STRIPE_BITS)) != (h >> (64 - CLAIM_STRIPE_BITS)) {
            h2 = f.hash(&format!("b{i}"));
            i += 1;
        }
        let g = std::sync::Arc::clone(&f); // the thread keeps the filter if it never returns
        let done = go(move || {
            g.touch_at(h2, 10, 1);
        });
        if !closed_within(&done, secs(5)) {
            errorf!(t, "constant_time={constant_time}: a first touch on the stripe never returned (the stripe stayed held)");
            return;
        }
        if !f.count_at(h2, 10, 1).seen {
            errorf!(t, "constant_time={constant_time}: the second key isn't Seen");
        }
        let c = f.touch_at(h, 10, 1).count;
        if c != 1 {
            errorf!(t, "constant_time={constant_time}: the first key's count {c}, want 1 (the panic came before its install)");
        }
    }
}

/// TestPanicBalancesMoves: a first touch of a key waiting in Background's
/// wait room takes it out with its stripe's move begun, and ends the move
/// once the key is placed. A panic out of the placement (a lazy table sized
/// past what an allocation holds) ends the move too: otherwise every later
/// first touch on the stripe would wait for it forever, in every thread that
/// shares the filter. Here the slot search's hook panics at an occupant that
/// expired while the key waited; the key's history is lost with the panic.
#[test]
fn TestPanicBalancesMoves() {
    let t = T::new("TestPanicBalancesMoves");
    if force_ct() {
        skip!(t, "Background has no effect with ConstantTime");
    }
    let f = std::sync::Arc::new(
        Filter::with(
            Config {
                capacity: 4096,
                horizon: MINUTE,
                resolution: ms(1),
                key: Some(vec_key()),
                clock: TestClock::new(1).func(),
                background: true,
                ..Config::default()
            },
            |c| c.bg_manual = true,
        )
        .unwrap_or_else(|e| panic!("New: {e}")),
    );
    let mut key = 0; // a first touch that waits: its lines full at tick 1
    let mut i = 1;
    while key == 0 {
        let w = f.waited();
        f.touch_at(mix64(i), 0, 1);
        if f.waited() != w {
            key = mix64(i);
        }
        i += 1;
    }
    f.touch_at(key, 0, 2); // waits again, now with its last touch at 2
    if f.waiting() != 1 {
        fatalf!(t, "Waiting() = {}, want 1", f.waiting());
    }
    let now = 2 + f.horizon; // its lines' occupants (tick 1) expired; the key not yet
    let m = f.move_of(tag(key));
    let hook = set_hook(&hooks::OPEN_SEEN, |_| panic!("an allocation failed"));
    let panicked = panics(|| {
        f.touch_at(key, 0, now);
    });
    drop(hook);
    if !panicked {
        fatalf!(t, "the hook's panic didn't reach the caller");
    }
    let (b, d) = (m.begun.load(SeqCst), m.done.load(SeqCst));
    if b != d {
        errorf!(t, "the stripe's moves after the panic: {b} begun, {d} done");
    }
    let mut h2 = 0; // a new key on the waiting key's move stripe
    let mut i = 0;
    while h2 == 0 || ((tag(h2) ^ tag(key)) & ((1 << CLAIM_STRIPE_BITS) - 1)) != 0 {
        h2 = f.hash(&format!("m{i}"));
        i += 1;
    }
    let g = std::sync::Arc::clone(&f); // the thread keeps the filter if it never returns
    let done = go(move || {
        g.touch_at(h2, 0, now);
    });
    if !closed_within(&done, secs(5)) {
        fatalf!(t, "a first touch on the stripe never returned (its move count stayed unbalanced)");
    }
    if !f.count_at(h2, 10, now).seen {
        errorf!(t, "the new key isn't Seen");
    }
}
