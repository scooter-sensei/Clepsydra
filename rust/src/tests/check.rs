//! Go's `testing` package in miniature, over the standard test harness: a
//! test makes a `T`, `errorf!` records a failure and continues (t.Errorf),
//! `fatalf!` stops the test (t.Fatalf), `logf!` logs (t.Logf), `skip!` skips
//! it (t.Skip). A `T` that recorded a failure fails its test when it goes out
//! of scope. Failures may be recorded from any thread (t.Errorf is
//! goroutine-safe).
//!
//! The suite reads the Go suite's switches from the environment: `TF_CT=1`
//! (every filter ConstantTime), `TF_BG=1` (every filter Background),
//! `TF_NOCAS2=1` (realign's hold instead of its 16-byte CAS), and `TF_SHORT=1` for `go test -short` (the long tests run fewer
//! rounds). Log lines are printed indented by four spaces, as `go test -v`
//! prints them; run with `--nocapture` to see them.

use std::io::Write;
use std::sync::Once;
use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicBool, AtomicU64};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crate::hooks::{self, Hook};
use crate::{ClockFn, Config, Filter};

static INIT: Once = Once::new();

/// Reads the suite's switches once (the reference's TestMain). Every filter's
/// construction calls it.
pub(crate) fn init() {
    INIT.call_once(|| {
        hooks::FORCE_CT.store(env_on("TF_CT"), SeqCst);
        hooks::FORCE_BG.store(env_on("TF_BG"), SeqCst);
        if env_on("TF_NOCAS2") {
            crate::cpu::set_has_cas2(false); // realign holds the slot, as on other architectures
        }
    });
}

/// Go: `os.Getenv(name) != ""` (so `TF_CT=0` is on, as there).
pub(crate) fn env_on(name: &str) -> bool {
    std::env::var_os(name).is_some_and(|v| !v.is_empty())
}

/// `os.Getenv(name)`.
pub(crate) fn getenv(name: &str) -> String {
    std::env::var(name).unwrap_or_default()
}

/// `testing.Short()`.
pub(crate) fn short() -> bool {
    env_on("TF_SHORT")
}

/// `forceCT`.
pub(crate) fn force_ct() -> bool {
    init();
    hooks::FORCE_CT.load(SeqCst)
}

/// `forceBG`.
pub(crate) fn force_bg() -> bool {
    init();
    hooks::FORCE_BG.load(SeqCst)
}

static OUT: Mutex<()> = Mutex::new(());

/// `TF_LOG=<file>`: the tests' lines are appended to it too, as `go test -v`
/// prints them (`=== RUN`, the log indented by four spaces, `--- PASS`),
/// without the standard harness's own lines between them: what test.sh's
/// regression tier compares with go/verify/expected/.
fn log_file() -> Option<&'static Mutex<std::fs::File>> {
    static FILE: std::sync::OnceLock<Option<Mutex<std::fs::File>>> = std::sync::OnceLock::new();
    FILE.get_or_init(|| {
        let path = std::env::var_os("TF_LOG").filter(|p| !p.is_empty())?;
        std::fs::OpenOptions::new().create(true).append(true).open(path).ok().map(Mutex::new)
    })
    .as_ref()
}

fn out_file(line: &str) {
    if let Some(f) = log_file() {
        let mut f = f.lock().unwrap_or_else(std::sync::PoisonError::into_inner);
        let _ = writeln!(f, "{line}");
    }
}

fn out(line: &str) {
    let _g = OUT.lock().unwrap_or_else(std::sync::PoisonError::into_inner);
    let mut o = std::io::stdout().lock();
    let _ = writeln!(o, "{line}");
    let _ = o.flush();
    out_file(line);
}

/// A test's `*testing.T`.
pub(crate) struct T {
    name: &'static str,
    failed: AtomicBool,
}

impl T {
    pub(crate) fn new(name: &'static str) -> T {
        init();
        out_file(&format!("=== RUN   {name}"));
        T { name, failed: AtomicBool::new(false) }
    }

    /// t.Errorf's record: the failure, with where it was reported.
    pub(crate) fn fail(&self, file: &str, line: u32, msg: &str) {
        self.failed.store(true, SeqCst);
        out(&format!("    {file}:{line}: {msg}"));
    }

    /// t.Logf.
    pub(crate) fn log(&self, msg: &str) {
        // As testing's decorate: a message's final newline ends its last
        // line, it doesn't start another.
        for l in msg.strip_suffix('\n').unwrap_or(msg).split('\n') {
            out(&format!("    {l}"));
        }
    }

    /// A line whatever the verbosity (the experiments' tables, which the
    /// regression tier compares with go/verify/expected/).
    pub(crate) fn print(&self, msg: &str) {
        out(msg);
    }

    pub(crate) fn skipped(&self, msg: &str) {
        out(&format!("--- SKIP: {} ({msg})", self.name));
    }
}

impl Drop for T {
    fn drop(&mut self) {
        let failed = self.failed.load(SeqCst) || std::thread::panicking();
        out_file(&format!("--- {}: {}", if failed { "FAIL" } else { "PASS" }, self.name));
        if self.failed.load(SeqCst) && !std::thread::panicking() {
            panic!("--- FAIL: {}", self.name);
        }
    }
}

/// t.Errorf.
macro_rules! errorf {
    ($t:expr, $($arg:tt)*) => {
        $t.fail(file!(), line!(), &format!($($arg)*))
    };
}

/// t.Fatalf.
macro_rules! fatalf {
    ($t:expr, $($arg:tt)*) => {{
        $t.fail(file!(), line!(), &format!($($arg)*));
        panic!("FailNow")
    }};
}

/// t.Logf.
macro_rules! logf {
    ($t:expr, $($arg:tt)*) => {
        $t.log(&format!($($arg)*))
    };
}

/// t.Skip: leaves the test function.
macro_rules! skip {
    ($t:expr, $($arg:tt)*) => {{
        $t.skipped(&format!($($arg)*));
        return;
    }};
}

pub(crate) use {errorf, fatalf, logf, skip};

/// Clears a test hook when it goes out of scope (the reference's
/// `defer func() { testHookX = nil }()`).
pub(crate) struct HookReset<A: 'static>(&'static Hook<A>);

impl<A: 'static> Drop for HookReset<A> {
    fn drop(&mut self) {
        self.0.clear();
    }
}

/// `testHookX = f`, undone at scope end.
#[must_use]
pub(crate) fn set_hook<A: 'static>(h: &'static Hook<A>, f: impl Fn(A) + Send + Sync + 'static) -> HookReset<A> {
    h.set(f);
    HookReset(h)
}

/// A clock a test moves: `var now uint64` and `Clock: func() uint64 { return
/// now }`.
#[derive(Clone)]
pub(crate) struct TestClock(pub(crate) Arc<AtomicU64>);

impl TestClock {
    pub(crate) fn new(now: u64) -> TestClock {
        TestClock(Arc::new(AtomicU64::new(now)))
    }
    pub(crate) fn get(&self) -> u64 {
        self.0.load(SeqCst)
    }
    pub(crate) fn set(&self, now: u64) {
        self.0.store(now, SeqCst);
    }
    pub(crate) fn add(&self, d: u64) -> u64 {
        self.0.fetch_add(d, SeqCst) + d
    }
    /// The `Config::clock` that reads it.
    pub(crate) fn func(&self) -> Option<ClockFn> {
        let c = self.0.clone();
        Some(Arc::new(move || c.load(SeqCst)))
    }
}

/// `New(cfg)`, failing the test on an error (`if err != nil { t.Fatal(err) }`).
#[track_caller]
pub(crate) fn must(cfg: Config) -> Filter {
    match Filter::new(cfg) {
        Ok(f) => f,
        Err(e) => panic!("New: {e}"),
    }
}

pub(crate) const MINUTE: Duration = Duration::from_secs(60);

pub(crate) fn secs(n: u64) -> Duration {
    Duration::from_secs(n)
}

pub(crate) fn ms(n: u64) -> Duration {
    Duration::from_millis(n)
}

pub(crate) fn us(n: u64) -> Duration {
    Duration::from_micros(n)
}

/// newTest: a filter of the given capacity and horizon at a 1 ms resolution.
#[track_caller]
pub(crate) fn new_test(capacity: i64, horizon: Duration) -> Filter {
    must(Config { capacity, horizon, resolution: ms(1), ..Config::default() })
}

/// settle waits until a Background filter's placer has placed every waiting
/// key and exited, for tests that read the table's words directly.
pub(crate) fn settle(f: &Filter) {
    while f.placing.load(SeqCst) || (f.bg && !f.ct && f.deferred.load(SeqCst) > 0) {
        std::thread::sleep(us(100));
    }
}

/// Reports whether call panics (the reference's `recover() != nil`).
pub(crate) fn panics(call: impl FnOnce()) -> bool {
    std::panic::catch_unwind(std::panic::AssertUnwindSafe(call)).is_err()
}

/// Go's `%.<prec>e`: the exponent signed, with at least two digits.
pub(crate) fn go_e(x: f64, prec: usize) -> String {
    let s = format!("{x:.prec$e}");
    match s.split_once('e') {
        Some((m, e)) => {
            let e: i32 = e.parse().unwrap_or(0);
            format!("{m}e{}{:02}", if e < 0 { '-' } else { '+' }, e.abs())
        }
        None => s,
    }
}

thread_local! {
    static RAND: std::cell::Cell<u64> = std::cell::Cell::new({
        use std::hash::{BuildHasher, Hasher};
        let mut h = std::collections::hash_map::RandomState::new().build_hasher();
        h.write_u64(0x2545f4914f6cdd1d);
        h.finish() | 1
    });
}

/// `rand.Uint64()` of math/rand/v2's top-level functions: a per-thread
/// generator, randomly seeded, no state shared between threads.
pub(crate) fn rand_uint64() -> u64 {
    RAND.with(|s| {
        // xorshift64*
        let mut x = s.get();
        x ^= x >> 12;
        x ^= x << 25;
        x ^= x >> 27;
        s.set(x);
        x.wrapping_mul(0x2545f4914f6cdd1d)
    })
}

/// Go's `%v` (and `%g`) of a float64: strconv's shortest 'g'. The shortest
/// digits that read back; `%e` form when the exponent is < -4 or >= 21 never
/// applies here: strconv decides, for the shortest, at < -4 or >= 6 (Go
/// prints 100000 and 1e+06); exponents have two digits or more; NaN and the
/// infinities as Go's.
pub(crate) fn go_g(x: f64) -> String {
    if x.is_nan() {
        return "NaN".into();
    }
    if x.is_infinite() {
        return if x > 0.0 { "+Inf".into() } else { "-Inf".into() };
    }
    if x == 0.0 {
        return if x.is_sign_negative() { "-0".into() } else { "0".into() };
    }
    let s = format!("{x:e}"); // the shortest digits that read back: d[.ddd]e[-]x
    let (m, e) = s.split_once('e').expect("an exponent");
    let exp: i32 = e.parse().expect("an exponent");
    let digits = m.chars().filter(char::is_ascii_digit).count() as i32;
    if !(-4..6).contains(&exp) {
        return format!("{m}e{}{:02}", if exp < 0 { '-' } else { '+' }, exp.abs());
    }
    format!("{:.*}", (digits - 1 - exp).max(0) as usize, x)
}

/// Go's `time.Duration.String`, of nanoseconds: "1m0s", "1.5s", "250ms", "0s".
pub(crate) fn go_duration(d: i64) -> String {
    if d == 0 {
        return "0s".into();
    }
    let neg = d < 0;
    let mut u = d.unsigned_abs();
    // frac: the fraction of v / 10^prec without trailing zeros (".25"), and v / 10^prec.
    fn frac(v: &mut u64, prec: u32) -> String {
        let mut f = String::new();
        let mut print = false;
        for _ in 0..prec {
            let digit = *v % 10;
            print = print || digit != 0;
            if print {
                f.insert(0, (b'0' + digit as u8) as char);
            }
            *v /= 10;
        }
        if print { format!(".{f}") } else { f }
    }

    let s = if u < 1_000_000_000 {
        let (prec, unit) = if u < 1_000 {
            (0, "ns")
        } else if u < 1_000_000 {
            (3, "µs")
        } else {
            (6, "ms")
        };
        let f = frac(&mut u, prec);
        format!("{u}{f}{unit}")
    } else {
        let f = frac(&mut u, 9);
        let mut t = format!("{}{f}s", u % 60);
        u /= 60;
        if u > 0 {
            t = format!("{}m{t}", u % 60);
            u /= 60;
            if u > 0 {
                t = format!("{u}h{t}");
            }
        }
        t
    };
    if neg { format!("-{s}") } else { s }
}

/// `math.Lgamma`'s value: the C library's (as the C++ port's `std::lgamma`),
/// which the standard library doesn't offer yet. Its arguments here are small
/// positive integers.
pub(crate) fn lgamma(x: f64) -> f64 {
    unsafe extern "C" {
        #[link_name = "lgamma"]
        fn c_lgamma(x: f64) -> f64;
    }
    // SAFETY: lgamma takes and returns a double by value; the sign it also
    // stores in the C library's signgam is not read here.
    unsafe { c_lgamma(x) }
}

/// Go's `%7.4f`.
pub(crate) fn pad7(x: f64) -> String {
    format!("{x:7.4}")
}
