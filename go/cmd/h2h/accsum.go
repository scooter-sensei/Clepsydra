package main

import "fmt"

// cmdAccSum prints one acc pass's answers scored against its trace, on one
// line: the flood, the strict decision's false allows and false denies (each
// as count and probes), the probes whose upper bound reads below the truth
// (with at least spillMin events, then fewer), the mean |estimate - truth|,
// and per probe class (c0 offender, c1 flood, c2 base, c3 never sent) its
// mean |estimate - truth| and its denies. Readings pool these over rounds.
func cmdAccSum(tracePath, answersPath string) error {
	t, err := readTrace(tracePath)
	if err != nil {
		return err
	}
	as, err := readAnswers(answersPath)
	if err != nil {
		return err
	}
	var s score
	s.add(t, as)
	fmt.Printf("%s allow %d %d deny %d %d upperlow %d %d abserr %.4f", floodName(t.Mult, t.Weight), s.allow[0], s.allow[1], s.deny[0], s.deny[1], s.upperLow, s.upperLowLt, s.absErr/float64(s.probes))
	for c := range s.byClass {
		b := &s.byClass[c]
		if b.n > 0 {
			fmt.Printf(" c%d %.4f %d", c, b.absErr/float64(b.n), b.deny)
		}
	}
	fmt.Println()
	return nil
}
