package temporalfilter

import (
	"os"
	"sync/atomic"
	"testing"
)

func init() {
	if os.Getenv("TF_NOCAS2") != "" {
		hasCAS2 = false // realign holds the slot, as on other architectures
	}
}

// realignModes runs fn with realign's 16-byte CAS, when the CPU has it, and
// then with its hold.
func realignModes(t *testing.T, fn func(t *testing.T)) {
	if hasCAS2 {
		t.Run("cas2", fn)
		hasCAS2 = false
		defer func() { hasCAS2 = true }()
	}
	t.Run("hold", fn)
}

func TestCAS2(t *testing.T) {
	if !hasCAS2 {
		t.Skip("no CMPXCHG16B, or TF_NOCAS2")
	}
	f := newMinuteFilter(t, 1000)
	p := &f.words[2] // a slot: 16-byte aligned
	f.words[2], f.words[3] = 10, 20
	for _, c := range []struct {
		old0, old1 uint64
		ok         bool
	}{{11, 20, false}, {10, 21, false}, {10, 20, true}} {
		w0, w1 := f.words[2], f.words[3]
		if ok := cas2(p, c.old0, c.old1, 30, 40); ok != c.ok {
			t.Fatalf("cas2 from %d, %d over %d, %d: %v", c.old0, c.old1, w0, w1, ok)
		}
		want := [2]uint64{w0, w1}
		if c.ok {
			want = [2]uint64{30, 40}
		}
		if [2]uint64{f.words[2], f.words[3]} != want || f.words[1] != 0 || f.words[4] != 0 {
			t.Fatalf("after cas2 from %d, %d: words %v", c.old0, c.old1, f.words[:5])
		}
	}
}

// TestRealignCAS2 (claims A33): K's register is full and its next touch is
// 10 units on, so realign's advance spills and K is promoted. The entry is
// published with both words by one CAS, with no move counted; if word0
// moved since realign read it, the slot is left as it was and the entry
// closed again.
func TestRealignCAS2(t *testing.T) {
	if !hasCAS2 {
		t.Skip("no CMPXCHG16B, or TF_NOCAS2")
	}
	for _, moved := range []bool{false, true} {
		f := newMinuteFilter(t, 1000)
		const tg = 0x321
		k := keyAt(f, 7, twoLineFPs(f, 7, 0x6600, 1)[0], tg)
		f.TouchAt(k, 0, sweepT0)
		sl := slotsOf(f, k)
		if len(sl) != 1 {
			t.Fatalf("setup: K in %v", sl)
		}
		i := sl[0]
		full := uint64(tg)<<tagShift | uint64(f.unitOf(sweepT0))&epochMask<<48 | regMask // every bucket full
		atomic.StoreUint64(&f.words[i+1], full)
		w0 := atomic.LoadUint64(&f.words[i])
		if moved { // a touch of K at the next tick, after realign's read
			atomic.StoreUint64(&f.words[i], w0+1)
		}
		m := f.moveOf(tg)
		begun := m.begun.Load()
		w1, over, ok := f.realign2(i, w0, full, tg, sweepT0+10*f.unit)
		n0, n1 := atomic.LoadUint64(&f.words[i]), atomic.LoadUint64(&f.words[i+1])
		switch {
		case m.begun.Load() != begun:
			t.Fatalf("moved %v: realign2 counted a move", moved)
		case moved && (ok || n0 != w0+1 || n1 != full || f.HotInUse() != 0 || f.Promoted() != 0):
			t.Fatalf("moved: ok %v, words %#x %#x, hot in use %d, promoted %d", ok, n0, n1, f.HotInUse(), f.Promoted())
		case !moved && (!ok || over || !isHot(w1) || n1 != w1 || n0 != w0&^tsMask|(sweepT0+10*f.unit) || f.HotInUse() != 1 || f.Promoted() != 1):
			t.Fatalf("ok %v, over %v, w1 %#x, words %#x %#x, hot in use %d, promoted %d", ok, over, w1, n0, n1, f.HotInUse(), f.Promoted())
		}
	}
}
