#!/bin/bash
# The overflow table's race lab (cpp/README.md, The overflow table's race
# tests): copies of src/ghost.cpp and go/ghost.go whose epoch guards count
# each rejection, and mutants that drop guards:
#   MUT 0 none; 1 the epoch read once before the retry loop, no guards (the
#   defect 42cc598 fixed); 2 the free-entry write's guard; 3 raiseFloor's
#   guard; 4 the victim path's two guards.
# Five C++ test binaries (the suite's ghost tests and tests/ghost_lab_test.cpp,
# TestGhostEpochRace with its arrangement from the environment) and five Go
# test binaries, in LAB. run.sh runs the matrix.
#
# The guards are found by line number in this revision's ghost.cpp and
# ghost.go; the count checks below stop the lab if those lines have moved.
# Needs CXX (C++20; zig's: a zcxx wrapper) and go on PATH.
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
SRC="$(cd "$HERE/../../.." && pwd)"
LAB=${LAB:-${TMPDIR:-/tmp}/clepsydra-ghost-race}
if [ -e "$LAB" ] && [ ! -f "$LAB/.ghost-race" ]; then echo "$LAB exists and is not a ghost-race lab"; exit 1; fi
rm -rf "$LAB"; mkdir -p "$LAB/cpp/tests"; touch "$LAB/.ghost-race"
cp -r "$SRC/cpp/src" "$SRC/cpp/include" $LAB/cpp/
cp "$SRC/cpp/tests/"*.hpp "$SRC/cpp/tests/main.cpp" "$SRC/cpp/tests/ghost_test.cpp" $LAB/cpp/tests/
cp "$HERE/ghost_lab_test.cpp" $LAB/cpp/tests/
cd $LAB/cpp
sed -i -e '3a\
#include <atomic>\
namespace clepsydra::detail { std::atomic<long long> labMiss[5]; }\
#ifndef MUT\
#define MUT 0\
#endif\
static inline bool labDrop(int s) { return MUT == 1 || (MUT == 2 \&\& s == 1) || (MUT == 3 \&\& s == 4) || (MUT == 4 \&\& (s == 2 || s == 3)); }\
static inline bool labOk(int s, bool ok) { if (!ok) clepsydra::detail::labMiss[s]++; return ok; }\
#define LABG(s, c) (labDrop(s) || labOk(s, (c)))' \
 -e '84s|^  for (;;) {  // Go: outer|#if MUT == 1\n  u64 ep = epoch.load();\n  if (sl + 3 < ep) return;\n#endif\n  for (;;) {|' \
 -e '86s|^    u64 ep = epoch.load();|#if MUT != 1\n    u64 ep = epoch.load();|' \
 -e '87s|^    if (sl + 3 < ep) return;.*|    if (sl + 3 < ep) return;\n#endif|' \
 -e '119s|epoch.load() == ep && cas|LABG(0, epoch.load() == ep) \&\& cas|' \
 -e '143s|epoch.load() == ep && cas|LABG(1, epoch.load() == ep) \&\& cas|' \
 -e '146s|if (epoch.load() != ep) continue;|if (!LABG(2, epoch.load() == ep)) continue;|' \
 -e '162s|epoch.load() == ep && cas|LABG(3, epoch.load() == ep) \&\& cas|' \
 -e '183s|(epoch.load() == ep && cas|(LABG(4, epoch.load() == ep) \&\& cas|' src/ghost.cpp
[ $(grep -c "LABG(" src/ghost.cpp) = 6 ] || { echo "C++ patch failed"; grep -n "LABG\|MUT" src/ghost.cpp; exit 1; }
FL=(-std=c++20 -pthread -O2 -g0 -ffp-contract=off -Isrc -Iinclude -Itests '-DCLEPSYDRA_VECTOR_DIR="/x"' '-DCLEPSYDRA_GO_DIR="/x"')
for m in 0 1 2 3 4; do
  ${CXX:-c++} "${FL[@]}" -DMUT=$m src/*.cpp tests/main.cpp tests/ghost_test.cpp tests/ghost_lab_test.cpp -o ghost-$m > build-$m.log 2>&1 &
done
wait
ls -la ghost-? || { cat build-*.log | head -40; exit 1; }
# Go
export GOTOOLCHAIN=local CGO_ENABLED=0 GOFLAGS=-mod=mod
for m in 0 1 2 3 4; do
  d=$LAB/go-$m; mkdir -p $d
  cp "$SRC/go/"*.go "$SRC/go/go.mod" $d/; cp -r "$SRC/go/internal" $d/
  [ -f "$SRC/go/go.sum" ] && cp "$SRC/go/go.sum" $d/
  sed -i -e 's|^import (|import (\n\t"os"\n\t"strconv"|' \
   -e '166s|^outer:|\tep0 := g.epoch.Load()\n\tif labMut == 1 \&\& sl+3 < ep0 {\n\t\treturn\n\t}\nouter:|' \
   -e '172s|ep := g.epoch.Load()|ep := g.epoch.Load()\n\t\tif labMut == 1 {\n\t\t\tep = ep0\n\t\t}|' \
   -e '212s|g.epoch.Load() == ep && |labG(0, g.epoch.Load() == ep) \&\& |' \
   -e '236s|g.epoch.Load() == ep && |labG(1, g.epoch.Load() == ep) \&\& |' \
   -e '241s|if g.epoch.Load() != ep {|if !labG(2, g.epoch.Load() == ep) {|' \
   -e '264s|g.epoch.Load() == ep && |labG(3, g.epoch.Load() == ep) \&\& |' \
   -e '284s|g.epoch.Load() == ep && |labG(4, g.epoch.Load() == ep) \&\& |' $d/ghost.go
  cat >> $d/ghost.go <<GO

var labMiss [5]atomic.Int64

const labMut = $m

func labG(s int, ok bool) bool {
	if labMut == 1 || labMut == 2 && s == 1 || labMut == 3 && s == 4 || labMut == 4 && (s == 2 || s == 3) {
		return true
	}
	if !ok {
		labMiss[s].Add(1)
	}
	return ok
}

func labProcs() int {
	if v, err := strconv.Atoi(os.Getenv("LAB_PROCS")); err == nil {
		return v
	}
	return 2
}
GO
  [ $(grep -c "labG(" $d/ghost.go) = 6 ] || { echo "Go patch failed"; exit 1; }
  sed -i -e 's|defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))|defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(labProcs()))|' \
   -e 's|^\tif lost.Load() > 0 {|\tt.Logf("LAB lost=%d miss=%d/%d/%d/%d/%d", lost.Load(), labMiss[0].Load(), labMiss[1].Load(), labMiss[2].Load(), labMiss[3].Load(), labMiss[4].Load())\n\tif lost.Load() > 0 {|' $d/ghost_test.go
  (cd $d && go test -c -o ghost.test . > build.log 2>&1 || { echo "go build $m failed"; head -20 build.log; }) &
done
wait
ls -la $LAB/go-?/ghost.test
