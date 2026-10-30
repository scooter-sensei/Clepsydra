package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/scooter-sensei/clepsydra/go/internal/ecm"
)

// The ECM-sketch under part 1's floods (docs/artifact/head-to-head.md,
// Supplement). The sketch is not safe for concurrent use, so the 12-thread
// passes share it under one mutex. Clepsydra's runner (clep.go) is left as
// it was read.

type ecmArm struct {
	s      *ecm.Sketch
	mu     sync.Mutex
	locked bool
	win    uint64
	limit  int
	sink   int
}

// op is one request: record, then the point query for the window, as a
// limiter checks and records.
func (e *ecmArm) op(key, tick uint64) {
	if e.locked {
		e.mu.Lock()
	}
	e.s.Add(key, tick)
	est, _ := e.s.Count(key, tick, e.win)
	e.sink += est
	if e.locked {
		e.mu.Unlock()
	}
}

// answer: the point estimate, and the strict decision on the upper bound
// (never below the truth, so it fails closed).
func (e *ecmArm) answer(key, tick uint64) answer {
	est, up := e.s.Count(key, tick, e.win)
	return answer{int32(est), int32(up), up >= e.limit}
}

func replayOp(op func(key, tick uint64), recs []record) {
	for _, r := range recs {
		for range r.count {
			op(r.key, uint64(r.tick))
		}
	}
}

func replayOpTimed(op func(key, tick uint64), recs []record, h *hist) {
	for _, r := range recs {
		for range r.count {
			t0 := rdtscp()
			op(r.key, uint64(r.tick))
			t1 := rdtscp()
			h.add(t1 - t0)
		}
	}
}

// ecmPeak replays a trace into a fresh sketch (records only) and returns its
// peak bytes.
func ecmPeak(t *trace, w, d int, eps float64, seed uint64) int {
	s := ecm.New(w, d, eps, t.HorizonMs, seed)
	for _, r := range t.recs {
		for range r.count {
			s.Add(r.key, uint64(r.tick))
		}
	}
	return s.Peak()
}

// cmdECMSize finds the widest sketch whose peak bytes on the trace fit the
// budget (binary search, seed 1).
func cmdECMSize(path string, budget, d int, eps float64) error {
	t, err := readTrace(path)
	if err != nil {
		return err
	}
	lo, hi := 0, budget/d+1
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if ecmPeak(t, mid, d, eps, 1) <= budget {
			lo = mid
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return fmt.Errorf("%s: no width of depth %d fits %d B", path, d, budget)
	}
	fmt.Printf("%s d=%d eps_sw=%g w=%d peak=%d budget=%d\n", floodName(t.Mult, t.Weight), d, eps, lo, ecmPeak(t, lo, d, eps, 1), budget)
	return nil
}

// cmdRunECM runs one pass of the ECM-sketch arm on a trace.
func cmdRunECM(path, pass string, round, threads int, answersPath string, w, d int, eps float64) error {
	t, err := readTrace(path)
	if err != nil {
		return err
	}
	r := result{Arm: "ecm", Flood: floodName(t.Mult, t.Weight), Pass: pass, Round: round, Threads: 1, Trace: t.sha, Ops: t.Ops,
		Note: fmt.Sprintf("d=%d eps_sw=%g w=%d", d, eps, w)}
	e := &ecmArm{s: ecm.New(w, d, eps, t.HorizonMs, uint64(round)), win: t.WindowMs, limit: int(t.Limit)}
	switch pass {
	case "acc":
		t0 := time.Now()
		replayOp(e.op, t.recs)
		r.Seconds = time.Since(t0).Seconds()
		as := make([]answer, len(t.probes))
		for i, p := range t.probes {
			as[i] = e.answer(p.key, t.ProbeTick)
		}
		r.Bytes = int64(e.s.Bytes())
		if answersPath != "" {
			if err := writeAnswers(answersPath, as); err != nil {
				return err
			}
		}
	case "t1":
		t0 := time.Now()
		replayOp(e.op, t.recs)
		r.Seconds = time.Since(t0).Seconds()
	case "t12":
		r.Threads, e.locked = threads, true
		r.Seconds = parallel(split(t.recs, threads), func(_ int, recs []record) { replayOp(e.op, recs) }).Seconds()
	case "lat1", "lat12":
		tpn := tscPerNs()
		h := new(hist)
		if pass == "lat1" {
			t0 := time.Now()
			replayOpTimed(e.op, t.recs, h)
			r.Seconds = time.Since(t0).Seconds()
		} else {
			r.Threads, e.locked = threads, true
			hs := make([]hist, threads)
			r.Seconds = parallel(split(t.recs, threads), func(i int, recs []record) { replayOpTimed(e.op, recs, &hs[i]) }).Seconds()
			for i := range hs {
				h.merge(&hs[i])
			}
		}
		r.P50, r.P99, r.P999, r.Max = h.quantile(0.5, tpn), h.quantile(0.99, tpn), h.quantile(0.999, tpn), h.quantile(1, tpn)
		r.TimerP = timerFloor(1_000_000).quantile(0.999, tpn)
	default:
		return fmt.Errorf("unknown pass %q", pass)
	}
	r.Mops = float64(r.Ops) / r.Seconds / 1e6
	emit(r)
	return nil
}
