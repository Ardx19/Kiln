// Command clock watches the wall clock against the monotonic clock and
// reports steps (jumps). File mtimes come from the wall clock, so steps
// make mtime-based build decisions depend on timing.
//
// Usage:
//
//	go run ./spikes/clock [-d 60s]
package main

import (
	"flag"
	"fmt"
	"time"
)

func main() {
	d := flag.Duration("d", 60*time.Second, "how long to watch")
	flag.Parse()

	start := time.Now()
	offset := func(now time.Time) time.Duration {
		return time.Duration(now.UnixNano()-start.UnixNano()) - now.Sub(start)
	}
	last := offset(start)
	steps, largestBack := 0, time.Duration(0)
	for time.Since(start) < *d {
		time.Sleep(time.Millisecond)
		now := time.Now()
		o := offset(now)
		if jump := o - last; jump > 2*time.Millisecond || jump < -2*time.Millisecond {
			steps++
			largestBack = min(largestBack, jump)
			fmt.Printf("%s  wall clock stepped %+v\n", now.Format("15:04:05.000"), jump.Round(time.Millisecond))
		}
		last = o
	}
	fmt.Printf("%d steps in %v; largest backwards step %v\n", steps, *d, (-largestBack).Round(time.Millisecond))
}
