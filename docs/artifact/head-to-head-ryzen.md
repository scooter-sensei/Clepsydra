# Head-to-head readings, Ryzen host

The output of `go/verify/h2h.sh` (part 1 of [the head-to-head](head-to-head.md)) on the Ryzen host, as `h2h report` printed it, with the rule-3 table added by the report of a later commit from the same results. Every pass's JSON line is in [head-to-head-ryzen.jsonl](head-to-head-ryzen.jsonl); `h2h report` re-derives the speed tables from it, and the accuracy tables from the answer files (12 B per probe, 110 MB in all, not kept here: a rerun regenerates them, within the stated intervals) and the traces (`h2h gen -n 262144` writes the same traces: their hashes are below).

- Harness: commit 073f9e0 (`go/cmd/h2h`, `bench/h2h`, `go/internal/ecm`); library: dfca83c.
- Host: AMD Ryzen 5 4600H (6 cores, 12 threads) under WSL2, Linux 6.18.54.1, 7.6 GB RAM; Go 1.24.7; rustc and cargo 1.99.1, `pingora-limits` 0.9.0, `ahash` 0.8.12, `dashmap` 6.2.1 (`bench/h2h/Cargo.lock`), linked with `zig cc`.
- Load: 0.45 (1-minute average) before the run; no other work of this study ran during it, and the host's other sessions were idle when checked before it. 5 rounds, 450 passes, then 60 sizing passes; no void pass (`pingora-limits` finished every pass inside its 60 s interval). 56 minutes.
- Sizing: Clepsydra peak bytes 8,139,136 after the 16x heavy flood, so `pingora-limits` ran with 2 estimators x 4 rows x 127,174 slots (8,139,136 B); the sizing arms `pingora-x8` and `pingora-x64` with 8x and 64x those slots.
- ECM-sketch supplement ([head-to-head.md](head-to-head.md#supplement-the-ecm-sketch-under-the-floods); harness c405d23 with `verify/h2h.sh`'s ECM stage): a run of its own, 23 minutes, load 0.45 before it, 150 passes after the sizing below. Depth by false denies on the 1x heavy flood: d=1 18,955, d=2 34,711, d=3 39,051, d=4 39,876, so d=1, ε_sw 0.5; widths, each the widest whose peak fits 8,139,136 B on its own flood: heavy 1x 480,378, 2x 235,623, 4x 174,971, 8x 142,866, 16x 125,695, light 16x 318,167. Its speed is compared with part 1's medians, not round by round (the rule-3 table has no ECM column).

| Trace | SHA-256 (first 8 bytes) | Bytes | Records | Ops |
|---|---|---|---|---|
| heavy1x.trace | 8195f9264061cc3b | 10120992 | 571554 | 6758714 |
| heavy2x.trace | a3ba3edf237abc47 | 15055520 | 879962 | 13235282 |
| heavy4x.trace | ead75495b8174f34 | 24924576 | 1496778 | 26188418 |
| heavy8x.trace | ae311a592de821dd | 44662688 | 2730410 | 52094690 |
| heavy16x.trace | 178eded3389430e5 | 84138912 | 5197674 | 103907234 |
| light16x.trace | aa661c72036370e2 | 84138912 | 5197674 | 5216674 |

Key count N = 262144 (Clepsydra: 308408 slots); limit 20 per 60000 ms; probes per round: 1000 offenders, 20000 flood, 20000 base, 20000 new.

Validity (rule 1), all rounds: probes whose upper bound reads below the truth, keys with >= 4 events | fewer | probes whose estimate differs from the truth
  clepsydra  0 | 477756 | 808903 of 1830000
  pingora    0 | 0 | 1815274 of 1830000
  dashmap    0 | 0 | 0 of 1830000
  ecm        0 | 0 | 1764091 of 1830000
  pingora-x64 0 | 0 | 32652 of 1830000
  pingora-x8 0 | 0 | 977268 of 1830000

| flood | arm | bytes/key | Mops/s, 1 thread | Mops/s, 12 threads | p99.9 ns, 1 thread | p99.9 ns, 12 threads | count error | false-allow | false-deny |
|---|---|---|---|---|---|---|---|---|---|
| heavy1x | clepsydra | 31.0 (31.0–31.0) | 3.06 (2.90–3.19) | 19.93 (18.25–21.13) | 8634 (7694–11627) | 19663 (18466–44456) | 0.292 | 0.00% [0.00, 0.00] | 0.00% [0.00, 0.01] |
| heavy1x | pingora | 31.1 (31.1–31.1) | 9.84 (9.65–9.99) | 84.60 (68.53–94.39) | 844 (513–940) | 555 (523–694) | 22.206 | 0.00% [0.00, 0.00] | 69.41% [69.21, 69.61] |
| heavy1x | dashmap | 331.1 (331.1–331.1) | 13.27 (12.59–13.95) | 49.70 (38.45–53.90) | 2052 (1987–2094) | 12824 (10601–19834) | 0.000 | 0.00% [0.00, 0.00] | 0.00% [0.00, 0.00] |
| heavy1x | ecm | 31.1 (31.0–31.1) | 9.17 (9.05–9.33) | 6.74 (6.68–6.78) | 4958 (4916–5001) | 459611 (454138–476026) | 11.125 | 0.00% [0.00, 0.00] | 47.47% [47.25, 47.69] |
| heavy1x | pingora-x64 | 1987.1 (1987.1–1987.1) | – | – | – | – | 0.000 | 0.00% [0.00, 0.00] | 0.00% [0.00, 0.00] |
| heavy1x | pingora-x8 | 248.4 (248.4–248.4) | – | – | – | – | 0.124 | 0.00% [0.00, 0.00] | 0.44% [0.41, 0.47] |
| heavy2x | clepsydra | 31.0 (31.0–31.0) | 2.43 (1.72–2.46) | 17.26 (6.61–17.64) | 7609 (6497–10686) | 24280 (19150–28383) | 0.343 | 0.00% [0.00, 0.00] | 0.07% [0.06, 0.09] |
| heavy2x | pingora | 31.1 (31.1–31.1) | 10.02 (8.47–10.14) | 73.28 (42.66–96.49) | 545 (513–833) | 582 (545–673) | 58.905 | 0.00% [0.00, 0.00] | 96.99% [96.91, 97.06] |
| heavy2x | dashmap | 481.7 (481.7–481.7) | 14.52 (12.70–14.86) | 54.25 (17.73–55.50) | 2073 (2052–2222) | 14704 (12653–20518) | 0.000 | 0.00% [0.00, 0.00] | 0.00% [0.00, 0.00] |
| heavy2x | ecm | 31.0 (31.0–31.0) | 9.86 (9.66–9.98) | 7.03 (6.88–7.04) | 2778 (2778–2821) | 459611 (454138–492439) | 43.691 | 0.00% [0.00, 0.00] | 92.78% [92.67, 92.89] |
| heavy2x | pingora-x64 | 1987.1 (1987.1–1987.1) | – | – | – | – | 0.000 | 0.00% [0.00, 0.00] | 0.00% [0.00, 0.01] |
| heavy2x | pingora-x8 | 248.4 (248.4–248.4) | – | – | – | – | 0.982 | 0.00% [0.00, 0.00] | 4.32% [4.23, 4.41] |
| heavy4x | clepsydra | 31.0 (31.0–31.0) | 2.06 (2.04–2.08) | 15.04 (11.16–15.44) | 5813 (5642–8036) | 19492 (16756–71130) | 0.376 | 0.00% [0.00, 0.00] | 0.27% [0.25, 0.29] |
| heavy4x | pingora | 31.1 (31.1–31.1) | 10.15 (9.95–10.18) | 88.79 (61.59–94.12) | 523 (513–534) | 523 (502–545) | 140.868 | 0.00% [0.00, 0.00] | 99.98% [99.98, 99.99] |
| heavy4x | dashmap | 946.9 (946.9–946.9) | 13.80 (13.06–13.97) | 53.66 (48.38–56.36) | 2052 (2030–2052) | 12482 (10601–12995) | 0.000 | 0.00% [0.00, 0.00] | 0.00% [0.00, 0.00] |
| heavy4x | ecm | 31.0 (31.0–31.0) | 10.35 (9.92–10.37) | 7.23 (7.09–7.24) | 1325 (1271–1432) | 476025 (426781–596398) | 120.656 | 0.00% [0.00, 0.00] | 99.91% [99.89, 99.92] |
| heavy4x | pingora-x64 | 1987.1 (1987.1–1987.1) | – | – | – | – | 0.008 | 0.00% [0.00, 0.00] | 0.04% [0.03, 0.04] |
| heavy4x | pingora-x8 | 248.4 (248.4–248.4) | – | – | – | – | 5.571 | 0.00% [0.00, 0.00] | 24.41% [24.22, 24.60] |
| heavy8x | clepsydra | 31.0 (31.0–31.0) | 1.84 (1.83–1.84) | 13.08 (12.34–13.37) | 5813 (5728–5813) | 21544 (18124–23938) | 3.913 | 0.00% [0.00, 0.00] | 98.99% [98.94, 99.03] |
| heavy8x | pingora | 31.1 (31.1–31.1) | 10.11 (10.06–10.24) | 88.26 (83.26–95.06) | 523 (513–523) | 513 (486–523) | 316.751 | 0.00% [0.00, 0.00] | 100.00% [100.00, 100.00] |
| heavy8x | dashmap | 1877.2 (1877.2–1877.2) | 12.87 (12.69–13.03) | 43.83 (43.66–45.16) | 2094 (2073–2116) | 19321 (17440–20005) | 0.000 | 0.00% [0.00, 0.00] | 0.00% [0.00, 0.00] |
| heavy8x | ecm | 31.0 (31.0–31.1) | 9.83 (8.90–10.01) | 6.99 (6.53–7.18) | 1389 (1250–1881) | 470552 (437724–514325) | 299.803 | 0.00% [0.00, 0.00] | 100.00% [100.00, 100.00] |
| heavy8x | pingora-x64 | 1987.1 (1987.1–1987.1) | – | – | – | – | 0.103 | 0.00% [0.00, 0.00] | 0.47% [0.45, 0.51] |
| heavy8x | pingora-x8 | 248.4 (248.4–248.4) | – | – | – | – | 20.486 | 0.00% [0.00, 0.00] | 69.12% [68.91, 69.32] |
| heavy16x | clepsydra | 31.0 (31.0–31.0) | 1.73 (1.73–1.74) | 12.10 (11.93–12.23) | 5813 (5728–5899) | 22570 (21202–23254) | 5.745 | 0.00% [0.00, 0.00] | 99.89% [99.88, 99.91] |
| heavy16x | pingora | 31.1 (31.1–31.1) | 10.16 (10.12–10.22) | 86.73 (65.89–91.11) | 513 (513–523) | 513 (486–523) | 684.211 | 0.00% [0.00, 0.00] | 100.00% [100.00, 100.00] |
| heavy16x | dashmap | 3738.0 (3738.0–3738.0) | 12.29 (11.76–12.48) | 42.63 (40.31–42.85) | 2137 (2094–2201) | 23596 (22570–24622) | 0.000 | 0.00% [0.00, 0.00] | 0.00% [0.00, 0.00] |
| heavy16x | ecm | 31.0 (31.0–31.0) | 9.35 (9.11–9.41) | 6.81 (6.65–6.85) | 1282 (1250–2052) | 519797 (470552–525269) | 688.724 | 0.00% [0.00, 0.00] | 100.00% [100.00, 100.00] |
| heavy16x | pingora-x64 | 1987.1 (1987.1–1987.1) | – | – | – | – | 0.906 | 0.00% [0.00, 0.00] | 4.25% [4.17, 4.34] |
| heavy16x | pingora-x8 | 248.4 (248.4–248.4) | – | – | – | – | 57.138 | 0.00% [0.00, 0.00] | 96.91% [96.84, 96.99] |
| light16x | clepsydra | 20.6 (20.6–20.6) | 0.49 (0.49–0.50) | 2.28 (2.24–2.36) | 23596 (21544–25306) | 229805 (205183–243484) | 0.617 | 0.00% [0.00, 0.08] | 0.00% [0.00, 0.00] |
| light16x | pingora | 31.1 (31.1–31.1) | 6.28 (5.75–6.56) | 45.66 (39.52–51.02) | 545 (534–555) | 716 (684–737) | 34.417 | 0.00% [0.00, 0.08] | 99.98% [99.97, 99.98] |
| light16x | dashmap | 1629.7 (1629.7–1629.7) | 2.01 (1.94–2.04) | 4.68 (4.43–4.89) | 7780 (7694–9233) | 281784 (279048–289991) | 0.000 | 0.00% [0.00, 0.08] | 0.00% [0.00, 0.00] |
| light16x | ecm | 31.0 (31.0–31.1) | 1.99 (1.79–2.03) | 1.71 (1.65–1.74) | 8720 (8549–9489) | 1181854 (1159971–1291284) | 12.857 | 0.00% [0.00, 0.08] | 26.18% [26.02, 26.34] |
| light16x | pingora-x64 | 1987.1 (1987.1–1987.1) | – | – | – | – | 0.050 | 0.00% [0.00, 0.08] | 0.00% [0.00, 0.00] |
| light16x | pingora-x8 | 248.4 (248.4–248.4) | – | – | – | – | 2.909 | 0.00% [0.00, 0.08] | 0.00% [0.00, 0.00] |

Per probe class, all rounds pooled: mean |estimate - truth| (mean signed error) and the share denied.

| flood | arm | offenders (20) | flood keys | base keys (1) | never sent (0) |
|---|---|---|---|---|---|
| heavy1x | clepsydra | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 100.00% | 0.89 (-0.89), denied 0.00% | 0.00 (+0.00), denied 0.01% |
| heavy1x | pingora | 22.19 (+22.19), denied 100.00% | 22.14 (+22.14), denied 100.00% | 22.20 (+22.20), denied 69.34% | 22.28 (+22.28), denied 69.48% |
| heavy1x | dashmap | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 0.00% | 0.00 (+0.00), denied 0.00% |
| heavy1x | ecm | 11.26 (+7.64), denied 100.00% | 10.65 (+7.01), denied 100.00% | 11.32 (+11.32), denied 47.44% | 11.40 (+11.40), denied 47.49% |
| heavy1x | pingora-x64 | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 0.00% | 0.00 (+0.00), denied 0.00% |
| heavy1x | pingora-x8 | 0.15 (+0.15), denied 100.00% | 0.12 (+0.12), denied 100.00% | 0.13 (+0.13), denied 0.46% | 0.12 (+0.12), denied 0.42% |
| heavy2x | clepsydra | 0.02 (+0.02), denied 100.00% | 0.01 (+0.01), denied 100.00% | 1.01 (-0.98), denied 0.07% | 0.02 (+0.02), denied 0.08% |
| heavy2x | pingora | 58.68 (+58.68), denied 100.00% | 58.84 (+58.84), denied 100.00% | 58.82 (+58.82), denied 96.97% | 59.07 (+59.07), denied 97.01% |
| heavy2x | dashmap | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 0.00% | 0.00 (+0.00), denied 0.00% |
| heavy2x | ecm | 41.91 (+41.48), denied 100.00% | 41.41 (+40.99), denied 100.00% | 44.86 (+44.86), denied 92.86% | 44.89 (+44.89), denied 92.70% |
| heavy2x | pingora-x64 | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 0.00% | 0.00 (+0.00), denied 0.00% |
| heavy2x | pingora-x8 | 0.97 (+0.97), denied 100.00% | 0.97 (+0.97), denied 100.00% | 0.99 (+0.99), denied 4.34% | 0.98 (+0.98), denied 4.29% |
| heavy4x | clepsydra | 0.07 (+0.06), denied 100.00% | 0.04 (+0.04), denied 100.00% | 1.05 (-0.95), denied 0.26% | 0.05 (+0.05), denied 0.28% |
| heavy4x | pingora | 141.66 (+141.66), denied 100.00% | 140.79 (+140.79), denied 100.00% | 141.05 (+141.05), denied 99.98% | 140.73 (+140.73), denied 99.98% |
| heavy4x | dashmap | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 0.00% | 0.00 (+0.00), denied 0.00% |
| heavy4x | ecm | 119.91 (+119.91), denied 100.00% | 118.79 (+118.79), denied 100.00% | 121.31 (+121.31), denied 99.92% | 121.90 (+121.90), denied 99.90% |
| heavy4x | pingora-x64 | 0.01 (+0.01), denied 100.00% | 0.01 (+0.01), denied 100.00% | 0.01 (+0.01), denied 0.04% | 0.01 (+0.01), denied 0.03% |
| heavy4x | pingora-x8 | 5.86 (+5.86), denied 100.00% | 5.56 (+5.56), denied 100.00% | 5.52 (+5.52), denied 24.18% | 5.62 (+5.62), denied 24.64% |
| heavy8x | clepsydra | 19.92 (-19.81), denied 100.00% | 9.83 (-9.72), denied 100.00% | 1.05 (-0.94), denied 98.96% | 0.06 (+0.06), denied 99.02% |
| heavy8x | pingora | 317.67 (+317.67), denied 100.00% | 316.82 (+316.82), denied 100.00% | 316.54 (+316.54), denied 100.00% | 316.85 (+316.85), denied 100.00% |
| heavy8x | dashmap | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 0.00% | 0.00 (+0.00), denied 0.00% |
| heavy8x | ecm | 297.78 (+297.78), denied 100.00% | 297.77 (+297.77), denied 100.00% | 300.48 (+300.48), denied 100.00% | 301.26 (+301.26), denied 100.00% |
| heavy8x | pingora-x64 | 0.10 (+0.10), denied 100.00% | 0.11 (+0.11), denied 100.00% | 0.10 (+0.10), denied 0.46% | 0.10 (+0.10), denied 0.49% |
| heavy8x | pingora-x8 | 20.60 (+20.60), denied 100.00% | 20.45 (+20.45), denied 100.00% | 20.50 (+20.50), denied 69.08% | 20.50 (+20.50), denied 69.15% |
| heavy16x | clepsydra | 19.95 (-19.90), denied 100.00% | 15.41 (-15.30), denied 100.00% | 1.05 (-0.94), denied 99.88% | 0.06 (+0.06), denied 99.90% |
| heavy16x | pingora | 684.84 (+684.84), denied 100.00% | 684.39 (+684.39), denied 100.00% | 684.30 (+684.30), denied 100.00% | 683.91 (+683.91), denied 100.00% |
| heavy16x | dashmap | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 0.00% | 0.00 (+0.00), denied 0.00% |
| heavy16x | ecm | 686.14 (+686.14), denied 100.00% | 685.85 (+685.85), denied 100.00% | 690.13 (+690.13), denied 100.00% | 690.32 (+690.32), denied 100.00% |
| heavy16x | pingora-x64 | 0.92 (+0.92), denied 100.00% | 0.91 (+0.91), denied 100.00% | 0.91 (+0.91), denied 4.30% | 0.89 (+0.89), denied 4.21% |
| heavy16x | pingora-x8 | 56.55 (+56.55), denied 100.00% | 57.17 (+57.17), denied 100.00% | 57.15 (+57.15), denied 96.96% | 57.12 (+57.12), denied 96.86% |
| light16x | clepsydra | 0.00 (+0.00), denied 100.00% | 0.94 (-0.94), denied 0.00% | 0.94 (-0.94), denied 0.00% | 0.00 (+0.00), denied 0.00% |
| light16x | pingora | 34.51 (+34.51), denied 100.00% | 34.42 (+34.42), denied 99.98% | 34.43 (+34.43), denied 99.98% | 34.40 (+34.40), denied 99.96% |
| light16x | dashmap | 0.00 (+0.00), denied 100.00% | 0.00 (+0.00), denied 0.00% | 0.00 (+0.00), denied 0.00% | 0.00 (+0.00), denied 0.00% |
| light16x | ecm | 8.64 (+8.64), denied 100.00% | 12.87 (+12.87), denied 28.67% | 12.87 (+12.87), denied 28.62% | 13.04 (+13.04), denied 21.24% |
| light16x | pingora-x64 | 0.05 (+0.05), denied 100.00% | 0.05 (+0.05), denied 0.00% | 0.05 (+0.05), denied 0.00% | 0.05 (+0.05), denied 0.00% |
| light16x | pingora-x8 | 2.94 (+2.94), denied 100.00% | 2.91 (+2.91), denied 0.00% | 2.91 (+2.91), denied 0.00% | 2.90 (+2.90), denied 0.00% |

Rule 3, round by round: rounds in which Clepsydra is faster (more ops/s, or a lower p99.9) than each competitor, of the rounds both ran.

| flood | Mops/s, 1 thread | Mops/s, 12 threads | p99.9, 1 thread | p99.9, 12 threads |
|---|---|---|---|---|
| heavy1x | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 |
| heavy2x | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 |
| heavy4x | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 |
| heavy8x | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 |
| heavy16x | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 5/5 |
| light16x | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 0/5 | pingora 0/5, dashmap 5/5 |

Latency detail, medians over rounds (ns): p50 / p99 / p99.9 / max; the timer's own p99.9 beside.

| flood | arm | 1 thread | 12 threads | timer p99.9 |
|---|---|---|---|---|
| heavy1x | clepsydra | 181 / 3077 / 8634 / 4858736 | 251 / 4403 / 19663 / 8054116 | 30 (30–30) |
| heavy1x | pingora | 121 / 411 / 844 / 3808187 | 131 / 323 / 555 / 1225624 | 30 (30–30) |
| heavy1x | dashmap | 70 / 502 / 2052 / 623755 | 80 / 812 / 12824 / 1553915 | 30 (30–40) |
| heavy1x | ecm | 60 / 758 / 4958 / 361122 | 91 / 37959 / 459611 / 12081211 | 30 (30–30) |
| heavy1x | pingora-x64 | – | – | – |
| heavy1x | pingora-x8 | – | – | – |
| heavy2x | clepsydra | 352 / 3377 / 7609 / 4858739 | 513 / 5086 / 24280 / 7091135 | 30 (30–40) |
| heavy2x | pingora | 121 / 243 / 545 / 787900 | 131 / 331 / 582 / 8404274 | 30 (30–40) |
| heavy2x | dashmap | 70 / 491 / 2073 / 766014 | 80 / 833 / 14704 / 1794662 | 30 (30–30) |
| heavy2x | ecm | 70 / 844 / 2778 / 410366 | 91 / 34881 / 459611 / 8842035 | 30 (30–30) |
| heavy2x | pingora-x64 | – | – | – |
| heavy2x | pingora-x8 | – | – | – |
| heavy4x | clepsydra | 384 / 3462 / 5813 / 5384006 | 566 / 5257 / 19492 / 8404290 | 30 (30–30) |
| heavy4x | pingora | 121 / 232 / 523 / 2888969 | 131 / 302 / 523 / 3895731 | 30 (30–30) |
| heavy4x | dashmap | 70 / 491 / 2052 / 2013524 | 80 / 812 / 12482 / 2560677 | 30 (30–30) |
| heavy4x | ecm | 70 / 865 / 1325 / 601869 | 91 / 29751 / 476025 / 10592909 | 30 (30–30) |
| heavy4x | pingora-x64 | – | – | – |
| heavy4x | pingora-x8 | – | – | – |
| heavy8x | clepsydra | 454 / 3590 / 5813 / 5208927 | 662 / 5557 / 21544 / 16108266 | 30 (30–30) |
| heavy8x | pingora | 121 / 232 / 523 / 5208900 | 131 / 302 / 513 / 4158369 | 30 (30–30) |
| heavy8x | dashmap | 70 / 593 / 2094 / 3895733 | 80 / 876 / 19321 / 4858721 | 30 (30–30) |
| heavy8x | ecm | 80 / 951 / 1389 / 541683 | 101 / 29751 / 470552 / 4771197 | 30 (30–30) |
| heavy8x | pingora-x64 | – | – | – |
| heavy8x | pingora-x8 | – | – | – |
| heavy16x | clepsydra | 475 / 3633 / 5813 / 5252683 | 705 / 5642 / 22570 / 15758109 | 30 (30–30) |
| heavy16x | pingora | 121 / 232 / 513 / 3370464 | 131 / 302 / 513 / 4508543 | 30 (30–30) |
| heavy16x | dashmap | 70 / 662 / 2137 / 7616373 | 80 / 908 / 23596 / 9192175 | 30 (30–40) |
| heavy16x | ecm | 80 / 908 / 1282 / 733189 | 111 / 25990 / 519797 / 9017106 | 30 (30–30) |
| heavy16x | pingora-x64 | – | – | – |
| heavy16x | pingora-x8 | – | – | – |
| light16x | clepsydra | 2030 / 3548 / 23596 / 5384018 | 3505 / 56767 / 229805 / 3851981 | 30 (30–30) |
| light16x | pingora | 192 / 443 / 545 / 186032 | 232 / 411 / 716 / 2254271 | 30 (30–30) |
| light16x | dashmap | 342 / 919 / 7780 / 7879007 | 523 / 14875 / 281784 / 8929541 | 30 (30–30) |
| light16x | ecm | 465 / 940 / 8720 / 645642 | 555 / 177825 / 1181854 / 5777945 | 30 (30–30) |
| light16x | pingora-x64 | – | – | – |
| light16x | pingora-x8 | – | – | – |
