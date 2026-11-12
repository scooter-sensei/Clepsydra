package temporalfilter

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/scooter-sensei/clepsydra/go/internal/workload"
)

// Buzz entries (buzz.go, claims A46): a conditional touch refused in doubt
// gives its key an exact log of admitted events.

type buzzRule struct{ w, limit uint64 }

func (ru buzzRule) String() string { return fmt.Sprintf("%gs/%d", float64(ru.w)/1000, ru.limit) }

// buzzRow is one reading: the same requests offered to an exact sliding
// log, to TouchIfUnderAt without buzz entries (base) and with them.
type buzzRow struct {
	events                 int
	admitted               [3]int // exact, base, buzz
	falseDeny              [2]int // base, buzz: refused while the key's own events in the window are under the limit
	given, dropped         uint64
	decided                uint64
	evictions, relocations uint64
	bytesBase, bytesBuzz   int
}

// ownWithin counts the events of ts (sorted) inside [now-w, now].
func ownWithin(ts []uint64, w, now uint64) int {
	return len(ts) - sort.Search(len(ts), func(j int) bool { return ts[j]+w >= now })
}

// buzzOffer offers nkeys keys' Poisson requests, at mult times the rule's
// rate, to the three limiters for the given horizons of 60 s (1 ms ticks),
// in filters of Capacity capN. One request in 1/plainFrac is a plain
// TouchAt in every arm (it counts, and no rule is asked). With churnLoad
// both filters also take tfbench's churn stream at that load (its 4
// horizons). It fails the test if an arm admits a request while the key's
// window already holds the limit.
func buzzOffer(t *testing.T, capN, nkeys int, ru buzzRule, mult float64, horizons int, plainFrac, churnLoad float64, churnSeed uint64) (row buzzRow) {
	const hT = 60_000
	defer func(b bool) { buzzOff = b }(buzzOff)
	newF := func() *Filter {
		f, err := New(Config{Capacity: capN, Horizon: time.Minute, Resolution: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	fb, fz := newF(), newF()
	r := rand.New(rand.NewPCG(4, ru.w+ru.limit))
	type ev struct {
		t     uint64
		k     int
		plain bool
	}
	var evs []ev
	rate := mult * float64(ru.limit) / float64(ru.w)
	end := float64(horizons * hT)
	for k := range nkeys {
		for x := r.ExpFloat64() / rate; x < end; x += r.ExpFloat64() / rate {
			evs = append(evs, ev{uint64(x) + 1, k, plainFrac > 0 && r.Float64() < plainFrac})
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].t < evs[j].t })
	var own [3][][]uint64 // per arm, per key: the ticks of its events (admitted, or plain)
	for a := range own {
		own[a] = make([][]uint64, nkeys)
	}
	limit := int(ru.limit)
	offer := func(e ev) {
		h := mix64(uint64(e.k) + 1<<50)
		if e.plain {
			for a := range own {
				own[a][e.k] = append(own[a][e.k], e.t)
			}
			fb.TouchAt(h, ru.w, e.t)
			fz.TouchAt(h, ru.w, e.t)
			return
		}
		row.events++
		if ownWithin(own[0][e.k], ru.w, e.t) < limit {
			own[0][e.k] = append(own[0][e.k], e.t)
			row.admitted[0]++
		}
		for a, f := range [2]*Filter{fb, fz} {
			buzzOff = a == 0
			in := ownWithin(own[a+1][e.k], ru.w, e.t)
			if _, ok := f.TouchIfUnderAt(h, ru.w, ru.limit, e.t); ok {
				if in >= limit {
					t.Fatalf("%v x%.1f arm %d: admitted with %d events in the window (key %d, tick %d)", ru, mult, a, in, e.k, e.t)
				}
				own[a+1][e.k] = append(own[a+1][e.k], e.t)
				row.admitted[a+1]++
			} else if in < limit {
				row.falseDeny[a]++
			}
		}
		buzzOff = false
	}
	if churnLoad > 0 {
		i := 0
		workload.Churn(churnLoad, churnSeed, capN, hT, mix64, func(h, w, now uint64) {
			for ; i < len(evs) && evs[i].t <= now; i++ {
				offer(evs[i])
			}
			fb.CheckAndTouchAt(h, w, now)
			fz.CheckAndTouchAt(h, w, now)
		})
		for ; i < len(evs); i++ {
			offer(evs[i])
		}
	} else {
		for _, e := range evs {
			offer(e)
		}
	}
	if fb.buzz.Load() != nil {
		t.Fatalf("the base arm's filter holds a buzz table")
	}
	if tb := fz.buzz.Load(); tb != nil {
		row.given, row.dropped, row.decided = tb.given.Load(), tb.dropped.Load(), tb.decided.Load()
	}
	row.evictions, row.relocations = fz.Evictions(), fz.Relocations()
	row.bytesBase, row.bytesBuzz = fb.Bytes(), fz.Bytes()
	return row
}

func (row buzzRow) share(a int) float64 { return float64(row.admitted[a]) / float64(row.admitted[0]) }
func (row buzzRow) denied(a int) float64 {
	return float64(row.falseDeny[a]) / float64(row.events)
}

func (row buzzRow) String() string {
	return fmt.Sprintf("requests %7d exact admits %7d | admitted/exact base %.3f buzz %.3f | false denials base %.4f buzz %.4f | entries given %5d dropped %6d | decided by a log %7d",
		row.events, row.admitted[0], row.share(1), row.share(2), row.denied(0), row.denied(1), row.given, row.dropped, row.decided)
}

// TestBuzzLimiter: 100 contested keys in a filter of Capacity 200,000, for 20
// horizons (4 for the 1 s rule; a log answers for a window only once the
// window starts after the log does, so a 60 s rule needs the length). Offered
// 2x a rule a log decides, the buzz admits at least 0.97 of what an exact
// sliding log admits; offered 0.8x, its false denials are at most a quarter
// of the register's; no arm admits past a limit (buzzOffer asserts it at
// every admission). A limit past the log (20) is decided as without entries.
func TestBuzzLimiter(t *testing.T) {
	skipBuzzCT(t)
	for _, ru := range []buzzRule{{1_000, 5}, {10_000, 5}, {60_000, 6}, {60_000, 16}} {
		horizons := 20
		if ru.w == 1_000 {
			horizons = 4
		}
		hi := buzzOffer(t, 200_000, 100, ru, 2, horizons, 0, 0, 0)
		lo := buzzOffer(t, 200_000, 100, ru, 0.8, horizons, 0, 0, 0)
		t.Logf("%-7v x2.0 | %v", ru, hi)
		t.Logf("%-7v x0.8 | %v", ru, lo)
		if hi.share(2) < 0.97 {
			t.Errorf("%v offered 2x: the buzz admits %.3f of the exact log (0.97 wanted)", ru, hi.share(2))
		}
		if lo.denied(1) > lo.denied(0)/4 {
			t.Errorf("%v offered 0.8x: false denials %.4f, the register's %.4f (a quarter wanted)", ru, lo.denied(1), lo.denied(0))
		}
	}
	past := buzzOffer(t, 200_000, 100, buzzRule{2_500, 20}, 2, 2, 0, 0, 0)
	t.Logf("2.5s/20 x2.0 | %v", past)
	if past.admitted[1] != past.admitted[2] || past.given != 0 {
		t.Errorf("a limit of 20 was decided by a log: admitted %d, without entries %d, entries given %d", past.admitted[2], past.admitted[1], past.given)
	}
}

// TestBuzzMemory: nothing is allocated before an uncertain refusal. 10,000
// keys asked 4 times each at one tick under a limit of 3: a refusal there is
// certain (the register is exact at one tick).
func TestBuzzMemory(t *testing.T) {
	f, _ := New(Config{Capacity: 200_000, Horizon: time.Minute, Resolution: time.Millisecond})
	b0 := f.Bytes()
	for k := range 10_000 {
		for range 4 {
			f.TouchIfUnderAt(mix64(uint64(k)+1<<51), 60_000, 3, 1000)
		}
	}
	if f.buzz.Load() != nil || f.Bytes() != b0 {
		t.Fatalf("memory before any uncertain refusal: table %v, Bytes %d -> %d", f.buzz.Load() != nil, b0, f.Bytes())
	}
	// An uncertain refusal: 5 events over 1.5 s, asked at 1 s / 5.
	h := mix64(7)
	for i := range uint64(5) {
		f.TouchIfUnderAt(h, 1_000, 5, 2000+i*200)
	}
	if forceCT { // ConstantTime keeps TouchIfUnder as without entries: no table, ever
		if _, ok := f.TouchIfUnderAt(h, 1_000, 5, 3300); ok || f.buzz.Load() != nil {
			t.Fatalf("ConstantTime: admitted %v, table %v after an uncertain refusal", ok, f.buzz.Load() != nil)
		}
		return
	}
	if _, ok := f.TouchIfUnderAt(h, 1_000, 5, 3300); ok || f.buzz.Load() == nil {
		t.Fatalf("no entry after an uncertain refusal")
	}
	per := float64(f.Bytes()-b0) / 200_000
	t.Logf("Bytes %d -> %d once allocated: %.3f B per key of Capacity", b0, f.Bytes(), per)
	if per > 0.6 {
		t.Errorf("%.3f B per key once allocated (0.6 at most)", per)
	}
}

// TestBuzzPlainTouch: a plain touch of a key that holds an entry changes its
// words, so the entry is dropped and the next request is the register's.
func TestBuzzPlainTouch(t *testing.T) {
	skipBuzzCT(t)
	f, _ := New(Config{Capacity: 1000, Horizon: time.Minute, Resolution: time.Millisecond})
	h := mix64(9)
	for i := range uint64(5) {
		f.TouchIfUnderAt(h, 1_000, 5, 2000+i*200)
	}
	f.TouchIfUnderAt(h, 1_000, 5, 3300) // refused in doubt: an entry, its log from 3300
	tb := f.buzz.Load()
	if tb == nil || tb.given.Load() != 1 {
		t.Fatalf("no entry given")
	}
	if _, ok := f.TouchIfUnderAt(h, 1_000, 5, 4400); !ok || tb.decided.Load() == 0 { // a window after the log's start
		t.Fatalf("the log decided nothing (admitted %v)", ok)
	}
	f.TouchAt(h, 0, 4500)
	d := tb.dropped.Load()
	f.TouchIfUnderAt(h, 1_000, 5, 4600)
	if tb.dropped.Load() != d+1 {
		t.Errorf("a plain touch did not drop the entry (dropped %d -> %d)", d, tb.dropped.Load())
	}
}

// TestBuzzConcurrent: G goroutines on a shared clock, as
// TestTouchIfSustained, with limits a log decides: the admitted events per
// key stay within the limit at every window position, and logs decide some
// of them. Also run under the race detector (stress tier).
func TestBuzzConcurrent(t *testing.T) {
	skipBuzzCT(t)
	const G, hTicks = 8, 60_000
	for _, c := range []buzzRule{{1_000, 5}, {5_000, 10}, {10_000, 5}} {
		for _, nk := range []int{1, 200} {
			f, _ := New(Config{Capacity: 200_000, Horizon: time.Minute, Resolution: time.Millisecond})
			logs := make([][]uint64, G) // per goroutine: key<<32 | tick
			var wg, done sync.WaitGroup
			step := make([]chan uint64, G)
			for g := range G {
				step[g] = make(chan uint64, 1)
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := rand.New(rand.NewPCG(uint64(g), 3))
					for now := range step[g] {
						for range 1 + r.IntN(2) { // each key is asked about twice its limit per window
							k := r.IntN(nk)
							if r.Float64() < 2*float64(c.limit)*float64(nk)/(float64(c.w)*1.5*G) {
								if _, ok := f.TouchIfUnderAt(mix64(uint64(k)+1<<43), c.w, c.limit, now); ok {
									logs[g] = append(logs[g], uint64(k)<<32|now)
								}
							}
						}
						done.Done()
					}
				}()
			}
			ticks := uint64(2 * hTicks)
			if testing.Short() {
				ticks = max(hTicks/4, 3*c.w) // a log decides once the window starts after it does
			}
			for now := uint64(1); now <= ticks; now++ {
				done.Add(G)
				for g := range G {
					step[g] <- now
				}
				done.Wait()
			}
			for g := range G {
				close(step[g])
			}
			wg.Wait()
			per := make([][]uint64, nk)
			total := 0
			for g := range G {
				for _, x := range logs[g] {
					per[x>>32] = append(per[x>>32], x&(1<<32-1))
					total++
				}
			}
			over, positions := 0, 0
			for k := range per {
				ts := per[k]
				sort.Slice(ts, func(a, b int) bool { return ts[a] < ts[b] })
				lo := 0
				for hi := range ts {
					for ts[lo]+c.w < ts[hi] {
						lo++
					}
					j := hi
					for j+1 < len(ts) && ts[j+1] == ts[hi] {
						j++
					}
					positions++
					if j-lo+1 > int(c.limit) {
						over++
					}
				}
			}
			var decided uint64
			if tb := f.buzz.Load(); tb != nil {
				decided = tb.decided.Load()
			}
			t.Logf("keys %3d %v: admitted %d (%.2f of limit x windows x keys), window positions %d, over the limit %d, decided by a log %d",
				nk, c, total, float64(total)/(float64(c.limit)*float64(ticks)/float64(c.w)*float64(nk)), positions, over, decided)
			if over != 0 {
				t.Errorf("keys %d %v: over the limit at %d window positions", nk, c, over)
			}
			if decided == 0 {
				t.Errorf("keys %d %v: no decision was made by a log", nk, c)
			}
		}
	}
}

// TestBuzzSnapshot: a strict limiter's decisions across a snapshot (version
// 3 carries the buzz table): 400 keys offered 2x 1 s / 5 for 4 horizons, once
// uninterrupted and once snapshotted at 2 horizons and restored into a new
// filter; every decision and answer must be the same.
func TestBuzzSnapshot(t *testing.T) {
	skipSnapModes(t)
	cfg := Config{Capacity: 20_000, Horizon: time.Minute, Resolution: time.Millisecond}
	r := rand.New(rand.NewPCG(5, 5))
	type ev struct {
		t uint64
		k int
	}
	var evs []ev
	for k := range 400 {
		for x := r.ExpFloat64() / 0.01; x < 240_000; x += r.ExpFloat64() / 0.01 {
			evs = append(evs, ev{uint64(x) + 1, k})
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].t < evs[j].t })
	run := func(snapAt uint64) (out []string, decided uint64) {
		f, _ := New(cfg)
		snapped := false
		for _, e := range evs {
			if !snapped && snapAt > 0 && e.t >= snapAt {
				snapped = true
				var buf bytes.Buffer
				if _, err := f.WriteTo(&buf); err != nil {
					t.Fatal(err)
				}
				g, _ := New(cfg)
				if _, err := g.ReadFrom(&buf); err != nil {
					t.Fatal(err)
				}
				f = g
			}
			res, ok := f.TouchIfUnderAt(mix64(uint64(e.k)+1<<49), 1_000, 5, e.t)
			out = append(out, fmt.Sprint(ok, res))
		}
		if tb := f.buzz.Load(); tb != nil {
			decided = tb.decided.Load()
		}
		return out, decided
	}
	u, du := run(0)
	s, ds := run(120_000)
	diff := 0
	for i := range u {
		if u[i] != s[i] {
			diff++
		}
	}
	t.Logf("%d decisions, %d differ; decided by a log: %d uninterrupted, %d after the restore", len(u), diff, du, ds)
	if diff != 0 || du == 0 {
		t.Errorf("a restore changed %d decisions, or no log decided (%d)", diff, du)
	}
}

// TestBuzzReading (TF_EXP=1) reads the buzz as the lab arm B12b read it
// (claims A46): admitted shares and false denials on seven rules, offered
// 0.8x, 1.2x and 2x, in five scenarios: A 400 contested keys in a filter of
// Capacity 200,000 (1,838 entries); B the same keys in a filter of 12,800
// (112 entries: 3.6 contested keys an entry); C A with one request in ten a
// plain touch; D 1,000 contested keys beside tfbench's churn stream at loads
// 1.00 and 1.15; E limits past the log (reported, not judged). Every
// admission is checked against the arm's own events (buzzOffer).
func TestBuzzReading(t *testing.T) {
	skipBuzzCT(t)
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	rules := []buzzRule{{1_000, 5}, {5_000, 10}, {10_000, 3}, {10_000, 5}, {30_000, 10}, {60_000, 6}, {60_000, 16}}
	mults := []float64{0.8, 1.2, 2}
	scen := func(name string, capN int, plain float64, rules []buzzRule) {
		for _, ru := range rules {
			for _, mult := range mults {
				row := buzzOffer(t, capN, 400, ru, mult, 20, plain, 0, 0)
				t.Logf("%s %-7v x%.1f | %v", name, ru, mult, row)
				if name == "A" && mult == 2 && ru == rules[0] {
					t.Logf("%s memory: Bytes without entries %d, with %d: %.3f B per key of Capacity", name, row.bytesBase, row.bytesBuzz,
						float64(row.bytesBuzz-row.bytesBase)/float64(capN))
				}
			}
		}
	}
	scen("A", 200_000, 0, rules)
	scen("B", 12_800, 0, rules)
	scen("C", 200_000, 0.1, rules)
	scen("E", 200_000, 0, []buzzRule{{2_500, 20}, {5_000, 40}})
	for _, c := range []struct {
		load float64
		seed uint64
	}{{1.0, 2}, {1.15, 4}} {
		for _, ru := range []buzzRule{{1_000, 5}, {10_000, 5}} {
			for _, mult := range []float64{0.8, 2} {
				row := buzzOffer(t, 200_000, 1000, ru, mult, 4, 0, c.load, c.seed)
				t.Logf("D load %.2f %-7v x%.1f | %v | evictions %d relocations %d", c.load, ru, mult, row, row.evictions, row.relocations)
			}
		}
	}
}

// skipBuzzCT: ConstantTime filters (TF_CT=1 makes every filter one) keep
// TouchIfUnder as without buzz entries (TestBuzzMemory checks that).
func skipBuzzCT(t *testing.T) {
	t.Helper()
	if forceCT {
		t.Skip("ConstantTime filters keep TouchIfUnder as without buzz entries")
	}
}
