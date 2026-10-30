package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	tf "github.com/scooter-sensei/clepsydra/go"
)

// result is one pass of one arm on one trace, printed as a JSON line. The
// Rust runner (bench/h2h) prints the same fields.
type result struct {
	Arm     string  `json:"arm"`
	Flood   string  `json:"flood"`
	Pass    string  `json:"pass"`
	Round   int     `json:"round"`
	Threads int     `json:"threads"`
	Trace   string  `json:"trace"` // the trace's SHA-256, first 8 bytes
	Ops     uint64  `json:"ops"`
	Seconds float64 `json:"seconds"`
	Mops    float64 `json:"mops"`
	P50     float64 `json:"p50_ns,omitempty"`
	P99     float64 `json:"p99_ns,omitempty"`
	P999    float64 `json:"p999_ns,omitempty"`
	Max     float64 `json:"max_ns,omitempty"`
	TimerP  float64 `json:"timer_p999_ns,omitempty"` // an empty timed region's p99.9
	Bytes   int64   `json:"bytes,omitempty"`
	Note    string  `json:"note,omitempty"`
}

func emit(r result) {
	b, _ := json.Marshal(r)
	fmt.Println(string(b))
}

// clep is the Clepsydra arm: the defaults, Capacity = the key count.
type clep struct {
	f     *tf.Filter
	win   uint64
	limit int
}

func newClep(t *trace) *clep {
	f, err := tf.New(tf.Config{Capacity: int(t.Capacity), Horizon: time.Duration(t.HorizonMs) * time.Millisecond, Resolution: time.Millisecond})
	if err != nil {
		panic(err)
	}
	return &clep{f: f, win: t.WindowMs, limit: int(t.Limit)}
}

// op is one request: check and record, as a limiter calls it.
func (c *clep) op(key, tick uint64) { c.f.TouchAt(c.f.HashUint64(key), c.win, tick) }

// answer reads a probe without recording it; the strict decision is the
// fail-closed recipe (docs/paper/04-operations.md, Using the trust flags).
func (c *clep) answer(key, tick uint64) answer {
	r := c.f.CountAt(c.f.HashUint64(key), c.win, tick)
	return answer{int32(r.Count), int32(r.Upper), r.Count >= c.limit || r.Saturated || r.Upper >= c.limit}
}

func replay(c *clep, recs []record) {
	for _, r := range recs {
		for range r.count {
			c.op(r.key, uint64(r.tick))
		}
	}
}

func replayTimed(c *clep, recs []record, h *hist) {
	for _, r := range recs {
		for range r.count {
			t0 := rdtscp()
			c.op(r.key, uint64(r.tick))
			t1 := rdtscp()
			h.add(t1 - t0)
		}
	}
}

// split gives each thread its keys' records, in trace order.
func split(recs []record, threads int) [][]record {
	parts := make([][]record, threads)
	for _, r := range recs {
		p := part(r.key, threads)
		parts[p] = append(parts[p], r)
	}
	return parts
}

// parallel runs fn on each part from one start signal and returns the time
// from the signal to the last thread's end.
func parallel(parts [][]record, fn func(i int, recs []record)) time.Duration {
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ends := make([]time.Time, len(parts))
	for i := range parts {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			fn(i, parts[i])
			ends[i] = time.Now()
		}()
	}
	ready.Wait()
	t0 := time.Now()
	close(start)
	done.Wait()
	last := t0
	for _, e := range ends {
		if e.After(last) {
			last = e
		}
	}
	return last.Sub(t0)
}

// cmdRun runs one pass of the Clepsydra arm on a trace.
func cmdRun(path, pass string, round, threads int, answersPath string) error {
	t, err := readTrace(path)
	if err != nil {
		return err
	}
	r := result{Arm: "clepsydra", Flood: floodName(t.Mult, t.Weight), Pass: pass, Round: round, Threads: 1, Trace: t.sha, Ops: t.Ops}
	c := newClep(t)
	switch pass {
	case "acc":
		t0 := time.Now()
		replay(c, t.recs)
		r.Seconds = time.Since(t0).Seconds()
		as := make([]answer, len(t.probes))
		for i, p := range t.probes {
			as[i] = c.answer(p.key, t.ProbeTick)
		}
		r.Bytes = int64(c.f.Bytes())
		if answersPath != "" {
			if err := writeAnswers(answersPath, as); err != nil {
				return err
			}
		}
	case "t1":
		t0 := time.Now()
		replay(c, t.recs)
		r.Seconds = time.Since(t0).Seconds()
	case "t12":
		r.Threads = threads
		r.Seconds = parallel(split(t.recs, threads), func(_ int, recs []record) { replay(c, recs) }).Seconds()
	case "lat1", "lat12":
		tpn := tscPerNs()
		h := new(hist)
		if pass == "lat1" {
			t0 := time.Now()
			replayTimed(c, t.recs, h)
			r.Seconds = time.Since(t0).Seconds()
		} else {
			r.Threads = threads
			hs := make([]hist, threads)
			r.Seconds = parallel(split(t.recs, threads), func(i int, recs []record) { replayTimed(c, recs, &hs[i]) }).Seconds()
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

func fail(err error) {
	fmt.Fprintln(os.Stderr, "h2h:", err)
	os.Exit(1)
}
