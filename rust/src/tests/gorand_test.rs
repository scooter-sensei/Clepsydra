//! The port's own: gorand.rs reproduces Go's math/rand/v2 over PCG.

use super::check::*;
use super::gorand::Rand;

/// TestGoRand: gorand reproduces Go's math/rand/v2 over PCG (values printed
/// by Go 1.24.7), ExpFloat64's ziggurat included: a million draws, whose tail
/// and wedge tests call log and exp (std's here, Go's own there).
#[test]
fn TestGoRand() {
    let t = T::new("TestGoRand");
    let mut r = Rand::new(7, 7);
    let u = [17709787720544228798u64, 14938139214418829421, 3591903268871140211];
    let (a, b) = ([0i64, 2, 5], [303i64, 175, 280]);
    let (c, d) = ([378986463u64, 623969817, 456040026], [235820u64, 520562, 597958]);
    let e = [0.13403964807970836f64, 0.8778376638805372, 0.3623008920975276];
    for i in 0..3 {
        let got = (r.uint64(), r.int_n(12), r.int_n(400), r.uint64_n(1 << 30), r.uint64_n(1000003), r.float64());
        let want = (u[i], a[i], b[i], c[i], d[i], e[i]);
        if got != want {
            errorf!(t, "draw {i}: {got:?}, want {want:?}");
        }
    }
    let mut r2 = Rand::new(5, 0x5eedf022);
    let got = (r2.int_n(9), r2.int_n(1 << 20), r2.uint64(), r2.float64());
    if got != (3, 219989, 9432458690240917045, 0.19443830499656145) {
        errorf!(t, "seed (5, 0x5eedf022): {got:?}");
    }
    let mut r3 = Rand::new(9, 9);
    let exp = [0x3fefe0d725eb4395u64, 0x3ffcbffed104dc2e, 0x40016dc65c8ad564, 0x3ff532ec0935ef4c, 0x3fd0fd40811f80bc, 0x3fcd8a51a34cd8f2];
    for (i, &want) in exp.iter().enumerate() {
        let got = r3.exp_float64().to_bits();
        if got != want {
            errorf!(t, "ExpFloat64 draw {i}: {got:#x}, want {want:#x}");
        }
    }
    let mut x = 0u64;
    for _ in 0..1_000_000 {
        x = x.wrapping_mul(31).wrapping_add(r3.exp_float64().to_bits());
    }
    let next = r3.uint64();
    if x != 0x3427a56a0ecd8798 || next != 0x42cd2d544ea1ad5e {
        errorf!(t, "a million ExpFloat64 draws: mix {x:#x}, then Uint64 {next:#x}; Go: 0x3427a56a0ecd8798, 0x42cd2d544ea1ad5e");
    }
}
