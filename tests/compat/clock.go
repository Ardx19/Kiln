package compat

import (
	"sync/atomic"
	"time"
)

// clockWatch notices when the wall clock steps (jumps) relative to the
// monotonic clock. File mtimes come from the wall clock, so a step during
// a build makes mtime-based decisions depend on timing, for Ninja as much
// as for kiln. WSL2 with systemd-timesyncd steps the clock back about
// 1.8 s every 30 s; a CI runner normally doesn't step at all.
//
// A watch runs continuously so that steps between two measured intervals
// (say, between a build and the next edit) are counted too.
type clockWatch struct {
	steps atomic.Int64
	stop  chan struct{}
	done  chan struct{}
}

// maxDrift is the largest wall-vs-monotonic change between two samples
// that still counts as normal slewing rather than a step.
const maxDrift = 2 * time.Millisecond

func watchClock() *clockWatch {
	w := &clockWatch{stop: make(chan struct{}), done: make(chan struct{})}
	start := time.Now()
	offset := func() time.Duration {
		now := time.Now()
		wall := time.Duration(now.UnixNano() - start.UnixNano())
		return wall - now.Sub(start) // now.Sub uses the monotonic readings
	}
	go func() {
		defer close(w.done)
		last := offset()
		t := time.NewTicker(time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-t.C:
			}
			o := offset()
			if d := o - last; d > maxDrift || d < -maxDrift {
				w.steps.Add(1)
			}
			last = o
		}
	}()
	return w
}

// Steps returns how many steps the watch has seen so far.
func (w *clockWatch) Steps() int64 { return w.steps.Load() }

// Stop ends the watch and reports whether the clock stepped at all.
func (w *clockWatch) Stop() bool {
	close(w.stop)
	<-w.done
	return w.steps.Load() > 0
}
