//! The reference's `internal/cpu`: the 16-byte compare-and-swap of a slot's
//! two words (LOCK CMPXCHG16B, after a CPUID check) and line prefetches, with
//! the reference's fallbacks on other architectures. `math/bits`' `Mul64` and
//! `Div64` are here too, over `u128`.

use std::sync::atomic::{AtomicU8, AtomicU64, Ordering};

/// 0: not probed yet; 1: no 16-byte CAS (or turned off); 2: CMPXCHG16B.
static HAS_CAS2: AtomicU8 = AtomicU8::new(0);

/// HasCAS2: the CPU has CMPXCHG16B (x86-64 since 2006, but the baseline
/// target doesn't promise it). Without it realign, stamp, unpoint and demotion
/// hold word1, as on other architectures. The suite turns it off under
/// `TF_NOCAS2` (tests/check.rs, as the reference's cas2_amd64_test.go does):
/// the library itself reads no environment.
#[inline(always)]
pub(crate) fn has_cas2() -> bool {
    match HAS_CAS2.load(Ordering::Relaxed) {
        2 => true,
        1 => false,
        _ => probe(),
    }
}

#[cold]
fn probe() -> bool {
    let has = cpu_has_cx16();
    // From unprobed only: never over a value the suite set meanwhile.
    match HAS_CAS2.compare_exchange(0, if has { 2 } else { 1 }, Ordering::Relaxed, Ordering::Relaxed) {
        Ok(_) => has,
        Err(now) => now == 2,
    }
}

/// Tests: turn the 16-byte CAS off (or back on, where the CPU has it).
#[cfg(test)]
pub(crate) fn set_has_cas2(on: bool) {
    HAS_CAS2.store(if on && cpu_has_cx16() { 2 } else { 1 }, Ordering::Relaxed);
}

#[cfg(all(target_arch = "x86_64", not(miri)))]
fn cpu_has_cx16() -> bool {
    std::arch::is_x86_feature_detected!("cmpxchg16b")
}

#[cfg(not(all(target_arch = "x86_64", not(miri))))]
fn cpu_has_cx16() -> bool {
    false
}

/// CAS2 swaps the two words at `p` (a slot: 16-byte aligned) from
/// `old0, old1` to `new0, new1` atomically, and reports whether it did. The
/// caller has checked `has_cas2`.
#[cfg(all(target_arch = "x86_64", not(miri)))]
#[inline(always)]
pub(crate) fn cas2(p: &[AtomicU64], old0: u64, old1: u64, new0: u64, new1: u64) -> bool {
    assert!(p.len() >= 2 && (p.as_ptr() as usize) % 16 == 0);
    let old = ((old1 as u128) << 64) | old0 as u128;
    let new = ((new1 as u128) << 64) | new0 as u128;
    // SAFETY: p is two adjacent, live AtomicU64 at a 16-byte aligned address
    // (checked above), which CMPXCHG16B reads and writes as one little-endian
    // 128-bit word: word0 is the low half. has_cas2 found the instruction by
    // CPUID before any call. Every other access to the two words is an atomic
    // 64-bit one: mixed-size atomics are outside Rust's memory model and
    // inside every x86-64 CPU's (as the reference's assembly, and the C++
    // port's, rely on).
    unsafe { core::arch::x86_64::cmpxchg16b(p.as_ptr() as *mut u128, old, new, Ordering::SeqCst, Ordering::SeqCst) == old }
}

#[cfg(not(all(target_arch = "x86_64", not(miri))))]
pub(crate) fn cas2(_p: &[AtomicU64], _old0: u64, _old1: u64, _new0: u64, _new1: u64) -> bool {
    unreachable!("cas2 without CMPXCHG16B")
}

/// Prefetch the cache line of `w` (PREFETCHT0; PRFM PLDL1KEEP on arm64; a
/// load elsewhere, as the reference's fallback).
#[inline(always)]
pub(crate) fn prefetch(w: &AtomicU64) {
    #[cfg(all(target_arch = "x86_64", not(miri)))]
    {
        use core::arch::x86_64::{_MM_HINT_T0, _mm_prefetch};
        // SAFETY: a prefetch reads nothing and faults on nothing; the address
        // is a live reference's.
        #[allow(unused_unsafe)]
        unsafe {
            _mm_prefetch::<_MM_HINT_T0>(w as *const AtomicU64 as *const i8)
        };
    }
    #[cfg(all(target_arch = "aarch64", not(miri)))]
    {
        // SAFETY: PRFM is a hint: it reads nothing and faults on nothing.
        unsafe {
            core::arch::asm!("prfm pldl1keep, [{p}]", p = in(reg) w as *const AtomicU64, options(nostack, preserves_flags, readonly));
        }
    }
    #[cfg(not(any(all(target_arch = "x86_64", not(miri)), all(target_arch = "aarch64", not(miri)))))]
    {
        let _ = w.load(Ordering::SeqCst);
    }
}

/// PrefetchLines prefetches line `l` (64 bytes: 8 words) of `words` for every
/// `l` in `lines`.
#[inline]
pub(crate) fn prefetch_lines(words: &[AtomicU64], lines: &[u64]) {
    for &l in lines {
        prefetch(&words[(l * 8) as usize]);
    }
}

/// `bits.Mul64`'s high word: floor(a * b / 2^64).
#[inline(always)]
pub(crate) fn mul_hi(a: u64, b: u64) -> u64 {
    ((a as u128 * b as u128) >> 64) as u64
}

/// `bits.Mul64`: (hi, lo) of a * b.
#[inline(always)]
pub(crate) fn mul64(a: u64, b: u64) -> (u64, u64) {
    let p = a as u128 * b as u128;
    ((p >> 64) as u64, p as u64)
}

/// `bits.Div64`'s quotient of (hi, lo) / y; panics, as Go, if it doesn't fit
/// (y <= hi).
#[inline(always)]
pub(crate) fn div64(hi: u64, lo: u64, y: u64) -> u64 {
    assert!(y > hi, "integer overflow");
    ((((hi as u128) << 64) | lo as u128) / y as u128) as u64
}
