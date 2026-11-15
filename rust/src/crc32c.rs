//! CRC32C (Castagnoli), the reference's `hash/crc32` with `crc32.Castagnoli`:
//! the snapshot's check words.

const fn make_table() -> [u32; 256] {
    let mut t = [0u32; 256];
    let mut i = 0;
    while i < 256 {
        let mut c = i as u32;
        let mut k = 0;
        while k < 8 {
            c = if c & 1 != 0 { (c >> 1) ^ 0x82F63B78 } else { c >> 1 };
            k += 1;
        }
        t[i] = c;
        i += 1;
    }
    t
}

static TABLE: [u32; 256] = make_table();

/// `crc32.Update(crc, castagnoli, p)`.
pub(crate) fn update(crc: u32, p: &[u8]) -> u32 {
    let mut c = !crc;
    for &b in p {
        c = TABLE[((c ^ b as u32) & 0xff) as usize] ^ (c >> 8);
    }
    !c
}
