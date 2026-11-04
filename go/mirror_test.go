package temporalfilter

import (
	"testing"
	"time"
)

// TestMirrorKeyInFoundSlotNotCounted: keys H and K share their fingerprint
// and tag, and K's primary line is H's alternate, so K lives in H's primary
// line displaced: only the displaced bit tells their entries apart (find
// matches it). When K's entry takes the slot a lookup found H in (a
// relocation moving H out and K in, between a batch's warm and its touch,
// or between a lookup and its load), H's touch and read must not take K's
// slot for H's. Before, they checked the fingerprint alone: H's touch
// counted in K's register (K one high, H's event gone).
func TestMirrorKeyInFoundSlotNotCounted(t *testing.T) {
	f, err := New(Config{Capacity: 54, Horizon: time.Minute, Resolution: time.Millisecond, Clock: func() uint64 { return 1 }})
	if err != nil {
		t.Fatal(err)
	}
	const fp, tg = 0x1234, 0x567
	p := uint64(0)
	if f.alt(p, fp) == p {
		p = 1
	}
	q := f.alt(p, fp)
	withPrimary := func(line uint64) uint64 { // bits 0..35 that primary maps to line
		return fp<<48 | tg<<36 | (line<<36+f.lines-1)/f.lines
	}
	h, k := withPrimary(p), withPrimary(q)
	if f.primary(h) != p || f.primary(k) != q || fingerprint(h) != fp || tag(h) != tg || q == p {
		t.Fatalf("keys not built as meant: lines %d, %d (want %d, %d)", f.primary(h), f.primary(k), p, q)
	}
	const t0 = 60_000
	f.TouchAt(h, 0, t0)
	i0 := f.locate(h, fp, tg)
	if i0 < 0 || uint64(i0/wordsPerLine) != p {
		t.Fatalf("H not in its primary line (slot %d)", i0)
	}
	// K's entry, displaced, in the slot H was found in.
	kw1, _ := f.nextReg(0, tg, t0, t0, true)
	f.words[i0], f.words[i0+1] = fp<<48|dispBit|t0, kw1
	if f.locate(k, fp, tg) != i0 {
		t.Fatal("K not found in H's old slot")
	}
	if w0, _, src := f.read(h, t0, i0); src == srcTable && w0&dispBit != 0 {
		t.Errorf("H's read from the found slot returned K's words")
	}
	if forceCT {
		f.TouchAt(h, 0, t0+1) // ConstantTime's touch takes no slot from warm: the lookup path only
	} else {
		f.touch(h, f.horizon, t0+1, true, i0) // as a batch does, from warm's slot
	}
	if r := f.CountAt(k, f.horizon, t0+1); r.Count != 1 {
		t.Fatalf("K reads %+v after H's touch, want count 1: H's touch counted in K's slot", r)
	}
	if f.locate(h, fp, tg) < 0 {
		t.Fatal("H's touch went to no slot")
	}
}
