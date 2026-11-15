//! abandon_test.go.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::Ordering::SeqCst;
use std::sync::atomic::{AtomicBool, AtomicI64};

use super::check::*;
use super::helpers::*;
use crate::Filter;
use crate::slots::*;

/// Few hot keys, eight workers and a fill to 99% slot load, repeated: two
/// touches of one key often overlap a move of it, so moves get abandoned (a
/// touch reached the source first). While copies were visible from their
/// install (until claims A23) the copy had often counted touches of its own,
/// which had to be added back, not dropped with it; now a copy is hidden
/// until the hand-off, and no touch may be lost either way.
///
/// It also catches a rarer race: a touch that checked its slot's fingerprint
/// counting into the next occupant's register, when the slot is handed over
/// before its CAS and the newcomer has the same 12-bit tag (bump counts again
/// after such a hand-over; what is left is A29's residual, a same-tag
/// hand-over just after the count, which reads one high). TF_DISTINCT_TAGS=1
/// keeps cold keys off the hot keys' tags (hot keys may still share one: in
/// about half the rounds); TF_SHARED_TAGS=1 puts every cold key on one (7 of
/// 1,800 runs read one high so, against 3 of ~6,700 with real tags). A
/// failing round logs each hot key whose count is off, with who shares its
/// tag (logOffKeys).
#[test]
fn TestAbandonedMoveKeepsCounts() {
    let t = T::new("TestAbandonedMoveKeepsCounts");
    let iters = if short() { 30 } else { 300 };
    let distinct = env_on("TF_DISTINCT_TAGS");
    let shared = env_on("TF_SHARED_TAGS");
    const HOT: u64 = 64;
    const WORKERS: usize = 8;
    const AT: u64 = 60_000;
    let (mut failed, mut lost) = (0, 0i64);
    for it in 0..iters as u64 {
        let f = new_test(4000, MINUTE);
        let salt = it << 32;
        let mut hot_tags: HashSet<u64> = HashSet::new();
        let mut hot_tag_list: Vec<u64> = Vec::new();
        let mut hot_ids: HashSet<u64> = HashSet::new();
        for k in 0..HOT {
            let h = mix64(salt + k);
            hot_tags.insert(tag(h));
            hot_tag_list.push(tag(h));
            hot_ids.insert((fingerprint(h) << TAG_BITS) | tag(h));
            for _ in 0..3 {
                f.check_and_touch_at(h, 0, AT);
            }
        }
        let stop = AtomicBool::new(false);
        let touches = AtomicI64::new(0);
        let mut fill_tags: HashMap<u64, i64> = HashMap::new();
        let ns: Vec<i64> = std::thread::scope(|s| {
            let workers: Vec<_> = (0..WORKERS)
                .map(|w| {
                    let (f, stop, touches) = (&f, &stop, &touches);
                    s.spawn(move || {
                        let mut n = 0i64;
                        let mut i = (w / 2) as u64;
                        while !stop.load(SeqCst) && n < 150 * HOT as i64 {
                            f.check_and_touch_at(mix64(salt + i % HOT), 0, AT);
                            n += 1;
                            i += 1;
                        }
                        touches.fetch_add(n, SeqCst);
                        n
                    })
                })
                .collect();
            let fill = (f.slots() as f64 * 0.99) as i64 - HOT as i64;
            let (mut k, mut n) = (0u64, 0i64);
            while n < fill {
                let mut h = mix64(salt + (1 << 40) + k);
                let ki = k;
                k += 1;
                if shared {
                    // the tag is h's bits 36-47
                    h = (h & !(((1 << TAG_BITS) - 1) << 36)) | (hot_tag_list[(ki % hot_tag_list.len() as u64) as usize] << 36);
                    if hot_ids.contains(&((fingerprint(h) << TAG_BITS) | tag(h))) {
                        continue;
                    }
                }
                if distinct && hot_tags.contains(&tag(h)) {
                    continue;
                }
                *fill_tags.entry(tag(h)).or_default() += 1;
                f.check_and_touch_at(h, 0, AT);
                n += 1;
            }
            stop.store(true, SeqCst);
            workers.into_iter().map(|h| h.join().unwrap()).collect()
        });
        let mut total = 0i64;
        for k in 0..HOT {
            total += f.velocity_count_at(mix64(salt + k), f.ticks(MINUTE), AT);
        }
        let want = touches.load(SeqCst) + HOT as i64 * 3;
        if total != want {
            failed += 1;
            lost += want - total;
            log_off_keys(&t, &f, it, salt, &ns, &hot_tag_list, &fill_tags, AT);
        }
    }
    logf!(t, "{failed} of {iters} rounds lost counts ({lost} in all)");
    if failed > 0 {
        fatalf!(t, "{failed} of {iters} rounds lost counts");
    }
}

/// logOffKeys logs each hot key of failing round it whose count is off: its
/// true count (3 first touches, and worker w's ns[w] touches of keys w/2,
/// w/2+1, ... in turn), the hot and cold keys on its tag, and where its
/// history is.
#[allow(clippy::too_many_arguments)]
pub(crate) fn log_off_keys(t: &T, f: &Filter, it: u64, salt: u64, ns: &[i64], hot_tags: &[u64], cold_tags: &HashMap<u64, i64>, at: u64) {
    let hot = hot_tags.len() as u64;
    let mut want = vec![3i64; hot as usize];
    for (w, &n) in ns.iter().enumerate() {
        let from = (w / 2) as u64;
        for i in from..from + n as u64 {
            want[(i % hot) as usize] += 1;
        }
    }
    for k in 0..hot {
        let h = mix64(salt + k);
        let got = f.velocity_count_at(h, f.ticks(MINUTE), at);
        if got == want[k as usize] {
            continue;
        }
        let same: Vec<u64> = hot_tags.iter().enumerate().filter(|&(j, &tg)| j as u64 != k && tg == tag(h)).map(|(j, _)| j as u64).collect();
        logf!(
            t,
            "round {it}, hot key {k}: count {got}, true {} ({:+}); tag {}: hot keys {} and {} cold keys share it; {}",
            want[k as usize],
            got - want[k as usize],
            tag(h),
            gov(&same),
            cold_tags.get(&tag(h)).copied().unwrap_or(0),
            where_is(f, h, at)
        );
    }
}
