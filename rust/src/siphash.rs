//! The reference's `internal/siphash`: SipHash-2-4 behind `Config::key`.

/// A 128-bit SipHash key.
#[derive(Clone, Copy, Default)]
pub(crate) struct Key {
    k0: u64,
    k1: u64,
}

#[inline(always)]
fn sip_round(v: &mut [u64; 4]) {
    v[0] = v[0].wrapping_add(v[1]);
    v[1] = v[1].rotate_left(13) ^ v[0];
    v[0] = v[0].rotate_left(32);
    v[2] = v[2].wrapping_add(v[3]);
    v[3] = v[3].rotate_left(16) ^ v[2];
    v[0] = v[0].wrapping_add(v[3]);
    v[3] = v[3].rotate_left(21) ^ v[0];
    v[2] = v[2].wrapping_add(v[1]);
    v[1] = v[1].rotate_left(17) ^ v[2];
    v[2] = v[2].rotate_left(32);
}

#[inline(always)]
fn sip_finish(mut v: [u64; 4], last: u64) -> u64 {
    v[3] ^= last;
    sip_round(&mut v);
    sip_round(&mut v);
    v[0] ^= last;
    v[2] ^= 0xff;
    for _ in 0..4 {
        sip_round(&mut v);
    }
    v[0] ^ v[1] ^ v[2] ^ v[3]
}

impl Key {
    /// NewKey: the key's 16 bytes, little-endian.
    pub(crate) fn new(k: &[u8; 16]) -> Key {
        Key { k0: u64::from_le_bytes(k[..8].try_into().unwrap()), k1: u64::from_le_bytes(k[8..].try_into().unwrap()) }
    }

    #[inline(always)]
    fn init(&self) -> [u64; 4] {
        [self.k0 ^ 0x736f6d6570736575, self.k1 ^ 0x646f72616e646f6d, self.k0 ^ 0x6c7967656e657261, self.k1 ^ 0x7465646279746573]
    }

    /// HashString: SipHash-2-4 of the key's bytes.
    pub(crate) fn hash_string(&self, s: &[u8]) -> u64 {
        let mut v = self.init();
        let n = s.len();
        let mut b = s;
        while b.len() >= 8 {
            let m = u64::from_le_bytes(b[..8].try_into().unwrap());
            v[3] ^= m;
            sip_round(&mut v);
            sip_round(&mut v);
            v[0] ^= m;
            b = &b[8..];
        }
        let mut last = (n as u64) << 56;
        for (i, &c) in b.iter().enumerate() {
            last |= u64::from(c) << (8 * i);
        }
        sip_finish(v, last)
    }

    /// HashUint64: SipHash-2-4 of x's 8 little-endian bytes.
    #[inline]
    pub(crate) fn hash_uint64(&self, x: u64) -> u64 {
        let mut v = self.init();
        v[3] ^= x;
        sip_round(&mut v);
        sip_round(&mut v);
        v[0] ^= x;
        sip_finish(v, 8 << 56)
    }

    /// HashUint64s: out[i] = hash_uint64(keys[i]). (The reference hashes 8
    /// keys at once with AVX-512F; one at a time here, the same results.)
    pub(crate) fn hash_uint64s(&self, keys: &[u64], out: &mut [u64]) {
        let out = &mut out[..keys.len()];
        for (o, &k) in out.iter_mut().zip(keys) {
            *o = self.hash_uint64(k);
        }
    }
}
