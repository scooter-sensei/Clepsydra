package temporalfilter

// lab/sw-2026-11, arm W12: target 7's metric when events reach the filter
// late, and from several goroutines (SW-RESULTS.md, Phase 12).
//
// tfbench's velocity workload (swWorkload) is recorded once per seed: its
// events in the generator's order, and its queries, one per tick from the
// first horizon on. Each event is then given an arrival tick, its own tick
// plus a delay drawn from a stream of its own (swLagOrder), and the filter
// is given the events in order of arrival (equal arrivals in an order drawn
// from that stream; with no delay, the generator's), each touched at its
// own tick (CheckAndTouchAt, as tfbench). The query of tick now
// (VelocityCountAt, the generator's key and window) comes after every
// event that arrived by now, and its exact count
// is over the events delivered by then. With no delay this is swRecord's
// replay, query for query.
//
// Delays (swLagDelays): "jitter", every event delayed uniformly in [0, J]
// ticks; "stall", 1% of events so delayed and the rest on time; J = 1, 10,
// 100 and 1,000 (ms, at Resolution 1 ms); and "none", the control.
//
// Leg A: one goroutine, plain mode. Leg B: 8 workers, the events of each
// arrival tick split among them behind a gate before the tick's query.
// "owned": key k's events always to worker k%8, each worker in arrival
// order, so it must answer as leg A query for query; "shared": the tick's
// events dealt to the workers in turn, so one key's events of one tick can
// run at once in two workers. Leg B runs in plain mode, Background, and
// ConstantTime (functional, no pad: as TF_CT=1 runs it).
//
// At each query the key's slot is classed (swLagClass): none (no history,
// or its last touch past the horizon), no log (the default build), says
// nothing (the drip log's valid bit clear) or valid; the queries, exact
// counts and errors are summed by class.

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	swLagNone = iota
	swLagNoLog
	swLagDead
	swLagValid
	swLagClasses
)

var swLagClassName = [swLagClasses]string{"none", "nolog", "dead", "valid"}

// swLagClass classes key h's slot at now (see the file's comment). It reads
// the slot as Count does (read: the table, the stash, the wait room).
func (f *Filter) swLagClass(h, now uint64) int {
	w0, _, src := f.read(h, now, -1)
	switch {
	case src == 0 || age(now, w0&tsMask) > f.horizon:
		return swLagNone
	case !dripOn:
		return swLagNoLog
	case dripOf(w0)&dripV == 0:
		return swLagDead
	}
	return swLagValid
}

// swLagFilter is swRecord's filter (tfbench's) in a W12 mode.
func swLagFilter(t testing.TB, mode string) *Filter {
	f, err := New(Config{Capacity: 200_000, Horizon: time.Minute, Resolution: time.Millisecond, Background: mode == "bg"})
	if err != nil {
		t.Fatal(err)
	}
	if mode == "ct" {
		f.ct = true
	}
	return f
}

// TestShortWindowLagClassify (TF_EXP=1) is W12's sight of swLagClass, on
// states made by hand: a key touched once (valid); a key touched again at
// a tick an earlier unit behind its last touch (says nothing), still so
// after a gap of 9 quanta and valid again after one of 11; a key never
// touched, and one a horizon and a tick past its touch (none); one exactly
// a horizon past it (live). In the default build every live key is no log.
func TestShortWindowLagClassify(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	f := swLagFilter(t, "plain")
	defer f.Close()
	const t0 = 100_000 // unit 40 (2,500 ticks), quantum 640 (156.25 ticks)
	a, b, c, d := mix64(1<<52|1), mix64(1<<52|2), mix64(1<<52|3), mix64(1<<52|4)
	live := swLagValid
	if !dripOn {
		live = swLagNoLog
	}
	want := func(name string, h, now uint64, cls int) {
		t.Helper()
		got := f.swLagClass(h, now)
		w0, _, _ := f.read(h, now, -1)
		t.Logf("W12 classify %s at %d: %s (word0 %#x)", name, now, swLagClassName[got], w0)
		if got != cls {
			t.Errorf("%s at %d: %s, want %s", name, now, swLagClassName[got], swLagClassName[cls])
		}
	}
	f.CheckAndTouchAt(a, 0, t0)
	want("a, touched once", a, t0, live)
	f.CheckAndTouchAt(b, 0, t0)
	f.CheckAndTouchAt(b, 0, t0-2_600) // unit 38, behind the last touch's 40
	dead := swLagDead
	if !dripOn {
		dead = swLagNoLog
	}
	want("b, touched an earlier unit behind", b, t0, dead)
	f.CheckAndTouchAt(b, 0, t0+1_500) // quantum 649: 9 after 640
	want("b, 9 quanta on", b, t0+1_500, dead)
	f.CheckAndTouchAt(b, 0, t0+1_500+1_719) // quantum 660: 11 after 649
	want("b, 11 quanta further", b, t0+1_500+1_719, live)
	want("c, never touched", c, t0, swLagNone)
	f.CheckAndTouchAt(d, 0, t0)
	want("d, a horizon on", d, t0+60_000, live)
	want("d, a horizon and a tick on", d, t0+60_001, swLagNone)
}

// swLagRec is one seed's workload: its events in the generator's order and
// its queries, the one of tick now at now-swLagH.
type swLagRec struct {
	ev []swLagEv
	qs []swLagQ
}

type swLagEv struct {
	t uint64
	k int32
}

type swLagQ struct{ k, wi int32 }

const (
	swLagH    = 60_000     // the horizon, in ticks
	swLagEnd  = 5 * swLagH // the workload's end
	swLagUnit = 2_500      // its unit
	swLagN    = 8          // leg B's workers
)

func swLagRecord(t testing.TB, s1, s2 uint64) *swLagRec {
	r := &swLagRec{}
	swWorkload(s1, s2, func(k int, tk uint64) { r.ev = append(r.ev, swLagEv{tk, int32(k)}) },
		func(k int, _ []uint64, wi int, now uint64) {
			if now != uint64(swLagH+len(r.qs)) {
				t.Fatalf("query at %d, want %d", now, swLagH+len(r.qs))
			}
			r.qs = append(r.qs, swLagQ{int32(k), int32(wi)})
		})
	return r
}

type swLagDelay struct {
	model string // "none", "jitter" or "stall"
	j     uint64
}

func (d swLagDelay) String() string {
	if d.model == "none" {
		return "none"
	}
	return fmt.Sprintf("%s:%d", d.model, d.j)
}

func swLagDelays() []swLagDelay {
	ds := []swLagDelay{{"none", 0}}
	for _, m := range []string{"jitter", "stall"} {
		for _, j := range []uint64{1, 10, 100, 1_000} {
			ds = append(ds, swLagDelay{m, j})
		}
	}
	return ds
}

// swLagOrder gives each event its arrival tick (its own plus a delay) and
// returns the arrivals and the order of delivery: by arrival; equal
// arrivals, with a delay, in an order drawn from the delay's stream (as
// arrival times finer than a tick would order them, so that a delay of 1
// can put an event behind the one a tick after it), and with none in the
// generator's order. The delays and the draws come from a stream of the
// seed and the delay alone, never the generator's.
func swLagOrder(rec *swLagRec, s1, s2 uint64, d swLagDelay) (arr []uint64, order []int32) {
	code := map[string]uint64{"none": 0, "jitter": 1, "stall": 2}[d.model]
	rng := rand.New(rand.NewPCG(s1<<32^s2, 0x12<<56|code<<48|d.j))
	arr = make([]uint64, len(rec.ev))
	order = make([]int32, len(rec.ev))
	tie := make([]uint64, len(rec.ev))
	for i, e := range rec.ev {
		var dl uint64
		switch d.model {
		case "jitter":
			dl = rng.Uint64N(d.j + 1)
		case "stall":
			if rng.Float64() < 0.01 {
				dl = rng.Uint64N(d.j + 1)
			}
		}
		arr[i], order[i] = e.t+dl, int32(i)
		if d.model != "none" {
			tie[i] = rng.Uint64()
		}
	}
	slices.SortStableFunc(order, func(a, b int32) int {
		if c := cmp.Compare(arr[a], arr[b]); c != 0 {
			return c
		}
		return cmp.Compare(tie[a], tie[b])
	})
	return arr, order
}

// swLagRun is one replay's sums.
type swLagRun struct {
	err, exact [5]float64
	q          [swLagClasses]int
	x, e       [swLagClasses]float64 // exact counts and errors, all windows, by class
	lagged     int                   // events delivered an earlier unit behind their key's newest
	ovl, ovlU  int                   // leg B, shared: (tick, key) pairs at two workers; of them, in two units
	answers    []int32
	del        [][]uint64 // each key's delivered ticks, sorted
	newest     []uint64
}

func newSwLagRun(rec *swLagRec) *swLagRun {
	return &swLagRun{answers: make([]int32, len(rec.qs)), del: make([][]uint64, 2000), newest: make([]uint64, 2000)}
}

func (r *swLagRun) deliver(e swLagEv) {
	ts := r.del[e.k]
	i := len(ts)
	for i > 0 && ts[i-1] > e.t {
		i--
	}
	r.del[e.k] = slices.Insert(ts, i, e.t)
	if e.t/swLagUnit < r.newest[e.k]/swLagUnit {
		r.lagged++
	}
	r.newest[e.k] = max(r.newest[e.k], e.t)
}

func (r *swLagRun) query(f *Filter, hs []uint64, q swLagQ, now uint64) {
	w := velWins[q.wi]
	ts := r.del[q.k]
	exact := float64(len(ts) - sort.Search(len(ts), func(j int) bool { return ts[j]+w >= now }))
	cls := f.swLagClass(hs[q.k], now)
	got := f.VelocityCountAt(hs[q.k], w, now)
	r.answers[now-swLagH] = int32(got)
	d := math.Abs(float64(got) - exact)
	r.err[q.wi] += d
	r.exact[q.wi] += exact
	r.q[cls]++
	r.x[cls] += exact
	r.e[cls] += d
}

func swLagKeys() []uint64 {
	hs := make([]uint64, 2000)
	for k := range hs {
		hs[k] = mix64(uint64(k) + 1<<50) // tfbench's and swRecord's keys
	}
	return hs
}

// swLagA is leg A: one goroutine, plain mode.
func swLagA(t testing.TB, rec *swLagRec, arr []uint64, order []int32) *swLagRun {
	f := swLagFilter(t, "plain")
	defer f.Close()
	hs, r, p := swLagKeys(), newSwLagRun(rec), 0
	for now := uint64(1); now < swLagEnd; now++ {
		for ; p < len(order) && arr[order[p]] <= now; p++ {
			e := rec.ev[order[p]]
			f.CheckAndTouchAt(hs[e.k], 0, e.t)
			r.deliver(e)
		}
		if now >= swLagH {
			r.query(f, hs, rec.qs[now-swLagH], now)
		}
	}
	return r
}

// swLagB is leg B: swLagN workers behind a gate per arrival tick, keys
// owned (key k to worker k%swLagN) or shared (the tick's events dealt in
// turn), in mode.
func swLagB(t testing.TB, rec *swLagRec, arr []uint64, order []int32, mode string, shared bool) *swLagRun {
	f := swLagFilter(t, mode)
	defer f.Close()
	hs, r := swLagKeys(), newSwLagRun(rec)
	var lists [swLagN][]int32
	var gen, done atomic.Uint64
	const stop = math.MaxUint64
	var wg sync.WaitGroup
	for w := range swLagN {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen := uint64(0)
			for {
				g := gen.Load()
				for spins := 0; g == seen; spins++ {
					if spins&1023 == 1023 {
						runtime.Gosched()
					}
					g = gen.Load()
				}
				if g == stop {
					return
				}
				seen = g
				for _, i := range lists[w] {
					e := rec.ev[i]
					f.CheckAndTouchAt(hs[e.k], 0, e.t)
				}
				done.Add(1)
			}
		}()
	}
	// Overlaps, per (tick, key): an event of the key's at another worker
	// than the one that took its first event of the tick (ovl); one also
	// in another unit than that first event's (ovlU).
	atW := make([]int32, 2000) // the worker that took key k's first event of the tick, +1
	twice, twiceU := make([]bool, 2000), make([]bool, 2000)
	unitOf := make([]uint64, 2000)
	gates, p := uint64(0), 0
	for now := uint64(1); now < swLagEnd; now++ {
		lo := p
		for p < len(order) && arr[order[p]] <= now {
			p++
		}
		if p > lo {
			for w := range lists {
				lists[w] = lists[w][:0]
			}
			for j, i := range order[lo:p] {
				e := rec.ev[i]
				w := int(e.k) % swLagN
				if shared {
					w = j % swLagN
				}
				lists[w] = append(lists[w], i)
				switch {
				case atW[e.k] == 0:
					atW[e.k], unitOf[e.k] = int32(w)+1, e.t/swLagUnit
				case atW[e.k] != int32(w)+1:
					if !twice[e.k] {
						twice[e.k] = true
						r.ovl++
					}
					if e.t/swLagUnit != unitOf[e.k] && !twiceU[e.k] {
						twiceU[e.k] = true
						r.ovlU++
					}
				}
			}
			gates++
			gen.Store(gates)
			for done.Load() != gates*swLagN {
				runtime.Gosched()
			}
			for _, i := range order[lo:p] {
				e := rec.ev[i]
				atW[e.k], twice[e.k], twiceU[e.k] = 0, false, false
				r.deliver(e)
			}
		}
		if now >= swLagH {
			r.query(f, hs, rec.qs[now-swLagH], now)
		}
	}
	gen.Store(stop)
	wg.Wait()
	return r
}

func swLagMismatch(a, b []int32) int {
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

// swLagLine is a run's line: key=value fields, which lab/w12.py reads.
func swLagLine(t testing.TB, build, leg string, sd [2]uint64, d swLagDelay, mode, keys string, rep int, r *swLagRun, mism int) {
	var b strings.Builder
	fmt.Fprintf(&b, "W12 run: build=%s leg=%s seed=%d,%d delay=%s mode=%s keys=%s rep=%d", build, leg, sd[0], sd[1], d, mode, keys, rep)
	mean := 0.0
	for wi, w := range []int{1, 5, 15, 30, 60} {
		v := r.err[wi] / r.exact[wi]
		mean += v / 5
		fmt.Fprintf(&b, " w%d=%.10f es%d=%.0f xs%d=%.0f", w, v, w, r.err[wi], w, r.exact[wi])
	}
	fmt.Fprintf(&b, " mean=%.10f", mean)
	for c := range swLagClasses {
		fmt.Fprintf(&b, " q_%s=%d x_%s=%.0f e_%s=%.0f", swLagClassName[c], r.q[c], swLagClassName[c], r.x[c], swLagClassName[c], r.e[c])
	}
	fmt.Fprintf(&b, " lagged=%d ovl=%d ovlu=%d mism=%d", r.lagged, r.ovl, r.ovlU, mism)
	t.Log(b.String())
}

func swLagEnvList(name, def string) []string {
	v := os.Getenv(name)
	if v == "" {
		v = def
	}
	return strings.Split(v, ";")
}

// TestShortWindowLag (TF_EXP=1) is W12: TF_W12_LEG=a (leg A, every seed of
// TF_W12_SEEDS, default tfbench's (9,9) and the fresh (2026,41)-(2026,44))
// or b (leg A and leg B on TF_W12_SEEDS, default (9,9), in the modes of
// TF_W12_MODES, default plain;bg;ct, the shared keys TF_W12_REPEATS times,
// default 3). TF_W12_DELAYS (default every delay: none;jitter:1;...)
// narrows the delays. It judges nothing by failing: lab/w12.py reads the
// lines of both builds. With no delay, leg A is compared with swRecord
// query for query (mism); leg B with leg A (mism; reported for the shared
// keys).
func TestShortWindowLag(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	leg := os.Getenv("TF_W12_LEG")
	if leg != "a" && leg != "b" {
		t.Fatalf("TF_W12_LEG=%q: a or b", leg)
	}
	defSeeds := "9,9;2026,41;2026,42;2026,43;2026,44"
	if leg == "b" {
		defSeeds = "9,9"
	}
	var seeds [][2]uint64
	for _, s := range swLagEnvList("TF_W12_SEEDS", defSeeds) {
		var a, b uint64
		if _, err := fmt.Sscanf(s, "%d,%d", &a, &b); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
		seeds = append(seeds, [2]uint64{a, b})
	}
	delays := swLagDelays()
	if v := os.Getenv("TF_W12_DELAYS"); v != "" {
		var ds []swLagDelay
		for _, s := range strings.Split(v, ";") {
			for _, d := range delays {
				if d.String() == s {
					ds = append(ds, d)
				}
			}
		}
		if len(ds) == 0 {
			t.Fatalf("TF_W12_DELAYS=%q names no delay", v)
		}
		delays = ds
	}
	modes := swLagEnvList("TF_W12_MODES", "plain;bg;ct")
	reps := 3
	if v := os.Getenv("TF_W12_REPEATS"); v != "" {
		reps, _ = strconv.Atoi(v)
	}
	build := "default"
	if dripOn {
		build = "tfdrip"
	}
	runs := 0
	for _, sd := range seeds {
		rec := swLagRecord(t, sd[0], sd[1])
		var base []int32
		for _, d := range delays {
			arr, order := swLagOrder(rec, sd[0], sd[1], d)
			a := swLagA(t, rec, arr, order)
			mism := -1
			if d.model == "none" {
				if base == nil {
					sr := swRecord(t, sd[0], sd[1])
					base = make([]int32, len(sr.qs))
					for i, q := range sr.qs {
						base[i] = int32(q.filter)
					}
				}
				mism = swLagMismatch(a.answers, base)
			}
			swLagLine(t, build, "A", sd, d, "plain", "-", 0, a, mism)
			runs++
			if leg != "b" {
				continue
			}
			for _, mode := range modes {
				o := swLagB(t, rec, arr, order, mode, false)
				swLagLine(t, build, "B", sd, d, mode, "owned", 0, o, swLagMismatch(o.answers, a.answers))
				runs++
				for rep := range reps {
					s := swLagB(t, rec, arr, order, mode, true)
					swLagLine(t, build, "B", sd, d, mode, "shared", rep, s, swLagMismatch(s.answers, a.answers))
					runs++
				}
			}
		}
	}
	t.Logf("W12 done: build=%s leg=%s runs=%d", build, leg, runs)
}
