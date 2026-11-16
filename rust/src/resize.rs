//! resize.go: Resizable.

use std::fmt;
use std::sync::atomic::Ordering::SeqCst;
use std::sync::{Arc, Mutex, MutexGuard, TryLockError};

use crate::cuckoo::CLAIM_FULL;
use crate::filter::{Config, Core};
use crate::hot::is_hot;
use crate::slots::*;
use crate::{Error, Result};

/// `Resizable` is a filter that can change its Capacity without losing state
/// (claims D28): `resize` starts a new filter at the new size, and for one
/// horizon the old one stays beside it. A touch of a key still live in the
/// old table moves its slot to the new one first; reads add the old filter's
/// answer (its stash and overflow table, and keys not touched since the
/// resize); once a horizon has passed nothing in the old filter is live and
/// it is dropped. Meanwhile both tables are held (`bytes` counts both), and a
/// key reads Seen if either filter has it.
///
/// A resize keeps what the old filter holds, not what it had already lost: a
/// filter that evicted before it grew carries those losses (histories shed
/// below SpillMin) until the transition ends, so a grown filter answers as
/// one built at the new size only once the old one is dropped, or from the
/// start if the old one had not evicted. Hot entries must be off
/// (`Config::hot` < 0): a hot key's exact counters don't fit a register.
/// ConstantTime and Background are refused. Calls are serialized by one
/// mutex.
pub struct Resizable {
    pub(crate) mu: Mutex<Resizing>,
}

impl fmt::Debug for Resizable {
    /// Without waiting for the mutex, as `std::sync::Mutex`'s: a call that
    /// holds it (a `Config::clock` that prints the filter) reads `<locked>`.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let z = match self.mu.try_lock() {
            Ok(z) => z,
            Err(TryLockError::Poisoned(e)) => e.into_inner(),
            Err(TryLockError::WouldBlock) => {
                return f.debug_struct("Resizable").field("state", &format_args!("<locked>")).finish_non_exhaustive();
            }
        };
        f.debug_struct("Resizable")
            .field("slots", &z.cur.slots())
            .field("in_transition", &z.old.is_some())
            .field("until", &z.until)
            .field("moved", &z.moved)
            .finish_non_exhaustive()
    }
}

/// What a Resizable's mutex guards.
pub(crate) struct Resizing {
    pub(crate) cfg: Config,
    pub(crate) cur: Arc<Core>,
    pub(crate) old: Option<Arc<Core>>,
    pub(crate) until: u64, // the transition ends after this tick
    pub(crate) moved: i64, // keys moved to the new table
}

impl Resizable {
    /// NewResizable is `Filter::new` for a Resizable.
    ///
    /// # Errors
    ///
    /// As [`Filter::new`](crate::Filter::new), and for a `cfg` with hot
    /// entries (`hot` >= 0), ConstantTime or Background.
    pub fn new(cfg: Config) -> std::result::Result<Resizable, Error> {
        if cfg.hot >= 0 {
            return Err(Error::new("temporalfilter: Resizable needs hot entries off (Config.Hot < 0)"));
        }
        let f = Core::new(cfg.clone(), |_| {})?;
        if f.ct || f.bg {
            return Err(Error::new("temporalfilter: Resizable of a ConstantTime or Background filter"));
        }
        Ok(Resizable { mu: Mutex::new(Resizing { cfg, cur: f, old: None, until: 0, moved: 0 }) })
    }

    pub(crate) fn guard(&self) -> MutexGuard<'_, Resizing> {
        self.mu.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Resize starts a transition to a filter of the given Capacity at now.
    ///
    /// # Errors
    ///
    /// During a transition, and as [`Filter::new`](crate::Filter::new) for
    /// the new Capacity.
    pub fn resize(&self, capacity: i64, now: u64) -> std::result::Result<(), Error> {
        let mut z = self.guard();
        z.settle(now);
        if z.old.is_some() {
            return Err(Error::new("temporalfilter: Resize during a transition"));
        }
        let mut cfg = z.cfg.clone();
        cfg.capacity = capacity;
        let f = Core::new(cfg, |_| {})?;
        let old = std::mem::replace(&mut z.cur, f);
        z.until = now + old.horizon;
        z.old = Some(old);
        Ok(())
    }

    /// TouchAt is `Filter::touch_at` across the transition.
    ///
    /// # Panics
    ///
    /// As [`Filter::touch_at`](crate::Filter::touch_at).
    pub fn touch_at(&self, h: u64, window: u64, now: u64) -> Result {
        let mut z = self.guard();
        z.settle(now);
        if z.old.is_none() {
            return z.cur.touch_at(h, window, now);
        }
        z.do_move(h, now);
        let r = z.cur.touch_at(h, window, now);
        match &z.old {
            Some(old) => sum_result(r, old.count_at(h, window, now)),
            None => r,
        }
    }

    /// CountAt is `Filter::count_at` across the transition.
    #[must_use]
    pub fn count_at(&self, h: u64, window: u64, now: u64) -> Result {
        let mut z = self.guard();
        z.settle(now);
        let mut r = z.cur.count_at(h, window, now);
        if let Some(old) = &z.old {
            r = sum_result(r, old.count_at(h, window, now));
        }
        r
    }

    /// VelocityCountAt is CountAt's Count.
    #[must_use]
    pub fn velocity_count_at(&self, h: u64, window: u64, now: u64) -> i64 {
        self.count_at(h, window, now).count
    }

    /// ContainsAt is `Filter::contains_at` across the transition.
    #[must_use]
    pub fn contains_at(&self, h: u64, window: u64, now: u64) -> bool {
        let mut z = self.guard();
        z.settle(now);
        z.cur.contains_at(h, window, now) || z.old.as_ref().is_some_and(|o| o.contains_at(h, window, now))
    }

    /// Bytes is the memory both filters hold.
    #[must_use]
    pub fn bytes(&self) -> i64 {
        let z = self.guard();
        let mut n = z.cur.bytes();
        if let Some(old) = &z.old {
            n += old.bytes();
        }
        n
    }

    /// Evictions sums both filters'.
    #[must_use]
    pub fn evictions(&self) -> u64 {
        let z = self.guard();
        let mut n = z.cur.evictions.load(SeqCst);
        if let Some(old) = &z.old {
            n += old.evictions.load(SeqCst);
        }
        n
    }
}

impl Resizing {
    pub(crate) fn settle(&mut self, now: u64) {
        if self.old.is_some() && now > self.until {
            self.old = None;
        }
    }

    /// move takes h's slot, live in the old table and absent from the new
    /// one, into the new table (as Merge installs a key), and clears it in
    /// the old. Unlike Merge's install it doesn't recall the new filter's
    /// stash: with calls serialized a key reaches that stash only from the
    /// new table, which it enters by this move (the old slot cleared) or,
    /// with no live copy in the old table, by a first touch, so the stash
    /// never holds a key the old table holds live.
    pub(crate) fn do_move(&mut self, h: u64, now: u64) {
        let (fp, tg) = (fingerprint(h), tag(h));
        let (f, Some(o)) = (&self.cur, self.old.as_ref()) else {
            return; // no transition
        };
        if f.lookup_stable(h, fp, tg, false).0 >= 0 {
            return;
        }
        let (i, w0, w1) = o.lookup_stable(h, fp, tg, false);
        if i < 0 || (w1 >> TAG_SHIFT) == TAG_BUSY || is_hot(w1) || age(now, w0 & TS_MASK) > o.horizon {
            return;
        }
        let mut ok = false;
        {
            let _mu = f.claim_mu[(h >> (64 - CLAIM_STRIPE_BITS)) as usize].lock();
            for _ in 0..8 {
                if f.claim(h, fp, w1, now, w0 & TS_MASK, w0 & SAT_BIT, CLAIM_FULL) >= 0 {
                    ok = true;
                    break;
                }
            }
        }
        if ok {
            st(o.w(i + 1), 0);
            st(o.w(i), 0);
            self.moved += 1;
        }
    }
}

pub(crate) fn sum_result(mut a: Result, b: Result) -> Result {
    a.count += b.count;
    a.upper += b.upper;
    a.seen = a.seen || b.seen;
    a.saturated = a.saturated || b.saturated;
    a.restored = a.restored || b.restored;
    a.degraded = a.degraded || b.degraded;
    a
}
