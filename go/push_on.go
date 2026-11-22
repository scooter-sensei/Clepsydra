//go:build tfpush && tfdrip

package temporalfilter

// lab/sw W13's push count, on (push.go).

import (
	"fmt"
	"strings"
	"sync/atomic"
)

const pushOn = true

const pushStripes = 16

// pushTally is one touch's stores of its push, kept in its frame.
type pushTally struct {
	stores              int
	site                [pushSites]int
	fresh, again, twice int // stores after the first, by class
	behind              bool
	skipT, skipM        int
	silent, placedN     int
	seq                 [4]pushStore // the first stores
	nseq                int
}

type pushStore struct {
	site   int
	fresh  bool
	last   uint64
	before bool // the library's pushed flag at the store
}

// store notes a word0 with this touch's push stored at site: a fresh start
// (no history recalled), or into a history whose last touch was last; before
// is the library's pushed flag at the store.
func (t *pushTally) store(site int, fresh bool, last, now uint64, before bool) {
	if t.stores > 0 {
		switch {
		case fresh:
			t.fresh++
		case last < now:
			t.again++
		default:
			t.twice++
		}
		if !before {
			t.behind = true
		}
	}
	if t.nseq < len(t.seq) {
		t.seq[t.nseq] = pushStore{site, fresh, last, before}
		t.nseq++
	}
	t.stores++
	t.site[site]++
}

// table notes touch's count in the key's unit: W4c's gate held, next0
// changed nothing, or the CAS stored the push.
func (t *pushTally) table(gate, same bool, last, now uint64, before bool) {
	switch {
	case gate:
		t.skipT++
	case same:
		t.silent++
	default:
		t.store(pushTable, false, last, now, before)
	}
}

func (t *pushTally) skipMiss() { t.skipM++ }
func (t *pushTally) placed()   { t.placedN++ }

func (t *pushTally) class() int {
	switch {
	case t.twice > 0:
		return pcTwice
	case t.again > 0:
		return pcAgain
	case t.fresh > 0:
		return pcFresh
	case t.stores == 1:
		return pcOnce
	}
	return pcNone
}

var pushSiteNames = [pushSites]string{"table", "stamp", "realign", "claim", "wait"}

func (t *pushTally) desc(now uint64) string {
	var s []string
	for _, x := range t.seq[:t.nseq] {
		d := fmt.Sprintf("%s into last %d", pushSiteNames[x.site], x.last)
		if x.fresh {
			d = pushSiteNames[x.site] + " afresh"
		}
		if x.last >= now && !x.fresh {
			d += " (at or after the tick)"
		}
		s = append(s, fmt.Sprintf("%s, flag %v", d, x.before))
	}
	return fmt.Sprintf("stores %d [%s], skips table %d miss %d, silent %d, missPlaced %d, behind %v",
		t.stores, strings.Join(s, "; "), t.skipT, t.skipM, t.silent, t.placedN, t.behind)
}

// done classes the touch at its return and counts it in f.
func (t *pushTally) done(f *Filter, h, now uint64) {
	c := t.class()
	s := &f.push.s[h&(pushStripes-1)]
	s.n[pcCalls].Add(1)
	s.n[c].Add(1)
	if t.behind {
		s.n[pcBehind].Add(1)
	}
	if t.nseq > 0 && t.seq[0].fresh {
		s.n[pcFirstFresh].Add(1)
	}
	if t.stores == 0 && t.silent == 0 {
		s.n[pcBare].Add(1)
	}
	for i, n := range t.site {
		if n > 0 {
			s.n[pcStores+i].Add(int64(n))
		}
	}
	for i, n := range [4]int{t.skipT, t.skipM, t.silent, t.placedN} {
		if n > 0 {
			s.n[pcSkipTable+i].Add(int64(n))
		}
	}
	if c == pcTwice && f.push.first.Load() == nil {
		d := fmt.Sprintf("key %#x at %d: %s", h, now, t.desc(now))
		f.push.first.CompareAndSwap(nil, &d)
	}
	if testHookPushDone != nil {
		testHookPushDone(h, now, pushNames[c], t.desc(now))
	}
}

// pushStats is a filter's push counts, striped by key.
type pushStats struct {
	s [pushStripes]struct {
		n [pushN]atomic.Int64
		_ [64]byte
	}
	first atomic.Pointer[string]
}

func (f *Filter) pushCounts() (c pushCount) {
	for i := range f.push.s {
		for j := range c.N {
			c.N[j] += f.push.s[i].n[j].Load()
		}
	}
	if p := f.push.first.Load(); p != nil {
		c.First = *p
	}
	return c
}
