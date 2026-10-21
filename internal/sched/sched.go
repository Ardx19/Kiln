// Package sched decides which ready edges may start so that the predicted
// memory of running jobs stays within a budget. Admit is a pure function,
// so the policy can be tested with a simulated clock.
package sched

import "time"

// Candidate is a ready edge.
type Candidate struct {
	ID   int    // the caller's handle
	Need uint64 // predicted peak memory, bytes
	// Waiting is how long memory has held it back so far.
	Waiting time.Duration
}

// State is everything Admit looks at.
type State struct {
	// Ready lists the ready edges, highest priority first.
	Ready []Candidate
	// Running is how many jobs are running, and Reserved their predicted
	// memory.
	Running  int
	Reserved uint64
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
//   - An edge starts if there's a slot and its prediction fits in what's
//     left of the budget. Otherwise it's held back.
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
	running, reserved := s.Running, s.Reserved
	var reservedNeed uint64
	for _, c := range s.Ready {
		if s.HasHolder && c.ID == s.Holder {
			res.Reservation, reservedNeed = c.ID, c.Need
		}
	}
	if res.Reservation < 0 {
		for _, c := range s.Ready {
			if c.Waiting >= s.MaxWait && reserved+c.Need > s.Budget {
				res.Reservation, reservedNeed = c.ID, c.Need
				break
			}
		}
	}
	start := func(c Candidate) {
		res.Start = append(res.Start, c.ID)
		running++
		reserved += c.Need
	}
	// The reservation gets the first slot once it fits, and the floor
	// before anything else; otherwise smaller edges could keep taking the
	// slots it needs.
	floor := running < s.MinJobs
	if res.Reservation >= 0 && s.Slots > 0 && (floor || !s.Paused && reserved+reservedNeed <= s.Budget) {
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
		case reserved+c.Need > s.Budget:
			res.Held = append(res.Held, c.ID)
			continue
		case res.Reservation >= 0 && c.ID != res.Reservation && reserved+c.Need+reservedNeed > s.Budget:
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
