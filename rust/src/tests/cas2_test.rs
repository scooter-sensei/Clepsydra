//! cas2_amd64_test.go (realignModes is in helpers.rs; the suite's TF_NOCAS2
//! switch in check.rs).

use std::sync::atomic::Ordering::SeqCst;

use super::check::*;
use super::helpers::*;
use crate::cpu;
use crate::hot::is_hot;
use crate::register::REG_MASK;
use crate::slots::*;

#[test]
fn TestCAS2() {
    let t = T::new("TestCAS2");
    if !cpu::has_cas2() {
        skip!(t, "no CMPXCHG16B, or TF_NOCAS2");
    }
    let f = new_minute_filter(1000);
    let p = &f.words[2..4]; // a slot: 16-byte aligned
    st(&f.words[2], 10);
    st(&f.words[3], 20);
    for (old0, old1, want_ok) in [(11u64, 20u64, false), (10, 21, false), (10, 20, true)] {
        let (w0, w1) = (ld(&f.words[2]), ld(&f.words[3]));
        let ok = cpu::cas2(p, old0, old1, 30, 40);
        if ok != want_ok {
            fatalf!(t, "cas2 from {old0}, {old1} over {w0}, {w1}: {ok}");
        }
        let want = if want_ok { [30, 40] } else { [w0, w1] };
        if [ld(&f.words[2]), ld(&f.words[3])] != want || ld(&f.words[1]) != 0 || ld(&f.words[4]) != 0 {
            let words: Vec<u64> = f.words[..5].iter().map(ld).collect();
            fatalf!(t, "after cas2 from {old0}, {old1}: words {}", gov(&words));
        }
    }
}

/// TestRealignCAS2 (claims A33): K's register is full and its next touch is
/// 10 units on, so realign's advance spills and K is promoted. The entry is
/// published with both words by one CAS, with no move counted; if word0 moved
/// since realign read it, the slot is left as it was and the entry closed
/// again.
#[test]
fn TestRealignCAS2() {
    let t = T::new("TestRealignCAS2");
    if !cpu::has_cas2() {
        skip!(t, "no CMPXCHG16B, or TF_NOCAS2");
    }
    for moved in [false, true] {
        let f = new_minute_filter(1000);
        const TG: u64 = 0x321;
        let k = key_at(&f, 7, two_line_fps(&f, 7, 0x6600, 1)[0], TG);
        f.touch_at(k, 0, SWEEP_T0);
        let sl = slots_of(&f, k);
        if sl.len() != 1 {
            fatalf!(t, "setup: K in {}", gov(&sl));
        }
        let i = sl[0];
        let full = (TG << TAG_SHIFT) | (((f.unit_of(SWEEP_T0) as u64) & ONSET_MASK) << ONSET_SHIFT) | REG_MASK; // every bucket full
        st(f.w(i + 1), full);
        let w0 = ld(f.w(i));
        if moved {
            // a touch of K at the next tick, after realign's read
            st(f.w(i), w0 + 1);
        }
        let m = f.move_of(TG);
        let begun = m.begun.load(SeqCst);
        let (w1, over, ok) = f.realign2(i, w0, full, TG, SWEEP_T0 + 10 * f.unit);
        let (n0, n1) = (ld(f.w(i)), ld(f.w(i + 1)));
        if m.begun.load(SeqCst) != begun {
            fatalf!(t, "moved {moved}: realign2 counted a move");
        } else if moved && (ok || n0 != w0 + 1 || n1 != full || f.hot_in_use() != 0 || f.promoted() != 0) {
            fatalf!(t, "moved: ok {ok}, words {n0:#x} {n1:#x}, hot in use {}, promoted {}", f.hot_in_use(), f.promoted());
        } else if !moved
            && (!ok
                || over
                || !is_hot(w1)
                || n1 != w1
                || n0 != ((w0 & !TS_MASK) | (SWEEP_T0 + 10 * f.unit))
                || f.hot_in_use() != 1
                || f.promoted() != 1)
        {
            fatalf!(t, "ok {ok}, over {over}, w1 {w1:#x}, words {n0:#x} {n1:#x}, hot in use {}, promoted {}", f.hot_in_use(), f.promoted());
        }
    }
}
