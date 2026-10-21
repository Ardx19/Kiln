package build

import (
	"fmt"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/memdb"
	"github.com/Ardx19/kiln/internal/memprobe"
	"github.com/Ardx19/kiln/internal/sched"
	"github.com/Ardx19/kiln/internal/state"
	"github.com/Ardx19/kiln/internal/sysinfo"
)

// MemConfig holds the memory-aware scheduler's options.
type MemConfig struct {
	// Off turns the scheduler off: edges start as soon as -j allows, like
	// Ninja.
	Off bool
	// Budget is the memory running jobs may use, in bytes; 0 means 90% of
	// the headroom when the build starts.
	Budget uint64
	// MinJobs is how many jobs always run, whatever their predictions
	// (0 means 1).
	MinJobs int
	// Debug prints scheduling decisions (-d sched).
	Debug bool
	// LogPath is the memory history, ".kiln/mem.log" if empty.
	LogPath string
	// Interval is how often jobs are sampled; 0 means
	// memprobe.DefaultInterval.
	Interval time.Duration
}

const (
	// batchLimit bounds how many ready edges one admission round looks at,
	// so a build with thousands of ready edges doesn't sort them on every
	// tick. Edges beyond it can't backfill ahead of the first batchLimit.
	batchLimit = 128
	// maxWait is how long memory may hold an edge back, while smaller edges
	// start past it, before it gets a reservation.
	maxWait = 10 * time.Second
	// killCooldown is how long to wait after killing a job for its memory
	// to come back before killing another.
	killCooldown = time.Second
	// thrashPages is the swap traffic in one tick (pages in plus out) that
	// counts as thrashing.
	thrashPages = 2048
	// retryFactor raises the estimate of an edge that was OOM-killed or
	// stopped by the brake.
	retryFactor = 1.5
)

// memJob is a running job.
type memJob struct {
	pgid  int // 0 for a console job, which can't be measured
	need  uint64
	start time.Time
	oom   uint64 // the OOM counter when it started
}

// memSched is the scheduler's state for one build.
type memSched struct {
	b       *Builder
	cfg     MemConfig
	sampler *memprobe.Sampler
	log     *memdb.Log
	pred    *memdb.Predictor
	budget  uint64
	minJobs int
	// low and critical are the headroom watermarks: below low nothing new
	// starts, below critical the youngest job is stopped.
	low, critical uint64

	descr     map[*graph.Edge]memdb.Edge
	boost     map[*graph.Edge]uint64 // raised estimates
	retried   map[*graph.Edge]bool
	held      []*graph.Edge // ready, but held back by memory; best first
	heldSince map[*graph.Edge]time.Time
	holder    *graph.Edge // holds a reservation
	jobs      map[*graph.Edge]*memJob
	killed    map[*graph.Edge]bool // stopped by the brake, awaiting their result

	paused   bool
	mem      sysinfo.Memory
	prevSwap uint64
	lastKill time.Time
	warned   bool
}

// newMemSched returns the scheduler for b's build, or nil if it's off or
// memory can't be read.
func newMemSched(b *Builder) *memSched {
	c := b.cfg.Mem
	if c.Off {
		return nil
	}
	mem := sysinfo.ReadMemory()
	budget := c.Budget
	if budget == 0 {
		budget = mem.Headroom() / 10 * 9
	}
	if budget == 0 {
		return nil
	}
	if c.LogPath == "" {
		c.LogPath = ".kiln/mem.log"
	}
	log, err := memdb.Open(c.LogPath)
	if err != nil {
		b.status.Warn(fmt.Sprintf("memory scheduling is off: %v", err))
		return nil
	}
	s := &memSched{b: b, cfg: c, log: log, budget: budget, minJobs: max(c.MinJobs, 1), mem: mem,
		low: max(budget/10, 256<<20), critical: max(budget/20, 128<<20),
		descr: map[*graph.Edge]memdb.Edge{}, boost: map[*graph.Edge]uint64{}, retried: map[*graph.Edge]bool{},
		heldSince: map[*graph.Edge]time.Time{}, jobs: map[*graph.Edge]*memJob{}, killed: map[*graph.Edge]bool{},
		sampler: memprobe.New()}
	s.pred = memdb.NewPredictor(log, s.history())
	s.prevSwap = mem.SwapIn + mem.SwapOut
	s.debugf("budget %s, headroom %s, low %s, critical %s, min jobs %d",
		size(s.budget), size(mem.Headroom()), size(s.low), size(s.critical), s.minJobs)
	return s
}

// history describes the edges that have a record, to teach the predictor's
// groups.
func (s *memSched) history() []memdb.Edge {
	var edges []memdb.Edge
	for _, e := range s.b.state.Edges {
		if e.IsPhony() || len(e.Outputs) == 0 || s.log.Records[e.Outputs[0].Path] == nil {
			continue
		}
		edges = append(edges, s.describe(e))
	}
	return edges
}

// describe returns what the predictor needs to know about e. The command
// is only worked out when the rule's name doesn't say what e does.
func (s *memSched) describe(e *graph.Edge) memdb.Edge {
	if d, ok := s.descr[e]; ok {
		return d
	}
	d := memdb.Edge{Rule: e.Rule.Name}
	if len(e.Outputs) > 0 {
		d.Output = e.Outputs[0].Path
	}
	if memdb.Classify(d.Rule, "") == memdb.Other {
		d.Command, _ = e.Command(false)
	}
	s.descr[e] = d
	return d
}

// start begins sampling; wake is signalled after each tick.
func (s *memSched) start(wake chan<- struct{}) {
	interval := s.cfg.Interval
	if interval == 0 {
		interval = memprobe.DefaultInterval
	}
	s.sampler.Start(interval, func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
}

func (s *memSched) close() {
	s.sampler.Stop()
	s.sampler.Close()
	if err := s.log.Close(); err != nil {
		s.warn(err)
	}
}

func (s *memSched) warn(err error) {
	if !s.warned {
		s.warned = true
		s.b.status.Warn(fmt.Sprintf("recording memory use: %v", err))
	}
}

// need is the memory e is expected to use.
func (s *memSched) need(e *graph.Edge) uint64 {
	if n, ok := s.boost[e]; ok {
		return n
	}
	return s.pred.Predict(s.describe(e))
}

// reserved is the memory the running jobs may use: each one's prediction,
// or what it has used so far if that's more.
func (s *memSched) reserved() uint64 {
	var sum uint64
	for _, j := range s.jobs {
		n := j.need
		if j.pgid != 0 {
			if cur, ok := s.sampler.Current(j.pgid); ok {
				n = max(n, cur.Peak)
			}
		}
		sum += n
	}
	return sum
}

// candidates returns the ready edges memory may hold back, best first:
// those already held, then more from the plan, up to batchLimit. Phony
// edges finish on the spot.
func (s *memSched) candidates() ([]*graph.Edge, error) {
	cands := s.held
	s.held = nil
	for len(cands) < batchLimit {
		e := s.b.plan.findWork()
		if e == nil {
			break
		}
		if e.IsPhony() {
			if err := s.b.startEdge(e); err != nil {
				return nil, err
			}
			if err := s.b.plan.edgeFinished(e, true); err != nil {
				return nil, err
			}
			continue
		}
		cands = append(cands, e)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].CriticalPath != cands[j].CriticalPath {
			return cands[i].CriticalPath > cands[j].CriticalPath
		}
		return cands[i].ID < cands[j].ID
	})
	return cands, nil
}

// admit picks the ready edges to start, up to slots.
func (s *memSched) admit(slots int) ([]*graph.Edge, error) {
	cands, err := s.candidates()
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, nil
	}
	now := time.Now()
	st := sched.State{Running: len(s.jobs), Reserved: s.reserved(), Budget: s.budget, Slots: slots,
		MinJobs: s.minJobs, Paused: s.paused, MaxWait: maxWait}
	for i, e := range cands {
		var waiting time.Duration
		if t, ok := s.heldSince[e]; ok {
			waiting = now.Sub(t)
		}
		st.Ready = append(st.Ready, sched.Candidate{ID: i, Need: s.need(e), Waiting: waiting})
		if e == s.holder {
			st.Holder, st.HasHolder = i, true
		}
	}
	res := sched.Admit(st)
	starting := make([]bool, len(cands))
	var edges []*graph.Edge
	for _, i := range res.Start {
		starting[i] = true
		edges = append(edges, cands[i])
		s.debugf("start %s (needs %s; reserved %s of %s)", name(cands[i]), size(st.Ready[i].Need), size(st.Reserved), size(s.budget))
	}
	for _, i := range res.Held {
		if _, ok := s.heldSince[cands[i]]; !ok {
			s.heldSince[cands[i]] = now
			s.debugf("hold %s (needs %s; reserved %s of %s)", name(cands[i]), size(st.Ready[i].Need), size(st.Reserved), size(s.budget))
		}
	}
	old := s.holder
	s.holder = nil
	if res.Reservation >= 0 {
		s.holder = cands[res.Reservation]
		if s.holder != old {
			s.debugf("reserve for %s (needs %s)", name(s.holder), size(st.Ready[res.Reservation].Need))
		}
	}
	for i, e := range cands {
		if starting[i] {
			delete(s.heldSince, e)
		} else {
			s.held = append(s.held, e)
		}
	}
	return edges, nil
}

// started records that e is running in process group pgid.
func (s *memSched) started(e *graph.Edge, pgid int) {
	s.jobs[e] = &memJob{pgid: pgid, need: s.need(e), start: time.Now(), oom: s.mem.OOMKills}
	if pgid != 0 {
		s.sampler.Track(pgid)
	}
}

// finished takes in a command's result. It reports whether the edge must
// run again: the brake stopped it, or the kernel's OOM killer did.
func (s *memSched) finished(res *result) (requeue bool) {
	e := res.edge
	j := s.jobs[e]
	if j == nil {
		return false
	}
	delete(s.jobs, e)
	var seen memprobe.Job
	if j.pgid != 0 {
		seen = s.sampler.Untrack(j.pgid)
	}
	peak := max(seen.Peak, res.maxRSS)
	if s.killed[e] {
		delete(s.killed, e)
		s.boost[e] = uint64(float64(max(peak, j.need)) * retryFactor)
		s.held = append(s.held, e)
		return true
	}
	if res.success() {
		cmd, err := e.Command(true)
		if err == nil && len(e.Outputs) > 0 {
			hash := state.HashCommand(s.b.log.Version(), cmd)
			err = s.pred.Observe(s.describe(e), peak, time.Since(j.start).Milliseconds(), hash, seen.Backend)
		}
		if err != nil {
			s.warn(err)
		}
		return false
	}
	if res.interrupted() || s.retried[e] {
		return false
	}
	// The kernel's OOM killer: its counter rose while the job ran, or the
	// compiler said so. A job that got SIGKILL some other way is retried
	// too; it costs one more run at most.
	oomed := s.mem.OOMKills > j.oom || strings.Contains(res.output, "Killed signal terminated program") ||
		res.signal == syscall.SIGKILL || res.exit == 128+int(syscall.SIGKILL)
	if !oomed {
		return false
	}
	s.retried[e] = true
	s.boost[e] = uint64(float64(max(peak, j.need)) * retryFactor)
	s.held = append(s.held, e)
	s.b.status.Info("%s was killed (peak %s); trying again with %s", name(e), size(peak), size(s.boost[e]))
	return true
}

// tick looks at memory and applies the brake: below the low watermark (or
// while swapping heavily) nothing new starts; below the critical one the
// youngest job is stopped, to run again once there's room, rather than
// leave the kernel to kill something at random.
func (s *memSched) tick() {
	m := sysinfo.ReadMemory()
	s.mem = m
	swap := m.SwapIn + m.SwapOut
	thrash := swap-s.prevSwap > thrashPages
	s.prevSwap = swap
	headroom := m.Headroom()
	paused := headroom < s.low || thrash
	if paused != s.paused {
		s.debugf("brake %s (headroom %s, swap %d pages)", map[bool]string{true: "on", false: "off"}[paused], size(headroom), swap)
	}
	s.paused = paused
	if headroom >= s.critical || len(s.jobs) <= s.minJobs || time.Since(s.lastKill) < killCooldown {
		return
	}
	var victim *graph.Edge
	for e, j := range s.jobs {
		if j.pgid == 0 || s.killed[e] {
			continue
		}
		if victim == nil || j.start.After(s.jobs[victim].start) {
			victim = e
		}
	}
	if victim == nil {
		return
	}
	s.killed[victim] = true
	s.lastKill = time.Now()
	s.b.runner.kill(victim)
	s.b.status.Info("memory is short (%s left); stopped %s to run it again later", size(headroom), name(victim))
}

func (s *memSched) debugf(format string, args ...any) {
	if s.cfg.Debug {
		s.b.status.Debug("sched: " + fmt.Sprintf(format, args...))
	}
}

// name is how messages refer to an edge: its first output.
func name(e *graph.Edge) string {
	if len(e.Outputs) == 0 {
		return "a command"
	}
	return e.Outputs[0].Path
}

// size formats bytes with a binary unit.
func size(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d KiB", n>>10)
}

// startAdmitted starts the edges the scheduler admits, returning how many.
func (b *Builder) startAdmitted(slots int) (int, error) {
	edges, err := b.mem.admit(slots)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range edges {
		if gen, _ := e.Binding("generator"); gen != "" && b.log != nil {
			if err := b.log.Close(); err != nil {
				return n, fmt.Errorf("closing the build log: %v", err)
			}
		}
		if err := b.startEdge(e); err != nil {
			return n, err
		}
		b.mem.started(e, b.started[e].pgid)
		n++
	}
	return n, nil
}

// requeue undoes a command that is going to run again: what it left
// half-written is removed, and it no longer counts as started.
func (b *Builder) requeue(e *graph.Edge) {
	delete(b.started, e)
	b.status.edgeRequeued(e)
	b.discard(e)
}
