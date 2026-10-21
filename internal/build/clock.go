package build

import (
	"fmt"
	"time"
)

// stepThreshold is how far the wall clock must move against the
// monotonic clock between two checks to count as a step rather than the
// gradual slewing time synchronisation does.
const stepThreshold = 100 * time.Millisecond

// clockWatch notices the wall clock stepping back during a build. File
// mtimes come from the wall clock, so a backward step can leave outputs
// older than the inputs they were built from (endless rebuilds) or edits
// older than existing outputs (missed rebuilds). A forward step keeps
// every timestamp in order, so it is ignored. On WSL2 the clock has been
// seen stepping back 1.8 s, and forward 3 s, about every 30 s
// (docs/problems/clock-steps.md).
type clockWatch struct {
	start  time.Time
	offset time.Duration // wall minus monotonic elapsed time, at the last check
	warned bool
}

func newClockWatch() *clockWatch { return &clockWatch{start: time.Now()} }

// check returns a warning the first time the clock has stepped back since
// the previous check, and "" otherwise.
func (c *clockWatch) check() string {
	now := time.Now()
	offset := time.Duration(now.UnixNano()-c.start.UnixNano()) - now.Sub(c.start)
	step := offset - c.offset
	c.offset = offset
	if c.warned || step > -stepThreshold {
		return ""
	}
	c.warned = true
	return fmt.Sprintf("the system clock jumped back by %v during this build; "+
		"files written around then may carry out-of-order timestamps, so the next build "+
		"may redo or miss work (see docs/problems/clock-steps.md in kiln)",
		step.Abs().Round(time.Millisecond))
}
