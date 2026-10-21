// Command memhog stands in for a compiler in memory tests: it touches
// -mib MiB of memory, growing over -secs seconds so the peak comes at the
// end (as a compile's does), holds it briefly, then writes -o.
//
// Usage:
//
//	memhog -mib 300 -secs 2 -o out.o
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	mib := flag.Int("mib", 100, "MiB to touch")
	secs := flag.Float64("secs", 1, "seconds to ramp up over")
	hold := flag.Float64("hold", 0.3, "seconds to hold the peak")
	out := flag.String("o", "", "file to write when done")
	flag.Parse()

	const step = 1 << 20
	chunks := make([][]byte, 0, *mib)
	ramp := time.Duration(*secs * float64(time.Second))
	start := time.Now()
	for i := range *mib {
		b := make([]byte, step)
		for j := 0; j < step; j += 4096 {
			b[j] = 1 // touch every page so it's resident
		}
		chunks = append(chunks, b)
		// Keep pace with the ramp.
		if want := ramp * time.Duration(i+1) / time.Duration(*mib); time.Since(start) < want {
			time.Sleep(want - time.Since(start))
		}
	}
	time.Sleep(time.Duration(*hold * float64(time.Second)))
	if *out != "" {
		if err := os.WriteFile(*out, []byte(fmt.Sprintf("%d MiB\n", len(chunks))), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "memhog:", err)
			os.Exit(1)
		}
	}
}
