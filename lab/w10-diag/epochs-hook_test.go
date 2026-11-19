package temporalfilter

import (
	"fmt"
	"strings"
	"sync"
)

var (
	w10Mu    sync.Mutex
	w10Parts = map[uint64][]string{} // h -> the overflow parts addSketch added in its last checked call
	w10Est   = map[uint64][]string{} // id -> the overflow table's reads of it since its last checked call began
)

func init() {
	testHookW10Sketch = func(h uint64, s Result, ok bool) {
		w10Mu.Lock()
		defer w10Mu.Unlock()
		if l, armed := w10Parts[h]; armed {
			w10Parts[h] = append(l, fmt.Sprintf("{ok %v Upper %d Count %d}", ok, s.Upper, s.Count))
		}
	}
	testHookW10Est = func(id, now, ep, until uint64, line [2][ghWords]uint64, nb int, r Result) {
		w10Mu.Lock()
		defer w10Mu.Unlock()
		l, armed := w10Est[id]
		if !armed {
			return
		}
		var ws []string
		for bi := range nb {
			ws = append(ws, fmt.Sprintf("%x", line[bi]))
		}
		w10Est[id] = append(l, fmt.Sprintf("{now %d epoch %d until %d -> Upper %d Count %d; words %s}", now, ep, until, r.Upper, r.Count, strings.Join(ws, " | ")))
	}
}

func (f *Filter) w10Arm(h uint64) {
	w10Mu.Lock()
	defer w10Mu.Unlock()
	w10Parts[h] = []string{}
	w10Est[f.stashID(h, fingerprint(h), tag(h))] = []string{}
}

func (f *Filter) w10Report(h uint64) string {
	w10Mu.Lock()
	defer w10Mu.Unlock()
	return fmt.Sprintf("OVERFLOW PARTS ADDED %v; READS %s", w10Parts[h], strings.Join(w10Est[f.stashID(h, fingerprint(h), tag(h))], " ;; "))
}
