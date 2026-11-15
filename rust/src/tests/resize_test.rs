//! resize_test.go.

use std::cell::RefCell;

use super::check::*;
use super::helpers::*;
use crate::slots::*;
use crate::{Config, Resizable};

/// TestResize (claims D28): a Resizable resized at 120 s of tfbench's
/// velocity workload (2,000 keys, hot entries off) against a filter built at
/// the new size and fed every event, every 5 s from the resize: every key at
/// windows of 1-60 s. Grown (2,560 -> 4,096) and shrunk (8,192 -> 4,096) with
/// no filter evicting, it must give the same Seen, a count error per window
/// within 1.05x the reference's and no unflagged Upper below the truth.
/// Shrunk into a size that evicts (4,096 -> 1,280), the error within 1.05x
/// and no more unflagged lows than the reference's (which sheds histories
/// below SpillMin at that load). Grown from a filter that had evicted (1,280
/// -> 1,792), the old filter's losses stay until the transition ends and none
/// after it (reported, the rest not judged).
#[test]
fn TestResize() {
    let t = T::new("TestResize");
    if force_ct() || force_bg() {
        skip!(t, "Resizable refuses ConstantTime and Background filters");
    }
    const H: u64 = 60_000;
    // name, from, to, evicts (a filter evicts: Seen not compared), lossy
    // (grown from a filter that evicted: reported)
    for (name, from, to, evicts, lossy) in [
        ("grow", 2560i64, 4096i64, false, false),
        ("shrink", 8192, 4096, false, false),
        ("shrink, evicting", 4096, 1280, true, false),
        ("grow from an evicting filter", 1280, 1792, true, true),
    ] {
        let mut cfg = Config { capacity: from, horizon: MINUTE, resolution: ms(1), hot: -1, ..Config::default() };
        let z = match Resizable::new(cfg.clone()) {
            Ok(z) => z,
            Err(e) => fatalf!(t, "{e}"),
        };
        cfg.capacity = to;
        let s = must(cfg);
        let hs: Vec<u64> = (0..2000u64).map(|k| mix64(k + (1 << 50))).collect();
        #[derive(Default)]
        struct St {
            times: Vec<Vec<u64>>,
            resize_at: u64,
            err_m: [f64; 5],
            err_s: [f64; 5],
            ex: [f64; 5],
            seen_diff: i64,
            low_m: i64,
            low_mflag: i64,
            low_s: i64,
            low_in: i64,
            low_after: i64,
            checkpoints: i64,
            in_transition: i64,
            false_m: i64,
            false_s: i64,
            probed: bool,
        }
        let st = RefCell::new(St { times: vec![Vec::new(); hs.len()], ..St::default() });
        vel_workload(
            |k, tk| {
                let mut st = st.borrow_mut();
                if st.resize_at == 0 && tk >= 2 * H {
                    if let Err(e) = z.resize(to, tk) {
                        fatalf!(t, "{e}");
                    }
                    st.resize_at = tk;
                }
                z.touch_at(hs[k], 0, tk);
                s.touch_at(hs[k], 0, tk);
                st.times[k].push(tk);
            },
            |_, _, _, now| {
                let mut st = st.borrow_mut();
                let st = &mut *st;
                if st.resize_at == 0 || now % 5000 != 0 {
                    return;
                }
                st.checkpoints += 1;
                let inside = z.guard().old.is_some();
                if inside {
                    st.in_transition += 1;
                }
                if inside && now + 5000 > st.resize_at + H && !st.probed {
                    // the last checkpoint in transition
                    st.probed = true;
                    for i in 0..100_000u64 {
                        let a = mix64(i + (1 << 52));
                        if z.contains_at(a, H, now) {
                            st.false_m += 1;
                        }
                        if s.contains_at(a, H, now) {
                            st.false_s += 1;
                        }
                    }
                }
                for (k, &h) in hs.iter().enumerate() {
                    let ts = &st.times[k];
                    for (wi, &w) in VEL_WINS.iter().enumerate() {
                        let exact = (ts.len() - ts.partition_point(|&x| x + w < now)) as i64;
                        let (rm, rs) = (z.count_at(h, w, now), s.count_at(h, w, now));
                        if !evicts && rm.seen != rs.seen {
                            st.seen_diff += 1;
                        }
                        st.err_m[wi] += ((rm.count - exact) as f64).abs();
                        st.err_s[wi] += ((rs.count - exact) as f64).abs();
                        st.ex[wi] += exact as f64;
                        if rm.upper < exact {
                            if rm.saturated {
                                st.low_mflag += 1;
                            } else if inside {
                                st.low_m += 1;
                                st.low_in += 1;
                            } else {
                                st.low_m += 1;
                                st.low_after += 1;
                            }
                        }
                        if rs.upper < exact && !rs.saturated {
                            st.low_s += 1;
                        }
                    }
                }
            },
        );
        let st = st.into_inner();
        let mut ratio = [0.0f64; 5];
        for wi in 0..5 {
            ratio[wi] = st.err_m[wi] / st.err_s[wi];
        }
        logf!(
            t,
            "{name} {from} -> {to}: checkpoints {} ({} in transition); evictions resized {}, reference {}; moved {}",
            st.checkpoints,
            st.in_transition,
            z.evictions(),
            s.evictions(),
            z.guard().moved
        );
        logf!(
            t,
            "{name}: count error ratio resized/reference at 1s 5s 15s 30s 60s: {:.4} {:.4} {:.4} {:.4} {:.4}",
            ratio[0],
            ratio[1],
            ratio[2],
            ratio[3],
            ratio[4]
        );
        logf!(
            t,
            "{name}: Seen differs {}; Upper < exact: resized unflagged {} (in transition {}, after {}; flagged {}), reference unflagged {}; false Seen of 100,000 absent keys in transition {}, reference {}",
            st.seen_diff,
            st.low_m,
            st.low_in,
            st.low_after,
            st.low_mflag,
            st.low_s,
            st.false_m,
            st.false_s
        );
        if lossy {
            if st.low_after != 0 {
                errorf!(t, "{name}: {} unflagged lows after the transition", st.low_after);
            }
            continue;
        }
        if st.seen_diff != 0 {
            errorf!(t, "{name}: Seen differs {} times", st.seen_diff);
        }
        for (wi, &r) in ratio.iter().enumerate() {
            if r > 1.05 {
                errorf!(t, "{name}: count error at {} ms {r:.4}x the reference's", VEL_WINS[wi]);
            }
        }
        if st.low_m > st.low_s {
            errorf!(t, "{name}: {} unflagged lows against the reference's {}", st.low_m, st.low_s);
        }
        if !evicts && (z.evictions() != 0 || s.evictions() != 0 || st.low_m != 0) {
            errorf!(t, "{name}: evictions or unflagged lows where none were expected");
        }
    }
}

/// TestResizeRefuses: hot entries, ConstantTime and Background are refused,
/// and so is a resize during a transition.
#[test]
fn TestResizeRefuses() {
    let t = T::new("TestResizeRefuses");
    if Resizable::new(Config { capacity: 1000, horizon: MINUTE, ..Config::default() }).is_ok() {
        errorf!(t, "hot entries on: no error");
    }
    if Resizable::new(Config { capacity: 1000, horizon: MINUTE, hot: -1, constant_time: us(1), ..Config::default() }).is_ok() {
        errorf!(t, "ConstantTime: no error");
    }
    if Resizable::new(Config { capacity: 1000, horizon: MINUTE, hot: -1, background: true, ..Config::default() }).is_ok() {
        errorf!(t, "Background: no error");
    }
    if force_ct() || force_bg() {
        return;
    }
    let z = match Resizable::new(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), hot: -1, ..Config::default() }) {
        Ok(z) => z,
        Err(e) => fatalf!(t, "{e}"),
    };
    if let Err(e) = z.resize(2000, 10) {
        fatalf!(t, "{e}");
    }
    if z.resize(4000, 20).is_ok() {
        errorf!(t, "a resize during a transition: no error");
    }
    let horizon = z.guard().cur.horizon;
    if let Err(e) = z.resize(4000, 10 + horizon + 1) {
        errorf!(t, "a resize after the transition: {e}");
    }
}
