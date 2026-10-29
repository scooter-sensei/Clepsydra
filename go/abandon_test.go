package temporalfilter

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Few hot keys, eight workers and a fill to 99% slot load, repeated: two
// touches of one key often overlap a move of it, so moves get abandoned (a
// touch reached the source first). While copies were visible from their
// install (until claims A23) the copy had often counted touches of its
// own, which had to be added back, not dropped with it; now a copy is
// hidden until the hand-off, and no touch may be lost either way.
//
// It also catches a rarer race: a touch that checked its slot's fingerprint
// counting into the next occupant's register, when the slot is handed over
// before its CAS and the newcomer has the same 12-bit tag (bump counts again
// after such a hand-over; what is left is A29's residual, a same-tag
// hand-over just after the count, which reads one high). TF_DISTINCT_TAGS=1
// keeps cold keys off the hot keys' tags (hot keys may still share one: in
// about half the rounds); TF_SHARED_TAGS=1 puts every cold key on one (7
// of 1,800 runs read one high so, against 3 of ~6,700 with real tags,
// 2026-10-28). A failing round logs each hot
// key whose count is off, with who shares its tag (logOffKeys).
func TestAbandonedMoveKeepsCounts(t *testing.T) {
	iters := 300
	if testing.Short() {
		iters = 30
	}
	distinct := os.Getenv("TF_DISTINCT_TAGS") != ""
	shared := os.Getenv("TF_SHARED_TAGS") != ""
	const hot, workers, at = 64, 8, 60_000
	failed, lost := 0, 0
	for it := range iters {
		f := newTest(t, 4000, time.Minute)
		salt := uint64(it) << 32
		hotTags := map[uint64]bool{}
		var hotTagList []uint64
		hotIDs := map[uint64]bool{}
		for k := range uint64(hot) {
			hotTags[tag(mix64(salt+k))] = true
			hotTagList = append(hotTagList, tag(mix64(salt+k)))
			hotIDs[fingerprint(mix64(salt+k))<<tagBits|tag(mix64(salt+k))] = true
			for range 3 {
				f.CheckAndTouchAt(mix64(salt+k), 0, at)
			}
		}
		var stop atomic.Bool
		var wg sync.WaitGroup
		var touches atomic.Int64
		ns := make([]int64, workers)
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n := int64(0)
				for i := uint64(w / 2); !stop.Load() && n < 150*hot; i++ {
					f.CheckAndTouchAt(mix64(salt+i%hot), 0, at)
					n++
				}
				ns[w] = n
				touches.Add(n)
			}()
		}
		fill := int(float64(f.Slots())*0.99) - hot
		fillTags := map[uint64]int{}
		for k, n := uint64(0), 0; n < fill; k++ {
			h := mix64(salt + 1<<40 + k)
			if shared { // the tag is h's bits 36-47
				h = h&^((1<<tagBits-1)<<36) | hotTagList[k%uint64(len(hotTagList))]<<36
				if hotIDs[fingerprint(h)<<tagBits|tag(h)] {
					continue
				}
			}
			if distinct && hotTags[tag(h)] {
				continue
			}
			fillTags[tag(h)]++
			f.CheckAndTouchAt(h, 0, at)
			n++
		}
		stop.Store(true)
		wg.Wait()
		total := 0
		for k := range uint64(hot) {
			total += f.VelocityCountAt(mix64(salt+k), f.Ticks(time.Minute), at)
		}
		if want := int(touches.Load()) + hot*3; total != want {
			failed++
			lost += want - total
			logOffKeys(t, f, it, salt, ns, hotTagList, fillTags, at)
		}
	}
	t.Logf("%d of %d rounds lost counts (%d in all)", failed, iters, lost)
	if failed > 0 {
		t.Fatalf("%d of %d rounds lost counts", failed, iters)
	}
}

// logOffKeys logs each hot key of failing round it whose count is off: its
// true count (3 first touches, and worker w's ns[w] touches of keys w/2,
// w/2+1, ... in turn), the hot and cold keys on its tag, and where its
// history is.
func logOffKeys(t *testing.T, f *Filter, it int, salt uint64, ns []int64, hotTags []uint64, coldTags map[uint64]int, at uint64) {
	hot := uint64(len(hotTags))
	want := make([]int, hot)
	for k := range want {
		want[k] = 3
	}
	for w, n := range ns {
		for i := uint64(w / 2); i < uint64(w/2)+uint64(n); i++ {
			want[i%hot]++
		}
	}
	for k := range hot {
		h := mix64(salt + k)
		got := f.VelocityCountAt(h, f.Ticks(time.Minute), at)
		if got == want[k] {
			continue
		}
		var same []uint64
		for j, tg := range hotTags {
			if uint64(j) != k && tg == tag(h) {
				same = append(same, uint64(j))
			}
		}
		where := "where unknown"
		if describeKey != nil {
			where = describeKey(f, h, at)
		}
		t.Logf("round %d, hot key %d: count %d, true %d (%+d); tag %d: hot keys %v and %d cold keys share it; %s", it, k, got, want[k], got-want[k], tag(h), same, coldTags[tag(h)], where)
	}
}

// describeKey says where h's history is (soak_test.go sets it to whereIs).
// verify/replay.sh copies this file alone into commits from before
// whereIs, where it stays nil and the file still builds.
var describeKey func(f *Filter, h, now uint64) string
