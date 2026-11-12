package temporalfilter

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"
)

// Snapshot format, version 3 (claims D27, D29; version 1 was E38's
// unversioned stream, never in the library; version 2 held each register's
// unit in word1's bits 48-51, which version 3 gives to the onset). A
// stream of little-endian 64-bit words in seven sections (six in version
// 2), each ended by a check word:
//
//	0 header:   magic "CLEPSNP1", version (3), the Config's shape (11 words:
//	            lines, horizon, unit, resolution, stash sets, hot entries,
//	            overflow bytes, overflow choices, floors per bucket, SpillMin,
//	            count-min bytes), the clock anchor (tick, wall-clock ns)
//	1 table:    lines x 8 words, as the table holds them
//	2 counters: decay cursor, evictions, relocations, pressure, stash gate,
//	            lostAt, lost, restored, folded, shed, waited, promoted,
//	            hot refused
//	3 stash:    presence (0/1); if 1: sets x 4 entries (id, word0, word1),
//	            the wait bitmap's words, the placement cursor
//	4 hot:      presence; if 1: entries (version, id, epoch, saturated unit,
//	            bucket-0 word, buckets 1-6 two to a word), the free list
//	            (length, then one index per word), next, hand, full, in use,
//	            demoted
//	5 overflow: presence; if 1: the table's words, epoch, until, saturated
//	            mark, floored
//	6 buzz:     presence; if 1: entries (id, word0, word1, meta, the ring's
//	            four words); version 3 only (claims A46)
//
// A check word holds the CRC32C (Castagnoli) of its section's bytes (every
// word since the previous check word) in bits 0-31 and the section number
// in bits 32-39; nothing follows section 6 (5 in version 2). ReadFrom refuses a bad magic,
// another version or Config, a short stream, a check word that doesn't
// match and trailing bytes; the filter it read into is then unspecified and
// is to be discarded. The check words catch accidental damage (every
// truncation and single-bit flip tried), not a crafted stream: a snapshot
// is trusted input.
const snapVersion = 3

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Snapshots (claims D27): WriteTo and ReadFrom save and restore a quiescent
// filter's state (the table, stash, hot entries and overflow table, the
// decay cursor and counters) into a filter New made with the same Config.
// With the built-in clock the restored filter's clock continues the old
// timeline (the old tick plus the wall time since the snapshot), so stored
// ticks stay valid; a caller's Clock must continue it itself. String keys
// need Config.Key: the default seed is per process. ConstantTime,
// Background and the count-min overflow are refused.

const snapMagic = 0x31504e5350454c43 // "CLEPSNP1"

var errSnapMode = errors.New("temporalfilter: snapshots of ConstantTime, Background or count-min filters are not supported")

func (f *Filter) snapShape() []uint64 {
	return []uint64{f.lines, f.horizon, f.unit, uint64(f.res), f.stashN, uint64(f.hotN), f.skW,
		uint64(f.ghChoices), uint64(f.ghSub), f.spillMin, uint64(f.skShape.bytes(f.skW))}
}

type snapW struct {
	w        *bufio.Writer
	n        int64
	err      error
	crc, sec uint32
}

// end writes the section's check word.
func (s *snapW) end() {
	x := uint64(s.crc) | uint64(s.sec)<<32
	s.u(x)
	s.crc, s.sec = 0, s.sec+1
}

func (s *snapW) u(x uint64) {
	if s.err == nil {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], x)
		s.crc = crc32.Update(s.crc, castagnoli, b[:])
		m, err := s.w.Write(b[:])
		s.n, s.err = s.n+int64(m), err
	}
}

type snapR struct {
	r        *bufio.Reader
	n        int64
	err      error
	crc, sec uint32
}

// end reads and checks the section's check word.
func (s *snapR) end() {
	want := uint64(s.crc) | uint64(s.sec)<<32
	if got := s.u(); s.err == nil && got != want {
		s.fail(fmt.Sprintf("section %d: check word %#x, want %#x", s.sec, got, want))
	}
	s.crc, s.sec = 0, s.sec+1
}

func (s *snapR) u() uint64 {
	if s.err != nil {
		return 0
	}
	var b [8]byte
	m, err := io.ReadFull(s.r, b[:])
	s.n, s.err = s.n+int64(m), err
	if err != nil {
		s.err = errors.New("temporalfilter: snapshot: short stream")
		return 0
	}
	s.crc = crc32.Update(s.crc, castagnoli, b[:])
	return binary.LittleEndian.Uint64(b[:])
}

func (s *snapR) fail(msg string) {
	if s.err == nil {
		s.err = errors.New("temporalfilter: snapshot: " + msg)
	}
}

// WriteTo writes f's state to w. f must be quiescent.
func (f *Filter) WriteTo(w io.Writer) (int64, error) {
	if f.ct || f.bg || f.cms {
		return 0, errSnapMode
	}
	bw := bufio.NewWriter(w)
	s := &snapW{w: bw}
	s.u(snapMagic)
	s.u(snapVersion)
	for _, x := range f.snapShape() {
		s.u(x)
	}
	s.u(f.clock())
	s.u(uint64(time.Now().UnixNano()))
	s.end()
	for i := range f.words {
		s.u(f.words[i])
	}
	s.end()
	for _, x := range []uint64{f.cursor.Load(), f.evictions.Load(), f.relocations.Load(), uint64(f.pressure.Load()),
		f.stashUntil.Load(), f.lostAt.Load(), f.lost.Load(), f.restored.Load(), f.folded.Load(), f.shed.Load(),
		f.waited.Load(), f.promoted.Load(), f.hotRefused.Load()} {
		s.u(x)
	}
	s.end()
	if st := f.stash.Load(); st != nil {
		s.u(1)
		for i := range st.e {
			s.u(st.e[i].id)
			s.u(st.e[i].w0)
			s.u(st.e[i].w1)
		}
		for i := range st.wait {
			s.u(st.wait[i].Load())
		}
		s.u(st.at.Load())
	} else {
		s.u(0)
	}
	s.end()
	if t := f.hot.Load(); t != nil {
		s.u(1)
		for i := range t.e {
			e := &t.e[i]
			s.u(e.ver.Load())
			s.u(e.id.Load())
			s.u(uint64(e.epoch.Load()))
			s.u(uint64(e.sat.Load()))
			s.u(e.c0.Load())
			for j := 0; j < len(e.c); j += 2 {
				s.u(uint64(e.c[j].Load()) | uint64(e.c[j+1].Load())<<32)
			}
		}
		s.u(uint64(len(t.free)))
		for _, x := range t.free {
			s.u(uint64(x))
		}
		s.u(t.next)
		s.u(t.hand)
		s.u(uint64(t.full.Load()))
		s.u(uint64(t.inUse.Load()))
		s.u(t.demoted.Load())
	} else {
		s.u(0)
	}
	s.end()
	if g := f.gh.Load(); g != nil {
		s.u(1)
		for i := range g.words {
			s.u(g.words[i])
		}
		s.u(g.epoch.Load())
		s.u(g.until.Load())
		sat := uint64(0)
		if g.sat.Load() {
			sat = 1
		}
		s.u(sat)
		s.u(g.floored.Load())
	} else {
		s.u(0)
	}
	s.end()
	if t := f.buzz.Load(); t != nil {
		s.u(1)
		for i := range t.b {
			b := &t.b[i]
			for j := range b.ids {
				e := &b.e[j]
				for _, x := range [...]uint64{b.ids[j], e.w0, e.w1, e.meta, e.ev[0], e.ev[1], e.ev[2], e.ev[3]} {
					s.u(x)
				}
			}
		}
	} else {
		s.u(0)
	}
	s.end()
	if s.err == nil {
		s.err = bw.Flush()
	}
	return s.n, s.err
}

// continued is the built-in clock's tick for a snapshot written at tick
// oldNow and wall-clock wall (Unix ns): the old tick plus the wall time since.
func (f *Filter) continued(oldNow uint64, wall int64) uint64 {
	return oldNow + uint64(max(0, time.Now().UnixNano()-wall))/uint64(f.res)
}

// ReadFrom restores a snapshot WriteTo wrote into f, which New made with the
// same Config and nothing has used yet. With the built-in clock it refuses a
// snapshot whose continued clock would be past MaxTick.
func (f *Filter) ReadFrom(r io.Reader) (int64, error) {
	if f.ct || f.bg || f.cms {
		return 0, errSnapMode
	}
	s := &snapR{r: bufio.NewReader(r)}
	if s.u() != snapMagic {
		s.fail("bad magic")
	}
	v := s.u()
	if v != snapVersion && v != 2 {
		s.fail(fmt.Sprintf("version %d, want %d or 2", v, snapVersion))
	}
	for _, x := range f.snapShape() {
		if s.u() != x {
			s.fail("another Config")
		}
	}
	oldNow, wall := s.u(), int64(s.u())
	s.end()
	// The built-in clock continues the old timeline (below), so a lineage of
	// snapshots can carry it past MaxTick, where hits read wrong until an
	// insert panics (claims A35): refuse it before anything is written.
	if s.err == nil && f.ownClock && f.continued(oldNow, wall) > MaxTick {
		s.fail("the continued clock is past MaxTick")
	}
	if s.err != nil {
		return s.n, s.err
	}
	for i := range f.words {
		f.words[i] = s.u()
	}
	if v == 2 {
		for i := 0; i < len(f.words); i += 2 {
			f.words[i], f.words[i+1] = f.fromV2(f.words[i], f.words[i+1])
		}
	}
	s.end()
	f.cursor.Store(s.u())
	f.evictions.Store(s.u())
	f.relocations.Store(s.u())
	f.pressure.Store(uint32(s.u()))
	f.stashUntil.Store(s.u())
	f.lostAt.Store(s.u())
	f.lost.Store(s.u())
	f.restored.Store(s.u())
	f.folded.Store(s.u())
	f.shed.Store(s.u())
	f.waited.Store(s.u())
	f.promoted.Store(s.u())
	f.hotRefused.Store(s.u())
	s.end()
	if s.u() == 1 {
		st := newStashTable(f.stashN, &f.stashMu)
		for i := range st.e {
			st.e[i].id, st.e[i].w0, st.e[i].w1 = s.u(), s.u(), s.u()
			if v == 2 {
				st.e[i].w0, st.e[i].w1 = f.fromV2(st.e[i].w0, st.e[i].w1)
			}
		}
		for i := range st.wait {
			st.wait[i].Store(s.u())
		}
		st.at.Store(s.u())
		f.stash.Store(st)
	}
	s.end()
	if s.u() == 1 {
		if f.hotN == 0 {
			s.fail("hot entries in a filter without them")
			return s.n, s.err
		}
		t := f.hotTable()
		for i := range t.e {
			e := &t.e[i]
			e.ver.Store(s.u())
			e.id.Store(s.u())
			e.epoch.Store(int64(s.u()))
			e.sat.Store(int64(s.u()))
			c0 := s.u()
			if v == 2 {
				c0 &^= hotOnMask // generation bits there; an onset of 0 says nothing
			}
			e.c0.Store(c0)
			for j := 0; j < len(e.c); j += 2 {
				x := s.u()
				e.c[j].Store(uint32(x))
				e.c[j+1].Store(uint32(x >> 32))
			}
		}
		n := s.u()
		if n > uint64(len(t.e)) {
			s.fail("free list")
			return s.n, s.err
		}
		t.free = make([]uint32, n)
		for i := range t.free {
			t.free[i] = uint32(s.u())
		}
		t.next, t.hand = s.u(), s.u()
		t.full.Store(int64(s.u()))
		t.inUse.Store(int64(s.u()))
		t.demoted.Store(s.u())
	}
	s.end()
	if s.u() == 1 {
		g := f.ghostTable()
		for i := range g.words {
			g.words[i] = s.u()
		}
		g.epoch.Store(s.u())
		g.until.Store(s.u())
		g.sat.Store(s.u() == 1)
		g.floored.Store(s.u())
	}
	s.end()
	if v >= 3 && s.u() == 1 {
		t := f.buzzTable()
		for i := range t.b {
			b := &t.b[i]
			for j := range b.ids {
				e := &b.e[j]
				b.ids[j], e.w0, e.w1, e.meta = s.u(), s.u(), s.u(), s.u()
				for k := range e.ev {
					e.ev[k] = s.u()
				}
			}
			t.mark(uint64(i))
		}
	}
	if v >= 3 {
		s.end()
	}
	if s.err == nil {
		if _, err := s.r.ReadByte(); err != io.EOF {
			s.fail("trailing bytes")
		}
	}
	if s.err == nil && f.ownClock {
		target := f.continued(oldNow, wall)
		if target > MaxTick { // crossed while the sections were read
			s.fail("the continued clock is past MaxTick")
		} else if cur := f.clock(); target > cur {
			f.clockAdd.Add(target - cur)
			if f.stop != nil { // CoarseClock: the cached tick moves on too
				f.coarse.Add(target - cur)
			}
		}
	}
	return s.n, s.err
}

// fromV2 is a version 2 snapshot's slot pair (w0, w1) as version 3 holds it.
// A register there kept 4 bits of its unit, read against its last touch's
// (as regEpoch did): it is moved to that unit, where a quiescent filter's
// registers are already, and its onset is 0, which says nothing, so it
// answers as it did. One ahead of its last touch (never at rest) keeps its
// unit, its last touch moved to that unit's start.
func (f *Filter) fromV2(w0, w1 uint64) (uint64, uint64) {
	if w0 == 0 || w1 == 0 || w1>>tagShift == tagBusy || isHot(w1) {
		return w0, w1
	}
	u := f.unitOf(w0 & tsMask)
	d := (u - int64(w1>>onsetShift)) & onsetMask
	if d > onsetMask/2 {
		d -= onsetMask + 1
	}
	r := unpack(w1 & regMask)
	switch e := u - d; {
	case e < u:
		if r.advance(e, u) {
			w0 |= satBit
		}
	case e > u:
		w0 = w0&^tsMask | uint64(e)*f.unit
	}
	return w0, w1&^(onsetMask<<onsetShift|regMask) | r.pack()
}
