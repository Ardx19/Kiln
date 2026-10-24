// Package sched decides which ready edges may start so that the predicted
// memory of running jobs stays within a budget. Admit is a pure function,
// so the policy can be tested with a simulated clock.
package sched

import "time"

// Load is a job's predicted memory over time, measured from now. It grows
// in a straight line from Start to Peak at Start+Ramp, stays at Peak until
// Start+Hold, then is gone; while it lasts it never drops below Floor,
// what a running job already uses. Hold 0 means it lasts for ever: a job
// whose duration isn't known holds its peak from start to finish.
type Load struct {
	Start       time.Duration // 0 for a job starting now, negative for a running one
	Ramp, Hold  time.Duration
	Floor, Peak uint64
}

// Flat is a load that needs n bytes from now on, for ever.
func Flat(n uint64) Load { return Load{Peak: n} }

func (l Load) at(t time.Duration) uint64 {
	if l.Hold > 0 && t > l.Start+l.Hold {
		return 0
	}
	v := l.Peak
	if l.Ramp > 0 && t < l.Start+l.Ramp {
		v = uint64(float64(l.Peak) * float64(max(0, t-l.Start)) / float64(l.Ramp))
	}
	return max(v, l.Floor)
}

// Most returns the highest total memory the loads need at any moment from
// now on. Between the moments where a load reaches its peak or ends, each
// load is a straight line or the larger of a line and its floor, so the
// total is convex there and its maximum is at one of those moments.
func Most(loads []Load) uint64 {
	times := []time.Duration{0}
	for _, l := range loads {
		if t := l.Start + l.Ramp; l.Ramp > 0 && t > 0 {
			times = append(times, t)
		}
		if t := l.Start + l.Hold; l.Hold > 0 && t > 0 {
			times = append(times, t)
		}
	}
	var most uint64
	for _, t := range times {
		var sum uint64
		for _, l := range loads {
			sum += l.at(t)
		}
		most = max(most, sum)
	}
	return most
}

// Candidate is a ready edge.
type Candidate struct {
	ID   int  // the caller's handle
	Load Load // its predicted memory once started
	// Waiting is how long memory has held it back so far.
	Waiting time.Duration
}

// State is everything Admit looks at.
type State struct {
	// Ready lists the ready edges, highest priority first.
	Ready []Candidate
	// Running is how many jobs are running, and Loads their predicted
	// memory.
	Running int
	Loads   []Load
	// Budget is the memory the running jobs may use.
	Budget uint64
	// Slots is how many more jobs -j and -l allow.
	Slots int
	// MinJobs is the floor: while fewer jobs run, an edge starts whatever
	// its prediction, so the build can't stall.
	MinJobs int
	// Paused is the brake: memory is short, so start nothing beyond the
	// floor.
	Paused bool
	// MaxWait is how long a held-back edge may be passed over by others
	// before it gets a reservation.
	MaxWait time.Duration
	// Holder is the edge given a reservation in an earlier round, if
	// HasHolder. A reservation is kept until its edge starts, so a
	// higher-priority edge can't take it over and starve its holder.
	Holder    int
	HasHolder bool
}

// Result is Admit's decision.
type Result struct {
	// Start lists the edges to start now, in order.
	Start []int
	// Held lists the edges that memory held back this time.
	Held []int
	// Reservation is the edge holding a reservation, or -1.
	Reservation int
}

// Admit decides which ready edges start now. The rules:
//   - An edge starts if there's a slot and, with it, the predicted total
//     stays within the budget at every moment from now on (Most).
//     Otherwise it's held back.
//   - While fewer than MinJobs run, edges start regardless of memory, so
//     an edge predicted above the whole budget runs once nothing else
//     does.
//   - Smaller edges may start ahead of a held-back bigger one
//     (backfilling) until it has waited MaxWait. Then it holds a
//     reservation: other edges start only if they fit alongside it, and
//     it gets the first slot as soon as it fits (and the floor before
//     anything else). That bounds how long any edge can starve, as in EASY
//     backfilling.
func Admit(s State) Result {
	res := Result{Reservation: -1}
	running := s.Running
	loads := append([]Load(nil), s.Loads...)
	// with is the most the running jobs need alongside the extra loads.
	with := func(extra ...Load) uint64 {
		all := make([]Load, 0, len(loads)+len(extra))
		return Most(append(append(all, loads...), extra...))
	}
	var reservedLoad Load
	for _, c := range s.Ready {
		if s.HasHolder && c.ID == s.Holder {
			res.Reservation, reservedLoad = c.ID, c.Load
		}
	}
	if res.Reservation < 0 {
		for _, c := range s.Ready {
			if c.Waiting >= s.MaxWait && with(c.Load) > s.Budget {
				res.Reservation, reservedLoad = c.ID, c.Load
				break
			}
		}
	}
	start := func(c Candidate) {
		res.Start = append(res.Start, c.ID)
		running++
		loads = append(loads, c.Load)
	}
	// The reservation gets the first slot once it fits, and the floor
	// before anything else; otherwise smaller edges could keep taking the
	// slots it needs.
	floor := running < s.MinJobs
	if res.Reservation >= 0 && s.Slots > 0 && (floor || !s.Paused && with(reservedLoad) <= s.Budget) {
		for _, c := range s.Ready {
			if c.ID == res.Reservation {
				start(c)
				res.Reservation = -1
			}
		}
	}
	for _, c := range s.Ready {
		if len(res.Start) >= s.Slots {
			break
		}
		if len(res.Start) > 0 && res.Start[0] == c.ID {
			continue // the reservation, started above
		}
		switch {
		case running < s.MinJobs:
			// The floor.
		case s.Paused:
			return res
		case with(c.Load) > s.Budget:
			res.Held = append(res.Held, c.ID)
			continue
		case res.Reservation >= 0 && c.ID != res.Reservation && with(c.Load, reservedLoad) > s.Budget:
			// Would delay the reservation.
			res.Held = append(res.Held, c.ID)
			continue
		}
		if c.ID == res.Reservation {
			res.Reservation = -1
		}
		start(c)
	}
	return res
}
