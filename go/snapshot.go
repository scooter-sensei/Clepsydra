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

// Snapshot format, version 2 (claims D27; version 1 was E38's unversioned
// stream, never in the library). A stream of little-endian 64-bit words in
// six sections, each ended by a check word:
//
//	0 header:   magic "CLEPSNP1", version (2), the Config's shape (11 words:
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
//
// A check word holds the CRC32C (Castagnoli) of its section's bytes (every
// word since the previous check word) in bits 0-31 and the section number
// in bits 32-39; nothing follows section 5. ReadFrom refuses a bad magic,
// another version or Config, a short stream, a check word that doesn't
// match and trailing bytes; the filter it read into is then unspecified and
// is to be discarded. The check words catch accidental damage (every
// truncation and single-bit flip tried), not a crafted stream: a snapshot
// is trusted input.
const snapVersion = 2

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
	if s.err == nil {
		s.err = bw.Flush()
	}
	return s.n, s.err
}

// ReadFrom restores a snapshot WriteTo wrote into f, which New made with the
// same Config and nothing has used yet.
func (f *Filter) ReadFrom(r io.Reader) (int64, error) {
	if f.ct || f.bg || f.cms {
		return 0, errSnapMode
	}
	s := &snapR{r: bufio.NewReader(r)}
	if s.u() != snapMagic {
		s.fail("bad magic")
	}
	if v := s.u(); v != snapVersion {
		s.fail(fmt.Sprintf("version %d, want %d", v, snapVersion))
	}
	for _, x := range f.snapShape() {
		if s.u() != x {
			s.fail("another Config")
		}
	}
	oldNow, wall := s.u(), int64(s.u())
	s.end()
	if s.err != nil {
		return s.n, s.err
	}
	for i := range f.words {
		f.words[i] = s.u()
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
			e.c0.Store(s.u())
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
	if s.err == nil {
		if _, err := s.r.ReadByte(); err != io.EOF {
			s.fail("trailing bytes")
		}
	}
	if s.err == nil && f.ownClock {
		target := oldNow + uint64(max(0, time.Now().UnixNano()-wall))/uint64(f.res)
		if cur := f.clock(); target > cur {
			f.clockAdd.Add(target - cur)
			if f.stop != nil { // CoarseClock: the cached tick moves on too
				f.coarse.Add(target - cur)
			}
		}
	}
	return s.n, s.err
}
