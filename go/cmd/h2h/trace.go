package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"time"

	tf "github.com/scooter-sensei/clepsydra/go"
)

// A trace is one flood experiment of part 1 (docs/artifact/head-to-head.md),
// written once by `h2h gen` and replayed by every arm. Little-endian: a
// 128-byte header, the records in tick order (count events of key at tick),
// then the probes (key, true count in the window at ProbeTick, class).
// bench/h2h/src/main.rs reads the same layout.

const traceMagic = "CLPH2H01"

type header struct {
	Capacity, Slots, HorizonMs, WindowMs, Limit uint64
	Mult, Weight                                uint64
	Records, Probes, ProbeTick, Ops, Seed       uint64
}

func (h *header) fields() []*uint64 {
	return []*uint64{&h.Capacity, &h.Slots, &h.HorizonMs, &h.WindowMs, &h.Limit, &h.Mult, &h.Weight,
		&h.Records, &h.Probes, &h.ProbeTick, &h.Ops, &h.Seed}
}

type record struct {
	key         uint64
	tick, count uint32
}

type probe struct {
	key          uint64
	truth, class uint32
}

// Probe classes.
const (
	classOffender = iota // at the limit: must be denied
	classFlood           // a flood key: denied if heavy (over the limit), allowed if light
	classBase            // one event: must be allowed
	classNew             // never sent: must be allowed
)

var classNames = []string{"offender", "flood", "base", "new"}

type trace struct {
	header
	recs   []record
	probes []probe
	sha    string
}

// The traffic of part 1: within one horizon, at 1 ms ticks, the base load
// (one event per key, ~N keys) over ticks 1-5,000, offenders (20 events
// each, one tick apiece) from 5,001, the flood (mult x slots keys of weight
// events, one tick per key) over 6,001-36,000, probes at 36,001.
const (
	offenders    = 1000
	probesPer    = 20_000
	limit        = 20
	horizonMs    = 60_000
	baseEnd      = 5000
	floodStart   = 6001
	floodTicks   = 30_000
	probeTickVal = floodStart + floodTicks
)

// floods are part 1's experiments: heavy floods of 1x-16x the table's slots
// in keys one over the limit, and a light flood of one-event keys.
var floods = []struct{ mult, weight int }{{1, limit + 1}, {2, limit + 1}, {4, limit + 1}, {8, limit + 1}, {16, limit + 1}, {16, 1}}

func floodName(mult, weight uint64) string {
	if weight == 1 {
		return fmt.Sprintf("light%dx", mult)
	}
	return fmt.Sprintf("heavy%dx", mult)
}

func generate(capacity int, mult, weight int, seed uint64) *trace {
	f, err := tf.New(tf.Config{Capacity: capacity, Horizon: horizonMs * time.Millisecond, Resolution: time.Millisecond})
	if err != nil {
		panic(err)
	}
	slots := f.Slots()
	t := &trace{header: header{Capacity: uint64(capacity), Slots: uint64(slots), HorizonMs: horizonMs, WindowMs: horizonMs,
		Limit: limit, Mult: uint64(mult), Weight: uint64(weight), ProbeTick: probeTickVal, Seed: seed}}
	rng := rand.New(rand.NewPCG(seed, uint64(mult*100+weight)))
	base := slots * 85 / 100
	for i := range base {
		t.recs = append(t.recs, record{rng.Uint64(), uint32(1 + i*baseEnd/base), 1})
	}
	for i := range offenders {
		t.recs = append(t.recs, record{rng.Uint64(), uint32(baseEnd + 1 + i), limit})
	}
	nf := mult * slots
	for i := range nf {
		t.recs = append(t.recs, record{rng.Uint64(), uint32(floodStart + i*floodTicks/nf), uint32(weight)})
	}
	for i := range offenders {
		t.probes = append(t.probes, probe{t.recs[base+i].key, limit, classOffender})
	}
	for i := range probesPer {
		t.probes = append(t.probes, probe{t.recs[base+offenders+i*nf/probesPer].key, uint32(weight), classFlood})
	}
	for i := range probesPer {
		t.probes = append(t.probes, probe{t.recs[i*base/probesPer].key, 1, classBase})
	}
	for range probesPer {
		t.probes = append(t.probes, probe{rng.Uint64(), 0, classNew})
	}
	// Truths hold only if every key is distinct: check.
	keys := make([]uint64, 0, len(t.recs)+probesPer)
	for _, r := range t.recs {
		keys = append(keys, r.key)
		t.Ops += uint64(r.count)
	}
	for _, p := range t.probes[len(t.probes)-probesPer:] {
		keys = append(keys, p.key)
	}
	slices.Sort(keys)
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			panic("duplicate key; pick another seed")
		}
	}
	t.Records, t.Probes = uint64(len(t.recs)), uint64(len(t.probes))
	return t
}

func (t *trace) write(path string) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := bufio.NewWriterSize(fh, 1<<20)
	var hdr [128]byte
	copy(hdr[:], traceMagic)
	for i, p := range t.fields() {
		binary.LittleEndian.PutUint64(hdr[8+8*i:], *p)
	}
	w.Write(hdr[:])
	var b [16]byte
	for _, r := range t.recs {
		binary.LittleEndian.PutUint64(b[:], r.key)
		binary.LittleEndian.PutUint32(b[8:], r.tick)
		binary.LittleEndian.PutUint32(b[12:], r.count)
		w.Write(b[:])
	}
	for _, p := range t.probes {
		binary.LittleEndian.PutUint64(b[:], p.key)
		binary.LittleEndian.PutUint32(b[8:], p.truth)
		binary.LittleEndian.PutUint32(b[12:], p.class)
		w.Write(b[:])
	}
	return w.Flush()
}

func readTrace(path string) (*trace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 128 || string(data[:8]) != traceMagic {
		return nil, fmt.Errorf("%s: not a trace", path)
	}
	sum := sha256.Sum256(data)
	t := &trace{sha: hex.EncodeToString(sum[:8])}
	for i, p := range t.fields() {
		*p = binary.LittleEndian.Uint64(data[8+8*i:])
	}
	if want := 128 + 16*(t.Records+t.Probes); uint64(len(data)) != want {
		return nil, fmt.Errorf("%s: %d bytes, want %d", path, len(data), want)
	}
	off := 128
	t.recs = make([]record, t.Records)
	for i := range t.recs {
		b := data[off:]
		t.recs[i] = record{binary.LittleEndian.Uint64(b), binary.LittleEndian.Uint32(b[8:]), binary.LittleEndian.Uint32(b[12:])}
		off += 16
	}
	t.probes = make([]probe, t.Probes)
	for i := range t.probes {
		b := data[off:]
		t.probes[i] = probe{binary.LittleEndian.Uint64(b), binary.LittleEndian.Uint32(b[8:]), binary.LittleEndian.Uint32(b[12:])}
		off += 16
	}
	return t, nil
}

// cmdGen writes every experiment's trace into dir.
func cmdGen(dir string, capacity int, seed uint64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, fl := range floods {
		t := generate(capacity, fl.mult, fl.weight, seed)
		path := filepath.Join(dir, floodName(t.Mult, t.Weight)+".trace")
		if err := t.write(path); err != nil {
			return err
		}
		fmt.Printf("%s: %d keys, %d slots, %d records, %d ops, %d probes\n", path, t.Capacity, t.Slots, t.Records, t.Ops, t.Probes)
	}
	return nil
}

// answer is one arm's reply to one probe: its estimate, its bound and its
// strict decision. Answer files hold one 12-byte record per probe, in probe
// order.
type answer struct {
	est, upper int32
	deny       bool
}

func writeAnswers(path string, as []answer) error {
	buf := make([]byte, 12*len(as))
	for i, a := range as {
		binary.LittleEndian.PutUint32(buf[12*i:], uint32(a.est))
		binary.LittleEndian.PutUint32(buf[12*i+4:], uint32(a.upper))
		if a.deny {
			buf[12*i+8] = 1
		}
	}
	return os.WriteFile(path, buf, 0o644)
}

func readAnswers(path string) ([]answer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data)%12 != 0 {
		return nil, io.ErrUnexpectedEOF
	}
	as := make([]answer, len(data)/12)
	for i := range as {
		as[i] = answer{int32(binary.LittleEndian.Uint32(data[12*i:])), int32(binary.LittleEndian.Uint32(data[12*i+4:])), data[12*i+8] != 0}
	}
	return as, nil
}

// part is the thread a key's records go to in the multi-threaded passes:
// the same function in bench/h2h, so both languages split alike.
func part(key uint64, threads int) int {
	return int((key * 0x9e3779b97f4a7c15 >> 32) % uint64(threads))
}
