//! attack_test.go.
//!
//! Attacks on the filter (docs/paper/06-security.md), each measured against
//! the defence: forged identities and lock-stripe floods need the hash to be
//! unkeyed; eviction floods need the stash to be off or overrun; heavy
//! hitters need the count to read low without saying so. SipHash's reference
//! vectors are in siphash_test.rs.

use std::collections::HashSet;
use std::time::Duration;

use super::check::*;
use super::gorand::Rand;
use super::helpers::*;
use crate::slots::*;
use crate::{Config, Filter, Result};

/// unmix64 inverts mix64: the unkeyed hash is a bijection anyone can run
/// backwards.
pub(crate) fn unmix64(mut y: u64) -> u64 {
    fn inv(c: u64) -> u64 {
        let mut x = c;
        for _ in 0..6 {
            x = x.wrapping_mul(2u64.wrapping_sub(c.wrapping_mul(x)));
        }
        x
    }
    y ^= (y >> 31) ^ (y >> 62);
    y = y.wrapping_mul(inv(0x94d049bb133111eb));
    y ^= (y >> 27) ^ (y >> 54);
    y = y.wrapping_mul(inv(0xbf58476d1ce4e5b9));
    y ^ (y >> 30) ^ (y >> 60)
}

type Opt = fn(&mut Config);

#[track_caller]
pub(crate) fn attack_filter(capacity: i64, stash: i64, opts: &[Opt]) -> (Filter, TestClock) {
    let now = TestClock::new(1);
    let mut cfg = Config { capacity, horizon: MINUTE, resolution: ms(1), clock: now.func(), stash, ..Config::default() };
    for o in opts {
        o(&mut cfg);
    }
    (must(cfg), now)
}

/// TestAttackForgedIdentity: with the old unkeyed integer hash an attacker
/// computes ids whose hashes differ from a victim's only in bit 0..3. They
/// land on the victim's line with its identity, so the attacker's events
/// count as the victim's (framing it) and the victim reads as seen. Keyed,
/// the same ids land elsewhere.
#[test]
fn TestAttackForgedIdentity() {
    let t = T::new("TestAttackForgedIdentity");
    const VICTIM: u64 = 4242;
    const PER_FORGERY: i64 = 25;
    let run = |hash: &dyn Fn(u64) -> u64| -> (i64, bool) {
        let (f, now) = attack_filter(1 << 14, 0, &[]);
        let hv = hash(VICTIM);
        for b in 0..4 {
            let forged = unmix64(mix64(VICTIM) ^ (1 << b)); // chosen against the unkeyed hash
            for _ in 0..PER_FORGERY {
                f.touch_at(hash(forged), 0, now.add(1));
            }
        }
        let r = f.count_at(hv, f.ticks(MINUTE), now.get());
        (r.count, r.seen)
    };
    let (before, seen_before) = run(&mix64);
    let (f, _) = attack_filter(1, 0, &[]);
    let (after, seen_after) = run(&|k| f.hash_uint64(k));
    logf!(
        t,
        "victim never touched; attacker sent 4 forged ids x {PER_FORGERY}: unkeyed reads {before} (seen {seen_before}), keyed reads {after} (seen {seen_after})"
    );
    if before != 4 * PER_FORGERY || !seen_before {
        errorf!(t, "the forgery should work on the unkeyed hash: read {before}");
    }
    if after != 0 || seen_after {
        errorf!(t, "keyed hash: victim reads {after}, seen {seen_after}; want 0, false");
    }
}

/// TestAttackStripeFlood: unkeyed, an attacker picks ids whose hashes share
/// the top 8 bits, so every first touch queues on one of the 256 claim locks.
/// Keyed, the same ids spread over the stripes.
#[test]
fn TestAttackStripeFlood() {
    let t = T::new("TestAttackStripeFlood");
    let (f, _) = attack_filter(1, 0, &[]);
    let (mut unkeyed, mut keyed): (HashSet<u64>, HashSet<u64>) = (HashSet::new(), HashSet::new());
    for i in 0..4096u64 {
        let id = unmix64((0xAB << 56) | (i.wrapping_mul(0x9e3779b97f4a7c15) >> 8));
        unkeyed.insert(mix64(id) >> (64 - CLAIM_STRIPE_BITS));
        keyed.insert(f.hash_uint64(id) >> (64 - CLAIM_STRIPE_BITS));
    }
    logf!(t, "4096 crafted ids hit {} stripe(s) unkeyed, {} keyed", unkeyed.len(), keyed.len());
    if unkeyed.len() != 1 || keyed.len() < 240 {
        errorf!(t, "stripes: unkeyed {} (want 1), keyed {} (want ~256)", unkeyed.len(), keyed.len());
    }
}

/// evictionSetup fills a table to its design load with one-touch keys and
/// adds an offender with k events.
fn eviction_setup(stash: i64, k: i64, opts: &[Opt]) -> (Filter, TestClock, Rand, u64) {
    let (f, now) = attack_filter(4096, stash, opts);
    let mut rng = Rand::new(7, (stash + 1) as u64);
    for _ in 0..f.slots() * 85 / 100 {
        f.touch_at(rng.uint64(), 0, now.add(1));
    }
    let off = rng.uint64();
    for _ in 0..k {
        f.touch_at(off, 0, now.add(1));
    }
    (f, now, rng, off)
}

/// sprayUntilEvicted touches each key from next k+1 times (so it outranks the
/// offender for eviction) until the offender is out of the table, then
/// returns the attacker's event count.
fn spray_until_evicted(f: &Filter, now: &TestClock, off: u64, k: i64, mut next: impl FnMut() -> u64) -> i64 {
    let (fp, tg) = (fingerprint(off), tag(off));
    let mut events = 0i64;
    while f.locate(off, fp, tg) >= 0 && events < (1 << 24) {
        let h = next();
        for _ in 0..k + 1 {
            f.touch_at(h, 0, now.get());
            events += 1;
        }
        now.add(1);
    }
    events
}

/// noSketch turns the overflow sketch off.
fn no_sketch(c: &mut Config) {
    c.overflow = -1;
}

/// TestAttackEvictionReset: an offender with k events gets evicted so that it
/// comes back as a first-time key. With the unkeyed hash the attacker builds
/// keys on the offender's two lines (same primary line and fingerprint, other
/// tags): a few hundred events. Keyed, the spray lands at random and needs
/// about a table's worth of keys, each outranking the offender. The stash
/// then brings the offender back with its history. A flood that also overruns
/// the stash with heavier keys pushes the history into the overflow sketch:
/// Upper still covers it, and Count does when the history stands out from the
/// flood's noise (then Degraded). Without the sketch the history is lost, and
/// every new key reads Degraded.
#[test]
fn TestAttackEvictionReset() {
    let t = T::new("TestAttackEvictionReset");
    if force_ct() || force_bg() {
        skip!(
            t,
            "ConstantTime evicts only when a waiting key's stash set is full; Background evicts in its placer thread, so the spray can't tell when"
        );
    }
    const K: i64 = 20;
    let window = 60_000u64;
    let report = |name: &str, f: &Filter, events: i64, r: &Result| {
        logf!(
            t,
            "{name:<30} attacker events {events:7}; offender returns: count {:2} upper {:3} (true {}) seen {:<5} restored {:<5} degraded {:<5}; evictions {} folded {} lost {}",
            r.count,
            r.upper,
            K + 1,
            r.seen,
            r.restored,
            r.degraded,
            f.evictions(),
            f.folded(),
            f.lost()
        );
    };
    let (mut targeted, mut sprayed) = (0i64, 0i64);
    for stash in [-1i64, 0] {
        let (f, now, _, off) = eviction_setup(stash, K, &[no_sketch]);
        let mut tag_n = 0u64;
        let events = spray_until_evicted(&f, &now, off, K, || {
            // forge: flip tag bits only
            tag_n += 1;
            off ^ (tag_n << 36)
        });
        let r = f.touch_at(off, window, now.add(1));
        report(&format!("unkeyed targeted, stash {}", stash >= 0), &f, events, &r);
        targeted = events;
        if stash < 0 && (r.count != 1 || !r.degraded) {
            errorf!(t, "no stash: expected a reset reported as Degraded, got {}", show(&r));
        }
        if stash >= 0 && (!r.restored || r.count != K + 1) {
            errorf!(t, "stash: offender not restored after a targeted eviction: {}", show(&r));
        }
    }
    let modes: [(&str, i64, &[Opt]); 3] =
        [("keyed spray, no stash/sketch", -1, &[no_sketch]), ("keyed spray, stash only", 0, &[no_sketch]), ("keyed spray, stash + sketch", 0, &[])];
    for (name, stash, opts) in modes {
        let (f, now, mut rng, off) = eviction_setup(stash, K, opts);
        let events = spray_until_evicted(&f, &now, off, K, || rng.uint64());
        let r = f.touch_at(off, window, now.add(1));
        report(name, &f, events, &r);
        sprayed = sprayed.max(events);
        if !r.restored && !r.degraded && r.upper < K + 1 {
            errorf!(t, "{name}: history lost silently: {}", show(&r));
        }
        if opts.is_empty() && (f.lost() != 0 || r.upper < K + 1) {
            errorf!(t, "{name}: lost {}, Upper {} < {}", f.lost(), r.upper, K + 1);
        }
    }
    logf!(
        t,
        "keyed hashing raises the cost of evicting a {K}-event key from {targeted} to {sprayed} attacker events ({:.0}x; table {} slots)",
        sprayed as f64 / targeted as f64,
        4096 * 100 / 85
    );
}

/// TestAttackFloodCollateral: offenders with k events each, then a flood of
/// keys sent k+1 times each (heavier than the offenders, so they evict them)
/// at 1x to 16x the table's slots, or a cheap flood of one-event keys, all
/// within 30 s. Afterwards: how the offenders read (Upper never below the
/// truth; Count's shortfall), and how keys never sent read (Degraded, Seen,
/// Upper). Without the sketch every new key reads Degraded once any history
/// is lost.
#[test]
fn TestAttackFloodCollateral() {
    let t = T::new("TestAttackFloodCollateral");
    const K: i64 = 20;
    const OFFENDERS: usize = 200;
    const PROBES: i64 = 20_000;
    let window = 60_000u64;
    let floods: [(i64, i64); 7] = [(1, K + 1), (2, K + 1), (3, K + 1), (4, K + 1), (8, K + 1), (16, K + 1), (16, 1)];
    for sk in [false, true] {
        for (mult, weight) in floods {
            let opts: &[Opt] = if sk { &[] } else { &[no_sketch] };
            let (f, now) = attack_filter(4096, 0, opts);
            let mut rng = Rand::new(11, (mult * 100 + weight) as u64);
            for _ in 0..f.slots() * 85 / 100 {
                f.touch_at(rng.uint64(), 0, now.add(1));
            }
            let mut offs = vec![0u64; OFFENDERS];
            for o in offs.iter_mut() {
                *o = rng.uint64();
                for _ in 0..K {
                    f.touch_at(*o, 0, now.get());
                }
                now.add(1);
            }
            let per = 1.max(f.slots() * mult / 30_000);
            for i in 0..f.slots() * mult {
                let h = rng.uint64();
                for _ in 0..weight {
                    f.touch_at(h, 0, now.get());
                }
                if i % per == 0 {
                    now.add(1); // the flood spans at most 30 s of the 60 s horizon
                }
            }
            let at = now.add(1);
            let (mut low, mut below, mut shortfall) = (0i64, 0i64, 0i64);
            for &h in &offs {
                let r = f.count_at(h, window, at);
                if r.upper < K {
                    below += 1;
                }
                if r.count < K {
                    low += 1;
                    shortfall += K - r.count;
                }
            }
            let (mut deg, mut seen, mut upper, mut strict) = (0i64, 0i64, 0i64, 0i64);
            for _ in 0..PROBES {
                let r = f.count_at(rng.uint64(), window, at);
                if r.degraded {
                    deg += 1;
                }
                if r.degraded || r.upper > K {
                    strict += 1; // a strict limit of k turns it away
                }
                if r.seen {
                    seen += 1;
                }
                upper += r.upper;
            }
            let probes = PROBES as f64;
            logf!(
                t,
                "sketch {sk:<5} flood {mult:2}x slots x {weight:2} ({:7} events): offenders Upper<true {below}/{OFFENDERS}, Count low {low:3}/{OFFENDERS} (mean shortfall {:4.1}); new keys Degraded {:5.1}% Seen {:5.2}% mean Upper {:6.1}, denied by a strict limit of {K}: {:5.1}%; folded {} shed {} lost {}",
                f.slots() * mult * weight,
                shortfall as f64 / low.max(1) as f64,
                100.0 * deg as f64 / probes,
                100.0 * seen as f64 / probes,
                upper as f64 / probes,
                100.0 * strict as f64 / probes,
                f.folded(),
                f.shed(),
                f.lost()
            );
            if sk && (below > 0 || f.lost() > 0) {
                errorf!(t, "sketch: {below} offenders read Upper below the truth, {} lost", f.lost());
            }
            if sk && deg * 100 > PROBES {
                errorf!(t, "sketch: {deg} of {PROBES} new keys read Degraded");
            }
        }
    }
}

/// TestAttackHeavyHitter: a key sending faster than the register holds reads
/// low (docs/paper/03-design.md, count limits). That must never happen
/// silently: whenever Saturated is false, Upper must be >= the exact count.
/// Steady and bursty senders from 1 to 500 events/s, queried for 1-60 s
/// windows at random times, against an exact log of their events.
#[test]
fn TestAttackHeavyHitter() {
    let t = T::new("TestAttackHeavyHitter");
    #[derive(Default)]
    struct Stats {
        queries: i64,
        sat: i64,
        upper_low: i64,
        count_low: i64,
    }
    let rates = [1.0f64, 5.0, 12.0, 13.0, 20.0, 31.0, 50.0, 100.0, 500.0];
    let mut all = Stats::default();
    for burst in [false, true] {
        for rate in rates {
            let mut s = Stats::default();
            for key in 0..8u64 {
                let (f, clock) = attack_filter(1024, 0, &[]);
                let mut now = 1u64;
                let mut rng = Rand::new((rate * 1000.0) as u64, key);
                let h = rng.uint64();
                let mut log: Vec<u64> = Vec::new();
                let end = 180_000 + rng.uint64_n(60_000);
                while now < end {
                    let mut gap = rng.exp_float64() * 1000.0 / rate;
                    if burst && rng.int_n(20) == 0 {
                        gap *= 20.0; // quiet spells, then catch-up bursts
                    }
                    now += 1.max(gap as u64);
                    clock.set(now);
                    let mut n = 1;
                    if burst {
                        n = 1 + rng.int_n(4);
                    }
                    for _ in 0..n {
                        f.touch_at(h, 0, now);
                        log.push(now);
                    }
                    if rng.int_n(25) == 0 {
                        let w = (1000 * (1 + rng.int_n(60))) as u64;
                        let exact = log.iter().filter(|&&ts| now - ts <= w).count() as i64;
                        let r = f.count_at(h, w, now);
                        s.queries += 1;
                        if r.saturated {
                            s.sat += 1;
                            continue;
                        }
                        if (r.count as f64) < 0.9 * exact as f64 - 1.0 {
                            s.count_low += 1;
                        }
                        if r.upper < exact {
                            s.upper_low += 1;
                            if s.upper_low <= 3 {
                                logf!(
                                    t,
                                    "rate {} burst {burst}: window {}s exact {exact}, Count {}, Upper {}, not Saturated",
                                    go_g(rate),
                                    w / 1000,
                                    r.count,
                                    r.upper
                                );
                            }
                        }
                    }
                }
            }
            logf!(
                t,
                "burst {burst:<5} {rate:4.0} ev/s: {:5} queries, {:5.1}% Saturated; unflagged: Upper < exact {}, Count >10% low {}",
                s.queries,
                100.0 * s.sat as f64 / s.queries.max(1) as f64,
                s.upper_low,
                s.count_low
            );
            all.queries += s.queries;
            all.sat += s.sat;
            all.upper_low += s.upper_low;
            all.count_low += s.count_low;
        }
    }
    t.print(&format!(
        "heavy hitter: {} queries, {} Saturated, {} unflagged with Upper < exact, {} unflagged with Count >10% low",
        all.queries, all.sat, all.upper_low, all.count_low
    ));
    if all.upper_low > 0 {
        errorf!(t, "{} unflagged queries had Upper below the exact count", all.upper_low);
    }
}

/// TestUpperBoundRandom is TestAttackHeavyHitter over random senders: each
/// key switches between random rates (0.2-60 events/s), silences and bursts
/// of up to 200 events in one tick, with horizons of 10 s and 60 s. It checks
/// the same contract (unflagged Upper >= exact) and reports how loose Upper
/// is.
#[test]
fn TestUpperBoundRandom() {
    let t = T::new("TestUpperBoundRandom");
    let keys = if short() { 300 } else { 3000 };
    let (mut queries, mut sat, mut low) = (0i64, 0i64, 0i64);
    let (mut slack, mut exact_sum) = (0.0f64, 0.0f64);
    for key in 0..keys as u64 {
        let mut rng = Rand::new(99, key);
        let hz = [Duration::from_secs(10), MINUTE][(key % 2) as usize];
        let clock = TestClock::new(1);
        let mut now = 1u64;
        let f = match Filter::new(Config { capacity: 64, horizon: hz, resolution: ms(1), clock: clock.func(), ..Config::default() }) {
            Ok(f) => f,
            Err(e) => fatalf!(t, "{e}"),
        };
        let h_ms = hz.as_millis() as u64;
        let h = rng.uint64();
        let mut log: Vec<u64> = Vec::new();
        let mut rate = 0.0f64;
        while now < 6 * h_ms {
            if rng.int_n(200) == 0 || rate == 0.0 {
                rate = [0.2, 1.0, 3.0, 8.0, 12.0, 20.0, 40.0, 60.0][rng.int_n(8) as usize];
            }
            now += 1.max((rng.exp_float64() * 1000.0 / rate) as u64);
            if rng.int_n(500) == 0 {
                now += rng.int_n(h_ms as i64) as u64; // silence
            }
            clock.set(now);
            let mut n = 1;
            if rng.int_n(100) == 0 {
                n = 1 + rng.int_n(200); // burst
            }
            for _ in 0..n {
                f.touch_at(h, 0, now);
                log.push(now);
            }
            if rng.int_n(10) == 0 {
                let w = 1 + rng.uint64_n(h_ms);
                let exact = log.iter().filter(|&&ts| now - ts <= w).count() as i64;
                let r = f.count_at(h, w, now);
                queries += 1;
                if r.saturated {
                    sat += 1;
                    continue;
                }
                if r.upper < exact {
                    low += 1;
                    if low <= 5 {
                        logf!(
                            t,
                            "key {key} horizon {} window {w}ms: exact {exact} Count {} Upper {} unflagged",
                            go_duration(hz.as_nanos() as i64),
                            r.count,
                            r.upper
                        );
                    }
                }
                slack += (r.upper - exact) as f64;
                exact_sum += exact as f64;
            }
        }
    }
    t.print(&format!(
        "upper bound: {queries} queries, {sat} Saturated, {low} unflagged with Upper < exact; unflagged Upper exceeds exact by {:.1}% of events",
        100.0 * slack / exact_sum.max(1.0)
    ));
    if low > 0 {
        errorf!(t, "{low} unflagged queries had Upper below the exact count");
    }
}
