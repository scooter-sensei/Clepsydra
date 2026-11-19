#!/bin/bash
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
SP=/mnt/c/Users/priya/AppData/Local/Temp/claude/48fa9124dd982f76/6581b790-710a-4839-a396-5a66b805b4d8/scratchpad
D=~/lab-sw-out/w10-diag
rm -rf $D/split2 && mkdir -p $D/split2 $D/rate2
git -C ~/lab-sw archive 223ce81 | tar -x -C $D/split2
cd $D/split2/go
cp $SP/w10hook.go $SP/w10parts_test.go .
perl -0pi -e 's/(\tif !ok \{\n\t\treturn\n\t\}\n\tr\.Count \+= s\.Count\n)/\tif testHookW10Sketch != nil {\n\t\ttestHookW10Sketch(h, s, ok)\n\t}\n$1/' touch.go
perl -0pi -e 's/(\tghost := func\(h, window, now uint64\) int \{\n)/$1\t\tw10Parts.Delete(h)\n/; s/(g, res\.ops, op, m, r, h, window, now, tr\.newest, tail\(tr\.ev\[h\], 12\), f\.whereIs\(h, now\)\)\n)/$1\t\t\t\t\tres.fail += "; OVERFLOW PARTS ADDED " + w10PartsOf(h)\n/' fuzzconc_test.go
for f in touch.go fuzzconc_test.go; do diff <(git -C ~/lab-sw show 223ce81:go/$f) $f; done
go vet -tags tfdrip . || exit 1
for b in tfdrip default; do
  tags=""; [ $b = tfdrip ] && tags="-tags tfdrip"
  go test $tags -count=1 -c -o $D/rate2/cf-$b.test . || { echo "build $b failed"; continue; }
  fails=0
  for i in $(seq 1 60); do
    TF_CFUZZ_CASE=865 TF_CFUZZ_RUNS=1 $D/rate2/cf-$b.test -test.run '^TestFuzzConcurrent$' -test.v > $D/rate2/$b-$i.txt 2>&1 || fails=$((fails+1))
  done
  echo "$b: case 865, $fails of 60 single runs fail"
  grep -hoE '(Count|Touch): Upper [0-9]+ \(the overflow[^)]*\) > [0-9]+ events|result \{[^}]*\}|OVERFLOW PARTS ADDED .*' $D/rate2/$b-*.txt | paste - - - | sort | uniq -c
  grep -hE 'Upper [0-9]+ < true|at rest' $D/rate2/$b-*.txt | cut -c1-300 | head -3
done
