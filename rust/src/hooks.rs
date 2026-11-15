//! The reference's package variables: the test hooks (`testHook*`, each nil
//! unless a test sets it), `forceCT`, `forceBG` and `buzzOff`.
//!
//! A hook is a `static` whose `run` costs one relaxed load while unset, as
//! the reference's nil check of a package variable. Hooks are process-wide,
//! as in Go: the suite runs one test at a time (`RUST_TEST_THREADS=1`, set in
//! `.cargo/config.toml`), as `go test` runs a package's tests.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

type HookFn<A> = Arc<dyn Fn(A) + Send + Sync>;

/// A test hook: `func()` (`Hook`) or `func(i int)` (`Hook<isize>`), nil
/// until set.
pub(crate) struct Hook<A: 'static = ()> {
    on: AtomicBool,
    f: Mutex<Option<HookFn<A>>>,
}

#[allow(dead_code)] // set and clear: the tests'
impl<A: 'static> Hook<A> {
    pub(crate) const fn new() -> Self {
        Hook { on: AtomicBool::new(false), f: Mutex::new(None) }
    }

    /// `if testHookX != nil { testHookX(a) }`.
    #[inline(always)]
    pub(crate) fn run(&self, a: A) {
        if self.on.load(Ordering::Relaxed) {
            self.call(a);
        }
    }

    /// `testHookX != nil`.
    #[inline(always)]
    pub(crate) fn is_set(&self) -> bool {
        self.on.load(Ordering::Relaxed)
    }

    #[cold]
    fn call(&self, a: A) {
        // The hook is taken out of the lock before it runs: a hook may clear
        // or replace itself, and may run the code that runs it.
        let f = self.f.lock().unwrap_or_else(|e| e.into_inner()).clone();
        if let Some(f) = f {
            f(a);
        }
    }

    /// `testHookX = f`.
    pub(crate) fn set(&self, f: impl Fn(A) + Send + Sync + 'static) {
        *self.f.lock().unwrap_or_else(|e| e.into_inner()) = Some(Arc::new(f));
        self.on.store(true, Ordering::SeqCst);
    }

    /// `testHookX = nil`.
    pub(crate) fn clear(&self) {
        self.on.store(false, Ordering::SeqCst);
        *self.f.lock().unwrap_or_else(|e| e.into_inner()) = None;
    }
}

// Test hooks, if set: COPIED runs between a relocation's copy and its
// hand-off, VICTIM between an eviction's choice of victim and its install,
// TAKEN after a touch takes a waiting key out of the wait room, STAMPED after
// a touch moves its slot's last touch to a later unit, before the register
// follows (realign, word1 busy); PICKED after a relocation reads the entry at
// word index i, before it copies it; PLACING after the Background placer
// marks a waiting key's wait-room entry busy, before it installs the key;
// SWEEPING after the decay sweep's first write to an expired slot, before its
// last; INSTALLING after an install takes slot i's word1 busy, before its
// word0; BUMPING after bump checks slot i's words, before its count's CAS;
// BUMPED after that CAS succeeds, before bump reads word0 again; OPEN_SEEN
// after an insert reads slot i's word0 free or expired, before its word1;
// SWEEP_SEEN after the decay sweep reads slot i's word0, before its word1;
// SWEEP_READ after it reads word1, before its hold; HOT_ADDING after bump
// checks slot i's words and finds a hot pointer, before it adds to the entry;
// STAMPING after a hot key's touch into a later unit reads word0 and the
// pointer, before its stamp.
pub(crate) static COPIED: Hook = Hook::new();
pub(crate) static VICTIM: Hook = Hook::new();
pub(crate) static TAKEN: Hook = Hook::new();
pub(crate) static STAMPED: Hook = Hook::new();
pub(crate) static STAMPING: Hook = Hook::new();
pub(crate) static PLACING: Hook = Hook::new();
pub(crate) static SWEEPING: Hook = Hook::new();
pub(crate) static PICKED: Hook<isize> = Hook::new();
pub(crate) static INSTALLING: Hook<isize> = Hook::new();
pub(crate) static BUMPING: Hook<isize> = Hook::new();
pub(crate) static BUMPED: Hook<isize> = Hook::new();
pub(crate) static OPEN_SEEN: Hook<isize> = Hook::new();
pub(crate) static SWEEP_SEEN: Hook<isize> = Hook::new();
pub(crate) static SWEEP_READ: Hook<isize> = Hook::new();
pub(crate) static HOT_ADDING: Hook<isize> = Hook::new();

// GHOST_READ runs after the overflow table's estimate loads its buckets,
// before it checks the epoch again; GHOST_ADDING after an add's pass loads
// the epoch, before it scans its buckets; GHOST_FLOOR after a floor raise
// loads the epoch, before it loads the floor. The last two open the window an
// epoch move must not slip into: what the pass loads next it decodes against
// an epoch that may no longer be current.
pub(crate) static GHOST_READ: Hook = Hook::new();
pub(crate) static GHOST_ADDING: Hook = Hook::new();
pub(crate) static GHOST_FLOOR: Hook = Hook::new();

/// forceCT turns ConstantTime on in every filter (tests: `TF_CT=1`).
pub(crate) static FORCE_CT: AtomicBool = AtomicBool::new(false);

/// forceBG turns Background on in every filter (tests: `TF_BG=1`).
pub(crate) static FORCE_BG: AtomicBool = AtomicBool::new(false);

/// buzzOff turns buzz entries off in every filter (tests: the register's own
/// decisions, beside them).
pub(crate) static BUZZ_OFF: AtomicBool = AtomicBool::new(false);
