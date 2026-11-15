//! merge_test.go, merge_stash_test.go.

use std::cell::{Cell, RefCell};

use super::check::*;
use super::gorand::Rand;
use super::helpers::*;
use crate::slots::*;
use crate::{Config, Filter};

/// TestMerge splits tfbench's velocity workload at random between filters A
/// and B, against S fed everything; every 5 s a fresh M = Merge(A) + Merge(B)
/// must answer as S does: the same Seen everywhere (no filter evicts here), a
/// count error per window at most 1.5x S's, and no Upper below the truth
/// unless flagged Saturated (hot entries are off, so some buckets overflow,
/// in M and S alike).
#[test]
fn TestMerge() {
    let t = T::new("TestMerge");
    if force_ct() || force_bg() {
        skip!(t, "Merge refuses ConstantTime and Background filters (TestMergeRefuses)");
    }
    let mk = || must(Config { capacity: 4096, horizon: MINUTE, resolution: ms(1), hot: -1, ..Config::default() });
    let (a, b, s) = (mk(), mk(), mk());
    let key = |k: usize| mix64(k as u64 + 1);
    let rng = RefCell::new(Rand::new(13, 13));
    let times: RefCell<Vec<Vec<u64>>> = RefCell::new(vec![Vec::new(); 2000]);
    let wins = [1_000u64, 5_000, 15_000, 30_000, 60_000];
    let (mut err_m, mut err_s, mut sum) = ([0.0f64; 5], [0.0f64; 5], [0.0f64; 5]);
    let (mut seen_diff, mut cmp, mut low_unflagged, mut low_flagged) = (0, 0, 0, 0);
    let checkpoints = Cell::new(0);
    let m: RefCell<Option<Filter>> = RefCell::new(None);
    vel_workload(
        |k, at| {
            times.borrow_mut()[k].push(at);
            s.check_and_touch_at(key(k), 0, at);
            if rng.borrow_mut().int_n(2) == 0 {
                a.check_and_touch_at(key(k), 0, at);
            } else {
                b.check_and_touch_at(key(k), 0, at);
            }
        },
        |_, _, _, now| {
            if now % 5000 != 0 || now < 65_000 {
                return;
            }
            checkpoints.set(checkpoints.get() + 1);
            let nm = mk();
            if let Err(e) = nm.merge(&a) {
                fatalf!(t, "{e}");
            }
            if let Err(e) = nm.merge(&b) {
                fatalf!(t, "{e}");
            }
            let times = times.borrow();
            for k in 0..times.len() {
                for (wi, &w) in wins.iter().enumerate() {
                    let mut exact = 0i64;
                    for &ts in times[k].iter().rev() {
                        if ts <= now - w {
                            break;
                        }
                        exact += 1;
                    }
                    let (rm, rs) = (nm.count_at(key(k), w, now), s.count_at(key(k), w, now));
                    cmp += 1;
                    if rm.seen != rs.seen {
                        seen_diff += 1;
                    }
                    err_m[wi] += (rm.count - exact).abs() as f64;
                    err_s[wi] += (rs.count - exact).abs() as f64;
                    sum[wi] += exact as f64;
                    if rm.upper < exact {
                        if rm.saturated {
                            low_flagged += 1;
                        } else {
                            low_unflagged += 1;
                        }
                    }
                }
            }
            *m.borrow_mut() = Some(nm);
        },
    );
    let checkpoints = checkpoints.get();
    if checkpoints == 0 {
        fatalf!(t, "no checkpoint");
    }
    let m = m.into_inner().unwrap();
    logf!(t, "checkpoints {checkpoints}, comparisons {cmp}; Upper < exact flagged Saturated {low_flagged}");
    let ev = a.evictions() + b.evictions() + s.evictions() + m.evictions();
    if ev != 0 {
        fatalf!(t, "{ev} evictions: the comparison needs none");
    }
    if seen_diff != 0 {
        errorf!(t, "Seen differs from the single filter's in {seen_diff} of {cmp} answers");
    }
    if low_unflagged != 0 {
        errorf!(t, "Upper below the truth, unflagged, in {low_unflagged} answers");
    }
    for (wi, &w) in wins.iter().enumerate() {
        logf!(t, "window {:2}s  MAE merged {:.4}  single {:.4}", w / 1000, err_m[wi] / sum[wi], err_s[wi] / sum[wi]);
        if err_m[wi] > 1.5 * err_s[wi] {
            errorf!(t, "window {}s: merged count error {:.0} over 1.5x the single filter's {:.0}", w / 1000, err_m[wi], err_s[wi]);
        }
    }
    const END: u64 = 5 * 60_000;
    let (mut fm, mut fs) = (0, 0);
    for i in 0..1_000_000u64 {
        let h = mix64(i + (1 << 40));
        if m.contains_at(h, 60_000, END) {
            fm += 1;
        }
        if s.contains_at(h, 60_000, END) {
            fs += 1;
        }
    }
    logf!(t, "absent probes 1e6: false Seen merged {fm}, single {fs}");
}

/// TestMergeRefuses: Merge returns an error and leaves f as it was for f
/// itself, another Config, ConstantTime, and hot entries in use.
#[test]
fn TestMergeRefuses() {
    let t = T::new("TestMergeRefuses");
    let base = || Config { capacity: 4096, horizon: MINUTE, ..Config::default() };
    let f = must(base());
    f.check_and_touch_at(mix64(1), 0, 1000);
    let hot = must(base());
    for i in 0..100u64 {
        hot.check_and_touch_at(mix64(2), 0, 1000 + i);
    }
    if hot.promoted() == 0 {
        fatalf!(t, "no key was promoted to a hot entry");
    }
    let words = |f: &Filter| -> Vec<u64> { f.words.iter().map(ld).collect() };
    let before = words(&f);
    let others = [
        ("other Capacity", must(Config { capacity: 8192, horizon: MINUTE, ..Config::default() })),
        ("other Horizon", must(Config { capacity: 4096, horizon: secs(120), ..Config::default() })),
        ("ConstantTime", must(Config { capacity: 4096, horizon: MINUTE, constant_time: us(1), ..Config::default() })),
        ("hot entries", hot),
    ];
    if f.merge(&f).is_ok() {
        errorf!(t, "Merge with itself: no error");
    }
    for (name, g) in &others {
        if f.merge(g).is_ok() {
            errorf!(t, "Merge with {name}: no error");
        }
    }
    if before != words(&f) {
        errorf!(t, "a refused Merge changed f");
    }
}

/// TestMergeKeepsOwnStash: a key evicted to f's stash and live in g's table;
/// after f.Merge(g), f must still count the stashed history (Upper at least
/// the truth), and still after the key's next eviction. D24's Merge failed it
/// (it installed the key from g without recalling f's stash; found by the
/// StashSet model, verify/tla/StashSet.tla; claims D26).
#[test]
fn TestMergeKeepsOwnStash() {
    let t = T::new("TestMergeKeepsOwnStash");
    if force_ct() || force_bg() {
        skip!(t, "Merge refuses ConstantTime and Background filters (TestMergeRefuses)");
    }
    let cfg = || Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), ..Config::default() };
    let (f, g) = (must(cfg()), must(cfg()));
    let k = mix64(42);
    for i in 0..5u64 {
        f.touch_at(k, 0, 100 + i);
    }
    for i in 0..3u64 {
        g.touch_at(k, 0, 110 + i);
    }
    let evict = |now: u64| {
        // as claimAt's eviction does
        let (i, w0, w1) = f.lookup_stable(k, fingerprint(k), tag(k), false);
        if i < 0 {
            fatalf!(t, "not found");
        }
        st(f.w(i + 1), 0);
        st(f.w(i), 0);
        f.stash_put(f.slot_id((i as u64) / WORDS_PER_LINE, w0, w1), w0, w1, now);
    };
    evict(120);
    if let Err(e) = f.merge(&g) {
        fatalf!(t, "{e}");
    }
    let r = f.count_at(k, 60_000, 130);
    if r.upper < 8 && !r.saturated {
        errorf!(t, "after Merge: {r:?}, truth 8 (5 in f's stash, 3 in g)");
    }
    f.touch_at(k, 0, 131);
    evict(132);
    let r = f.count_at(k, 60_000, 133);
    if r.upper < 9 && !r.saturated {
        errorf!(t, "after the next eviction: {r:?}, truth 9");
    }
}
