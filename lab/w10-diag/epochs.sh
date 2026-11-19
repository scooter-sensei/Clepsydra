#!/bin/bash
export PATH=$HOME/sdk/go1.24.7/bin:$HOME/go/bin:$HOME/bin:/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=local CGO_ENABLED=0
SP=/mnt/c/Users/priya/AppData/Local/Temp/claude/48fa9124dd982f76/6581b790-710a-4839-a396-5a66b805b4d8/scratchpad
D=~/lab-sw-out/w10-diag
rm -rf $D/split3 $D/rate3 && mkdir -p $D/split3 $D/rate3
git -C ~/lab-sw archive 223ce81 | tar -x -C $D/split3
cd $D/split3/go
cp $SP/w10hook3.go w10hook.go
cp $SP/w10parts3_test.go w10parts_test.go
perl -0pi -e 's/(\tif !ok \{\n\t\treturn\n\t\}\n\tr\.Count \+= s\.Count\n)/\tif testHookW10Sketch != nil {\n\t\ttestHookW10Sketch(h, s, ok)\n\t}\n$1/' touch.go
perl -0pi -e 's/(func \(g \*ghost\) estimate\(id, window, now uint64\) \(r Result, ok bool\) \{\n\tbases, nb, fp := g\.slot\(id\)\n\tvar line \[2\]\[ghWords\]uint64\n\tvar ep uint64\n)/$1\tif testHookW10Est != nil {\n\t\tdefer func() { testHookW10Est(id, now, ep, g.until.Load(), line, nb, r) }()\n\t}\n/' ghost.go
perl -0pi -e 's/(\tghost := func\(h, window, now uint64\) int \{\n)/\tcb := uint64(0)\n$1\t\tf.w10Arm(h)\n\t\tcb = clock.Load()\n/; s/(g, res\.ops, op, m, r, h, window, now, tr\.newest, tail\(tr\.ev\[h\], 12\), f\.whereIs\(h, now\)\)\n)/$1\t\t\t\t\tres.fail += fmt.Sprintf("; CLOCK before %d (now %+d), after %d (now %+d); ", cb, int64(cb-now), clock.Load(), int64(clock.Load()-now)) + f.w10Report(h)\n/' fuzzconc_test.go
for f in touch.go ghost.go fuzzconc_test.go; do diff <(git -C ~/lab-sw show 223ce81:go/$f) $f; done
go vet -tags tfdrip . || exit 1
go test -tags tfdrip -count=1 -c -o $D/rate3/cf.test . || exit 1
fails=0
for i in $(seq 1 40); do
  TF_CFUZZ_CASE=865 TF_CFUZZ_RUNS=1 $D/rate3/cf.test -test.run '^TestFuzzConcurrent$' -test.v > $D/rate3/r-$i.txt 2>&1 || fails=$((fails+1))
done
echo "tfdrip, epoch-recording copy: case 865, $fails of 40 single runs fail"
