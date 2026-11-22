//go:build !(tfpush && tfdrip)

package temporalfilter

// lab/sw W13's push count, off: an empty tally whose notes do nothing
// (push.go).

const pushOn = false

type pushTally struct{}

type pushStats struct{}

func (*pushTally) store(site int, fresh bool, last, now uint64, before bool) {}
func (*pushTally) table(gate, same bool, last, now uint64, before bool)      {}
func (*pushTally) skipMiss()                                                 {}
func (*pushTally) placed()                                                   {}
func (*pushTally) done(f *Filter, h, now uint64)                             {}

func (f *Filter) pushCounts() pushCount { return pushCount{} }
