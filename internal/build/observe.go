package build

import (
	"sort"
	"time"
)

// JobInfo is a running command, for an Observer.
type JobInfo struct {
	Output  string
	RSSMiB  float64 // 0 when memory isn't being sampled
	Seconds float64
}

// Progress is a snapshot of a build, for an Observer.
type Progress struct {
	Total, Done int
	Jobs        []JobInfo
	BudgetMiB   float64
	ReservedMiB float64
}

// Observer is told how a build is going. Its methods are called from the
// build's own goroutine, so they need no locking; Progress is called at
// least about once a second while commands run, and can throttle itself.
type Observer interface {
	// Progress reports the build's state. force asks for it to be recorded
	// now (the build is starting or ending).
	Progress(p Progress, force bool)
	// Failed reports a command that failed.
	Failed(output string, exit int, command, text string)
}

// report sends the observer a snapshot.
func (b *Builder) report(force bool) {
	o := b.cfg.Observer
	if o == nil {
		return
	}
	p := Progress{Total: b.status.total, Done: b.status.finished}
	now := time.Now()
	for e, r := range b.started {
		j := JobInfo{Output: name(e), Seconds: now.Sub(b.start.Add(time.Duration(r.startMS) * time.Millisecond)).Seconds()}
		if b.mem != nil && r.pgid != 0 {
			if cur, ok := b.mem.sampler.Current(r.pgid); ok {
				j.RSSMiB = float64(cur.Peak) / (1 << 20)
			}
		}
		p.Jobs = append(p.Jobs, j)
	}
	sort.Slice(p.Jobs, func(i, k int) bool { return p.Jobs[i].Output < p.Jobs[k].Output })
	if b.mem != nil {
		p.BudgetMiB = float64(b.mem.budget) / (1 << 20)
		p.ReservedMiB = float64(b.mem.reserved()) / (1 << 20)
	}
	o.Progress(p, force)
}

// reportFailure tells the observer about a failed command.
func (b *Builder) reportFailure(res *result) {
	if b.cfg.Observer == nil {
		return
	}
	command, _ := res.edge.Command(false)
	b.cfg.Observer.Failed(name(res.edge), res.status(), command, res.output)
}

// startHeartbeat wakes the build loop once a second, so an observer sees
// progress even while one long command runs. It's only needed when memory
// scheduling isn't already waking the loop. The returned func stops it.
func (b *Builder) startHeartbeat() func() {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				select {
				case b.runner.wake <- struct{}{}:
				default:
				}
			}
		}
	}()
	return func() { close(stop) }
}
