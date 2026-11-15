//! batch_test.go, hashbatch_test.go.

use super::check::*;
use super::gorand::Rand;
use crate::batch::BATCH_CHUNK;
use crate::slots::*;
use crate::{Config, Filter};

/// At 90% load, where a quarter of the keys live in their alternate line,
/// batches of repeated, present and new keys answer as the same calls made
/// one at a time, in order.
#[test]
fn TestBatchAtHighLoadMatchesSingle() {
    let t = T::new("TestBatchAtHighLoadMatchesSingle");
    let (a, b) = (new_test(1 << 14, MINUTE), new_test(1 << 14, MINUTE));
    let mut pool = vec![0u64; (a.slots() * 90 / 100) as usize];
    for (i, p) in pool.iter_mut().enumerate() {
        *p = mix64(i as u64 + (1 << 44));
        a.touch_at(*p, 0, 1000);
        b.touch_at(*p, 0, 1000);
    }
    let mut rng = Rand::new(3, 4);
    let mut hs = vec![0u64; 4096];
    let (mut seen, mut counts) = (vec![false; hs.len()], vec![0i64; hs.len()]);
    let (mut missed, mut fresh) = (0u64, 1u64 << 50);
    for round in 0..8u64 {
        let now = 2000 + round * 500;
        let mut c = 0;
        while c < hs.len() {
            // 16 keys, 4 times each, a quarter of them new
            for i in 0..16 {
                let mut k = pool[rng.int_n(pool.len() as i64) as usize];
                if rng.int_n(4) == 0 {
                    (k, fresh) = (mix64(fresh), fresh + 1);
                }
                for j in 0..4 {
                    hs[c + i * 4 + j] = k;
                }
            }
            rng.shuffle(64, |i, j| hs.swap(c + i, c + j));
            c += 64;
        }
        for &h in &hs {
            if a.find(a.primary(h), fingerprint(h) << 1, tag(h)) < 0 {
                missed += 1;
            }
        }
        if round % 2 == 0 {
            a.touch_and_count_batch(&hs, 3000, now, &mut seen, &mut counts);
        } else {
            a.check_and_touch_batch(&hs, 3000, now, &mut seen);
        }
        for (i, &h) in hs.iter().enumerate() {
            let (s, c) = b.touch_and_count_at(h, 3000, now);
            if seen[i] != s || (round % 2 == 0 && counts[i] != c) {
                fatalf!(t, "round {round} key {i}: batch {}/{} single {s}/{c}", seen[i], counts[i]);
            }
        }
    }
    logf!(
        t,
        "{:.1}% of keys not in their primary line before their batch; evictions {}, relocations {}",
        100.0 * missed as f64 / (8 * hs.len()) as f64,
        a.evictions(),
        a.relocations()
    );
}

/// warm hands each touch the slot where it found the key, and the chunk's
/// earlier touches can move or replace the entry meanwhile, so a touch first
/// checks that the slot still holds its key. Handed key b's slot (b shares
/// a's fingerprint and lines, not its tag) or an empty one, a touch of a
/// counts in a's own slot and leaves b's alone.
#[test]
fn TestTouchStaleSlot() {
    let t = T::new("TestTouchStaleSlot");
    let f = new_test(1000, MINUTE);
    let a = mix64(7);
    let b = a ^ (1 << 36); // another tag (hash bits 36..47); the same fingerprint and lines
    if fingerprint(a) != fingerprint(b) || tag(a) == tag(b) || f.primary(a) != f.primary(b) {
        fatalf!(t, "b must share a's fingerprint and lines and differ in tag");
    }
    f.touch_at(a, 0, 1000);
    f.touch_at(b, 0, 1000);
    let ib = f.locate(b, fingerprint(b), tag(b));
    let mut empty = -1isize;
    let mut i = 0;
    while i < f.words.len() && empty < 0 {
        if ld(&f.words[i]) == 0 {
            empty = i as isize;
        }
        i += 2;
    }
    let (w0, w1) = (ld(f.w(ib)), ld(f.w(ib + 1)));
    for (n, i0) in [ib, empty].into_iter().enumerate() {
        let r = f.c.touch(a, 60_000, 2000, true, i0);
        if r.count != 2 + n as i64 || !r.seen {
            fatalf!(t, "a handed slot {i0}: {r:?}, want count {}", 2 + n);
        }
    }
    if ld(f.w(ib)) != w0 || ld(f.w(ib + 1)) != w1 {
        fatalf!(t, "a's touch changed b's slot");
    }
    let r = f.count_at(b, 60_000, 2000);
    if r.count != 1 {
        fatalf!(t, "b: {r:?}, want count 1");
    }
}

/// In ConstantTime a batch's warm does the same work whatever the table
/// holds (it runs outside the calls' pads): it hands every touch no slot,
/// present key or not. Outside ConstantTime it hands each present key the
/// slot its primary line holds it in.
#[test]
fn TestWarmConstantTime() {
    let t = T::new("TestWarmConstantTime");
    for ct in [false, true] {
        // The keys go in before the mode is set (the reference sets f.ct on
        // a filter it has built): two filters with the same seedless keys.
        let f = Filter::with(Config { capacity: 1000, horizon: MINUTE, resolution: ms(1), ..Config::default() }, |c| c.ct = ct).unwrap();
        let hs: Vec<u64> = (0..BATCH_CHUNK as u64).map(mix64).collect();
        for (i, &h) in hs.iter().enumerate() {
            if i % 2 == 0 {
                f.touch_at(h, 0, 1000);
            }
        }
        let mut at = [0isize; BATCH_CHUNK];
        f.warm(&hs, &mut at);
        for (i, &h) in hs.iter().enumerate() {
            let mut want = -1;
            if !ct && i % 2 == 0 {
                want = f.find(f.primary(h), fingerprint(h) << 1, tag(h));
            }
            if at[i] != want {
                fatalf!(t, "ConstantTime {ct} key {i}: warm handed slot {}, want {want}", at[i]);
            }
        }
    }
}

/// TestHashUint64sMatchesFilter: Filter.HashUint64s equals HashUint64 with
/// and without Config.Key, on every batch length 0-130.
#[test]
fn TestHashUint64sMatchesFilter() {
    let t = T::new("TestHashUint64sMatchesFilter");
    let mut key = [0u8; 16];
    key[..3].copy_from_slice(&[9, 8, 7]);
    for cfg in
        [Config { capacity: 10, horizon: MINUTE, ..Config::default() }, Config { capacity: 10, horizon: MINUTE, key: Some(key), ..Config::default() }]
    {
        let keyed = cfg.key.is_some();
        let f = match Filter::new(cfg) {
            Ok(f) => f,
            Err(e) => fatalf!(t, "{e}"),
        };
        let (mut keys, mut out) = (vec![0u64; 130], vec![0u64; 130]);
        for n in 0..131usize {
            for (i, k) in keys[..n.min(130)].iter_mut().enumerate() {
                *k = mix64(((n << 8) + i) as u64);
            }
            let n = n.min(130);
            f.hash_uint64s(&keys[..n], &mut out);
            for i in 0..n {
                let want = f.hash_uint64(keys[i]);
                if out[i] != want {
                    fatalf!(t, "keyed {keyed}, batch of {n}, key {i}: {:#x}, want {want:#x}", out[i]);
                }
            }
        }
    }
}
