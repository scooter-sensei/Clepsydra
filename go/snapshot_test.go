package temporalfilter

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"testing"
	"time"
)

// Snapshots (claims D27): answers across a restore, the format's refusals,
// its continuity across builds (toolchains, architectures) and its size.

var snapCfg = Config{Capacity: 512, Horizon: time.Minute, Resolution: time.Millisecond}

func snapKey(k int) uint64 { return mix64(uint64(k) + 1) }

func skipSnapModes(t *testing.T) {
	t.Helper()
	if forceCT || forceBG {
		t.Skip("snapshots refuse ConstantTime and Background filters")
	}
}

// TestSnapshot runs tfbench's velocity workload once uninterrupted and once
// snapshotted at 150 s and restored into a new filter, at Capacity 512 so
// the stash, the overflow table and hot entries are all in use across the
// snapshot: every answer must be the same. Then the built-in clock: a
// filter restored 20 ms after its snapshot continues the old timeline.
func TestSnapshot(t *testing.T) {
	skipSnapModes(t)
	wins := []uint64{1_000, 5_000, 15_000, 30_000, 60_000}
	run := func(snapAt uint64) (ans []Result, f *Filter, size, bytesAt int) {
		f, _ = New(snapCfg)
		velWorkload(func(k int, at uint64) {
			ans = append(ans, f.TouchAt(snapKey(k), 60_000, at))
		}, func(k int, _ []uint64, wi int, now uint64) {
			if now == snapAt {
				var buf bytes.Buffer
				if _, err := f.WriteTo(&buf); err != nil {
					t.Fatal(err)
				}
				size, bytesAt = buf.Len(), f.Bytes()
				g, _ := New(snapCfg)
				if _, err := g.ReadFrom(&buf); err != nil {
					t.Fatal(err)
				}
				f = g
			}
			ans = append(ans, f.CountAt(snapKey(k), wins[wi], now))
		})
		return ans, f, size, bytesAt
	}
	u, fu, _, _ := run(0)
	r, fr, size, bytesAt := run(150_000)
	diff, first := 0, -1
	for i := range min(len(u), len(r)) {
		if u[i] != r[i] {
			diff++
			if first < 0 {
				first = i
			}
		}
	}
	t.Logf("answers %d / %d, differing %d (first at %d); end: evictions %d / %d, folded %d / %d, promoted %d / %d, floored %d / %d",
		len(u), len(r), diff, first, fu.Evictions(), fr.Evictions(), fu.folded.Load(), fr.folded.Load(), fu.promoted.Load(), fr.promoted.Load(), fu.Floored(), fr.Floored())
	t.Logf("snapshot %d bytes, Bytes() %d at the snapshot (%.3f%%)", size, bytesAt, 100*float64(size-bytesAt)/float64(bytesAt))
	if diff != 0 || len(u) != len(r) {
		t.Errorf("a restore changed answers")
	}
	if fr.Evictions() == 0 || fr.folded.Load() == 0 || fr.promoted.Load() == 0 {
		t.Errorf("the stash, overflow table or hot entries were not in use")
	}
	c, _ := New(Config{Capacity: 512, Horizon: time.Minute})
	c.TouchAt(1, 0, c.clock())
	var buf bytes.Buffer
	if _, err := c.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	old := c.clock()
	time.Sleep(20 * time.Millisecond)
	d, _ := New(Config{Capacity: 512, Horizon: time.Minute})
	if _, err := d.ReadFrom(&buf); err != nil {
		t.Fatal(err)
	}
	now := d.clock()
	t.Logf("clock: old %d, restored %d (+%d ticks, want >= %d)", old, now, now-old, c.Ticks(20*time.Millisecond))
	if now < old+c.Ticks(20*time.Millisecond) {
		t.Errorf("the restored clock did not continue the old timeline")
	}
}

// snapHalf runs the velocity workload's first half (to 150 s) into a new
// filter and returns it.
func snapHalf() *Filter {
	f, _ := New(snapCfg)
	velWorkload(func(k int, at uint64) {
		if at < 150_000 {
			f.TouchAt(snapKey(k), 60_000, at)
		}
	}, func(int, []uint64, int, uint64) {})
	return f
}

// snapRest runs the second half on f (from 150 s) and hashes every answer.
func snapRest(f *Filter) (uint64, int) {
	h := fnv.New64a()
	n := 0
	put := func(r Result) {
		fmt.Fprintf(h, "%v", r)
		n++
	}
	velWorkload(func(k int, at uint64) {
		if at >= 150_000 {
			put(f.TouchAt(snapKey(k), 60_000, at))
		}
	}, func(k int, _ []uint64, wi int, now uint64) {
		if now >= 150_000 {
			put(f.CountAt(snapKey(k), velWins[wi], now))
		}
	})
	return h.Sum64(), n
}

func crc32c(b []byte) uint32 { return crc32.Update(0, castagnoli, b) }

// TestSnapshotCorrupt: a snapshot cut at every length to 512, every 997th
// and the last 16, one with a trailing byte, and one bit flipped at 2,000
// seeded positions and at the first and last byte of every section: every
// one is refused.
func TestSnapshotCorrupt(t *testing.T) {
	skipSnapModes(t)
	f := snapHalf()
	var buf bytes.Buffer
	if _, err := f.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	snap := buf.Bytes()
	g, _ := New(snapCfg)
	if _, err := g.ReadFrom(bytes.NewReader(snap)); err != nil {
		t.Fatalf("intact snapshot refused: %v", err)
	}
	// The sections' ends: each a word holding the section's number above
	// the CRC32C of the bytes since the previous end.
	var ends []int
	for i := 8; i+8 <= len(snap); i += 8 {
		w := binary.LittleEndian.Uint64(snap[i:])
		if w>>32 == uint64(len(ends)) {
			from := 0
			if len(ends) > 0 {
				from = ends[len(ends)-1]
			}
			if uint64(crc32c(snap[from:i])) == w&0xffffffff {
				ends = append(ends, i+8)
			}
		}
	}
	accepted := 0
	try := func(b []byte) {
		h, _ := New(snapCfg)
		if _, err := h.ReadFrom(bytes.NewReader(b)); err == nil {
			accepted++
		}
	}
	cuts := 0
	for n := 0; n < len(snap); n++ {
		if n <= 512 || n%997 == 0 || n >= len(snap)-16 {
			try(snap[:n])
			cuts++
		}
	}
	try(append(append([]byte(nil), snap...), 0))
	cuts++
	flips := 0
	r := rand.New(rand.NewPCG(16, 16))
	pos := []int{}
	for range 2000 {
		pos = append(pos, r.IntN(len(snap)))
	}
	prev := 0
	for _, e := range ends {
		pos = append(pos, prev, e-1)
		prev = e
	}
	for _, p := range pos {
		b := append([]byte(nil), snap...)
		b[p] ^= 1 << (p % 8)
		try(b)
		flips++
	}
	t.Logf("snapshot %d bytes, sections ending at %v; truncations %d and bit flips %d tried, accepted %d", len(snap), ends, cuts, flips, accepted)
	if accepted != 0 || len(ends) != 6 {
		t.Errorf("a corrupted or truncated snapshot was accepted (%d), or the sections were not found (%d)", accepted, len(ends))
	}
}

// TestSnapshotCross: SNAP_OUT=<file> writes the first half's snapshot;
// SNAP_IN=<file> restores it, finishes the workload and compares the answers
// with this build's uninterrupted run (another toolchain or architecture
// wrote the file).
func TestSnapshotCross(t *testing.T) {
	skipSnapModes(t)
	if out := os.Getenv("SNAP_OUT"); out != "" {
		var buf bytes.Buffer
		if _, err := snapHalf().WriteTo(&buf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d bytes", buf.Len())
		return
	}
	in := os.Getenv("SNAP_IN")
	if in == "" {
		t.Skip("set SNAP_OUT or SNAP_IN")
	}
	data, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := New(snapCfg)
	if _, err := g.ReadFrom(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	hr, nr := snapRest(g)
	hu, nu := snapRest(snapHalf())
	t.Logf("restored from %s: %d answers, hash %x; uninterrupted here: %d answers, hash %x", in, nr, hr, nu, hu)
	if hr != hu || nr != nu {
		t.Errorf("answers differ after a restore across builds")
	}
}

// TestSnapshotSize (TF_EXP=1): a snapshot is within 1% of Bytes() at
// Capacity 200,000 and 1,000,000 (3 x Capacity keys over 30 s: the stash
// allocated), and at 200,000 with the stash, the hot table and the overflow
// table all allocated (a 64-entry stash and SpillMin 1, so every dropped
// history folds; 100 keys touched 64 times in one tick take hot entries).
func TestSnapshotSize(t *testing.T) {
	skipSnapModes(t)
	if os.Getenv("TF_EXP") == "" {
		t.Skip("set TF_EXP=1")
	}
	check := func(name string, f *Filter, all bool) {
		var buf bytes.Buffer
		if _, err := f.WriteTo(&buf); err != nil {
			t.Fatal(err)
		}
		st, hot, gh := f.stash.Load() != nil, f.hot.Load() != nil, f.gh.Load() != nil
		over := float64(buf.Len()-f.Bytes()) / float64(f.Bytes())
		t.Logf("%s: snapshot %d bytes, Bytes() %d (%+.3f%%); stash %v, hot %v, overflow %v; evictions %d, folded %d, promoted %d",
			name, buf.Len(), f.Bytes(), 100*over, st, hot, gh, f.Evictions(), f.Folded(), f.Promoted())
		if over > 0.01 || over < -0.01 || !st || all && (!hot || !gh) {
			t.Errorf("%s: not within 1%% of Bytes(), or a section not allocated", name)
		}
	}
	for _, c := range []int{200_000, 1_000_000} {
		f, _ := New(Config{Capacity: c, Horizon: time.Minute, Resolution: time.Millisecond})
		for i := range 3 * c {
			f.TouchAt(mix64(uint64(i)), 0, 1+uint64(i)*30_000/uint64(3*c))
		}
		check(fmt.Sprintf("Capacity %d", c), f, false)
	}
	const c = 200_000
	f, _ := New(Config{Capacity: c, Horizon: time.Minute, Resolution: time.Millisecond, Stash: 64, SpillMin: 1})
	for i := range 3 * c {
		f.TouchAt(mix64(uint64(i)), 0, 1+uint64(i)*30_000/uint64(3*c))
	}
	for k := range uint64(100) {
		for range 64 {
			f.TouchAt(mix64(k+1<<45), 0, 30_001)
		}
	}
	check("Capacity 200000, Stash 64, SpillMin 1", f, true)
}
