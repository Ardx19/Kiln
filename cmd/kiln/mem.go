package main

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/Ardx19/kiln/internal/memdb"
)

const memLogPath = ".kiln/mem.log"

// runMem is `kiln mem top [N]` and `kiln mem forget PATTERN`: look at and
// edit what kiln has learned about each output's memory use.
func runMem(p program, args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintf(stderr, "usage: %s mem [-C DIR] top [N]\n       %s mem [-C DIR] forget PATTERN\n\n"+
			"top lists the N (default 20) outputs that needed the most memory; forget drops\n"+
			"the history of outputs matching the glob PATTERN (path.Match syntax)\n", p.name, p.name)
		return 1
	}
	if len(args) >= 2 && args[0] == "-C" {
		if err := os.Chdir(args[1]); err != nil {
			fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
			return 1
		}
		args = args[2:]
	}
	if len(args) == 0 {
		return usage()
	}
	log, err := memdb.Open(memLogPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	switch args[0] {
	case "top":
		n := 20
		if len(args) > 2 {
			return usage()
		}
		if len(args) == 2 {
			if _, err := fmt.Sscanf(args[1], "%d", &n); err != nil || n < 1 {
				return usage()
			}
		}
		recs := make([]*memdb.Record, 0, len(log.Records))
		for _, r := range log.Records {
			recs = append(recs, r)
		}
		sort.Slice(recs, func(i, j int) bool {
			if recs[i].Estimate != recs[j].Estimate {
				return recs[i].Estimate > recs[j].Estimate
			}
			return recs[i].Output < recs[j].Output
		})
		if len(recs) == 0 {
			fmt.Fprintf(stdout, "no memory history yet; it's recorded as builds run\n")
			return 0
		}
		fmt.Fprintf(stdout, "%10s %10s %9s  %s\n", "ESTIMATE", "LAST", "TIME", "OUTPUT")
		for _, r := range recs[:min(n, len(recs))] {
			fmt.Fprintf(stdout, "%10s %10s %8.1fs  %s\n", mib(r.Estimate), mib(r.Last), float64(r.DurationMS)/1000, r.Output)
		}
	case "forget":
		if len(args) != 2 {
			return usage()
		}
		n, err := log.Forget(args[1])
		if err != nil {
			fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
			return 1
		}
		fmt.Fprintf(stdout, "forgot %d output(s)\n", n)
	default:
		return usage()
	}
	return 0
}

func mib(n uint64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
