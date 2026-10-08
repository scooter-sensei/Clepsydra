// Command tfbench is the scorecard for the temporal filter: missed keys,
// false positives, and latency, measured the same way every run so
// iterations are comparable.
//
//	go run ./cmd/tfbench -label "what changed"   # prints and appends to results/history.md
//
// Accuracy is measured against exact ground truth (a map of last-seen times)
// on a churn workload: a stream mixing new keys and repeats, run for 4
// horizons, where every op is CheckAndTouch with a random window.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type speeds struct{ hitCold, batch, hitHot, insert, insertSat, velocity, parallel, str, strCoarse, hitDRAM float64 }

func main() {
	label := flag.String("label", "", "what changed in this iteration")
	quick := flag.Bool("quick", false, "skip speed benchmarks")
	exact := flag.Bool("exact", false, "print the deterministic metrics as raw counts (no timing, nothing written) and exit")
	flag.Parse()
	if *exact {
		printExact()
		return
	}

	t0 := time.Now()
	commit, _ := exec.Command("git", "-c", "safe.directory=*", "rev-parse", "--short", "HEAD").Output()
	dirty, _ := exec.Command("git", "-c", "safe.directory=*", "status", "--porcelain", "--", "*.go").Output()
	rev := strings.TrimSpace(string(commit))
	if len(dirty) > 0 {
		rev += "+"
	}
	c10, c20, c25 := churn(1.0, 2), churn(1.1, 3), churn(1.15, 4)
	sfpr := float64(staticFPR(fprProbes)) / fprProbes
	sp := float64(spray()) / sprayOffenders
	vmae, per := velocityMAE()
	for _, v := range per {
		fmt.Fprintf(os.Stderr, "vel MAE %2ds: %.3f\n", v.w/1000, v.errSum/v.exact)
	}
	bpk := bytesPerKey()
	var s speeds
	if !*quick {
		s = speed()
	}
	row := fmt.Sprintf("| %s | %s | %.3f%% | %.3f%% | %.3f%% | %.2e | %.2e | %.0f%% | %.1f | %.3f | %.0f | %.0f | %.0f | %.0f | %.0f | %.0f | %.1f | %.0f | %.0f |",
		time.Now().UTC().Format("01-02 15:04"), rev,
		100*c10.fnr, 100*c20.fnr, 100*c25.fnr, c25.fpr, sfpr, 100*sp, bpk, vmae,
		s.hitCold, s.batch, s.hitHot, s.insert, s.insertSat, s.velocity, s.parallel, s.str, s.strCoarse)
	if *label != "" {
		row += " " + *label + " |"
	} else {
		row += " |"
	}
	row += fmt.Sprintf(" %.0f |", s.hitDRAM)
	fmt.Println(header)
	fmt.Println(row)
	fmt.Fprintf(os.Stderr, "(%.0fs; live keys/capacity: %.2f %.2f %.2f)\n", time.Since(t0).Seconds(), c10.live, c20.live, c25.live)
	if *label != "" && !*quick {
		path := "results/history.md"
		_ = os.MkdirAll("results", 0o755)
		if _, err := os.Stat(path); err != nil {
			os.WriteFile(path, []byte(intro+header+"\n"), 0o644)
		}
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintln(fh, row)
			fh.Close()
		}
	}
}

const intro = `# Temporal filter scorecard history

Produced by ` + "`go run ./cmd/tfbench -label ...`" + `. Capacity 200k, horizon 60s.
FN = missed keys on the churn workload at 1.0x / 1.1x / 1.15x the configured Capacity, i.e. ~85% / 94% / 98% slot load at the default Load (ground truth: exact map). Two-choice, 4-slot cuckoo tables saturate at ~97.7% load, so the 1.15x column cannot reach 0 with this layout.
FP churn = false "seen" on the churn workload at the last load point. Static FPR = 1e9 never-inserted probes at capacity.
Spray = repeat offenders keeping exact counts after 20x-capacity spray. Latencies in ns/op (best of 3),
1M-key table (20 MB, which fits this host's 260 MB L3, so "cold" means L3); hit DRAM = hit on a 32M-key (~600 MB) table, every lookup a DRAM miss (last column, from 15:39); batch = CheckAndTouchBatch per key; insert = steady state (keys expire as fast as they arrive), insert sat = every line full; parallel is per-op throughput with GOMAXPROCS goroutines; str = string key + built-in clock on a fresh 1M-key table; str coarse = same with CoarseClock (100µs cached clock).

`

const header = `| when (UTC) | rev | FN 1.0x | FN 1.1x | FN 1.15x | FP churn 1.15x | static FPR | spray | B/key | vel MAE | hit cold | batch | hit hot | insert | insert sat | velocity | parallel | str | str coarse | change | hit DRAM |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|`
