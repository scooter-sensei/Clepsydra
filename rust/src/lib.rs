//! Clepsydra: a fixed-size filter that answers "have we seen key X in the
//! last W?" and "how many times in the last W?" for any W up to a configured
//! horizon. Hits never lock; memory is fixed at construction.
//!
//! This is the Rust port of the Go reference implementation (`go/`). It
//! follows the format (`docs/specification/format.md`) and reproduces
//! `spec/vectors/` exactly. Where the specification is silent the Go code is
//! normative, so the port translates it function by function: each module is
//! one `.go` file, each function keeps its Go name in snake_case, and every
//! claim tag in a comment (`[A29]`, `[D29]`) points at the same code in both.
//! `rust/README.md` has the architecture, the type mapping and the rules the
//! translation keeps.
//!
//! ```
//! use std::time::Duration;
//! use clepsydra::{Config, Filter};
//!
//! # fn main() -> Result<(), clepsydra::Error> {
//! let f = Filter::new(Config {
//!     capacity: 100_000,
//!     horizon: Duration::from_secs(60),
//!     ..Config::default()
//! })?;
//! assert!(!f.check_and_touch("203.0.113.9", Duration::from_secs(10)));
//! assert!(f.check_and_touch("203.0.113.9", Duration::from_secs(10)));
//! let r = f.touch("203.0.113.9", Duration::from_secs(10));
//! assert_eq!((r.seen, r.count), (true, 3));
//! # Ok(())
//! # }
//! ```

use std::fmt;

mod api;
mod batch;
mod buzz;
mod cpu;
mod crc32c;
mod ct;
mod cuckoo;
mod decay;
mod filter;
mod ghost;
mod hooks;
mod hot;
mod merge;
mod overflow;
mod register;
mod resize;
mod siphash;
mod slots;
mod snapshot;
mod stash;
mod touch;

#[cfg(test)]
mod tests;

pub use api::Filter;
pub use filter::{AUTO_PAD, ClockFn, Config, MAX_TICK};
pub use resize::Resizable;

// Every public type can be shared between threads and sent to one (the API
// Guidelines' C-SEND-SYNC): a change that loses either fails the build here,
// not a caller's.
const _: () = {
    const fn send_sync<T: Send + Sync>() {}
    send_sync::<Filter>();
    send_sync::<Resizable>();
    send_sync::<Config>();
    send_sync::<Result>();
    send_sync::<Error>();
};

/// `Result` is the full answer of Touch and Count, including how far it can
/// be trusted. A fail-closed caller (rate limit, abuse check) should treat
/// `saturated` as "over any limit above `count`" and, while `degraded`, a key
/// not `seen` as possibly a returning one. docs/paper/06-security.md has the
/// threat model.
///
/// It keeps the reference's name; the standard library's is written
/// `std::result::Result` in this crate.
#[derive(Clone, Copy, Default, PartialEq, Eq, Hash, Debug)]
pub struct Result {
    /// The key was touched within the window (before this touch).
    pub seen: bool,
    /// Estimates the key's events within the window (including this touch,
    /// for Touch). It interpolates the one bucket the window's edge cuts, so
    /// it can read a little high or low.
    pub count: i64,
    /// Counts that cut bucket whole, so it is >= the true count unless
    /// `saturated`.
    pub upper: i64,
    /// Within the window the key ran at or above the register's capacity (31
    /// events per Horizon/24), so events may have spilled to older buckets or
    /// been dropped. `count` and `upper` are then lower bounds.
    pub saturated: bool,
    /// The key had been evicted; its history came back from the stash.
    pub restored: bool,
    /// The key's history had been pushed out of the stash, and this answer
    /// comes from the overflow table: `seen`, `count` and `upper` cover the
    /// key's real events but may include another key's (a fingerprint match).
    /// (Without an overflow table: the key was not found and the filter lost
    /// some history within the last Horizon, so "not seen" may be a key whose
    /// history was lost.)
    pub degraded: bool,
}

/// An error a constructor or an operation returns: the reference's `error`,
/// with its message word for word.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Error(String);

impl Error {
    pub(crate) fn new(msg: impl Into<String>) -> Error {
        Error(msg.into())
    }

    /// The message, as the reference's `err.Error()`.
    #[must_use]
    pub fn message(&self) -> &str {
        &self.0
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for Error {}
