//! The suite: the reference's `*_test.go`, under their Go names, with Go's
//! constants, windows, rounds and skip conditions. `check.rs` is the harness
//! (Go's `testing`), `json.rs` reads the vectors, `gorand.rs` is Go's
//! `math/rand/v2` over PCG bit for bit (so a seeded case replays as in Go).

#![allow(non_snake_case, reason = "the tests keep their Go names")]

pub(crate) mod check;
pub(crate) mod goexp;
pub(crate) mod gorand;
pub(crate) mod gosort;
pub(crate) mod helpers;
pub(crate) mod json;

mod abandon_test;
mod answers_test;
mod attack_test;
mod background_test;
mod batch_test;
mod buzz_test;
#[cfg(target_arch = "x86_64")]
mod cas2_test;
mod conformance_test;
mod ctwait_test;
mod filter_test;
mod ghost_test;
mod gorand_test;
mod hot_test;
mod merge_test;
mod overflowread_test;
mod placement_test;
mod properties_test;
mod race_test;
mod rate_test;
mod register_test;
mod registersim_test;
mod regress_test;
mod resize_test;
mod restore_test;
mod roomrace_test;
mod secexp_test;
mod siphash_test;
mod smoke_test;
mod snapshot_test;
mod split_race_test;
mod stress_test;
mod tagstripe_test;
mod timing_test;
mod touchif_test;
mod unit_test;
mod velbound_test;
mod velinfo_test;
