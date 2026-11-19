package temporalfilter

import (
	"fmt"
	"sync"
)

var w10Parts sync.Map // h -> []string: the overflow parts addSketch added in h's last checked call

func init() {
	testHookW10Sketch = func(h uint64, s Result, ok bool) {
		v, _ := w10Parts.LoadOrStore(h, &[]string{})
		p := v.(*[]string)
		*p = append(*p, fmt.Sprintf("{ok %v Upper %d Count %d Seen %v}", ok, s.Upper, s.Count, s.Seen))
	}
}

func w10PartsOf(h uint64) string {
	v, ok := w10Parts.Load(h)
	if !ok {
		return "addSketch not called"
	}
	return fmt.Sprint(*v.(*[]string))
}
