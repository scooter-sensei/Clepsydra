//! go/internal/registersim/registers_experiment_test.go.
//!
//! Accuracy experiment: three 48-bit per-key velocity encodings, all sharing
//! the exact lastTouch from word0. Time unit is 1ms; horizon H = 60s.

use std::collections::HashMap;

use super::check::*;
use super::gorand::{Rand, go_log};

const EXP_H: f64 = 60_000.0;

trait VelReg {
    fn name(&self) -> String;
    /// record an event at t (non-decreasing)
    fn add(&mut self, t: f64);
    /// estimate events in (now-w, now]
    fn count(&mut self, now: f64, w: f64, last: f64) -> f64;
}

/// gate applies the two exact facts every encoding gets from word0.
fn gate(est: f64, now: f64, w: f64, last: f64) -> f64 {
    if now - last > w {
        return 0.0;
    }
    est.max(1.0)
}

// --- Linear: 6x8-bit shift register, sub-window H/5 (what filter.go shipped) ---

#[derive(Clone)]
struct LinearReg {
    g: f64,
    e: i64,
    c: [f64; 6],
    last: f64,
}

impl LinearReg {
    fn new() -> LinearReg {
        LinearReg { g: EXP_H / 5.0, e: -1 << 40, c: [0.0; 6], last: 0.0 }
    }

    fn advance(&mut self, t: f64) {
        let e = (t / self.g) as i64;
        let d = e - self.e;
        if d <= 0 {
            return;
        }
        if d >= 6 {
            self.c = [0.0; 6];
        } else {
            let d = d as usize;
            self.c.copy_within(0..6 - d, d);
            self.c[..d].fill(0.0);
        }
        self.e = e;
    }
}

impl VelReg for LinearReg {
    fn name(&self) -> String {
        "linear 6x8 (H/5)".into()
    }

    fn add(&mut self, t: f64) {
        self.advance(t);
        self.c[0] = 255.0f64.min(self.c[0] + 1.0);
        self.last = t;
    }

    fn count(&mut self, now: f64, w: f64, last: f64) -> f64 {
        let mut cp = self.clone();
        cp.advance(now);
        let sub = cp.e as f64 * self.g;
        let from = now - w;
        let mut est = cp.c[0];
        if from > sub && last > sub {
            est *= 1.0f64.min((last - from.min(last)) / (last - sub));
        }
        let mut rem = w / self.g - (now - sub) / self.g;
        let mut k = 1;
        while k < 6 && rem > 0.0 {
            est += cp.c[k] * 1.0f64.min(rem);
            rem -= 1.0;
            k += 1;
        }
        gate(est, now, w, last)
    }
}

// --- Dyadic carry-shift: buckets whose widths grow 1,1,1-2,2-4,4-8,... units ---
//
// Boundaries: b0 = u (current unit), bj = floor(u/2^(j-1))*2^(j-1) - 2^(j-1).
// When the unit index becomes u with m trailing zeros, buckets 0..m shift up
// one place and bucket m+1 merges into m+2 (or drops past the top). Recent
// time is kept at unit resolution, older time coarser; n buckets cover
// >= 2^(n-2) units. Counter widths grow with bucket size; they sum to 48.

#[derive(Clone)]
struct DyadicReg {
    unit: f64,
    u: i64,
    c: Vec<f64>,
    max: Vec<f64>,
}

fn bits_trailing(mut u: i64) -> usize {
    let mut m = 0;
    while (u & 1) == 0 && m < 62 {
        u >>= 1;
        m += 1;
    }
    m
}

impl DyadicReg {
    fn new(widths: &[u32]) -> DyadicReg {
        let n = widths.len();
        DyadicReg {
            unit: EXP_H / (1i64 << (n - 2)) as f64,
            u: -1 << 40,
            c: vec![0.0; n],
            max: widths.iter().map(|&w| ((1i64 << w) - 1) as f64).collect(),
        }
    }

    fn advance(&mut self, t: f64) {
        let n = self.c.len();
        let u = (t / self.unit) as i64;
        if u - self.u > (4 << n) {
            self.c.fill(0.0);
            self.u = u;
            return;
        }
        while self.u < u {
            self.u += 1;
            let mut top = bits_trailing(self.u) + 1; // buckets 0..top-1 shift; top merges up
            if top + 1 < n {
                self.c[top + 1] = self.max[top + 1].min(self.c[top + 1] + self.c[top]);
            } else {
                top = n - 1; // shift everything, oldest drops
            }
            for j in (1..=top).rev() {
                self.c[j] = self.max[j].min(self.c[j - 1]);
            }
            self.c[0] = 0.0;
        }
    }

    /// start of bucket j, in ms
    fn bound(&self, j: usize) -> f64 {
        if j == 0 {
            return self.u as f64 * self.unit;
        }
        let s = j - 1;
        (((self.u >> s) << s) - (1i64 << s)) as f64 * self.unit
    }
}

impl VelReg for DyadicReg {
    fn name(&self) -> String {
        format!("dyadic {} buckets (H/{} near now)", self.c.len(), 1 << (self.c.len() - 2))
    }

    fn add(&mut self, t: f64) {
        self.advance(t);
        self.c[0] = self.max[0].min(self.c[0] + 1.0);
    }

    fn count(&mut self, now: f64, w: f64, last: f64) -> f64 {
        let mut cp = self.clone();
        cp.advance(now);
        let from = now - w;
        let mut est = 0.0;
        let mut hi = now;
        for j in 0..cp.c.len() {
            let lo = cp.bound(j);
            let mut top = hi;
            if j == 0 {
                top = lo.max(last.min(now)); // c0's events lie in [lo, last]
            }
            if from <= lo {
                est += cp.c[j];
            } else if from < top {
                est += cp.c[j] * (top - from) / (top - lo);
            }
            hi = lo;
        }
        gate(est, now, w, last)
    }
}

// --- Laplace: k exponential decays (log-encoded), window synthesized per query ---
//
// Stores S_j = sum_i exp(-(last-t_i)/tau_j) as of lastTouch. At query time
// S_j(now) = S_j * exp(-(now-last)/tau_j): decay is pure clock arithmetic
// with no epochs or shifts. The box window 1[age <= W] is approximated by
// sum_j a_j exp(-age/tau_j) with a_j from a least-squares fit over ages in
// [0, 1.5H]; the count estimate is sum_j a_j S_j(now).

struct LaplaceReg {
    tau: Vec<f64>,
    s: Vec<f64>,
    bits: u32, // per-register encoding width (log2(1+S) fixed point)
    last: f64,
    fit: HashMap<u64, Vec<f64>>,
}

/// Go's math.Log2: Frexp, then Log(frac)/Ln2 + exp, exact for powers of two.
/// x is positive and normal.
fn go_log2(x: f64) -> f64 {
    let bits = x.to_bits();
    let exp = ((bits >> 52) & 0x7ff) as i64 - 1022;
    let frac = f64::from_bits((bits & !(0x7ffu64 << 52)) | (1022u64 << 52));
    if frac == 0.5 {
        return (exp - 1) as f64;
    }
    go_log(frac) * (1.0 / std::f64::consts::LN_2) + exp as f64
}

impl LaplaceReg {
    fn new(tau: &[f64], bits: u32) -> LaplaceReg {
        LaplaceReg { tau: tau.to_vec(), s: vec![0.0; tau.len()], bits, last: f64::NEG_INFINITY, fit: HashMap::new() }
    }

    fn quant(&self, s: f64) -> f64 {
        let frac = (1i64 << (self.bits - 5)) as f64; // 5 integer bits of log2: S < 2^32
        ((go_log2(1.0 + s) * frac).round() / frac).exp2() - 1.0
    }

    fn weights(&mut self, w: f64) -> Vec<f64> {
        if let Some(a) = self.fit.get(&w.to_bits()) {
            return a.clone();
        }
        let k = self.tau.len();
        let mut m = vec![vec![0.0f64; k + 1]; k]; // A
        let l = 1.5 * EXP_H;
        for i in 0..k {
            for j in 0..k {
                let s = 1.0 / self.tau[i] + 1.0 / self.tau[j];
                m[i][j] = (1.0 - (-s * l).exp()) / s;
            }
            m[i][k] = self.tau[i] * (1.0 - (-w / self.tau[i]).exp());
        }
        for c in 0..k {
            // Gauss-Jordan, k <= 4
            let mut p = c;
            for i in c + 1..k {
                if m[i][c].abs() > m[p][c].abs() {
                    p = i;
                }
            }
            m.swap(c, p);
            for i in 0..k {
                if i != c {
                    let f = m[i][c] / m[c][c];
                    for j in c..=k {
                        m[i][j] -= f * m[c][j];
                    }
                }
            }
        }
        let a: Vec<f64> = (0..k).map(|i| m[i][k] / m[i][i]).collect();
        self.fit.insert(w.to_bits(), a.clone());
        a
    }
}

impl VelReg for LaplaceReg {
    fn name(&self) -> String {
        format!("laplace {}x{}", self.tau.len(), self.bits)
    }

    fn add(&mut self, t: f64) {
        for j in 0..self.tau.len() {
            self.s[j] = self.quant(self.s[j] * (-(t - self.last) / self.tau[j]).exp() + 1.0);
        }
        self.last = t;
    }

    fn count(&mut self, now: f64, w: f64, last: f64) -> f64 {
        let a = self.weights(w);
        let mut est = 0.0;
        for j in 0..self.tau.len() {
            est += a[j] * self.s[j] * (-(now - self.last) / self.tau[j]).exp();
        }
        gate(est.max(0.0), now, w, last)
    }
}

// --- traffic ---

/// rate per ms
fn poisson(r: &mut Rand, t0: f64, t1: f64, rate: f64) -> Vec<f64> {
    let mut ev = Vec::new();
    let mut t = t0 + r.exp_float64() / rate;
    while t < t1 {
        ev.push(t);
        t += r.exp_float64() / rate;
    }
    ev
}

type Gen = (&'static str, fn(&mut Rand, f64) -> Vec<f64>);

const GENS: [Gen; 5] = [
    ("steady 2/s", |r, end| poisson(r, 0.0, end, 2e-3)),
    ("bursty", |r, end| {
        let mut ev = Vec::new();
        for c in poisson(r, 0.0, end, 1e-4) {
            // bursts every ~10s
            let n = 5 + r.int_n(15);
            for _ in 0..n {
                ev.push(c + r.float64() * 200.0);
            }
        }
        ev.sort_by(f64::total_cmp);
        ev
    }),
    ("ramp 0.1->10/s", |r, end| {
        let mut ev = Vec::new();
        let mut t = 0.0f64;
        while t < end {
            let rate = (0.1 + 9.9 * t / end) * 1e-3;
            t += r.exp_float64() / rate;
            ev.push(t);
        }
        ev
    }),
    ("sparse 1/20s", |r, end| poisson(r, 0.0, end, 5e-5)),
    ("on/off 5s", |r, end| {
        let mut ev = Vec::new();
        let mut t = 0.0f64;
        while t < end {
            ev.extend(poisson(r, t, t + 5000.0, 4e-3));
            t += 10_000.0;
        }
        ev
    }),
];

#[test]
fn TestRegisterExperiment() {
    let t = T::new("TestRegisterExperiment");
    if short() {
        skip!(t, "");
    }
    let wins = [1000.0f64, 5000.0, 15_000.0, 30_000.0, 60_000.0];
    let makers: [fn() -> Box<dyn VelReg>; 5] = [
        || Box::new(LinearReg::new()),
        || Box::new(DyadicReg::new(&[6, 6, 7, 8, 9, 12])),
        || Box::new(DyadicReg::new(&[5, 5, 6, 6, 7, 8, 11])),
        || Box::new(LaplaceReg::new(&[EXP_H / 16.0, EXP_H / 4.0, EXP_H], 16)),
        || Box::new(LaplaceReg::new(&[EXP_H / 32.0, EXP_H / 8.0, EXP_H / 3.0, EXP_H], 12)),
    ];
    let mut out = format!("\nnormalized MAE (sum|est-exact| / sum exact), by window\n{:<34} {:<15}", "register", "traffic");
    for w in wins {
        out += &format!(" {:6.0}s", w / 1000.0);
    }
    out += "\n";
    let mut total: HashMap<String, Vec<f64>> = HashMap::new();
    for mk in makers {
        for (gen_name, g) in GENS {
            let mut errs = [0.0f64; 5];
            let mut sums = [0.0f64; 5];
            let mut name = String::new();
            for trial in 0..40u64 {
                let mut r = Rand::new(trial, 77);
                let ev = g(&mut r, 10.0 * EXP_H);
                let mut reg = mk();
                name = reg.name();
                let qs = poisson(&mut r, 2.0 * EXP_H, 10.0 * EXP_H, 1.0 / 500.0); // query every ~0.5s
                let (mut i, mut last) = (0usize, f64::NEG_INFINITY);
                for &q in &qs {
                    while i < ev.len() && ev[i] <= q {
                        reg.add(ev[i]);
                        last = ev[i];
                        i += 1;
                    }
                    for (wi, &w) in wins.iter().enumerate() {
                        let exact = ev[..i].iter().rev().take_while(|&&x| x > q - w).count();
                        errs[wi] += (reg.count(q, w, last) - exact as f64).abs();
                        sums[wi] += exact as f64;
                    }
                }
            }
            out += &format!("{name:<34} {gen_name:<15}");
            for wi in 0..wins.len() {
                let e = errs[wi] / sums[wi].max(1.0);
                out += &format!(" {e:7.3}");
                total.entry(name.clone()).or_default().push(e);
            }
            out += "\n";
        }
    }
    out += "\nmean over all traffic x windows:\n";
    for mk in makers {
        let n = mk().name();
        let es = &total[&n];
        let s: f64 = es.iter().sum();
        out += &format!("  {n:<34} {:.3}\n", s / es.len() as f64);
    }
    logf!(t, "{out}\n"); // t.Log: Sprintln, a newline after the table's own
}
