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
			cs = append(cs, Candidate{ID: i, Load: Flat(n)})
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
		{"MaxWait 0 reserves for a held edge at once", State{Ready: ready(2*gib, 1*gib), Running: 1, Loads: []Load{Flat(3 * gib)}, Budget: 4 * gib, Slots: 8, MinJobs: 1}, nil, []int{0, 1}, 0},
		{"a reservation is kept", State{Ready: []Candidate{{ID: 0, Load: Flat(3 * gib), Waiting: time.Hour}, {ID: 1, Load: Flat(3 * gib), Waiting: time.Minute}},
			Running: 1, Loads: []Load{Flat(2 * gib)}, Budget: 4 * gib, Slots: 8, MinJobs: 1, MaxWait: 30 * time.Second, Holder: 1, HasHolder: true}, nil, []int{0, 1}, 1},
		{"slots", State{Ready: ready(1, 1, 1), Budget: 4 * gib, Slots: 2, MinJobs: 1, MaxWait: long}, []int{0, 1}, nil, -1},
		{"oversized waits", State{Ready: ready(9*gib, 1*gib), Running: 1, Loads: []Load{Flat(1 * gib)}, Budget: 4 * gib, Slots: 8, MinJobs: 1, MaxWait: long}, []int{1}, []int{0}, -1},
		{"oversized alone", State{Ready: ready(9*gib, 1*gib), Budget: 4 * gib, Slots: 8, MinJobs: 1, MaxWait: long}, []int{0}, []int{1}, -1},
		{"brake", State{Ready: ready(1, 1), Running: 2, Budget: 4 * gib, Slots: 8, MinJobs: 1, Paused: true}, nil, nil, -1},
		{"brake keeps the floor", State{Ready: ready(1, 1), Budget: 4 * gib, Slots: 8, MinJobs: 1, Paused: true}, []int{0}, nil, -1},
		{"reservation", State{Ready: []Candidate{{ID: 0, Load: Flat(1 * gib)}, {ID: 1, Load: Flat(3 * gib), Waiting: time.Minute}, {ID: 2, Load: Flat(1 * gib)}},
			Running: 1, Loads: []Load{Flat(2 * gib)}, Budget: 4 * gib, Slots: 8, MinJobs: 1, MaxWait: 30 * time.Second}, nil, []int{0, 1, 2}, 1},
		{"reservation gets the floor", State{Ready: []Candidate{{ID: 0, Load: Flat(1 * gib)}, {ID: 1, Load: Flat(9 * gib), Waiting: time.Minute}},
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
// clock and checks the policy's guarantees: with flat loads (jobs of
// unknown duration) and with ramped ones, whose memory follows the model
// that Load describes and ends before its Hold.
func TestSimulation(t *testing.T) {
	for _, ramped := range []bool{false, true} {
		simulate(t, ramped)
	}
}

func simulate(t *testing.T, ramped bool) {
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
			startAt  time.Duration
			finishAt time.Duration
		}
		load := func(j *job, now time.Duration) Load {
			if !ramped {
				return Flat(j.need)
			}
			l := Load{Ramp: j.dur / 2, Hold: j.dur * 5 / 4, Peak: j.need}
			if j.started {
				l.Start = j.startAt - now
			}
			return l
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
					ready = append(ready, Candidate{ID: j.id, Load: load(j, now), Waiting: w})
				}
			}
			var loads []Load
			for _, r := range running {
				loads = append(loads, load(r, now))
			}
			r := Admit(State{Ready: ready, Running: len(running), Loads: loads,
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
				j.started, j.startAt, j.finishAt = true, now, now+j.dur
				running = append(running, j)
			}
			loads = loads[:0]
			for _, r := range running {
				loads = append(loads, load(r, now))
			}
			if len(running) > jobs {
				t.Fatalf("round %d: %d running, -j is %d", round, len(running), jobs)
			}
			if most := Most(loads); len(running) > minJobs && most > budget {
				var ns []uint64
				for _, r := range running {
					ns = append(ns, r.need)
				}
				t.Fatalf("round %d (ramped %v): %d jobs need %d at most, over a budget of %d: %v", round, ramped, len(running), most, budget, ns)
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

func TestMost(t *testing.T) {
	ramp := func(start, dur time.Duration, peak uint64) Load {
		return Load{Start: start, Ramp: dur / 2, Hold: dur * 5 / 4, Peak: peak}
	}
	for _, c := range []struct {
		name  string
		loads []Load
		want  uint64
	}{
		{"flat loads add up", []Load{Flat(1 * gib), Flat(2 * gib)}, 3 * gib},
		{"one ramp reaches its peak", []Load{ramp(0, 100*time.Second, 2*gib)}, 2 * gib},
		// A job near its end and a new one: when the old one ends, at 25 s,
		// the new one has reached a quarter of its 3 GiB.
		{"peaks that don't meet", []Load{ramp(-100*time.Second, 100*time.Second, 3*gib), ramp(0, 200*time.Second, 3*gib)}, 3*gib + 3*gib/4},
		// A young job reaches its peak while the new one still grows.
		{"peaks that meet", []Load{ramp(-10*time.Second, 100*time.Second, 3*gib), ramp(0, 100*time.Second, 3*gib)}, 6 * gib},
		{"a floor above the ramp", []Load{{Start: -time.Second, Ramp: 100 * time.Second, Hold: 125 * time.Second, Peak: 2 * gib, Floor: 1 * gib}, Flat(1 * gib)}, 3 * gib},
		{"an ended load counts for nothing", []Load{ramp(-200*time.Second, 100*time.Second, 9*gib), Flat(1 * gib)}, 1 * gib},
	} {
		if got := Most(c.loads); got != c.want {
			t.Errorf("%s: Most = %.2f GiB, want %.2f", c.name, float64(got)/gib, float64(c.want)/gib)
		}
	}
}

func TestAdmitOverlapsKnownDurations(t *testing.T) {
	ramp := func(start, dur time.Duration, peak uint64) Load {
		return Load{Start: start, Ramp: dur / 2, Hold: dur * 5 / 4, Peak: peak}
	}
	s := State{Ready: []Candidate{{ID: 0, Load: ramp(0, 200*time.Second, 3*gib)}}, Running: 1,
		Budget: 4 * gib, Slots: 4, MinJobs: 1, MaxWait: time.Hour}
	// The running job is at its 3 GiB peak but ends within 25 s: the new
	// one fits beside it, where peak plus peak (6 GiB) would not.
	s.Loads = []Load{ramp(-100*time.Second, 100*time.Second, 3*gib)}
	if r := Admit(s); !equal(r.Start, []int{0}) {
		t.Errorf("beside a job about to end: Admit = %+v, want the new job started", r)
	}
	// A young job reaches its peak while the new one is near its own.
	s.Loads = []Load{ramp(-10*time.Second, 100*time.Second, 3*gib)}
	if r := Admit(s); !equal(r.Held, []int{0}) {
		t.Errorf("beside a young job: Admit = %+v, want the new job held", r)
	}
	// With no known durations it is the old rule: peaks add up.
	s.Loads = []Load{Flat(3 * gib)}
	s.Ready[0].Load = Flat(3 * gib)
	if r := Admit(s); !equal(r.Held, []int{0}) {
		t.Errorf("flat loads: Admit = %+v, want the new job held", r)
	}
}
