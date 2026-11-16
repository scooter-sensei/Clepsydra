#!/bin/bash
# Export lab/sw-2026-11 as patches for go/verify/experiments/ (the earlier labs' form).
# Usage: export.sh <dir>
set -eu
D=$1
mkdir -p "$D"
cd $HOME/lab-sw
base=8b3ba44
# the notebook, its scripts and readings
git diff $base lab/sw-2026-11 -- SW-RESULTS.md lab >"$D/lab-notebook-sw-2026-11.patch"
# the models (W1-W3)
git diff $base lab/sw-2026-11 -- go/swinfo_test.go go/swenc_test.go >"$D/sw-information.patch"
# the drip log as each arm built it: W4 (b8739a9), W4b (06940e8), W4c (lab/sw-2026-11)
lib() { git diff $base "$1" -- go ':!go/swinfo_test.go' ':!go/swenc_test.go'; }
lib b8739a9 >"$D/drip-log-w4.patch"
lib 06940e8 >"$D/drip-log-w4b.patch"
lib lab/sw-2026-11 >"$D/drip-log.patch"
# the wrap (W5a, W5b) on top of W4c
git diff lab/sw-2026-11 lab/sw-w5 -- go >"$D/wrapped-last-touch-w5a.patch"
git diff lab/sw-2026-11 lab/sw-w5b -- go >"$D/wrapped-last-touch.patch"
wc -l "$D"/*.patch
