#!/bin/bash
# The lab's matrix (setup.sh first): every mutant against each arrangement,
# N runs each (default 20), each run its own process; MODE=-short (default)
# or MODE= for full length. One summary line per cell, also in LAB/results.
#   CPPV: name:pin:inject:k:vary (ghost_lab_test.cpp's LAB_* knobs). The
#   defaults are cpp/README.md's columns: the reference's arrangement
#   ported, the same on 2 CPUs, and the suite's stops; GOPROCS: the Go
#   test at GOMAXPROCS(2) (its own) and on every core (0).
set -u
LAB=${LAB:-${TMPDIR:-/tmp}/clepsydra-ghost-race}
N=${N:-20}
MODE=${MODE--short}
OUT=$LAB/results${TAG:-}$MODE.txt
: > $OUT
summ() { # per-run LAB lines -> means
  awk '{for(i=1;i<=NF;i++){split($i,a,"="); v[a[1]]=a[2]} n++; lost+=v["lost"]; split(v["miss"],m,"/"); for(j=1;j<=5;j++) ms[j]+=m[j]; st+=v["stale"]; secs+=v["secs"]}
       END{if(n) printf "lost/run %7.1f  rejections/run: free %7.1f victim %5.1f floor %5.1f merge %4.1f  s/run %.2f", lost/n, ms[2]/n, (ms[3]+ms[4])/n, ms[5]/n, ms[1]/n, secs/n}' "$1"
}
for m in ${MUTS:-0 1 2 3 4}; do
  for v in ${CPPV:-ported:0:0:1:0 two-cpus:1:0:1:0 stops:0:1:3:0}; do
    IFS=: read name pin inj k vary <<< "$v"
    f=$LAB/raw-cpp-$m-$name$MODE.txt; : > $f; fails=0
    for i in $(seq $N); do
      LAB_PIN=$pin LAB_INJECT=$inj LAB_K=$k LAB_VARY=$vary $LAB/cpp/ghost-$m -run '^TestGhostLab$' $MODE > $LAB/tmp.log 2>&1 || fails=$((fails+1))
      grep '^LAB' $LAB/tmp.log | sed 's/^LAB //' >> $f
    done
    printf 'C++ mut=%s %-12s failed %2d/%d  %s\n' $m $name $fails $N "$(summ $f)" | tee -a $OUT
  done
  for procs in ${GOPROCS:-2 0}; do
    f=$LAB/raw-go-$m-p$procs$MODE.txt; : > $f; fails=0
    for i in $(seq $N); do
      ( cd $LAB/go-$m && LAB_PROCS=$procs ./ghost.test -test.run '^TestGhostEpochRace$' -test.count 1 -test.v ${MODE:+-test.${MODE#-}} > $LAB/tmp.log 2>&1 ) || fails=$((fails+1))
      grep -o 'LAB .*' $LAB/tmp.log | sed 's/^LAB //' >> $f
    done
    printf 'Go  mut=%s procs=%-7s failed %2d/%d  %s\n' $m $procs $fails $N "$(summ $f)" | tee -a $OUT
  done
done
