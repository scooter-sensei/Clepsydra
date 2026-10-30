// Command h2h is the head-to-head harness (docs/artifact/head-to-head.md):
// Clepsydra against industry structures on one host.
//
//	h2h gen -dir D [-n 262144]       write part 1's traces (one per flood)
//	h2h run -trace F -pass P ...     one pass of the Clepsydra arm (JSON line)
//	h2h report -dir D                score every arm's answers, summarize passes
//	h2h ecm [-keys 2000]             part 2: Clepsydra and the ECM-sketch
//	h2h velcheck                     the part 2 workload at the scorecard's settings (claim D5)
//
// The competitors of part 1 run in bench/h2h (Rust) on the same traces;
// go/verify/h2h.sh runs everything.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: h2h gen|run|report|ecm|velcheck [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	dir := fs.String("dir", "", "work directory (traces, answers, results.jsonl)")
	n := fs.Int("n", 1<<18, "key count (Clepsydra's Capacity)")
	seed := fs.Uint64("seed", 11, "trace seed")
	tracePath := fs.String("trace", "", "trace file")
	pass := fs.String("pass", "acc", "acc | t1 | t12 | lat1 | lat12")
	round := fs.Int("round", 0, "round number (reported)")
	threads := fs.Int("threads", 12, "threads for t12 and lat12")
	answers := fs.String("answers", "", "answers file (acc pass)")
	keys := fs.Int("keys", 2000, "part 2: key count")
	capacity := fs.Int("capacity", 0, "part 2: Clepsydra's Capacity (default: the key count)")
	mults := fs.String("mults", "1,2,4,8", "part 2: ECM budgets, as multiples of Clepsydra's bytes")
	cpuprofile := fs.String("cpuprofile", "", "write a CPU profile of the command")
	fs.Parse(os.Args[2:])
	if *cpuprofile != "" {
		fh, perr := os.Create(*cpuprofile)
		if perr != nil {
			fail(perr)
		}
		pprof.StartCPUProfile(fh)
		defer pprof.StopCPUProfile()
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = cmdGen(*dir, *n, *seed)
	case "run":
		err = cmdRun(*tracePath, *pass, *round, *threads, *answers)
	case "report":
		err = cmdReport(*dir)
	case "ecm":
		c := *capacity
		if c == 0 {
			c = *keys
		}
		var ms []int
		for _, s := range strings.Split(*mults, ",") {
			m, perr := strconv.Atoi(s)
			if perr != nil {
				fail(perr)
			}
			ms = append(ms, m)
		}
		cmdECM(*keys, c, ms)
	case "velcheck":
		evs, hs, qs := velocityWorkload(2000)
		e, _ := runClepVel(200_000, evs, hs, qs)
		for i, w := range velWins {
			fmt.Printf("velocity window=%ds abs_error_sum=%.0f exact_sum=%.0f  => %.4f\n", w/1000, e.est[i], e.exact[i], e.per(i))
		}
		fmt.Printf("velocity mean_of_windows => %.4f\n", e.mean())
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		pprof.StopCPUProfile()
		fail(err)
	}
}
