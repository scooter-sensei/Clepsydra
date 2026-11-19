#!/bin/bash
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
D=~/lab-sw-out/w10-diag
rm -rf $D/plain && mkdir -p $D/plain $D/rate
git -C ~/lab-sw archive 223ce81 | tar -x -C $D/plain
cd $D/plain/go
for b in tfdrip default; do
  tags=""; [ $b = tfdrip ] && tags="-tags tfdrip"
  go test $tags -count=1 -c -o $D/rate/cf-$b.test . || { echo "build $b failed"; continue; }
  fails=0
  for i in $(seq 1 40); do
    TF_CFUZZ_CASE=865 TF_CFUZZ_RUNS=1 $D/rate/cf-$b.test -test.run '^TestFuzzConcurrent$' -test.v > $D/rate/$b-$i.txt 2>&1 || { fails=$((fails+1)); }
  done
  echo "$b: case 865, $fails of 40 single runs fail"
  grep -hoE 'Count: Upper [0-9]+ \(the overflow[^)]*\) > [0-9]+ events|Touch: Upper [0-9]+ \(the overflow[^)]*\) > [0-9]+ events|Upper [0-9]+ < true [0-9]+|at rest[^;]*;[^;]*' $D/rate/$b-*.txt | sort | uniq -c | head
  grep -hoE 'result \{[^}]*\}' $D/rate/$b-*.txt | sort | uniq -c | head
done
