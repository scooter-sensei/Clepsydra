package temporalfilter

// lab/sw-2026-11, arm W12b: W12's lag loss split by the drip log's class at
// each query (SW-RESULTS.md, W12b).
//
// This file holds the probe's model of the drip log. swSplitField is the
// 11-bit field the log would hold for a key whose delivered ticks arrived in
// order, and swSplitNext tracks whether the key's log is dead (its valid bit
// clear) after each delivery. Both mirror next0's in-order cases and its
// dead and valid transitions, nothing else: the default build has no log
// (dripBits is 0 there), so the field's width and the quantum, floor(16 t /
// 2,500), are written out here.
//
// The run (swSplitA) is W12's leg A, its loop copied with a read of the
// key's slot after each delivery (swSplitState.touch) and beside each query
// (swSplitState.ask); the reads only read, so W12's line comes out as it
// did. Each query is classed from the slot as W12 classed it, with a valid
// log split in two: clean, its field equal to swSplitField of the key's
// delivered ticks, and off, not.

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// swSplitQuantum is the drip log's quantum index of tick t at unit 2,500:
// floor(16 t / 2,500).
func swSplitQuantum(t uint64) int64 { return int64(t << 4 / 2_500) }

// swSplitField is the 11-bit field next0 leaves when ts, sorted ascending
// and not empty, is pushed in order from dripFresh at ts[0], keeping the
// newest min(len(ts), 12) ticks (a field of 11 bits holds the gaps of 12
// events). It starts at all ones; a gap of 11 quanta or more sets all ones
// again, and any other gap g shifts the field by g+1 and sets g ones.
func swSplitField(ts []uint64) uint64 {
	if len(ts) > 12 {
		ts = ts[len(ts)-12:]
	}
	d := uint64(0x7ff)
	for i := 1; i < len(ts); i++ {
		g := uint64(swSplitQuantum(ts[i]) - swSplitQuantum(ts[i-1]))
		if g >= 11 {
			d = 0x7ff
		} else {
			d = (d<<(g+1) | (1<<g - 1)) & 0x7ff
		}
	}
	return d
}

// swSplitNext is whether the key's log is dead after delivering t, newest
// being the key's newest delivered tick before it. The first delivery of a
// key starts a valid log; a tick later than newest by 11 quanta or more
// makes it valid again; a tick in an earlier unit than newest's kills it
// (next0 sets the field to zero, and says nothing); any other tick leaves
// it as it was.
func swSplitNext(dead, first bool, newest, t uint64) bool {
	switch {
	case first:
		return false
	case t > newest && swSplitQuantum(t)-swSplitQuantum(newest) >= 11:
		return false
	case t/2_500 < newest/2_500:
		return true
	}
	return dead
}

// TestShortWindowLagSplitField (TF_EXP=1) is W12b's sight of swSplitField
// and swSplitNext on ticks made by hand. It reads no filter.
func TestShortWindowLagSplitField(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	const t0 = 100_000 // unit 40, quantum 640
	for _, c := range []struct {
		name string
		ts   []uint64
		want uint64
	}{
		{"one", []uint64{t0}, 0x7ff},
		{"same tick", []uint64{t0, t0}, 0x7fe},
		{"same quantum", []uint64{t0, t0 + 1}, 0x7fe},
		{"gap of 2", []uint64{t0, t0 + 400}, 0x7fb},
		{"gap of 11", []uint64{t0, t0 + 1_719}, 0x7ff},
		{"twelve in a quantum", []uint64{t0, t0 + 1, t0 + 2, t0 + 3, t0 + 4, t0 + 5, t0 + 6, t0 + 7, t0 + 8, t0 + 9, t0 + 10, t0 + 11}, 0},
	} {
		if got := swSplitField(c.ts); got != c.want {
			t.Errorf("swSplitField %s = %#x, want %#x", c.name, got, c.want)
		}
	}

	// Thirteen ticks, each 157 to 1,562 ticks after the last (1 to 10
	// quanta, so no gap sets all ones again), against a fold of all thirteen
	// by next0's in-order rule: the oldest gap has been shifted out of the
	// field by the twelve newest, so the newest 12 must leave the same field.
	r := rand.New(rand.NewPCG(12, 2))
	for n := 0; n < 100; n++ {
		ts := make([]uint64, 13)
		ts[0] = t0
		for i := 1; i < len(ts); i++ {
			ts[i] = ts[i-1] + 157 + r.Uint64N(1_406)
		}
		d := uint64(0x7ff)
		for i := 1; i < len(ts); i++ {
			g := uint64(swSplitQuantum(ts[i]) - swSplitQuantum(ts[i-1]))
			if g >= 11 {
				t.Fatalf("draw %d: a gap of %d quanta resets the field", n, g)
			}
			d = (d<<(g+1) | (1<<g - 1)) & 0x7ff
		}
		if got := swSplitField(ts); got != d {
			t.Errorf("draw %d: swSplitField of 13 ticks = %#x, fold of all 13 = %#x (ticks %v)", n, got, d, ts)
		}
	}

	for _, c := range []struct {
		name        string
		dead, first bool
		newest, t   uint64
		want        bool
	}{
		{"first", false, true, 0, t0, false},
		{"earlier unit", false, false, t0, t0 - 2_600, true},
		{"later, gap under 11", true, false, t0, t0 + 1_500, true},
		{"later, gap of 11", true, false, t0 + 1_500, t0 + 1_500 + 1_719, false},
		{"same unit behind, live", false, false, t0 + 400, t0 + 100, false},
		{"same unit behind, dead", true, false, t0 + 400, t0 + 100, true},
	} {
		if got := swSplitNext(c.dead, c.first, c.newest, c.t); got != c.want {
			t.Errorf("swSplitNext %s = %v, want %v", c.name, got, c.want)
		}
	}
}

// A query's class: W12's none, no log and dead (swLagClass), and W12's
// valid split into clean and off. swSplitHot is or-ed in when the key's
// slot points to a hot entry.
const (
	swSplitNone = iota
	swSplitNoLog
	swSplitDead
	swSplitClean
	swSplitOff
	swSplitHot = 0x80
)

var swSplitClassName = [5]string{"none", "nolog", "dead", "clean", "off"}

// swSplitLag is each class's W12 class: clean and off are both valid.
var swSplitLag = [5]int{swLagNone, swLagNoLog, swLagDead, swLagValid, swLagValid}

// swSplitQ is one query's record (16 bytes, packed, for encoding/binary):
// the key, the window's index, the class with swSplitHot, the exact count
// over the events delivered by then, the filter's answer, and for a dead
// query the ticks since its log died (0xffffffff if not dead).
type swSplitQ struct {
	K                  uint16
	Wi, Cls            uint8
	Exact, Answer, Age uint32
}

// swSplitState is a run's per-key state beside swLagRun's, and its counts.
//
// mdead is the probe's model of each key's log (swSplitNext). since is the
// arrival tick of the delivery that turned the slot's log from valid to
// dead, kept while the slot reads dead, and 0 while it reads valid (no
// arrival is at 0: the replay starts at 1). seen marks the keys delivered
// to.
//
// lastBad counts valid queries whose slot's last touch is not the key's
// newest delivered tick; modelMism, deliveries after which the model and
// the slot disagree (tfdrip); clsMism, queries whose class is not the one
// W12's query counted; qMoved, queries across which the slot's word0
// changed. q counts the queries by class, hot or not; hotDead, the dead
// queries of hot keys. qs is every query's record.
type swSplitState struct {
	mdead                               []bool
	since                               []uint64
	seen                                []bool
	lastBad, modelMism, clsMism, qMoved int
	q                                   [5]int
	hotDead                             int
	qs                                  []swSplitQ
}

// newSwSplitState is a state for swLagKeys' 2,000 keys and nq queries.
func newSwSplitState(nq int) *swSplitState {
	return &swSplitState{mdead: make([]bool, 2000), since: make([]uint64, 2000), seen: make([]bool, 2000), qs: make([]swSplitQ, 0, nq)}
}

// touch is swLagA's delivery of e at now (its touch, then the run's
// deliver), then one read of the key's slot. The model steps from the key's
// newest delivered tick before e. The slot reads dead when its log's valid
// bit is clear, or when it is not live, which no touch leaves: counted
// dead, such a slot shows as a model mismatch where the model reads valid,
// instead of passing. In tfdrip the slot's dead sets since (on its change
// from valid, or at the key's first delivery) and is checked against the
// model; without the log there is nothing to read (the read is made in
// both builds, so both runs do the same work).
func (s *swSplitState) touch(f *Filter, r *swLagRun, hs []uint64, e swLagEv, now uint64) {
	first, prev := !s.seen[e.k], r.newest[e.k]
	f.CheckAndTouchAt(hs[e.k], 0, e.t)
	r.deliver(e)
	s.seen[e.k] = true
	s.mdead[e.k] = swSplitNext(s.mdead[e.k], first, prev, e.t)
	w0, _, src := f.read(hs[e.k], now, -1)
	if !dripOn {
		return
	}
	dead := src == 0 || age(now, w0&tsMask) > f.horizon || dripOf(w0)&dripV == 0
	if dead != s.mdead[e.k] {
		s.modelMism++
	}
	switch {
	case !dead:
		s.since[e.k] = 0
	case s.since[e.k] == 0:
		s.since[e.k] = now
	}
}

// classify is key k's class at now from one read of its slot: none or no
// log as swLagClass reads them, dead on the log's valid bit clear, and
// otherwise clean if the log's field equals the in-order field of the key's
// delivered ticks, off if not (equal fields at the same last touch read the
// same quanta in dripCount). It returns the slot's field and the in-order
// one (model) beside the class. A valid log whose last touch is not the
// key's newest delivered tick counts in lastBad: the comparison assumes it.
func (s *swSplitState) classify(f *Filter, r *swLagRun, hs []uint64, k int32, now uint64) (cls uint8, field, model uint64) {
	w0, w1, src := f.read(hs[k], now, -1)
	field, model = dripOf(w0)&dripMask, swSplitField(r.del[k])
	switch {
	case src == 0 || age(now, w0&tsMask) > f.horizon:
		cls = swSplitNone
	case !dripOn:
		cls = swSplitNoLog
	case dripOf(w0)&dripV == 0:
		cls = swSplitDead
	default:
		if w0&tsMask != r.newest[k] {
			s.lastBad++
		}
		cls = swSplitOff
		if field == model {
			cls = swSplitClean
		}
	}
	if isHot(w1) {
		cls |= swSplitHot
	}
	return cls, field, model
}

// ask is swLagA's query q at now with its class beside it: classify, W12's
// query, then the slot read again, whose word0 the query must leave as it
// was (qMoved; classify's signature carries no word, so ask reads one just
// before the query). The record's exact count is the difference of the
// run's exact sum for the window across the query, and its answer the one
// W12's query kept; the error, |answer - exact|, is left to the file's
// trailer. The class must be the one W12's query counted, read as the
// difference of its class counts (clsMism).
func (s *swSplitState) ask(f *Filter, r *swLagRun, hs []uint64, q swLagQ, now uint64) {
	cls, _, _ := s.classify(f, r, hs, q.k, now)
	w0, _, _ := f.read(hs[q.k], now, -1)
	x0, q0 := r.exact[q.wi], r.q
	r.query(f, hs, q, now)
	if w, _, _ := f.read(hs[q.k], now, -1); w != w0 {
		s.qMoved++
	}
	c := cls &^ swSplitHot
	counted := -1
	for lc := range swLagClasses {
		if r.q[lc] != q0[lc] {
			counted = lc
		}
	}
	if counted != swSplitLag[c] {
		s.clsMism++
	}
	s.q[c]++
	dt := uint32(0xffffffff)
	if c == swSplitDead {
		dt = uint32(now - s.since[q.k])
		if cls&swSplitHot != 0 {
			s.hotDead++
		}
	}
	s.qs = append(s.qs, swSplitQ{K: uint16(q.k), Wi: uint8(q.wi), Cls: cls,
		Exact: uint32(r.exact[q.wi] - x0), Answer: uint32(r.answers[now-swLagH]), Age: dt})
}

// swSplitA is swLagA (leg A: one goroutine, plain mode) with the probe's
// reads: its loop, copied, with s.touch in place of the touch and deliver
// and s.ask in place of the query. It returns the run, the probe's state
// and the seconds the run took.
func swSplitA(t testing.TB, rec *swLagRec, arr []uint64, order []int32) (*swLagRun, *swSplitState, float64) {
	start := time.Now()
	f := swLagFilter(t, "plain")
	defer f.Close()
	hs, r, p := swLagKeys(), newSwLagRun(rec), 0
	s := newSwSplitState(len(rec.qs))
	for now := uint64(1); now < swLagEnd; now++ {
		for ; p < len(order) && arr[order[p]] <= now; p++ {
			s.touch(f, r, hs, rec.ev[order[p]], now)
		}
		if now >= swLagH {
			s.ask(f, r, hs, rec.qs[now-swLagH], now)
		}
	}
	return r, s, time.Since(start).Seconds()
}

// TestShortWindowLagSplitSight (TF_EXP=1) is W12b's sight of the run's
// classes and dead since, on states made by hand. Every touch goes through
// the run's touch, at a now one past its key's newest delivered tick, and
// each state is classed at that now as soon as it is made. In tfdrip each
// must read as its row says:
//
//	1   key 0 in order: clean
//	2   key 1, a late event in its last quantum: clean
//	3a  key 2, a late event an earlier quantum behind in its unit: off
//	3b  one push on: off
//	3c  eleven more, the late event's code out of the field: clean, field 0
//	4a  key 3, a late event an earlier unit behind: dead, since its now
//	4b  9 quanta on: dead
//	4c  11 quanta on: clean, field 0x7ff
//	5a  key 4, 32 events in a unit: promoted, hot, clean, field 0
//	5b  hot, in order: clean
//	5c  hot, an event an earlier unit behind: dead
//	6   key 5, touched again past the horizon: clean, field 0x7ff
//	7   key 6, twelve events in one quantum: clean, field 0
//	8   key 7, 40 ticks in order at random gaps: clean, the slot's field
//	    the in-order one at every touch
//
// and key 8, never touched, reads none. The model and the slot agree and
// every valid log's last touch is its key's newest delivered tick
// throughout. In the default build every touched key reads no log. State 6
// has a filter, run and state of its own, so that its touch 70 s on sweeps
// nothing the other states read; state 8 comes last on the first filter,
// its ticks running tens of thousands past t0.
func TestShortWindowLagSplitSight(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	build := "default"
	if dripOn {
		build = "tfdrip"
	}
	const t0 = 100_000 // unit 40, quantum 640
	f := swLagFilter(t, "plain")
	defer f.Close()
	hs := swLagKeys()
	r, s := newSwLagRun(&swLagRec{qs: make([]swLagQ, 1)}), newSwSplitState(0)
	// touch delivers key k's ticks in order of delivery, each at now its
	// key's newest delivered tick after it plus 1.
	touch := func(f *Filter, r *swLagRun, s *swSplitState, k int32, ts ...uint64) {
		for _, tk := range ts {
			s.touch(f, r, hs, swLagEv{tk, k}, max(r.newest[k], tk)+1)
		}
	}
	// see classes key k at its newest delivered tick plus 1 (key 8, never
	// touched, at t0+1) and logs it. In tfdrip it must read want, with the
	// field if field is not -1; in the default build, no log, or none where
	// none is wanted. more is the state's other conditions, in this build.
	states := 0
	see := func(name string, f *Filter, r *swLagRun, s *swSplitState, k int32, want uint8, field int64, more bool) {
		t.Helper()
		now := max(r.newest[k], t0) + 1
		cls, fd, model := s.classify(f, r, hs, k, now)
		ok := cls == want && (field < 0 || fd == uint64(field))
		if !dripOn {
			w := uint8(swSplitNoLog)
			if want == swSplitNone {
				w = swSplitNone
			}
			ok = cls&^swSplitHot == w
		}
		ok = ok && more && s.modelMism == 0 && s.lastBad == 0
		t.Logf("W12b sight %s: build=%s cls=%s field=%#x model=%#x since=%d hot=%v ok=%v",
			name, build, swSplitClassName[cls&^swSplitHot], fd, model, s.since[k], cls&swSplitHot != 0, ok)
		if !ok {
			t.Errorf("state %s: key %d at %d reads %s field %#x hot %v (modelMism %d, lastBad %d), want %s hot %v field %d",
				name, k, now, swSplitClassName[cls&^swSplitHot], fd, cls&swSplitHot != 0, s.modelMism, s.lastBad,
				swSplitClassName[want&^swSplitHot], want&swSplitHot != 0, field)
		}
		states++
	}

	touch(f, r, s, 0, t0, t0+400, t0+800) // quanta 640, 642, 645
	see("1", f, r, s, 0, swSplitClean, -1, true)
	touch(f, r, s, 1, t0+100, t0+50) // both quantum 640
	see("2", f, r, s, 1, swSplitClean, -1, true)
	touch(f, r, s, 2, t0+400, t0+100) // quantum 640 behind 642, both unit 40
	see("3a", f, r, s, 2, swSplitOff, -1, true)
	touch(f, r, s, 2, t0+600) // quantum 643
	see("3b", f, r, s, 2, swSplitOff, -1, true)
	for range 11 {
		touch(f, r, s, 2, t0+610)
	}
	see("3c", f, r, s, 2, swSplitClean, 0, true)
	touch(f, r, s, 3, t0, t0-2_600) // unit 38 behind 40
	see("4a", f, r, s, 3, swSplitDead, -1, !dripOn || s.since[3] == t0+1)
	touch(f, r, s, 3, t0+1_500) // quantum 649: 9 after 640
	see("4b", f, r, s, 3, swSplitDead, -1, true)
	touch(f, r, s, 3, t0+3_219) // quantum 660: 11 after 649
	see("4c", f, r, s, 3, swSplitClean, 0x7ff, true)
	for i := range uint64(32) { // past the register's 31 events in a unit: promoted
		touch(f, r, s, 4, t0+i)
	}
	see("5a", f, r, s, 4, swSplitClean|swSplitHot, 0, !dripOn || f.Promoted() == 1)
	touch(f, r, s, 4, t0+400)
	see("5b", f, r, s, 4, swSplitClean|swSplitHot, -1, true)
	touch(f, r, s, 4, t0-2_600)
	see("5c", f, r, s, 4, swSplitDead|swSplitHot, -1, true)

	f6 := swLagFilter(t, "plain")
	defer f6.Close()
	r6, s6 := newSwLagRun(&swLagRec{qs: make([]swLagQ, 1)}), newSwSplitState(0)
	touch(f6, r6, s6, 5, t0, t0+70_000)
	see("6", f6, r6, s6, 5, swSplitClean, 0x7ff, true)

	for i := range uint64(12) {
		touch(f, r, s, 6, t0+i)
	}
	see("7", f, r, s, 6, swSplitClean, 0, true)

	// State 8: each touch is classed as made, and must read clean with the
	// slot's field the in-order one (in the default build, no log).
	rng := rand.New(rand.NewPCG(12, 2))
	all, tk := true, uint64(t0)
	for i := range 40 {
		if i > 0 {
			tk += rng.Uint64N(2_001)
		}
		touch(f, r, s, 7, tk)
		cls, fd, model := s.classify(f, r, hs, 7, tk+1)
		good := cls == swSplitClean && fd == model
		if !dripOn {
			good = cls&^swSplitHot == swSplitNoLog
		}
		if !good {
			all = false
			t.Errorf("state 8, touch %d at %d: %s field %#x, in-order field %#x", i, tk, swSplitClassName[cls&^swSplitHot], fd, model)
		}
	}
	see("8", f, r, s, 7, swSplitClean, -1, all)
	see("k8", f, r, s, 8, swSplitNone, -1, true)
	t.Logf("W12b sight done: build=%s states=%d", build, states)
}

// The per-query file: one run's records, for lab/w12b.py and the join.
// Little-endian, packed: the header, one 16-byte record per query, and a
// trailer of the five windows' errors and exact counts (es, xs) and the
// CRC-32 (IEEE) of the records' bytes as written. The reader checks the
// CRC against the bytes it read, which catches an edit to any record byte,
// and es and xs against the records' sums.

var swSplitMagic = [4]byte{'W', '1', '2', 'B'}

// swSplitModel is swLagOrder's code for each delay model.
var swSplitModel = map[string]uint8{"none": 0, "jitter": 1, "stall": 2}

// swSplitHead names the run: Build is 0 for the default build and 1 for
// tfdrip, Model and J the delay (swSplitModel), Head the library's commit as
// 40 hex digits, N the records that follow.
type swSplitHead struct {
	Magic   [4]byte
	Version uint32
	Build   uint8
	S1, S2  uint64
	Model   uint8
	J       uint64
	Head    [40]byte
	N       uint64
}

// swSplitPath is the file of build's run at seed sd and delay d in dir.
func swSplitPath(dir, build string, sd [2]uint64, d swLagDelay) string {
	return filepath.Join(dir, fmt.Sprintf("%s-%d_%d-%s.bin", build, sd[0], sd[1], strings.ReplaceAll(d.String(), ":", "_")))
}

// swSplitAbs is |a - b|.
func swSplitAbs(a, b uint32) uint64 {
	if a > b {
		return uint64(a - b)
	}
	return uint64(b - a)
}

// swSplitSums is each window's error, |Answer - Exact|, and exact count over
// qs. A record in a window past the fifth is an error.
func swSplitSums(qs []swSplitQ) (es, xs [5]uint64, err error) {
	for i, q := range qs {
		if q.Wi >= 5 {
			return es, xs, fmt.Errorf("record %d: window %d", i, q.Wi)
		}
		es[q.Wi] += swSplitAbs(q.Answer, q.Exact)
		xs[q.Wi] += uint64(q.Exact)
	}
	return es, xs, nil
}

// swSplitWrite writes h (with N set to len(qs)), qs and the trailer to path.
func swSplitWrite(path string, h swSplitHead, qs []swSplitQ) error {
	es, xs, err := swSplitSums(qs)
	if err != nil {
		return err
	}
	h.N = uint64(len(qs))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w, crc := bufio.NewWriter(f), crc32.NewIEEE()
	if err = binary.Write(w, binary.LittleEndian, h); err == nil {
		err = binary.Write(io.MultiWriter(w, crc), binary.LittleEndian, qs)
	}
	for _, v := range []any{es, xs, crc.Sum32()} {
		if err == nil {
			err = binary.Write(w, binary.LittleEndian, v)
		}
	}
	if err != nil {
		f.Close()
		return err
	}
	if err = w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// swSplitRead reads a file swSplitWrite made. It errors on a wrong magic or
// version, a record count that the file's size does not hold (short or
// long), records whose CRC-32 is not the trailer's, and a trailer whose
// sums are not the records'.
func swSplitRead(path string) (h swSplitHead, qs []swSplitQ, es, xs [5]uint64, err error) {
	fail := func(format string, a ...any) (swSplitHead, []swSplitQ, [5]uint64, [5]uint64, error) {
		return swSplitHead{}, nil, [5]uint64{}, [5]uint64{}, fmt.Errorf("%s: "+format, append([]any{path}, a...)...)
	}
	f, err := os.Open(path)
	if err != nil {
		return fail("%v", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fail("%v", err)
	}
	r := bufio.NewReader(f)
	if err = binary.Read(r, binary.LittleEndian, &h); err != nil {
		return fail("header: %v", err)
	}
	if h.Magic != swSplitMagic {
		return fail("magic %q", h.Magic[:])
	}
	if h.Version != 1 {
		return fail("version %d", h.Version)
	}
	hs, qsz, tsz := int64(binary.Size(h)), int64(binary.Size(swSplitQ{})), int64(2*binary.Size(es)+4)
	if h.N > uint64(fi.Size()) || fi.Size() != hs+qsz*int64(h.N)+tsz {
		return fail("%d bytes, but the header counts %d records", fi.Size(), h.N)
	}
	qs = make([]swSplitQ, h.N)
	crc := crc32.NewIEEE()
	if err = binary.Read(io.TeeReader(r, crc), binary.LittleEndian, qs); err != nil {
		return fail("records: %v", err)
	}
	var tes, txs [5]uint64
	var tcrc uint32
	for _, v := range []any{&tes, &txs, &tcrc} {
		if err = binary.Read(r, binary.LittleEndian, v); err != nil {
			return fail("trailer: %v", err)
		}
	}
	if tcrc != crc.Sum32() {
		return fail("records' CRC-32 %#x, the trailer's %#x", crc.Sum32(), tcrc)
	}
	if _, err = r.ReadByte(); err != io.EOF {
		return fail("bytes after the trailer")
	}
	if es, xs, err = swSplitSums(qs); err != nil {
		return fail("%v", err)
	}
	if es != tes || xs != txs {
		return fail("trailer es %v xs %v, records' sums es %v xs %v", tes, txs, es, xs)
	}
	return h, qs, es, xs, nil
}

// swSplitFile is a file read by swSplitRead.
type swSplitFile struct {
	H      swSplitHead
	Qs     []swSplitQ
	Es, Xs [5]uint64
}

// swSplitCheck is what the join takes on trust: each file's header names
// its build, the seed and the delay (the none files, none), the four heads
// are the same commit, and the four files hold the same number of queries.
func swSplitCheck(sd [2]uint64, d swLagDelay, def, drip, def0, drip0 *swSplitFile) error {
	var errs []error
	nd := swLagDelay{"none", 0}
	for _, c := range []struct {
		name  string
		f     *swSplitFile
		build uint8
		d     swLagDelay
	}{{"default", def, 0, d}, {"tfdrip", drip, 1, d}, {"default none", def0, 0, nd}, {"tfdrip none", drip0, 1, nd}} {
		h := c.f.H
		if h.Build != c.build || h.S1 != sd[0] || h.S2 != sd[1] || h.Model != swSplitModel[c.d.model] || h.J != c.d.j {
			errs = append(errs, fmt.Errorf("%s file: build %d seed %d,%d model %d j %d, want build %d seed %d,%d delay %s",
				c.name, h.Build, h.S1, h.S2, h.Model, h.J, c.build, sd[0], sd[1], c.d))
		}
		if h.Head != def.H.Head {
			errs = append(errs, fmt.Errorf("%s file: head %s, the default file's is %s", c.name, h.Head[:], def.H.Head[:]))
		}
		if len(c.f.Qs) != len(def.Qs) {
			errs = append(errs, fmt.Errorf("%s file: %d records, the default file's %d", c.name, len(c.f.Qs), len(def.Qs)))
		}
	}
	return errors.Join(errs...)
}

// The join's groups, in the order of the line: tfdrip's class at the delay
// (none, dead, clean, off), the dead queries of hot and of other keys, and
// the dead queries by ticks dead.
const swSplitGroups = 11

var swSplitGroupName = [swSplitGroups]string{"none", "dead", "clean", "off", "deadhot", "deadreg", "age0", "age1", "age2", "age3", "age4"}

// swSplitAgeBin is the bin of a dead query's age: [0,1000), [1000,2500),
// [2500,10000), [10000,60000), 60000 and over.
func swSplitAgeBin(age uint32) int {
	for i, edge := range [4]uint32{1_000, 2_500, 10_000, 60_000} {
		if age < edge {
			return i
		}
	}
	return 4
}

// swSplitCell is one group's sums in one window: queries, exact counts and
// the errors of the default and tfdrip builds at the delay (ed, et) and, on
// the same queries, in the none run (ed0, et0).
type swSplitCell struct{ q, x, ed, et, ed0, et0 uint64 }

// swSplitPair is one seed's line for delay d, from the default and tfdrip
// files at d and at none, paired query by query by their index. Its sums
// are all integers. A query whose tfdrip record is classed nolog or outside
// 0-4, or whose default record is classed other than none or nolog, counts
// in clsbad and joins no group.
func swSplitPair(sd [2]uint64, d swLagDelay, def, drip, def0, drip0 *swSplitFile) string {
	n := min(len(def.Qs), len(drip.Qs), len(def0.Qs), len(drip0.Qs))
	var keymism, winmism, exactmism, keymism0, winmism0, deadmism, clsbad int
	var cells [swSplitGroups][5]swSplitCell
	for i := range n {
		a, b, a0, b0 := def.Qs[i], drip.Qs[i], def0.Qs[i], drip0.Qs[i]
		if a.K != b.K {
			keymism++
		}
		if a.Wi != b.Wi {
			winmism++
		}
		if a.Exact != b.Exact {
			exactmism++
		}
		if a.K != a0.K || b.K != b0.K {
			keymism0++
		}
		if a.Wi != a0.Wi || b.Wi != b0.Wi {
			winmism0++
		}
		ca, cb := a.Cls&^swSplitHot, b.Cls&^swSplitHot
		bad := false
		if cb == swSplitNoLog || cb > swSplitOff {
			clsbad++
			bad = true
		}
		if ca != swSplitNone && ca != swSplitNoLog {
			clsbad++
			bad = true
		}
		if cb == swSplitDead && b.Answer != a.Answer {
			deadmism++
		}
		if bad || b.Wi >= 5 {
			continue
		}
		// The groups of a query: its class, and for a dead one its hot or
		// regular group and its age bin.
		gs, ng := [3]int{int(cb)}, 1
		switch cb {
		case swSplitDead:
			gs[0] = 1
		case swSplitClean:
			gs[0] = 2
		case swSplitOff:
			gs[0] = 3
		}
		if cb == swSplitDead {
			gs[1], gs[2], ng = 5, 6+swSplitAgeBin(b.Age), 3
			if b.Cls&swSplitHot != 0 {
				gs[1] = 4
			}
		}
		for _, g := range gs[:ng] {
			c := &cells[g][b.Wi]
			c.q++
			c.x += uint64(b.Exact)
			c.ed += swSplitAbs(a.Answer, a.Exact)
			c.et += swSplitAbs(b.Answer, b.Exact)
			c.ed0 += swSplitAbs(a0.Answer, a0.Exact)
			c.et0 += swSplitAbs(b0.Answer, b0.Exact)
		}
	}
	var sb strings.Builder
	win := func(v [5]uint64) string {
		return fmt.Sprintf("%d,%d,%d,%d,%d", v[0], v[1], v[2], v[3], v[4])
	}
	fmt.Fprintf(&sb, "W12b split: seed=%d,%d delay=%s recs=%d keymism=%d winmism=%d exactmism=%d keymism0=%d winmism0=%d deadmism=%d clsbad=%d head=%s X=%s X0=%s",
		sd[0], sd[1], d, len(def.Qs), keymism, winmism, exactmism, keymism0, winmism0, deadmism, clsbad, drip.H.Head[:], win(drip.Xs), win(drip0.Xs))
	for g, name := range swSplitGroupName {
		fmt.Fprintf(&sb, " c_%s=", name)
		for w, c := range cells[g] {
			if w > 0 {
				sb.WriteByte(';')
			}
			fmt.Fprintf(&sb, "%d/%d/%d/%d/%d/%d", c.q, c.x, c.ed, c.et, c.ed0, c.et0)
		}
	}
	return sb.String()
}

// TestShortWindowLagSplitFile (TF_EXP=1) is W12b's sight of the per-query
// file, on records made by hand: it must read back what was written, and
// refuse a file cut or lengthened, one whose trailer or record count or
// magic or version is wrong. It reads no filter.
func TestShortWindowLagSplitFile(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	dir := t.TempDir()
	h := swSplitHead{Magic: swSplitMagic, Version: 1, Build: 1, S1: 2026, S2: 41, Model: 2, J: 100}
	copy(h.Head[:], "0123456789abcdef0123456789abcdef01234567")
	qs := []swSplitQ{
		{K: 7, Wi: 0, Cls: swSplitClean, Exact: 10, Answer: 12, Age: 0xffffffff},
		{K: 1999, Wi: 4, Cls: swSplitDead | swSplitHot, Exact: 40, Answer: 35, Age: 1234},
		{K: 0, Wi: 4, Cls: swSplitNone, Exact: 3, Answer: 3, Age: 0xffffffff},
	}
	path := swSplitPath(dir, "tfdrip", [2]uint64{2026, 41}, swLagDelay{"stall", 100})
	if want := filepath.Join(dir, "tfdrip-2026_41-stall_100.bin"); path != want {
		t.Errorf("swSplitPath = %q, want %q", path, want)
	}
	if err := swSplitWrite(path, h, qs); err != nil {
		t.Fatal(err)
	}
	h2, qs2, es, xs, err := swSplitRead(path)
	if err != nil {
		t.Fatal(err)
	}
	h.N = 3
	if h2 != h || !slices.Equal(qs2, qs) {
		t.Errorf("read back %+v %+v, wrote %+v %+v", h2, qs2, h, qs)
	}
	if wantE, wantX := [5]uint64{2, 0, 0, 0, 5}, [5]uint64{10, 0, 0, 0, 43}; es != wantE || xs != wantX {
		t.Errorf("trailer es %v xs %v, want %v %v", es, xs, wantE, wantX)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	refuse := func(name string, b []byte) {
		t.Helper()
		p := filepath.Join(dir, name+".bin")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, err := swSplitRead(p); err == nil {
			t.Errorf("%s: read without error", name)
		}
	}
	edit := func(off int, f func(byte) byte) []byte {
		b := slices.Clone(raw)
		b[off] = f(b[off])
		return b
	}
	flip := func(b byte) byte { return b ^ 1 }
	refuse("cut", raw[:len(raw)-1])
	refuse("extra", append(slices.Clone(raw), 0))
	refuse("trailer-last", edit(len(raw)-1, flip))
	refuse("trailer-first", edit(len(raw)-84, flip))
	refuse("trailer-xs", edit(len(raw)-44, flip))
	// An edit to a record byte the sums do not read: the key, the class
	// (and the hot bit), the age; only the CRC catches them.
	rec0 := binary.Size(h)
	refuse("record-k", edit(rec0, flip))
	refuse("record-cls", edit(rec0+3, flip))
	refuse("record-hot", edit(rec0+16+3, func(b byte) byte { return b ^ swSplitHot }))
	refuse("record-age", edit(rec0+12, flip))
	refuse("count-short", edit(binary.Size(h)-8, func(b byte) byte { return b - 1 }))
	refuse("count-long", edit(binary.Size(h)-8, func(b byte) byte { return b + 1 }))
	refuse("version", edit(4, flip))
	refuse("empty", nil)
	hx := h
	hx.Magic = [4]byte{'W', '1', '2', 'X'}
	px := filepath.Join(dir, "magic.bin")
	if err := swSplitWrite(px, hx, qs); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := swSplitRead(px); err == nil {
		t.Error("magic W12X: read without error")
	}
	if err := swSplitWrite(filepath.Join(dir, "wi.bin"), h, []swSplitQ{{Wi: 5}}); err == nil {
		t.Error("a record in window 5: written without error")
	}

	// The join's arithmetic, on two hand-written pairs of files: the delay
	// run (jitter:10) and the none run, each in both builds, 11 queries.
	// Queries 6 and 7 are bad records (a tfdrip nolog; a default dead): they
	// count in clsbad and join no group. Query 3 is a dead query whose
	// answers differ (deadmism). The dead queries' ages, 500, 1,000, 2,499,
	// 2,500, 10,000 and 60,000, fall in the five bins.
	sd, jd := [2]uint64{9, 9}, swLagDelay{"jitter", 10}
	const (
		nn, nl, dd, cl, of, hot = swSplitNone, swSplitNoLog, swSplitDead, swSplitClean, swSplitOff, swSplitHot
		na                      = 0xffffffff
	)
	mk := func(k uint16, wi uint8, cls uint8, exact, ans, age uint32) swSplitQ {
		return swSplitQ{K: k, Wi: wi, Cls: cls, Exact: exact, Answer: ans, Age: age}
	}
	// delay run, default: class, exact, answer.
	def := []swSplitQ{
		mk(10, 0, nl, 5, 7, na), mk(11, 0, nl, 10, 12, na), mk(12, 1, nl, 20, 15, na), mk(13, 1, nl, 8, 9, na),
		mk(14, 2, nl, 30, 26, na), mk(15, 4, nn, 40, 41, na), mk(16, 0, nl, 3, 3, na), mk(17, 0, dd, 2, 2, 7),
		mk(18, 3, nl, 6, 7, na), mk(19, 3, nl, 4, 3, na), mk(20, 3, nl, 9, 12, na),
	}
	// delay run, tfdrip: class, ages, answers (query 3's differs from the
	// default's).
	drip := []swSplitQ{
		mk(10, 0, nn, 5, 4, na), mk(11, 0, dd, 10, 12, 500), mk(12, 1, dd|hot, 20, 15, 1_000), mk(13, 1, dd, 8, 10, 2_499),
		mk(14, 2, cl, 30, 30, na), mk(15, 4, of|hot, 40, 38, na), mk(16, 0, nl, 3, 3, na), mk(17, 0, cl, 2, 2, na),
		mk(18, 3, dd, 6, 7, 60_000), mk(19, 3, dd|hot, 4, 3, 10_000), mk(20, 3, dd, 9, 12, 2_500),
	}
	// none run: the exact counts differ from the delay run's at query 4.
	def0 := []swSplitQ{
		mk(10, 0, nl, 5, 5, na), mk(11, 0, nl, 10, 11, na), mk(12, 1, nl, 20, 20, na), mk(13, 1, nl, 8, 8, na),
		mk(14, 2, nl, 31, 29, na), mk(15, 4, nl, 40, 40, na), mk(16, 0, nl, 3, 3, na), mk(17, 0, nl, 2, 2, na),
		mk(18, 3, nl, 6, 7, na), mk(19, 3, nl, 4, 4, na), mk(20, 3, nl, 9, 9, na),
	}
	drip0 := []swSplitQ{
		mk(10, 0, cl, 5, 6, na), mk(11, 0, cl, 10, 9, na), mk(12, 1, cl, 20, 18, na), mk(13, 1, cl, 8, 8, na),
		mk(14, 2, cl, 31, 31, na), mk(15, 4, cl, 40, 37, na), mk(16, 0, cl, 3, 3, na), mk(17, 0, cl, 2, 2, na),
		mk(18, 3, cl, 6, 6, na), mk(19, 3, cl, 4, 5, na), mk(20, 3, cl, 9, 9, na),
	}
	hd := func(build uint8, d swLagDelay) swSplitHead {
		h := swSplitHead{Magic: swSplitMagic, Version: 1, Build: build, S1: sd[0], S2: sd[1], Model: swSplitModel[d.model], J: d.j}
		copy(h.Head[:], "0123456789abcdef0123456789abcdef01234567")
		return h
	}
	load := func(name string, build uint8, d swLagDelay, qs []swSplitQ) *swSplitFile {
		p := filepath.Join(dir, name+".bin")
		if err := swSplitWrite(p, hd(build, d), qs); err != nil {
			t.Fatal(err)
		}
		h, qs, es, xs, err := swSplitRead(p)
		if err != nil {
			t.Fatal(err)
		}
		return &swSplitFile{H: h, Qs: qs, Es: es, Xs: xs}
	}
	nd := swLagDelay{"none", 0}
	fDef, fDrip, fDef0, fDrip0 := load("def", 0, jd, def), load("drip", 1, jd, drip), load("def0", 0, nd, def0), load("drip0", 1, nd, drip0)
	if err := swSplitCheck(sd, jd, fDef, fDrip, fDef0, fDrip0); err != nil {
		t.Errorf("a good set of files: %v", err)
	}
	if err := swSplitCheck(sd, swLagDelay{"jitter", 100}, fDef, fDrip, fDef0, fDrip0); err == nil {
		t.Error("files of another delay: no error")
	}
	if err := swSplitCheck(sd, jd, fDef, fDrip, fDrip0, fDef0); err == nil {
		t.Error("none files of the wrong builds: no error")
	}
	other := *fDrip
	other.H.Head[0] = 'f'
	if err := swSplitCheck(sd, jd, fDef, &other, fDef0, fDrip0); err == nil {
		t.Error("another head: no error")
	}
	z := "0/0/0/0/0/0"
	cells := func(c ...string) string { return strings.Join(c, ";") }
	c := func(name string, w ...string) string { return " c_" + name + "=" + cells(w...) }
	want := "W12b split: seed=9,9 delay=jitter:10 recs=11 keymism=0 winmism=0 exactmism=0 keymism0=0 winmism0=0 deadmism=1 clsbad=2" +
		" head=0123456789abcdef0123456789abcdef01234567 X=20,28,30,19,40 X0=20,28,31,19,40" +
		c("none", "1/5/2/1/0/1", z, z, z, z) +
		c("dead", "1/10/2/2/1/1", "2/28/6/7/0/2", z, "3/19/5/5/1/1", z) +
		c("clean", z, z, "1/30/4/0/2/0", z, z) +
		c("off", z, z, z, z, "1/40/1/2/0/3") +
		c("deadhot", z, "1/20/5/5/0/2", z, "1/4/1/1/0/1", z) +
		c("deadreg", "1/10/2/2/1/1", "1/8/1/2/0/0", z, "2/15/4/4/1/0", z) +
		c("age0", "1/10/2/2/1/1", z, z, z, z) +
		c("age1", z, "2/28/6/7/0/2", z, z, z) +
		c("age2", z, z, z, "1/9/3/3/0/0", z) +
		c("age3", z, z, z, "1/4/1/1/0/1", z) +
		c("age4", z, z, z, "1/6/1/1/1/0", z)
	if got := swSplitPair(sd, jd, fDef, fDrip, fDef0, fDrip0); got != want {
		t.Errorf("split line:\n got %s\nwant %s", got, want)
	}
	// At none the pair is the none files against themselves: every query is
	// clean in the tfdrip file, the loss is nil (ed = ed0, et = et0) and the
	// default file's nolog records are good.
	wantNone := "W12b split: seed=9,9 delay=none recs=11 keymism=0 winmism=0 exactmism=0 keymism0=0 winmism0=0 deadmism=0 clsbad=0" +
		" head=0123456789abcdef0123456789abcdef01234567 X=20,28,31,19,40 X0=20,28,31,19,40" +
		c("none", z, z, z, z, z) + c("dead", z, z, z, z, z) +
		c("clean", "4/20/1/2/1/2", "2/28/0/2/0/2", "1/31/2/0/2/0", "3/19/1/1/1/1", "1/40/0/3/0/3") +
		c("off", z, z, z, z, z) + c("deadhot", z, z, z, z, z) + c("deadreg", z, z, z, z, z) +
		c("age0", z, z, z, z, z) + c("age1", z, z, z, z, z) + c("age2", z, z, z, z, z) + c("age3", z, z, z, z, z) + c("age4", z, z, z, z, z)
	if err := swSplitCheck(sd, nd, fDef0, fDrip0, fDef0, fDrip0); err != nil {
		t.Errorf("the none pair: %v", err)
	}
	if got := swSplitPair(sd, nd, fDef0, fDrip0, fDef0, fDrip0); got != wantNone {
		t.Errorf("none split line:\n got %s\nwant %s", got, wantNone)
	}
	// A planted key, window and exact mismatch in the delay run, and a key
	// mismatch against none, are counted one each.
	bad := *fDrip
	bad.Qs = slices.Clone(fDrip.Qs)
	bad.Qs[0].K++
	bad.Qs[4].Wi = 1
	bad.Qs[5].Exact++
	got := swSplitPair(sd, jd, fDef, &bad, fDef0, fDrip0)
	for _, f := range []string{" keymism=1 ", " winmism=1 ", " exactmism=1 ", " keymism0=1 ", " winmism0=1 "} {
		if !strings.Contains(got, f) {
			t.Errorf("planted mismatches: %q not in %s", f, got)
		}
	}
}

// swSplitEnv is TF_W12B_SEEDS (default every seed: tfbench's (9,9) and the
// fresh (2026,41)-(2026,44)) and TF_W12B_DELAYS (default every delay), in
// W12's syntax.
func swSplitEnv(t *testing.T) ([][2]uint64, []swLagDelay) {
	var seeds [][2]uint64
	for _, s := range swLagEnvList("TF_W12B_SEEDS", "9,9;2026,41;2026,42;2026,43;2026,44") {
		var a, b uint64
		if _, err := fmt.Sscanf(s, "%d,%d", &a, &b); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
		seeds = append(seeds, [2]uint64{a, b})
	}
	delays := swLagDelays()
	if v := os.Getenv("TF_W12B_DELAYS"); v != "" {
		var ds []swLagDelay
		for _, s := range strings.Split(v, ";") {
			for _, d := range delays {
				if d.String() == s {
					ds = append(ds, d)
				}
			}
		}
		if len(ds) == 0 {
			t.Fatalf("TF_W12B_DELAYS=%q names no delay", v)
		}
		delays = ds
	}
	return seeds, delays
}

// TestShortWindowLagSplit (TF_EXP=1) is W12b: W12's leg A, plain mode, at
// every seed and delay of TF_W12B_SEEDS and TF_W12B_DELAYS, with each query's
// class (swSplitA). It writes a per-query file in TF_W12B_Q for each run
// (swSplitWrite), naming TF_W12B_HEAD, the library's commit as 40 hex
// digits, and prints W12's line for the run beside its own. It judges
// nothing: TestShortWindowLagSplitJoin reads the files of both builds.
func TestShortWindowLagSplit(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	dir := os.Getenv("TF_W12B_Q")
	if dir == "" {
		t.Fatal("TF_W12B_Q: the directory of the per-query files")
	}
	hd := os.Getenv("TF_W12B_HEAD")
	if _, err := hex.DecodeString(hd); err != nil || len(hd) != 40 {
		t.Fatalf("TF_W12B_HEAD=%q: 40 hex digits", hd)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	seeds, delays := swSplitEnv(t)
	build, bi := "default", uint8(0)
	if dripOn {
		build, bi = "tfdrip", 1
	}
	runs := 0
	for _, sd := range seeds {
		rec := swLagRecord(t, sd[0], sd[1])
		var base []int32
		for _, d := range delays {
			arr, order := swLagOrder(rec, sd[0], sd[1], d)
			r, s, secs := swSplitA(t, rec, arr, order)
			mism := -1
			if d.model == "none" {
				if base == nil {
					sr := swRecord(t, sd[0], sd[1])
					base = make([]int32, len(sr.qs))
					for i, q := range sr.qs {
						base[i] = int32(q.filter)
					}
				}
				mism = swLagMismatch(r.answers, base)
			}
			swLagLine(t, build, "A", sd, d, "plain", "-", 0, r, mism)
			h := swSplitHead{Magic: swSplitMagic, Version: 1, Build: bi, S1: sd[0], S2: sd[1], Model: swSplitModel[d.model], J: d.j}
			copy(h.Head[:], hd)
			path := swSplitPath(dir, build, sd, d)
			if err := swSplitWrite(path, h, s.qs); err != nil {
				t.Fatal(err)
			}
			t.Logf("W12b run: build=%s seed=%d,%d delay=%s q_none=%d q_nolog=%d q_dead=%d q_clean=%d q_off=%d hotdead=%d lastbad=%d modelmism=%d clsmism=%d qmoved=%d recs=%d file=%s secs=%.1f",
				build, sd[0], sd[1], d, s.q[swSplitNone], s.q[swSplitNoLog], s.q[swSplitDead], s.q[swSplitClean], s.q[swSplitOff],
				s.hotDead, s.lastBad, s.modelMism, s.clsMism, s.qMoved, len(s.qs), path, secs)
			runs++
		}
	}
	t.Logf("W12b done: build=%s runs=%d", build, runs)
}

// TestShortWindowLagSplitJoin (TF_EXP=1) joins the per-query files in
// TF_W12B_Q, those of the seeds and delays TF_W12B_SEEDS and TF_W12B_DELAYS
// name, in both builds: each seed's none files and each delay's, a line per
// pair (swSplitPair). A file that cannot be read, or a pair whose files do
// not name the same run (swSplitCheck), is an error, and the pair is
// skipped. It reads no filter.
func TestShortWindowLagSplitJoin(t *testing.T) {
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	dir := os.Getenv("TF_W12B_Q")
	if dir == "" {
		t.Fatal("TF_W12B_Q: the directory of the per-query files")
	}
	seeds, delays := swSplitEnv(t)
	nd := swLagDelay{"none", 0}
	pairs := 0
	for _, sd := range seeds {
		read := func(build string, d swLagDelay) *swSplitFile {
			h, qs, es, xs, err := swSplitRead(swSplitPath(dir, build, sd, d))
			if err != nil {
				t.Errorf("%v", err)
				return nil
			}
			return &swSplitFile{H: h, Qs: qs, Es: es, Xs: xs}
		}
		def0, drip0 := read("default", nd), read("tfdrip", nd)
		for _, d := range delays {
			def, drip := def0, drip0
			if d.model != "none" {
				def, drip = read("default", d), read("tfdrip", d)
			}
			if def == nil || drip == nil || def0 == nil || drip0 == nil {
				t.Errorf("seed %d,%d delay %s: a file is missing, the pair is skipped", sd[0], sd[1], d)
				continue
			}
			if err := swSplitCheck(sd, d, def, drip, def0, drip0); err != nil {
				t.Errorf("seed %d,%d delay %s: the pair is skipped: %v", sd[0], sd[1], d, err)
				continue
			}
			t.Log(swSplitPair(sd, d, def, drip, def0, drip0))
			pairs++
		}
	}
	t.Logf("W12b join done: pairs=%d", pairs)
}
