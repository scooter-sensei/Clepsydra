package temporalfilter

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"testing"
	"time"
)

type digestCell struct {
	mode   string
	seed   uint64
	repeat int
	spill1 bool
	cms    bool
}

// TestAnswerDigest replays a seeded single-goroutine workload across 30 cells
// and prints a digest of every value every call returns, per call kind, plus
// the counters at the end.
func TestAnswerDigest(t *testing.T) {
	if os.Getenv("TF_EXP") != "1" {
		t.Skip("skipping answer digest test; TF_EXP=1 not set")
	}

	modes := []string{"", "bg", "ct"}
	seeds := []uint64{1, 2, 3}

	var cells []digestCell

	// 1. seeds 1, 2, 3 × modes default / bg / ct × 64 repeat keys
	for _, m := range modes {
		for _, s := range seeds {
			cells = append(cells, digestCell{mode: m, seed: s, repeat: 64, spill1: false, cms: false})
		}
	}

	// 2. seeds 1, 2, 3 × modes default / bg / ct × 6,000 repeat keys
	for _, m := range modes {
		for _, s := range seeds {
			cells = append(cells, digestCell{mode: m, seed: s, repeat: 6000, spill1: false, cms: false})
		}
	}

	// 3. seeds 1, 2, 3 × modes default / bg / ct × 6,000 with SpillMin: 1
	for _, m := range modes {
		for _, s := range seeds {
			cells = append(cells, digestCell{mode: m, seed: s, repeat: 6000, spill1: true, cms: false})
		}
	}

	// 4. seeds 1–3, default mode, 6,000 with SpillMin: 1, and f.cms = true
	for _, s := range seeds {
		cells = append(cells, digestCell{mode: "", seed: s, repeat: 6000, spill1: true, cms: true})
	}

	if len(cells) != 30 {
		t.Fatalf("expected 30 cells, got %d", len(cells))
	}

	key := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	for _, c := range cells {
		cfg := Config{
			Capacity:   2048,
			Horizon:    time.Minute,
			Resolution: time.Millisecond,
			Key:        &key,
		}
		if c.spill1 {
			cfg.SpillMin = 1
		}
		switch c.mode {
		case "ct":
			cfg.ConstantTime = 200 * time.Nanosecond
		case "bg":
			cfg.Background = true
		}
		now := uint64(1)
		cfg.Clock = func() uint64 { return now }

		f, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if c.mode == "bg" {
			f.bgManual = true
		}
		if c.cms {
			f.cms = true
		}

		r := rand.New(rand.NewPCG(c.seed, 99))
		digests := map[string]uint64{}
		hs := map[string]*fnvState{}
		put := func(kind string, vals ...any) {
			h := hs[kind]
			if h == nil {
				h = &fnvState{fnv.New64a()}
				hs[kind] = h
			}
			fmt.Fprint(h.h, vals...)
			fmt.Fprint(h.h, ";")
		}
		res := func(x Result) string {
			return fmt.Sprintf("%v/%d/%d/%v/%v/%v", x.Seen, x.Count, x.Upper, x.Saturated, x.Restored, x.Degraded)
		}

		repeat := make([]uint64, c.repeat)
		for i := range repeat {
			repeat[i] = f.HashUint64(uint64(i) + 1<<40)
		}

		var oneoff uint64
		liveSeenChecks := 0
		wins := []uint64{1, 500, 1_000, 5_000, 15_000, 30_000, 60_000}
		batch := make([]uint64, 40)
		out := make([]bool, 40)
		counts := make([]int, 40)

		for op := 0; op < 400_000; op++ {
			phase := (op / 25_000) % 5
			if r.IntN(1000) < 3+phase*4 {
				now++
			}
			w := wins[r.IntN(len(wins))]
			var h uint64
			if r.IntN(100) < 30 {
				h = repeat[r.IntN(len(repeat))]
			} else if r.IntN(100) < 10 && oneoff > 0 {
				h = f.HashUint64(oneoff - uint64(r.IntN(int(min(oneoff, 5000)))))
			} else {
				oneoff++
				h = f.HashUint64(oneoff)
			}
			switch k := r.IntN(100); {
			case k < 40:
				live := f.sketchLive(now)
				seen := f.CheckAndTouchAt(h, w, now)
				if live && seen {
					liveSeenChecks++
				}
				put("check", seen)
			case k < 70:
				put("touch", res(f.TouchAt(h, w, now)))
			case k < 85:
				put("count", res(f.CountAt(h, w, now)))
			case k < 90:
				put("contains", f.ContainsAt(h, w, now))
			case k < 95:
				for i := range batch {
					batch[i] = repeat[r.IntN(len(repeat))]
					if r.IntN(3) == 0 {
						oneoff++
						batch[i] = f.HashUint64(oneoff)
					}
				}
				f.CheckAndTouchBatch(batch, w, now, out)
				put("cbatch", fmt.Sprint(out))
			default:
				for i := range batch {
					batch[i] = repeat[r.IntN(len(repeat))]
				}
				f.TouchAndCountBatch(batch, w, now, out, counts)
				put("tcbatch", fmt.Sprint(out, counts))
			}
			if c.mode == "bg" && r.IntN(50) == 0 {
				for f.room.Load() != nil && f.placeNext(f.room.Load(), now) {
				}
			}
			if r.IntN(500) == 0 {
				put("decay", f.decayStep(now, 4))
			}
		}

		put("end", f.Evictions(), f.Relocations(), f.Restored(), f.Folded(), f.Shed(), f.Floored(), f.Lost(),
			f.Promoted(), f.HotRefused(), f.Demoted(), f.HotInUse(), f.Waited(), f.Waiting(), f.Occupancy(now), f.Bytes())

		for k, h := range hs {
			digests[k] = h.h.Sum64()
		}

		if c.repeat == 6000 && liveSeenChecks == 0 {
			t.Fatalf("overflow cell %+v read 0 seen checks with overflow live", c)
		}

		fmt.Printf("cell mode=%q seed=%d repeat=%d spill1=%v cms=%v", c.mode, c.seed, c.repeat, c.spill1, c.cms)
		for _, k := range []string{"check", "touch", "count", "contains", "cbatch", "tcbatch", "decay", "end"} {
			fmt.Printf(" %s=%016x", k, digests[k])
		}
		fmt.Printf(" (evictions %d, folded %d, floored %d, promoted %d, waited %d, seen checks with overflow live %d)\n",
			f.Evictions(), f.Folded(), f.Floored(), f.Promoted(), f.Waited(), liveSeenChecks)
		f.Close()
	}
}

type fnvState struct {
	h interface {
		Write([]byte) (int, error)
		Sum64() uint64
	}
}

func (s *fnvState) Write(p []byte) (int, error) { return s.h.Write(p) }
