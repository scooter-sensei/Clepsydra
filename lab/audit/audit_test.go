package temporalfilter

// Gap audit probes for lab/sw-2026-11 (not part of any arm).

import (
	"testing"
	"time"
)

const auditWrap = uint64(1) << 34

func auditPanics(call func()) (p bool) {
	defer func() { p = recover() != nil }()
	call()
	return false
}

// TestAuditTickPoison: a call past MaxTick must panic and leave the filter as
// it was (TestTickRange's "after a recovered panic"). Under the wrap, keepUp
// runs before checkTick: does the call sweep the table at the bad tick, and
// does the deadline still work after it?
func TestAuditTickPoison(t *testing.T) {
	for _, via := range []string{"none", "TouchAt", "CountAt", "ContinuousDecay", "CountAt ahead 2^31+2", "CountAt ahead 2^35"} {
		t.Run(via, func(t *testing.T) {
			clk := uint64(1)
			f, err := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond, Clock: func() uint64 { return clk }})
			if err != nil {
				t.Fatal(err)
			}
			k := mix64(7)
			t0 := uint64(1_000_000)
			for range 3 {
				f.TouchAt(k, 0, t0)
			}
			before := f.CountAt(k, 60_000, t0+1)
			var p bool
			switch via {
			case "TouchAt":
				p = auditPanics(func() { f.TouchAt(mix64(8), 0, MaxTick+1) })
			case "CountAt":
				p = auditPanics(func() { f.CountAt(mix64(8), 0, MaxTick+1) })
			case "ContinuousDecay":
				clk = MaxTick + 1
				p = auditPanics(func() { f.ContinuousDecay() })
				clk = t0 + 1
			case "CountAt ahead 2^31+2": // within MaxTick: one caller ahead of the others
				p = auditPanics(func() { f.CountAt(mix64(8), 0, t0+1<<31+2) })
			case "CountAt ahead 2^35":
				p = auditPanics(func() { f.CountAt(mix64(8), 0, t0+1<<35) })
			}
			after := f.CountAt(k, 60_000, t0+1)
			t.Logf("%s past MaxTick: panicked %v; key before %d/%v, after %d/%v; doneAt %d cursor %d",
				via, p, before.Count, before.Seen, after.Count, after.Seen, f.doneAt.Load(), f.cursor.Load())
			if after.Count != before.Count {
				t.Errorf("%s: the call changed a live key's Count %d -> %d", via, before.Count, after.Count)
			}
			if !wrapOn {
				return
			}
			k2 := mix64(9)
			f.TouchAt(k2, 0, t0+2)
			end := t0 + 2 + auditWrap + 500
			clk = end
			if r := f.CountAt(k2, 60_000, end); r.Seen || r.Count != 0 {
				t.Errorf("%s: a key touched after the bad call reads %+v a wrap later, want unseen (deadline off)", via, r)
			}
		})
	}
}

// TestAuditAliasSettled: TestWrapAlias/stash with the Background placer
// drained (Waiting() == 0) before the clock jumps.
func TestAuditAliasSettled(t *testing.T) {
	if !wrapOn {
		t.Skip("tfdrip")
	}
	f, err := New(Config{Capacity: 64, Horizon: time.Minute, Resolution: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t0 := uint64(1_000_000)
	var ks []uint64
	for i := range 400 {
		k := mix64(uint64(i) + 1<<50)
		ks = append(ks, k)
		for range 1 + i%3 {
			f.TouchAt(k, 0, t0)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.Waiting() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if w := f.Waiting(); w > 0 {
		t.Fatalf("placer did not drain: %d waiting", w)
	}
	end := t0 + auditWrap + 500
	for _, k := range ks {
		if r := f.CountAt(k, 60_000, end); r.Seen || r.Count != 0 {
			t.Fatalf("key %x at the alias (placer drained): %+v, want unseen", k, r)
		}
	}
}
