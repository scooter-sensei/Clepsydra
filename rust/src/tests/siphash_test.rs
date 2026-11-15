//! go/internal/siphash: siphash_test.go, batch_test.go.

use super::check::*;
use crate::siphash::Key;

#[test]
fn TestSipHashVectors() {
    let t = T::new("TestSipHashVectors");
    let key: [u8; 16] = std::array::from_fn(|i| i as u8);
    let k = Key::new(&key);
    let msg: [u8; 15] = std::array::from_fn(|i| i as u8);
    // Reference outputs from the SipHash paper and its test vectors.
    for (n, want) in [(0usize, 0x726fdb47dd0e0e31u64), (15, 0xa129ca6149be45e5)] {
        let got = k.hash_string(&msg[..n]);
        if got != want {
            errorf!(t, "siphash({n} bytes) = {got:#x}, want {want:#x}");
        }
    }
    for x in [0u64, 1, 0x0706050403020100, !0] {
        if k.hash_uint64(x) != k.hash_string(&x.to_le_bytes()) {
            errorf!(t, "uint64({x:#x}) differs from the byte path");
        }
    }
}

/// TestHashUint64sMatches: HashUint64s equals HashUint64 on every batch
/// length 0-130 and on 1M keys in batches of random lengths, under random
/// keys. (The reference runs it through hash8x, where the CPU has AVX-512,
/// and with it off; the port hashes one key at a time: one pass.)
#[test]
fn TestHashUint64sMatches() {
    let t = T::new("TestHashUint64sMatches");
    logf!(t, "AVX-512F: false (the port's batch hash takes one key at a time)");
    let mut x = 0x13198a2e03707344u64;
    let mut next = || {
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        x
    };
    let (mut keys, mut out) = ([0u64; 130], [0u64; 130]);
    let mut total = 0usize;
    let mut c = 0usize;
    while total < 1_000_000 {
        let mut kb = [0u8; 16];
        kb[..8].copy_from_slice(&next().to_le_bytes());
        kb[8..].copy_from_slice(&next().to_le_bytes());
        let k = Key::new(&kb);
        let mut n = c % 131; // every length 0-130 first, then the same again
        if c > 131 {
            n = (next() % 131) as usize;
        }
        for key in keys[..n].iter_mut() {
            *key = next();
        }
        out.fill(0xdeadbeef);
        k.hash_uint64s(&keys[..n], &mut out);
        for i in 0..n {
            let want = k.hash_uint64(keys[i]);
            if out[i] != want {
                fatalf!(t, "batch of {n}, key {i} ({:#x}): {:#x}, want {want:#x}", keys[i], out[i]);
            }
        }
        for (i, &o) in out.iter().enumerate().skip(n) {
            if o != 0xdeadbeef {
                fatalf!(t, "batch of {n}: out[{i}] written");
            }
        }
        total += n;
        c += 1;
    }
}
