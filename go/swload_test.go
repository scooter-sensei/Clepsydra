package temporalfilter

// lab/sw-2026-11, arm W14: target 7's metric while the table is full
// (SW-RESULTS.md, W14).

import (
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scooter-sensei/clepsydra/go/internal/workload"
)

// swLoadExp skips a W14 test unless TF_EXP is set.
func swLoadExp(t testing.TB) {
	t.Helper()
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
}

// swLoadChurnSeeds are tfbench's churn points W14 reads (cmd/tfbench/main.go:40).
var swLoadChurnSeeds = []struct {
	load float64
	seed uint64
}{{1.0, 2}, {1.15, 4}}

// swLoadOp is one operation of the churn stream: CheckAndTouchAt(h, w, now).
type swLoadOp struct{ h, w, now uint64 }

// swLoadChurn is workload.Churn's stream (internal/workload/churn.go), with
// tfbench's capacity (200,000), horizon (60,000 ticks) and hash (mix64),
// run over the given number of horizons instead of four. Its body is
// workload.Churn's but for the loop's bound; swLoadChurnPrefix checks it.
func swLoadChurn(load float64, seed uint64, horizons int) []swLoadOp {
	const capacity, hTicks = 200_000, 60_000
	r := rand.New(rand.NewPCG(seed, 1))
	const newFrac = 0.6
	ratePerMs := load * float64(capacity) / (newFrac * float64(hTicks)) / 1.375
	type born struct {
		h uint64
		t uint64
	}
	var keys []born
	var next uint64
	var ops []swLoadOp
	for now := uint64(1); now < uint64(horizons)*hTicks; now++ {
		n := int(ratePerMs)
		if r.Float64() < ratePerMs-float64(n) {
			n++
		}
		for range n {
			var h uint64
			if len(keys) == 0 || r.Float64() < newFrac {
				next++
				h = mix64(next)
				keys = append(keys, born{h, now})
			} else {
				lo := sort.Search(len(keys), func(i int) bool { return keys[i].t+hTicks*3/2 > now })
				h = keys[lo+r.IntN(len(keys)-lo)].h
			}
			ops = append(ops, swLoadOp{h, workload.Windows[r.IntN(len(workload.Windows))], now})
		}
		if now%hTicks == 0 {
			cut := 0
			for cut < len(keys) && keys[cut].t+2*hTicks < now {
				cut++
			}
			keys = keys[cut:]
		}
	}
	return ops
}

// swLoadChurnRes is the churn stream's check at one load: its ops, the ops
// of workload.Churn compared (prefix) against the ops before four horizons
// (four), how many differ, how many churn keys are velocity keys (vel), and
// whether its last op is at or past four horizons.
type swLoadChurnRes struct {
	load                         float64
	seed                         uint64
	ops, prefix, four, mism, vel int
	lastOK                       bool
}

func (c swLoadChurnRes) ok() bool {
	return c.mism == 0 && c.prefix == c.four && c.prefix > 0 && c.vel == 0 && c.lastOK
}

func (c swLoadChurnRes) String() string {
	return fmt.Sprintf("load=%.2f seed=%d ops=%d prefix=%d four=%d mism=%d velkeys=%d lastok=%v",
		c.load, c.seed, c.ops, c.prefix, c.four, c.mism, c.vel, c.lastOK)
}

// swLoadChurnCheck checks ops, swLoadChurn's stream at load and seed: its
// first four horizons are workload.Churn's op for op, and no churn key is a
// velocity key.
func swLoadChurnCheck(load float64, seed uint64, ops []swLoadOp) swLoadChurnRes {
	r := swLoadChurnRes{load: load, seed: seed, ops: len(ops)}
	r.prefix, r.mism = swLoadChurnPrefix(load, seed, ops)
	vel := map[uint64]bool{}
	for _, h := range swLagKeys() {
		vel[h] = true
	}
	for _, op := range ops {
		if op.now < 4*swLagH {
			r.four++
		}
		if vel[op.h] {
			r.vel++
		}
	}
	r.lastOK = len(ops) > 0 && ops[len(ops)-1].now >= 4*swLagH
	return r
}

// swLoadChurnPrefix compares ops with workload.Churn's own stream at load
// and seed: n ops compared (all of workload.Churn's), mism of them differ
// (an op past the end of ops counts as differing).
func swLoadChurnPrefix(load float64, seed uint64, ops []swLoadOp) (n, mism int) {
	workload.Churn(load, seed, 200_000, 60_000, mix64, func(h, w, now uint64) {
		if n >= len(ops) || ops[n] != (swLoadOp{h, w, now}) {
			mism++
		}
		n++
	})
	return n, mism
}

// Where the watcher finds a key (swLoadSeen.where).
const (
	swLoadTable = iota // in a slot of the table
	swLoadStash        // evicted, in the stash
	swLoadWait         // waiting for a slot: the wait room (Background) or the stash (ConstantTime)
	swLoadNone         // in none of them, or past the horizon
	swLoadWheres
)

var swLoadWhereName = [swLoadWheres]string{"table", "stash", "wait", "none"}

// The found history's drip log (swLoadSeen.log).
const (
	swLoadNoLog = iota // the default build, or nothing found
	swLoadValid
	swLoadDead // says nothing (valid bit clear)
	swLoadLogs
)

var swLoadLogName = [swLoadLogs]string{"nolog", "valid", "dead"}

// swLoadSeen is what the watcher saw of a key: where its history is, its
// slot (table only, else -1), its log, and what the overflow table holds of
// it: an entry of its own, whose count Count adds (and sets Degraded: part),
// or, with none, a raised floor that adds to its Upper alone (floor: the
// key's own history may be in it, or only keys sharing the floor's).
type swLoadSeen struct {
	where, slot, log int
	part, floor      bool
}

// swLoadPartIdx is s's overflow class: 0 none, 1 an entry, 2 a floor's alone.
func swLoadPartIdx(s swLoadSeen) int {
	switch {
	case s.part:
		return 1
	case s.floor:
		return 2
	}
	return 0
}

var swLoadPartName = [3]string{"nopart", "part", "floor"}

// swLoadWhere is W14's watcher. It finds h at now as read's locked branch
// does (touch.go: under h's claim stripe, the table read stably, then the
// stash and the wait room, again until no move of h's stripe overlapped a
// miss), but names the place: a stash entry marked deferMark waits for a
// slot (ConstantTime), one without was evicted; a wait-room entry waits
// (Background). It changes nothing: no Count (whose ConstantTime pads place
// waiting keys), no touch, no take.
func (f *Filter) swLoadWhere(h, now uint64) swLoadSeen {
	fp, tg := fingerprint(h), tag(h)
	id := f.stashID(h, fp, tg)
	s := swLoadSeen{where: swLoadNone, slot: -1}
	var w0 uint64
	mu := &f.claimMu[h>>(64-claimStripeBits)]
	mu.Lock()
	m := f.moveOf(tg)
	for {
		b, d := m.begun.Load(), m.done.Load()
		if i, x0, _ := f.lookupStable(h, fp, tg, true); i >= 0 {
			s.where, s.slot, w0 = swLoadTable, i, x0
			break
		}
		if x0, _, ok := f.stashGet(id, now, false); ok {
			s.where, w0 = swLoadStash, x0
			if x0&deferMark != 0 {
				s.where = swLoadWait
			}
			break
		}
		if p := f.room.Load(); p != nil && p.waiting(id) {
			x0, _, ok, retry := f.roomGet(p, id, now, nil)
			if retry {
				continue
			}
			if ok {
				s.where, w0 = swLoadWait, x0
				break
			}
		}
		if f.settled(m, b, d) {
			break
		}
	}
	mu.Unlock()
	if s.where != swLoadNone && age(now, w0&tsMask) > f.horizon {
		s.where, s.slot = swLoadNone, -1
	}
	if s.where != swLoadNone && dripOn {
		s.log = swLoadValid
		if dripOf(w0)&dripV == 0 {
			s.log = swLoadDead
		}
	}
	var r Result
	if !f.cms {
		if g := f.gh.Load(); g != nil {
			r, _ = g.estimate(id, f.horizon, now)
		}
	} else if sk := f.sk.Load(); sk != nil {
		r, _ = sk.estimate(id, f.horizon, now, f.ct)
	}
	s.part = r.Count > 0 // as addSketch's Degraded (touch.go)
	s.floor = !s.part && r.Upper > 0
	return s
}

// Where a touch restores its key from (swLoadSource).
const (
	swLoadHit       = iota // the table: no move
	swLoadFromStash        // the stash: the history and its log kept
	swLoadFromWait         // the wait room or ConstantTime's stash: a key that waited
	swLoadDropped          // nowhere but an entry of its own in the overflow table: a fresh slot, the log lost
	swLoadFloor            // nowhere but a raised floor of the overflow table (Upper alone)
	swLoadShed             // nowhere, its newest event within the horizon: shed or lost
	swLoadNew              // nowhere, no event within the horizon: a new life, no loss
	swLoadSrcs
)

var swLoadSrcName = [swLoadSrcs]string{"hit", "stash", "wait", "dropped", "floor", "shed", "new"}

// swLoadSource names where a touch at now restores its key from, given what
// the watcher saw just before it (s), the key's newest event before it, and
// whether it had any.
func swLoadSource(s swLoadSeen, newest, now uint64, any bool) int {
	switch {
	case s.where == swLoadTable:
		return swLoadHit
	case s.where == swLoadStash:
		return swLoadFromStash
	case s.where == swLoadWait:
		return swLoadFromWait
	case s.part:
		return swLoadDropped
	case s.floor:
		return swLoadFloor
	case any && now-newest <= swLagH:
		return swLoadShed
	}
	return swLoadNew
}

// swLoadCfg is one W14 run: leg 1 (tfbench's filter beside the churn stream
// at load, 0 for none) or leg 2 (the workload alone at capacity, the hot
// entries pinned to tfbench's 1,838), in mode (plain, bg or ct), with the
// watcher or without.
type swLoadCfg struct {
	leg      int
	load     float64
	capacity int
	mode     string
	watch    bool
}

// swLoadSums is one run's sums: per window, the error and exact sums; with
// the watcher, the queries, exact counts and errors by class (where, part,
// log), the touches by source, and the keys seen leave the table (left) or
// move within it (moved); the filter's counters; the answers; the time.
type swLoadSums struct {
	err, exact [5]float64
	q          [swLoadWheres][3][swLoadLogs]int
	x, e       [swLoadWheres][3][swLoadLogs]float64
	src        [swLoadSrcs]int
	left       int
	moved      int
	answers    []int32
	ctr        map[string]uint64
	secs       float64
}

// swLoadFilter is a run's filter (see swLoadCfg).
func swLoadFilter(t testing.TB, c swLoadCfg) *Filter {
	cfg := Config{Capacity: c.capacity, Horizon: time.Minute, Resolution: time.Millisecond, Background: c.mode == "bg"}
	if c.leg == 2 {
		cfg.Hot = 1838 // tfbench's filter's default (Capacity 200,000)
	}
	f, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.mode == "ct" {
		f.ct = true // functional, no pad, as TF_CT=1 runs it
	}
	return f
}

// swLoadRun replays rec (one goroutine): at each tick, its velocity events
// (each looked at by the watcher first), then the churn's operations of the
// tick, then from the first horizon on the tick's query (looked at first),
// whose exact count is over the key's own events.
func swLoadRun(t testing.TB, rec *swLagRec, churn []swLoadOp, c swLoadCfg) *swLoadSums {
	f := swLoadFilter(t, c)
	defer f.Close()
	hs := swLagKeys()
	r := &swLoadSums{answers: make([]int32, len(rec.qs))}
	times := make([][]uint64, len(hs))
	lastW, lastS := make([]int, len(hs)), make([]int, len(hs))
	for k := range lastW {
		lastW[k] = -1
	}
	// see notes key k's place at now: it left the table if it was there and
	// now is not while its newest event is within the horizon; it moved if
	// it is at another slot.
	see := func(k int, s swLoadSeen, now uint64) {
		ts := times[k]
		live := len(ts) > 0 && now-ts[len(ts)-1] <= swLagH
		if lastW[k] == swLoadTable {
			switch {
			case s.where != swLoadTable && live:
				r.left++
			case s.where == swLoadTable && s.slot != lastS[k]:
				r.moved++
			}
		}
		lastW[k], lastS[k] = s.where, s.slot
	}
	start := time.Now()
	p, cp := 0, 0
	for now := uint64(1); now < swLagEnd; now++ {
		for ; p < len(rec.ev) && rec.ev[p].t <= now; p++ {
			e := rec.ev[p]
			if c.watch {
				s := f.swLoadWhere(hs[e.k], e.t)
				ts := times[e.k]
				var newest uint64
				if len(ts) > 0 {
					newest = ts[len(ts)-1]
				}
				r.src[swLoadSource(s, newest, e.t, len(ts) > 0)]++
				see(int(e.k), s, e.t)
			}
			f.CheckAndTouchAt(hs[e.k], 0, e.t)
			times[e.k] = append(times[e.k], e.t)
		}
		for ; cp < len(churn) && churn[cp].now <= now; cp++ {
			op := churn[cp]
			f.CheckAndTouchAt(op.h, op.w, op.now)
		}
		if now < swLagH {
			continue
		}
		q := rec.qs[now-swLagH]
		w := velWins[q.wi]
		ts := times[q.k]
		exact := float64(len(ts) - sort.Search(len(ts), func(j int) bool { return ts[j]+w >= now }))
		var s swLoadSeen
		if c.watch {
			s = f.swLoadWhere(hs[q.k], now)
			see(int(q.k), s, now)
		}
		got := f.VelocityCountAt(hs[q.k], w, now)
		r.answers[now-swLagH] = int32(got)
		d := math.Abs(float64(got) - exact)
		r.err[q.wi] += d
		r.exact[q.wi] += exact
		if c.watch {
			pi := swLoadPartIdx(s)
			r.q[s.where][pi][s.log]++
			r.x[s.where][pi][s.log] += exact
			r.e[s.where][pi][s.log] += d
		}
	}
	r.secs = time.Since(start).Seconds()
	r.ctr = map[string]uint64{
		"evictions": f.Evictions(), "relocations": f.Relocations(), "shed": f.Shed(), "lost": f.Lost(),
		"floored": f.Floored(), "folded": f.folded.Load(), "waited": f.Waited(), "promoted": f.Promoted(),
		"hotrefused": f.HotRefused(),
	}
	return r
}

// swLoadLine is a run's line: key=value fields, which lab/w14.py reads.
// Quiet (the checks before the rule) prints the run's identity, its
// mismatch count and its time alone.
func swLoadLine(t testing.TB, build string, sd [2]uint64, c swLoadCfg, rep int, r *swLoadSums, mism int, quiet bool) {
	var b strings.Builder
	load := "none"
	if c.load > 0 {
		load = fmt.Sprintf("%.2f", c.load)
	}
	fmt.Fprintf(&b, "W14 run: build=%s leg=%d seed=%d,%d load=%s cap=%d mode=%s rep=%d watch=%v mism=%d secs=%.1f",
		build, c.leg, sd[0], sd[1], load, c.capacity, c.mode, rep, c.watch, mism, r.secs)
	if !quiet {
		mean := 0.0
		for wi, w := range []int{1, 5, 15, 30, 60} {
			v := r.err[wi] / r.exact[wi]
			mean += v / 5
			fmt.Fprintf(&b, " w%d=%.10f es%d=%.0f xs%d=%.0f", w, v, w, r.err[wi], w, r.exact[wi])
		}
		fmt.Fprintf(&b, " mean=%.10f", mean)
		for wh := range swLoadWheres {
			for pi, pn := range swLoadPartName {
				for lg := range swLoadLogs {
					if n := r.q[wh][pi][lg]; n > 0 {
						cls := swLoadWhereName[wh] + "_" + pn + "_" + swLoadLogName[lg]
						fmt.Fprintf(&b, " q_%s=%d x_%s=%.0f e_%s=%.0f", cls, n, cls, r.x[wh][pi][lg], cls, r.e[wh][pi][lg])
					}
				}
			}
		}
		for s := range swLoadSrcs {
			fmt.Fprintf(&b, " src_%s=%d", swLoadSrcName[s], r.src[s])
		}
		fmt.Fprintf(&b, " left=%d moved=%d", r.left, r.moved)
		for _, k := range slices.Sorted(maps.Keys(r.ctr)) {
			fmt.Fprintf(&b, " %s=%d", k, r.ctr[k])
		}
	}
	t.Log(b.String())
}

// swLoadSeedList parses TF_W14_SEEDS (or def): "a,b;c,d".
func swLoadSeedList(t testing.TB, def string) [][2]uint64 {
	var seeds [][2]uint64
	for _, s := range swLagEnvList("TF_W14_SEEDS", def) {
		var a, b uint64
		if _, err := fmt.Sscanf(s, "%d,%d", &a, &b); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
		seeds = append(seeds, [2]uint64{a, b})
	}
	return seeds
}

// swLoadChurnFor is the churn stream at load (W14's seeds), over the
// workload's five horizons; none (0) has none.
func swLoadChurnFor(t testing.TB, cache map[float64][]swLoadOp, load float64) []swLoadOp {
	if load == 0 {
		return nil
	}
	if ops, ok := cache[load]; ok {
		return ops
	}
	for _, c := range swLoadChurnSeeds {
		if math.Abs(c.load-load) < 1e-9 {
			cache[load] = swLoadChurn(c.load, c.seed, 5)
			return cache[load]
		}
	}
	t.Fatalf("load %.2f: not one of W14's churn points", load)
	return nil
}

// TestShortWindowLoad (TF_EXP=1) is W14. TF_W14_LEG=1: tfbench's filter
// beside the churn stream at each load of TF_W14_LOADS (default
// none;1.00;1.15); 2: the workload alone at each Capacity of TF_W14_CAPS
// (default 1739;1000;500), the hot entries pinned to 1,838; both on every
// seed of TF_W14_SEEDS (default (9,9) and (2026,41)-(2026,44)), in every
// mode of TF_W14_MODES (default plain;bg;ct), Background TF_W14_REPEATS
// times (default 5), with the watcher. Leg 1's runs with no churn in plain
// mode are compared with swRecord query for query (mism). check: on
// TF_W14_SEEDS (default (9,9)), the churn's prefix at both loads, and at
// leg 1's 1.15 and leg 2's Capacity 500, in plain and ConstantTime, a run
// with the watcher against one without (offmism) and, in ConstantTime, a
// second watched run (replaymism); it prints no metric. TF_W14_QUIET
// prints each run's identity, mismatch count and time alone. It judges
// nothing by failing but the check: lab/w14.py reads both builds' lines.
func TestShortWindowLoad(t *testing.T) {
	swLoadExp(t)
	leg := os.Getenv("TF_W14_LEG")
	quiet := os.Getenv("TF_W14_QUIET") != ""
	build := "default"
	if dripOn {
		build = "tfdrip"
	}
	modes := swLagEnvList("TF_W14_MODES", "plain;bg;ct")
	reps := 5
	if v := os.Getenv("TF_W14_REPEATS"); v != "" {
		reps, _ = strconv.Atoi(v)
	}
	cache := map[float64][]swLoadOp{}
	runs := 0
	// legs runs every point of one leg (points: its loads or Capacities).
	legs := func(n int, points []swLoadCfg) {
		for _, sd := range swLoadSeedList(t, "9,9;2026,41;2026,42;2026,43;2026,44") {
			rec := swLagRecord(t, sd[0], sd[1])
			var base []int32
			for _, pt := range points {
				churn := swLoadChurnFor(t, cache, pt.load)
				for _, mode := range modes {
					nr := 1
					if mode == "bg" {
						nr = reps
					}
					for rep := range nr {
						c := swLoadCfg{leg: n, load: pt.load, capacity: pt.capacity, mode: mode, watch: true}
						r := swLoadRun(t, rec, churn, c)
						mism := -1
						if n == 1 && pt.load == 0 && mode == "plain" {
							if base == nil {
								sr := swRecord(t, sd[0], sd[1])
								base = make([]int32, len(sr.qs))
								for i, q := range sr.qs {
									base[i] = int32(q.filter)
								}
							}
							mism = swLagMismatch(r.answers, base)
						}
						swLoadLine(t, build, sd, c, rep, r, mism, quiet)
						runs++
					}
				}
			}
		}
	}
	switch leg {
	case "1":
		var pts []swLoadCfg
		for _, s := range swLagEnvList("TF_W14_LOADS", "none;1.00;1.15") {
			load := 0.0
			if s != "none" {
				var err error
				if load, err = strconv.ParseFloat(s, 64); err != nil {
					t.Fatalf("load %q: %v", s, err)
				}
			}
			pts = append(pts, swLoadCfg{load: load, capacity: 200_000})
		}
		legs(1, pts)
	case "2":
		var pts []swLoadCfg
		for _, s := range swLagEnvList("TF_W14_CAPS", "1739;1000;500") {
			c, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("capacity %q: %v", s, err)
			}
			pts = append(pts, swLoadCfg{capacity: c})
		}
		legs(2, pts)
	case "check":
		for _, c := range swLoadChurnSeeds {
			r := swLoadChurnCheck(c.load, c.seed, swLoadChurnFor(t, cache, c.load))
			t.Logf("W14 check churn: build=%s %s", build, r)
			if !r.ok() {
				t.Errorf("load %.2f: %s", c.load, r)
			}
		}
		for _, sd := range swLoadSeedList(t, "9,9") {
			rec := swLagRecord(t, sd[0], sd[1])
			for _, pt := range []swLoadCfg{{leg: 1, load: 1.15, capacity: 200_000}, {leg: 2, capacity: 500}} {
				churn := swLoadChurnFor(t, cache, pt.load)
				for _, mode := range []string{"plain", "ct"} {
					c := pt
					c.mode, c.watch = mode, true
					on := swLoadRun(t, rec, churn, c)
					c.watch = false
					off := swLoadRun(t, rec, churn, c)
					runs += 2
					replay, secs2 := -1, 0.0
					if mode == "ct" {
						c.watch = true
						again := swLoadRun(t, rec, churn, c)
						replay, secs2 = swLagMismatch(on.answers, again.answers), again.secs
						runs++
					}
					offm := swLagMismatch(on.answers, off.answers)
					t.Logf("W14 check: build=%s seed=%d,%d leg=%d cap=%d load=%.2f mode=%s offmism=%d replaymism=%d secs_on=%.1f secs_off=%.1f secs_again=%.1f",
						build, sd[0], sd[1], c.leg, c.capacity, c.load, mode, offm, replay, on.secs, off.secs, secs2)
					if offm != 0 || replay > 0 {
						t.Errorf("leg %d %s: %d answers differ without the watcher, %d on a second run", c.leg, mode, offm, replay)
					}
				}
			}
		}
	default:
		t.Fatalf("TF_W14_LEG=%q: 1, 2 or check", leg)
	}
	t.Logf("W14 done: build=%s leg=%s runs=%d", build, leg, runs)
}

// swLoadSightFilter is TestShortWindowLoadWhere's filter: Capacity 1,000,
// as hotLegSetup's (W11), with cfg's stash and Background, and ct for
// ConstantTime (functional, no pad).
func swLoadSightFilter(t *testing.T, cfg Config, ct bool) *Filter {
	t.Helper()
	cfg.Capacity, cfg.Horizon, cfg.Resolution = 1000, time.Minute, time.Millisecond
	cfg.Clock = func() uint64 { return 1 }
	f, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.ct = ct
	f.bgManual = cfg.Background // nothing places a waiting key: it stays in the room
	return f
}

// swLoadFill fills every line of f to 4 live slots with fresh keys, each
// touched n times at now (a register of n events), placing each only in
// its primary line while that line has room, so no key is evicted.
func swLoadFill(t *testing.T, f *Filter, n int, now uint64) {
	t.Helper()
	need := map[uint64]bool{}
	for line := range f.lines {
		if countLine(f, line, now) < 4 {
			need[line] = true
		}
	}
	for j := uint64(1 << 40); len(need) > 0; j++ {
		h := mix64(j)
		if line := f.primary(h); need[line] {
			for range n {
				f.TouchAt(h, 0, now)
			}
			if countLine(f, line, now) == 4 {
				delete(need, line)
			}
		}
	}
	if f.Evictions() != 0 || f.Waiting() != 0 {
		t.Fatalf("the fill evicted %d keys or left %d waiting", f.Evictions(), f.Waiting())
	}
}

// swLoadNewcomer is a fresh key whose primary line is h's.
func swLoadNewcomer(f *Filter, h uint64) uint64 {
	for j := uint64(1 << 44); ; j++ {
		if n := mix64(j); f.primary(n) == f.primary(h) && n != h {
			return n
		}
	}
}

// TestShortWindowLoadWhere (TF_EXP=1) is W14's sight of its watcher
// (swLoadWhere): hand-made states, each made through the filter's own
// paths and checked against a sign the watcher does not read. A key in the
// table; one evicted to the stash (Evictions); one whose eviction drops it
// to the overflow table (no stash: Count Degraded) and one whose eviction
// sheds it (Shed); one waiting in the wait room (Background, nothing
// placing) and one in ConstantTime's stash (Waiting); one never touched;
// one a horizon and a tick past its touch; with tfdrip, one whose log says
// nothing (W12's state b).
func TestShortWindowLoadWhere(t *testing.T) {
	swLoadExp(t)
	const t0 = 60_000
	v := mix64(1<<52 | 1)
	live := swLoadValid
	if !dripOn {
		live = swLoadNoLog
	}
	// want reads h with the watcher, then its sign (which may touch h: it
	// runs after the watcher's look).
	want := func(state string, f *Filter, h, now uint64, where, log int, part bool, sign string, signFn func() bool) {
		t.Helper()
		s := f.swLoadWhere(h, now)
		signOK := signFn == nil || signFn()
		t.Logf("W14 where %s: where=%s slot=%d part=%v log=%s sign=%q signok=%v", state, swLoadWhereName[s.where], s.slot, s.part, swLoadLogName[s.log], sign, signOK)
		if s.where != where || s.log != log || s.part != part || (where == swLoadTable) != (s.slot >= 0) || !signOK {
			t.Errorf("%s: where %s slot %d part %v log %s (sign %s %v), want where %s part %v log %s",
				state, swLoadWhereName[s.where], s.slot, s.part, swLoadLogName[s.log], sign, signOK, swLoadWhereName[where], part, swLoadLogName[log])
		}
	}

	f := swLoadSightFilter(t, Config{}, false)
	f.TouchAt(v, 0, t0)
	want("table", f, v, t0, swLoadTable, live, false, "", nil)
	want("past", f, v, t0+60_001, swLoadNone, swLoadNoLog, false, "", nil)
	want("never", f, mix64(1<<52|2), t0, swLoadNone, swLoadNoLog, false, "", nil)
	b := mix64(1<<52 | 3)
	f.TouchAt(b, 0, t0)
	f.TouchAt(b, 0, t0-2_600) // an earlier unit behind its last touch
	dead := swLoadDead
	if !dripOn {
		dead = swLoadNoLog
	}
	want("dead", f, b, t0, swLoadTable, dead, false, "", nil)
	f.Close()

	evict := func(stash, vTouches, fill int) *Filter {
		f := swLoadSightFilter(t, Config{Stash: stash}, false)
		for range vTouches {
			f.TouchAt(v, 0, t0)
		}
		swLoadFill(t, f, fill, t0)
		f.TouchAt(swLoadNewcomer(f, v), 0, t0)
		return f
	}
	f = evict(0, 1, 2)
	want("stash", f, v, t0, swLoadStash, live, false, "Evictions 1, then V's touch restores it", func() bool {
		return f.Evictions() == 1 && f.TouchAt(v, 0, t0).Restored
	})
	f.Close()
	f = evict(-1, 4, 5)
	want("dropped", f, v, t0, swLoadNone, swLoadNoLog, true, "Evictions 1, Folded 1, Count Degraded with its 4", func() bool {
		r := f.CountAt(v, 60_000, t0)
		return f.Evictions() == 1 && f.Folded() == 1 && r.Degraded && r.Count >= 4
	})
	f.Close()
	f = evict(-1, 1, 2)
	want("shed", f, v, t0, swLoadNone, swLoadNoLog, false, "Evictions 1, Shed 1, then V's touch finds no history", func() bool {
		return f.Evictions() == 1 && f.Shed() == 1 && !f.CheckAndTouchAt(v, 60_000, t0)
	})
	f.Close()

	for _, m := range []struct {
		name string
		cfg  Config
		ct   bool
	}{{"wait room", Config{Background: true}, false}, {"CT wait", Config{}, true}} {
		f := swLoadSightFilter(t, m.cfg, m.ct)
		swLoadFill(t, f, 1, t0)
		w := mix64(1<<52 | 4)
		f.TouchAt(w, 0, t0)
		want(m.name, f, w, t0, swLoadWait, live, false, "Waiting 1", func() bool { return f.Waiting() == 1 })
		f.Close()
	}
}

// TestShortWindowLoadSource (TF_EXP=1): swLoadSource names where a touch
// restores its key from, given what the watcher saw before it.
func TestShortWindowLoadSource(t *testing.T) {
	swLoadExp(t)
	const now = 200_000
	for _, c := range []struct {
		name   string
		s      swLoadSeen
		newest uint64
		any    bool
		want   int
	}{
		{"table", swLoadSeen{where: swLoadTable, slot: 8}, now - 5, true, swLoadHit},
		{"stash", swLoadSeen{where: swLoadStash, slot: -1}, now - 5, true, swLoadFromStash},
		{"wait", swLoadSeen{where: swLoadWait, slot: -1}, now - 5, true, swLoadFromWait},
		{"none with a part", swLoadSeen{where: swLoadNone, slot: -1, part: true}, now - 5, true, swLoadDropped},
		{"none with a floor's alone", swLoadSeen{where: swLoadNone, slot: -1, floor: true}, now - 5, true, swLoadFloor},
		{"stash with a floor's", swLoadSeen{where: swLoadStash, slot: -1, floor: true}, now - 5, true, swLoadFromStash},
		{"none, newest within the horizon", swLoadSeen{where: swLoadNone, slot: -1}, now - 60_000, true, swLoadShed},
		{"none, newest past the horizon", swLoadSeen{where: swLoadNone, slot: -1}, now - 60_001, true, swLoadNew},
		{"no events yet", swLoadSeen{where: swLoadNone, slot: -1}, 0, false, swLoadNew},
	} {
		if got := swLoadSource(c.s, c.newest, now, c.any); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, swLoadSrcName[got], swLoadSrcName[c.want])
		}
	}
}

// TestShortWindowLoadChurn (TF_EXP=1) is W14's check of its churn stream:
// over five horizons, its first four are workload.Churn's op for op, and
// no churn key is a velocity key.
func TestShortWindowLoadChurn(t *testing.T) {
	swLoadExp(t)
	for _, c := range swLoadChurnSeeds {
		r := swLoadChurnCheck(c.load, c.seed, swLoadChurn(c.load, c.seed, 5))
		t.Logf("W14 check churn: %s", r)
		if !r.ok() {
			t.Errorf("load %.2f: %s", c.load, r)
		}
	}
}
