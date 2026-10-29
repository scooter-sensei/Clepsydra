package temporalfilter

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Conformance vectors: language-neutral traces in ../spec/vectors/ that pin
// the filter's observable behaviour bit for bit. Every port (Python, C++,
// Java) must reproduce them; docs/specification/conformance.md describes the format.
//
//	go test -run TestConformance -update   rewrites them from this code

var update = flag.Bool("update", false, "rewrite spec/vectors from this implementation")

const vectorDir = "../spec/vectors"

// vecKey is the SipHash key every vector uses: bytes 0x00..0x0f.
var vecKey = func() *[16]byte {
	var k [16]byte
	for i := range k {
		k[i] = byte(i)
	}
	return &k
}()

type vecConfig struct {
	Capacity     int     `json:"capacity"`
	HorizonMs    int64   `json:"horizon_ms"`
	ResolutionMs int64   `json:"resolution_ms"`
	Load         float64 `json:"load,omitempty"`
	Stash        int     `json:"stash,omitempty"`
	Overflow     int     `json:"overflow,omitempty"`
	SpillMin     int     `json:"spill_min,omitempty"`
}

type vecResult struct {
	Seen      bool `json:"seen"`
	Count     int  `json:"count"`
	Upper     int  `json:"upper"`
	Saturated bool `json:"saturated,omitempty"`
	Restored  bool `json:"restored,omitempty"`
	Degraded  bool `json:"degraded,omitempty"`
}

// vecOp is one call. Op is "touch" (Touch) or "count" (Count); the clock
// reads NowMs (in ticks of the resolution) during the call.
type vecOp struct {
	Op       string    `json:"op"`
	Key      string    `json:"key"`
	WindowMs int64     `json:"window_ms"`
	NowMs    uint64    `json:"now_ms"`
	Want     vecResult `json:"want"`
}

type vecCounters struct {
	Evictions   uint64 `json:"evictions"`
	Relocations uint64 `json:"relocations"`
	Lost        uint64 `json:"lost"`
	Shed        uint64 `json:"shed"`
	Restored    uint64 `json:"restored"`
	Occupancy   int    `json:"occupancy"`
	Slots       int    `json:"slots"`
}

type vecFile struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Key         string      `json:"key_hex"`
	Config      vecConfig   `json:"config"`
	Hashes      []vecHash   `json:"hashes,omitempty"`
	Ops         []vecOp     `json:"ops,omitempty"`
	End         vecCounters `json:"end"`
}

type vecHash struct {
	String *string `json:"string,omitempty"`
	Uint64 *uint64 `json:"uint64,omitempty"`
	Hash   string  `json:"hash"`
}

type vecScenario struct {
	name, desc string
	cfg        vecConfig
	hashes     bool
	ops        func(rng *rand.Rand) []vecOp // Want left empty
}

func vecScenarios() []vecScenario {
	base := vecConfig{Capacity: 64, HorizonMs: 60_000, ResolutionMs: 1}
	return []vecScenario{
		{
			name: "basic", hashes: true, cfg: base,
			desc: "a few keys touched and counted at assorted windows, including events exactly one window old",
			ops: func(rng *rand.Rand) []vecOp {
				var ops []vecOp
				now := uint64(1000)
				for i := range 400 {
					k := fmt.Sprintf("user:%d", rng.IntN(12))
					w := []int64{1, 250, 1000, 5000, 30_000, 60_000}[rng.IntN(6)]
					op := "touch"
					if i%3 == 2 {
						op = "count"
					}
					ops = append(ops, vecOp{Op: op, Key: k, WindowMs: w, NowMs: now})
					if i%25 == 0 { // an event exactly one window ago is inside it
						ops = append(ops, vecOp{Op: "count", Key: k, WindowMs: 250, NowMs: now + 250})
					}
					now += uint64(rng.IntN(400))
				}
				return ops
			},
		},
		{
			name: "burst", cfg: base,
			desc: "one key at rates above the register's capacity (Saturated), then decaying",
			ops: func(rng *rand.Rand) []vecOp {
				var ops []vecOp
				now := uint64(5)
				for i := range 600 {
					ops = append(ops, vecOp{Op: "touch", Key: "hot", WindowMs: 1000, NowMs: now})
					if i%20 == 0 {
						for _, w := range []int64{100, 2500, 10_000, 60_000} {
							ops = append(ops, vecOp{Op: "count", Key: "hot", WindowMs: w, NowMs: now})
						}
					}
					if i < 300 {
						now += uint64(rng.IntN(3))
					} else {
						now += uint64(rng.IntN(400))
					}
				}
				for range 40 {
					now += 2000
					ops = append(ops, vecOp{Op: "count", Key: "hot", WindowMs: 60_000, NowMs: now})
				}
				return ops
			},
		},
		{
			name: "churn", cfg: base,
			desc: "4x capacity of one-off keys flood the table while 8 repeat keys keep touching: velocity-aware eviction",
			ops: func(rng *rand.Rand) []vecOp {
				var ops []vecOp
				now := uint64(1)
				for i := range 300 {
					ops = append(ops, vecOp{Op: "touch", Key: fmt.Sprintf("rep:%d", i%8), WindowMs: 60_000, NowMs: now})
					now += uint64(rng.IntN(20))
				}
				for i := range 4 * base.Capacity * 4 {
					ops = append(ops, vecOp{Op: "touch", Key: fmt.Sprintf("flood:%d", i), WindowMs: 60_000, NowMs: now})
					if i%16 == 0 {
						ops = append(ops, vecOp{Op: "touch", Key: fmt.Sprintf("rep:%d", rng.IntN(8)), WindowMs: 60_000, NowMs: now})
					}
					now += uint64(rng.IntN(15))
				}
				for i := range 8 {
					ops = append(ops, vecOp{Op: "count", Key: fmt.Sprintf("rep:%d", i), WindowMs: 60_000, NowMs: now})
				}
				for i := range 64 {
					ops = append(ops, vecOp{Op: "count", Key: fmt.Sprintf("never:%d", i), WindowMs: 60_000, NowMs: now})
				}
				return ops
			},
		},
		{
			name: "overflow", cfg: vecConfig{Capacity: 64, HorizonMs: 60_000, ResolutionMs: 1, Stash: 8, SpillMin: 1},
			desc: "3x capacity of keys with 4-8 events each, a small stash: evicted histories come back from the stash (Restored) or the overflow table (Degraded)",
			ops: func(rng *rand.Rand) []vecOp {
				var ops []vecOp
				now := uint64(1)
				n := 3 * 64
				for round := range 8 {
					for i := range n {
						if round < 4 || i%2 == 0 {
							ops = append(ops, vecOp{Op: "touch", Key: fmt.Sprintf("mid:%d", i), WindowMs: 60_000, NowMs: now})
						}
						now += uint64(rng.IntN(10))
					}
				}
				for i := range n {
					w := []int64{5000, 60_000}[i%2]
					ops = append(ops, vecOp{Op: "count", Key: fmt.Sprintf("mid:%d", i), WindowMs: w, NowMs: now})
				}
				for i := range 64 {
					ops = append(ops, vecOp{Op: "count", Key: fmt.Sprintf("never:%d", i), WindowMs: 60_000, NowMs: now})
				}
				return ops
			},
		},
	}
}

func (c vecConfig) config(clock func() uint64) Config {
	return Config{
		Capacity: c.Capacity, Horizon: time.Duration(c.HorizonMs) * time.Millisecond,
		Resolution: time.Duration(c.ResolutionMs) * time.Millisecond,
		Load:       c.Load, Stash: c.Stash, Overflow: c.Overflow, SpillMin: c.SpillMin,
		Key: vecKey, Clock: clock,
	}
}

// runVectors replays ops on a fresh filter and fills in the results.
func runVectors(t *testing.T, sc vecScenario, ops []vecOp) (vecFile, error) {
	var now uint64
	f, err := New(sc.cfg.config(func() uint64 { return now }))
	if err != nil {
		return vecFile{}, err
	}
	defer f.Close()
	out := vecFile{Name: sc.name, Description: sc.desc, Key: fmt.Sprintf("%x", vecKey[:]), Config: sc.cfg}
	if sc.hashes {
		for _, s := range []string{"", "a", "user:0", "the quick brown fox jumps over the lazy dog"} {
			out.Hashes = append(out.Hashes, vecHash{String: &s, Hash: fmt.Sprintf("%016x", f.Hash(s))})
		}
		for _, x := range []uint64{0, 1, 42, 1 << 63, ^uint64(0)} {
			out.Hashes = append(out.Hashes, vecHash{Uint64: &x, Hash: fmt.Sprintf("%016x", f.HashUint64(x))})
		}
	}
	for _, op := range ops {
		now = op.NowMs
		w := time.Duration(op.WindowMs) * time.Millisecond
		var r Result
		switch op.Op {
		case "touch":
			r = f.Touch(op.Key, w)
		case "count":
			r = f.Count(op.Key, w)
		default:
			return out, fmt.Errorf("unknown op %q", op.Op)
		}
		op.Want = vecResult{r.Seen, r.Count, r.Upper, r.Saturated, r.Restored, r.Degraded}
		out.Ops = append(out.Ops, op)
	}
	out.End = vecCounters{f.Evictions(), f.Relocations(), f.Lost(), f.Shed(), f.Restored(), f.Occupancy(now), f.Slots()}
	return out, nil
}

// marshalVectors writes one op per line, so diffs stay readable.
func marshalVectors(v vecFile) ([]byte, error) {
	ops := v.Ops
	v.Ops = nil
	head, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	head = head[:len(head)-2] // drop "\n}"
	buf := append(head, ",\n  \"ops\": [\n"...)
	for i, op := range ops {
		b, err := json.Marshal(op)
		if err != nil {
			return nil, err
		}
		buf = append(append(buf, "    "...), b...)
		if i < len(ops)-1 {
			buf = append(buf, ',')
		}
		buf = append(buf, '\n')
	}
	return append(buf, "  ]\n}"...), nil
}

func TestConformance(t *testing.T) {
	for _, sc := range vecScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			path := filepath.Join(vectorDir, sc.name+".json")
			if *update {
				got, err := runVectors(t, sc, sc.ops(rand.New(rand.NewPCG(7, 7))))
				if err != nil {
					t.Fatal(err)
				}
				b, err := marshalVectors(got)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(vectorDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (generate with -update)", err)
			}
			var want vecFile
			if err := json.Unmarshal(b, &want); err != nil {
				t.Fatal(err)
			}
			if (forceCT || forceBG) && want.End.Evictions > 0 {
				t.Skip("ConstantTime defers inserts that find no free slot, so which keys are evicted differs")
			}
			got, err := runVectors(t, vecScenario{name: want.Name, desc: want.Description, cfg: want.Config, hashes: len(want.Hashes) > 0}, want.Ops)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Hashes, want.Hashes) {
				t.Errorf("hashes differ:\n got %v\nwant %v", got.Hashes, want.Hashes)
			}
			bad := 0
			for i := range want.Ops {
				if got.Ops[i].Want != want.Ops[i].Want {
					if bad++; bad <= 5 {
						t.Errorf("op %d %+v: got %+v", i, want.Ops[i], got.Ops[i].Want)
					}
				}
			}
			if bad > 5 {
				t.Errorf("... %d ops differ in all", bad)
			}
			if got.End != want.End {
				t.Errorf("end counters: got %+v want %+v", got.End, want.End)
			}
		})
	}
}
