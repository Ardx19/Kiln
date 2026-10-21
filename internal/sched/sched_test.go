package sched

import (
	"math/rand"
	"sort"
	"testing"
	"time"
)

const gib = 1 << 30

func TestAdmitRules(t *testing.T) {
	ready := func(needs ...uint64) []Candidate {
		var cs []Candidate
		for i, n := range needs {
			cs = append(cs, Candidate{ID: i, Need: n})
		}
		return cs
	}
	const long = time.Hour
	for _, c := range []struct {
		name        string
		s           State
		start, held []int
		reservation int
	}{
		{"fits", State{Ready: ready(1*gib, 1*gib), Budget: 4 * gib, Slots: 8, MinJobs: 1, MaxWait: long}, []int{0, 1}, nil, -1},
		{"backfill", State{Ready: ready(3*gib, 2*gib, 1*gib), Budget: 4 * gib, Slots: 8, MinJobs: 1, MaxWait: long}, []int{0, 2}, []int{1}, -1},
		{"MaxWait 0 reserves for a held edge at once", State{Ready: ready(2*gib, 1*gib), Running: 1, Reserved: 3 * gib, Budget: 4 * gib, Slots: 8, MinJobs: 1}, nil, []int{0, 1}, 0},
		{"a reservation is kept", State{Ready: []Candidate{{ID: 0, Need: 3 * gib, Waiting: time.Hour}, {ID: 1, Need: 3 * gib, Waiting: time.Minute}},
			Running: 1, Reserved: 2 * gib, Budget: 4 * gib, Slots: 8, MinJobs: 1, MaxWait: 30 * time.Second, Holder: 1, HasHolder: true}, nil, []int{0, 1}, 1},
		{"slots", State{Ready: ready(1, 1, 1), Budget: 4 * gib, Slots: 2, MinJobs: 1, MaxWait: long}, []int{0, 1}, nil, -1},
		{"oversized waits", State{Ready: ready(9*gib, 1*gib), Running: 1, Reserved: 1 * gib, Budget: 4 * gib, Slots: 8, MinJobs: 1, MaxWait: long}, []int{1}, []int{0}, -1},
		{"oversized alone", State{Ready: ready(9*gib, 1*gib), Budget: 4 * gib, Slots: 8, MinJobs: 1, MaxWait: long}, []int{0}, []int{1}, -1},
		{"brake", State{Ready: ready(1, 1), Running: 2, Budget: 4 * gib, Slots: 8, MinJobs: 1, Paused: true}, nil, nil, -1},
		{"brake keeps the floor", State{Ready: ready(1, 1), Budget: 4 * gib, Slots: 8, MinJobs: 1, Paused: true}, []int{0}, nil, -1},
		{"reservation", State{Ready: []Candidate{{ID: 0, Need: 1 * gib}, {ID: 1, Need: 3 * gib, Waiting: time.Minute}, {ID: 2, Need: 1 * gib}},
			Running: 1, Reserved: 2 * gib, Budget: 4 * gib, Slots: 8, MinJobs: 1, MaxWait: 30 * time.Second}, nil, []int{0, 1, 2}, 1},
		{"reservation gets the floor", State{Ready: []Candidate{{ID: 0, Need: 1 * gib}, {ID: 1, Need: 9 * gib, Waiting: time.Minute}},
			Budget: 4 * gib, Slots: 8, MinJobs: 1, MaxWait: 30 * time.Second}, []int{1}, []int{0}, -1},
	} {
		r := Admit(c.s)
		if !equal(r.Start, c.start) || !equal(r.Held, c.held) || r.Reservation != c.reservation {
			t.Errorf("%s: Admit = %+v; want start %v, held %v, reservation %d", c.name, r, c.start, c.held, c.reservation)
		}
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSimulation runs random workloads through Admit with a simulated
// clock and checks the policy's guarantees.
func TestSimulation(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for round := range 1000 {
		budget := uint64(2+rng.Intn(14)) * gib
		jobs, minJobs := 2+rng.Intn(14), 1+rng.Intn(2)
		const maxWait = 20 * time.Second
		type job struct {
			id       int
			need     uint64
			dur      time.Duration
			ready    time.Duration
			blocked  time.Duration // when memory first held it back
			reserved time.Duration // when it got a reservation
			started  bool
			finishAt time.Duration
		}
		var all []*job
		for i := range 30 + rng.Intn(70) {
			need := uint64(rng.Int63n(int64(budget) * 2 / 3))
			if rng.Intn(15) == 0 {
				need = budget + uint64(rng.Int63n(int64(budget))) // oversized
			}
			all = append(all, &job{id: i, need: need, dur: time.Duration(1+rng.Intn(60)) * time.Second,
				ready: time.Duration(rng.Intn(30)) * time.Second, blocked: -1, reserved: -1})
		}
		var now, maxDur time.Duration
		for _, j := range all {
			maxDur = max(maxDur, j.dur)
		}
		var running []*job
		done, holder := 0, -1
		for done < len(all) {
			// Ready edges in priority order (by ID).
			var ready []Candidate
			for _, j := range all {
				if !j.started && j.ready <= now {
					w := time.Duration(0)
					if j.blocked >= 0 {
						w = now - j.blocked
					}
					ready = append(ready, Candidate{ID: j.id, Need: j.need, Waiting: w})
				}
			}
			var reserved uint64
			for _, r := range running {
				reserved += r.need
			}
			r := Admit(State{Ready: ready, Running: len(running), Reserved: reserved,
				Budget: budget, Slots: jobs - len(running), MinJobs: minJobs, MaxWait: maxWait,
				Holder: holder, HasHolder: holder >= 0})
			holder = r.Reservation
			for _, id := range r.Held {
				if all[id].blocked < 0 {
					all[id].blocked = now
				}
			}
			if r.Reservation >= 0 && all[r.Reservation].reserved < 0 {
				all[r.Reservation].reserved = now
			}
			for _, id := range r.Start {
				j := all[id]
				if j.need > budget && len(running) >= minJobs {
					t.Fatalf("round %d: oversized job %d started beside %d others", round, id, len(running))
				}
				if j.reserved >= 0 && now-j.reserved > maxDur {
					t.Errorf("round %d: job %d started %v after its reservation; the longest job takes %v", round, id, now-j.reserved, maxDur)
				}
				j.started, j.finishAt = true, now+j.dur
				running = append(running, j)
				reserved += j.need
			}
			if len(running) > jobs {
				t.Fatalf("round %d: %d running, -j is %d", round, len(running), jobs)
			}
			if len(running) > minJobs && reserved > budget {
				var ns []uint64
				for _, r := range running {
					ns = append(ns, r.need)
				}
				t.Fatalf("round %d: %d jobs reserve %d over a budget of %d: %v", round, len(running), reserved, budget, ns)
			}
			// Advance to the next event.
			next := time.Duration(1 << 62)
			for _, r := range running {
				next = min(next, r.finishAt)
			}
			for _, j := range all {
				if !j.started && j.ready > now {
					next = min(next, j.ready)
				}
			}
			for _, id := range r.Held {
				if b := all[id].blocked; b >= 0 && b+maxWait > now {
					next = min(next, b+maxWait)
				}
			}
			if next == 1<<62 {
				t.Fatalf("round %d: stuck with %d left", round, len(all)-done)
			}
			now = max(next, now+time.Millisecond)
			sort.Slice(running, func(a, b int) bool { return running[a].finishAt < running[b].finishAt })
			for len(running) > 0 && running[0].finishAt <= now {
				running = running[1:]
				done++
			}
		}
	}
}
