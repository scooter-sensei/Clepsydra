#!/bin/bash
# lab/sw W4b's mends (SW-RESULTS.md), applied to the tree once W4's readings are done.
set -eu
cd $HOME/lab-sw/go
# 1. next0: an event behind the last touch in an earlier unit is counted at the
#    last touch's unit, not its own: the log says nothing.
perl -0pi -e 's/\td := dripOf\(w0\)\n\tvar g uint64\n\tif now > last \{\n\t\tg = uint64\(f\.quantum\(now\) - f\.quantum\(last\)\)\n\t\}\n\tswitch \{\n\tcase g >= dripBits:\n\t\td = dripMask\n\tcase d != 0:\n\t\td = \(d<<\(g\+1\) \| \(1<<g - 1\)\) & dripMask\n\t\}\n/\td := dripOf(w0)\n\tswitch {\n\tcase now > last:\n\t\tif g := uint64(f.quantum(now) - f.quantum(last)); g >= dripBits {\n\t\t\td = dripMask\n\t\t} else if d != 0 {\n\t\t\td = (d<<(g+1) | (1<<g - 1)) & dripMask\n\t\t}\n\tcase f.unitOf(now) < f.unitOf(last):\n\t\td = 0 \/\/ the register counts it at the last touch\x27s unit, not its own (W4b)\n\tdefault:\n\t\td = d << 1 & dripMask \/\/ at or behind the last touch in its unit: a gap of 0\n\t}\n/' drip.go
grep -q 'W4b' drip.go
cat >>drip.go <<'EOF'

// dripForget makes the drip log of slot i (word0 index), if it still holds
// the key fpd (fingerprint and displaced bit), say nothing: the touch's count
// landed at a later unit than its event's (a touch moved the last touch into
// a later unit between this touch's word0 CAS and its count), where the log
// keeps the event at its own tick (W4b).
func (f *Filter) dripForget(i int, fpd uint64) {
	for {
		x := atomic.LoadUint64(&f.words[i])
		if x>>47 != fpd || dripOf(x) == 0 || atomic.CompareAndSwapUint64(&f.words[i], x, x&^(dripMask<<dripShift)) {
			return
		}
	}
}
EOF
sed -i 's/^package temporalfilter$/package temporalfilter\n\nimport "sync\/atomic"/' drip.go
# 2. touch: after the count, a count at a later unit than the event's forgets the log
perl -0pi -e 's/(\t\tif !ok \{\n\t\t\tcontinue \/\/ the slot was handed to another key, or moved on, meanwhile; look again\n\t\t\}\n)(\t\tsat := w0&satBit != 0\n)/$1\t\tif dripOn \&\& ref > now \&\& f.unitOf(ref) > nu {\n\t\t\tf.dripForget(i, fpd) \/\/ counted at a later unit than its own (W4b)\n\t\t}\n$2/' touch.go
grep -q 'dripForget' touch.go
# 3. resize: the move carries the log
perl -0pi -e 's/f\.claim\(h, fp, w1, now, w0&tsMask, w0&satBit, claimFull\)/f.claim(h, fp, w1, now, w0\&(tsMask|dripMask<<dripShift), w0\&satBit, claimFull) \/\/ the drip log with it (W4b)/' resize.go
grep -q 'W4b' resize.go
gofmt -l . || true
echo applied
# 4. W4b's clause 7: a periodic workload for TestShortWindowRobust (TF_SW_WORKLOADS)
perl -0pi -e 's/\t\tif variant == "loguni" \{\n/\t\tif variant == "periodic" { \/\/ a fixed period log-uniform in 0.1-20 s, a random phase\n\t\t\tp := math.Exp(math.Log(100) + r.Float64()*math.Log(20_000\/100))\n\t\t\tfor t := r.Float64() * p; t < end; t += p {\n\t\t\t\tevs = append(evs, ev{uint64(t) + 1, k})\n\t\t\t}\n\t\t\tcontinue\n\t\t}\n\t\tif variant == "loguni" {\n/; s/"loguni": 1\}\[variant\]/"loguni": 1, "periodic": 1}[variant]/; s/\tfor _, v := range \[\]string\{"base", "half", "double", "loguni"\} \{/\tworkloads := []string{"base", "half", "double", "loguni"}\n\tif w := os.Getenv("TF_SW_WORKLOADS"); w != "" {\n\t\tworkloads = strings.Split(w, ",")\n\t}\n\tfor _, v := range workloads {/' swenc_test.go
grep -q 'TF_SW_WORKLOADS' swenc_test.go && grep -q '"periodic": 1' swenc_test.go
gofmt -l . || true
echo applied-4
# 5. W4b's reported reading: sustained traffic with bursts inside it
perl -0pi -e 's/\t\tif variant == "periodic" \{/\t\tif variant == "sustained" { \/\/ 5\/s Poisson, and 15 events in 200 ms every ~10 s\n\t\t\tpoisson(0, end, 5.0\/1000, k)\n\t\t\tfor t := r.ExpFloat64() * 10_000; t < end; t += r.ExpFloat64() * 10_000 {\n\t\t\t\tfor range 15 {\n\t\t\t\t\tevs = append(evs, ev{uint64(t+r.Float64()*200) + 1, k})\n\t\t\t\t}\n\t\t\t}\n\t\t\tcontinue\n\t\t}\n\t\tif variant == "periodic" {/; s/"periodic": 1\}\[variant\]/"periodic": 1, "sustained": 1}[variant]/' swenc_test.go
grep -q '"sustained": 1' swenc_test.go
gofmt -l . || true
echo applied-5
