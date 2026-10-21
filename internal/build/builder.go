package build

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Ardx19/kiln/internal/depfile"
	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/state"
)

// Config holds a build's options.
type Config struct {
	// DryRun pretends every command succeeds without running it, like
	// ninja -n. Unlike Ninja, a kiln dry run doesn't touch the disk.
	DryRun bool
	// Parallelism is the -j limit; 0 means no limit.
	Parallelism int
	// FailuresAllowed is how many failed commands to tolerate before
	// stopping (-k); Ninja's default is 1.
	FailuresAllowed int
	// KeepDepfile and KeepRsp keep files Ninja would delete (-d).
	KeepDepfile, KeepRsp bool
	Profile              Profile
}

// Builder brings targets up to date. It is a port of Ninja's Builder.
type Builder struct {
	state  *graph.State
	scan   *Scanner
	plan   *Plan
	status *Status
	cfg    Config
	log    *state.LogWriter // nil in a dry run
	deps   *state.DepsWriter

	lockPath   string
	start      time.Time
	started    map[*graph.Edge]running
	runner     *runner
	exitStatus int   // the last failing command's status
	avgCost    int64 // average command duration in the log, for edges without one
}

type running struct {
	startMS    int
	startStamp int64 // the lock file's mtime when the edge started (v6+)
}

// NewBuilder returns a builder for st. log and deps may be nil for a dry
// run.
func NewBuilder(st *graph.State, scan *Scanner, status *Status, cfg Config, log *state.LogWriter, deps *state.DepsWriter) *Builder {
	if cfg.FailuresAllowed == 0 {
		cfg.FailuresAllowed = 1
	}
	lock := ".ninja_lock"
	if dir := st.Bindings.LookupVariable("builddir"); dir != "" {
		lock = dir + "/" + lock
	}
	b := &Builder{state: st, scan: scan, status: status, cfg: cfg, log: log, deps: deps,
		lockPath: lock, start: time.Now(), started: map[*graph.Edge]running{}}
	b.plan = newPlan(b)
	status.prev = b.prevDuration
	return b
}

// AddTarget scans target and plans whatever it needs, including any
// validations the scan finds.
func (b *Builder) AddTarget(target *graph.Node) error {
	validations, err := b.scan.RecomputeDirty(target)
	if err != nil {
		return err
	}
	if target.InEdge == nil || !target.InEdge.OutputsReady {
		if err := b.plan.AddTarget(target); err != nil {
			return err
		}
	}
	for _, v := range validations {
		if v.InEdge != nil && !v.InEdge.OutputsReady {
			if err := b.plan.AddTarget(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// AlreadyUpToDate reports whether there's nothing to run.
func (b *Builder) AlreadyUpToDate() bool { return !b.plan.moreToDo() }

// BuildError is a failed build: Msg is Ninja's reason ("subcommand
// failed", "interrupted by user"...) and Exit the exit code to use.
type BuildError struct {
	Msg  string
	Exit int
}

func (e *BuildError) Error() string { return e.Msg }

// Build runs the plan, a port of Ninja's Builder::Build.
func (b *Builder) Build() error {
	b.plan.computeCriticalPath(b.edgeCost)
	b.plan.scheduleInitialEdges()
	b.status.buildStarted()
	defer b.status.buildFinished()
	var clock *clockWatch
	if !b.cfg.DryRun {
		b.runner = newRunner(b.cfg.Parallelism)
		defer b.runner.close()
		defer b.removeLock()
		clock = newClockWatch()
		defer b.checkClock(clock)
	}
	var dryQueue []*graph.Edge
	pending := 0
	failuresAllowed := b.cfg.FailuresAllowed
	for b.plan.moreToDo() {
		if failuresAllowed > 0 {
			capacity := 1 << 30
			if b.runner != nil {
				capacity = b.runner.canRunMore()
			}
			for capacity > 0 {
				e := b.plan.findWork()
				if e == nil {
					break
				}
				if gen, _ := e.Binding("generator"); gen != "" && b.log != nil {
					// The generator may run `ninja -t restat` on the log.
					if err := b.log.Close(); err != nil {
						return b.fail(fmt.Errorf("closing the build log: %v", err))
					}
				}
				if err := b.startEdge(e); err != nil {
					return b.fail(err)
				}
				if e.IsPhony() {
					if err := b.plan.edgeFinished(e, true); err != nil {
						return b.fail(err)
					}
					continue
				}
				if b.cfg.DryRun {
					dryQueue = append(dryQueue, e)
				}
				pending++
				capacity--
				if b.runner != nil {
					capacity = min(capacity, b.runner.canRunMore())
				}
			}
			if pending == 0 && !b.plan.moreToDo() {
				break
			}
		}
		if pending > 0 {
			var res *result
			if b.cfg.DryRun {
				res = &result{edge: dryQueue[0]}
				dryQueue = dryQueue[1:]
			} else if res = b.runner.wait(); res == nil || res.interrupted() {
				b.checkClock(clock)
				b.cleanup()
				return &BuildError{Msg: "interrupted by user", Exit: b.cfg.Profile.InterruptExit()}
			}
			pending--
			err := b.finishCommand(res)
			b.checkClock(clock)
			if !res.success() {
				b.exitStatus = res.status()
			}
			if err != nil {
				return b.fail(err)
			}
			if !res.success() && failuresAllowed > 0 {
				failuresAllowed--
			}
			continue
		}
		msg := "stuck [this is a bug]"
		switch {
		case failuresAllowed == 0 && b.cfg.FailuresAllowed > 1:
			msg = "subcommands failed"
		case failuresAllowed == 0:
			msg = "subcommand failed"
		case failuresAllowed < b.cfg.FailuresAllowed:
			msg = "cannot make progress due to previous errors"
		}
		return &BuildError{Msg: msg, Exit: b.cfg.Profile.FailureExit(b.exitStatus)}
	}
	return nil
}

func (b *Builder) checkClock(c *clockWatch) {
	if c == nil {
		return
	}
	if msg := c.check(); msg != "" {
		b.status.Warn(msg)
	}
}

// prevDuration returns how long the edge's command took last time,
// according to the build log.
func (b *Builder) prevDuration(e *graph.Edge) (int64, bool) {
	for _, o := range e.Outputs {
		if en := b.scan.log.Entries[o.Path]; en != nil {
			return max(int64(en.EndMS-en.StartMS), 1), true
		}
	}
	return 0, false
}

// edgeCost is an edge's expected cost for critical-path ordering: its
// last duration, or the average duration of edges that have one.
func (b *Builder) edgeCost(e *graph.Edge) int64 {
	if e.IsPhony() {
		return 0
	}
	if d, ok := b.prevDuration(e); ok {
		return d
	}
	if b.avgCost == 0 {
		var sum, n int64
		for _, other := range b.state.Edges {
			if d, ok := b.prevDuration(other); ok && !other.IsPhony() {
				sum += d
				n++
			}
		}
		b.avgCost = 1
		if n > 0 {
			b.avgCost = max(sum/n, 1)
		}
	}
	return b.avgCost
}

// fail cleans up after an error that stops the build.
func (b *Builder) fail(err error) error {
	b.cleanup()
	status := b.exitStatus
	if status == 0 {
		status = 1
	}
	return &BuildError{Msg: err.Error(), Exit: b.cfg.Profile.FailureExit(status)}
}

func (b *Builder) startEdge(e *graph.Edge) error {
	if e.IsPhony() {
		return nil
	}
	r := running{startMS: int(time.Since(b.start).Milliseconds())}
	b.status.edgeStarted(e)
	if !b.cfg.DryRun {
		r.startStamp = -1
		for _, o := range e.Outputs {
			if err := os.MkdirAll(filepath.Dir(o.Path), 0o777); err != nil {
				return err
			}
			if r.startStamp == -1 {
				// Ninja 1.12+ records this edge's start as the mtime of a
				// freshly written lock file, from the file system's clock.
				r.startStamp = b.touchLock()
			}
		}
		if df, _ := e.UnescapedBinding("depfile"); df != "" {
			if err := os.MkdirAll(filepath.Dir(df), 0o777); err != nil {
				return err
			}
		}
		if rsp, _ := e.UnescapedBinding("rspfile"); rsp != "" {
			content, _ := e.Binding("rspfile_content")
			if err := os.WriteFile(rsp, []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	b.started[e] = r
	if b.cfg.DryRun {
		return nil
	}
	command, err := e.Command(false)
	if err != nil {
		return err
	}
	if err := b.runner.start(e, command); err != nil {
		return fmt.Errorf("command '%s' failed to start: %v", command, err)
	}
	return nil
}

// touchLock writes the lock file and returns its mtime, or 0 on error.
func (b *Builder) touchLock() int64 {
	if os.WriteFile(b.lockPath, nil, 0o644) != nil {
		return 0
	}
	m, err := RealDisk{}.Stat(b.lockPath)
	if err != nil {
		return 0
	}
	return m
}

func (b *Builder) removeLock() {
	if m, _ := (RealDisk{}).Stat(b.lockPath); m > 0 {
		os.Remove(b.lockPath)
	}
}

func (b *Builder) finishCommand(res *result) error {
	e := res.edge
	var depsNodes []*graph.Node
	depsType, _ := e.Binding("deps")
	if depsType != "" && !b.cfg.DryRun {
		nodes, err := b.extractDeps(e, depsType)
		if err != nil && res.success() {
			if res.output != "" {
				res.output += "\n"
			}
			res.output += err.Error()
			res.exit = 1
		}
		depsNodes = nodes
	}
	r := b.started[e]
	delete(b.started, e)
	endMS := int(time.Since(b.start).Milliseconds())
	b.status.edgeFinished(e, res, int64(endMS-r.startMS))
	if !res.success() {
		return b.plan.edgeFinished(e, false)
	}

	var recordMtime int64
	if !b.cfg.DryRun {
		var err error
		if recordMtime, err = b.recordMtime(e, r.startStamp, depsType); err != nil {
			return err
		}
	}
	if err := b.plan.edgeFinished(e, true); err != nil {
		return err
	}
	if b.cfg.DryRun {
		return nil
	}
	if rsp, _ := e.UnescapedBinding("rspfile"); rsp != "" && !b.cfg.KeepRsp {
		os.Remove(rsp)
	}
	command, err := e.Command(true)
	if err != nil {
		return err
	}
	outs := make([]string, len(e.Outputs))
	for i, o := range e.Outputs {
		outs[i] = o.Path
	}
	if err := b.log.Record(outs, r.startMS, endMS, recordMtime, state.HashCommand(b.log.Version(), command)); err != nil {
		return fmt.Errorf("Error writing to build log: %v", err)
	}
	if depsType != "" {
		ins := make([]string, len(depsNodes))
		for i, n := range depsNodes {
			ins[i] = n.Path
		}
		for _, o := range e.Outputs {
			m, err := RealDisk{}.Stat(o.Path)
			if err != nil {
				return err
			}
			if err := b.deps.Record(o.Path, m, ins); err != nil {
				return fmt.Errorf("Error writing to deps log: %v", err)
			}
			b.scan.deps[o] = &nodeDeps{mtime: m, inputs: depsNodes}
		}
	}
	return nil
}

// recordMtime decides the mtime to log for a finished edge, and applies
// restat: an output the command left unchanged is cleaned, which can
// remove dependent edges from the plan.
func (b *Builder) recordMtime(e *graph.Edge, startStamp int64, depsType string) (int64, error) {
	restat, _ := e.Binding("restat")
	generator, _ := e.Binding("generator")
	var record int64
	cleaned := false
	restatOutputs := func() error {
		for _, o := range e.Outputs {
			m, err := RealDisk{}.Stat(o.Path)
			if err != nil {
				return err
			}
			record = max(record, m)
			if o.Mtime() == m && restat != "" {
				// Also applies to outputs that still don't exist.
				if err := b.plan.cleanNode(b.scan, o); err != nil {
					return err
				}
				cleaned = true
			}
		}
		return nil
	}

	if b.cfg.Profile.RecordsStartTime() {
		// Ninja 1.12+: the start time, except that restat and generator
		// edges record their outputs' newest mtime (if later).
		record = startStamp
		if record == 0 || restat != "" || generator != "" {
			if err := restatOutputs(); err != nil {
				return 0, err
			}
		}
		if cleaned {
			record = startStamp
		}
		return record, nil
	}

	// Ninja 1.10–1.11: the outputs' newest mtime; when restat cleaned an
	// output, the newest mtime among the inputs (and the depfile, if the
	// edge doesn't use the deps log) instead.
	if err := restatOutputs(); err != nil {
		return 0, err
	}
	if cleaned {
		var restatMtime int64
		for _, in := range e.Inputs[:len(e.Inputs)-e.OrderOnlyDeps] {
			m, err := RealDisk{}.Stat(in.Path)
			if err != nil {
				return 0, err
			}
			restatMtime = max(restatMtime, m)
		}
		if df, _ := e.UnescapedBinding("depfile"); restatMtime != 0 && depsType == "" && df != "" {
			m, err := RealDisk{}.Stat(df)
			if err != nil {
				return 0, err
			}
			restatMtime = max(restatMtime, m)
		}
		record = restatMtime
	}
	return record, nil
}

// extractDeps reads a deps = gcc edge's depfile after its command ran,
// returning the inputs it lists and deleting it, as Ninja does.
func (b *Builder) extractDeps(e *graph.Edge, depsType string) ([]*graph.Node, error) {
	if depsType != "gcc" {
		return nil, fmt.Errorf("deps = %s isn't supported on this platform", depsType)
	}
	path, _ := e.UnescapedBinding("depfile")
	if path == "" {
		return nil, errors.New("edge with deps=gcc but no depfile makes no sense")
	}
	content, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if len(content) == 0 {
		return nil, nil
	}
	_, ins, err := depfile.ParseDialect(content, b.cfg.Profile.DepfileDialect())
	if err != nil {
		return nil, err
	}
	nodes := make([]*graph.Node, len(ins))
	for i, in := range ins {
		nodes[i] = b.state.DepNode(graph.CanonicalizePath(in))
	}
	if !b.cfg.KeepDepfile {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("deleting depfile: %v", err)
		}
	}
	return nodes, nil
}

func (b *Builder) loadDyndeps(node *graph.Node) error {
	ddf, err := b.scan.LoadDyndeps(node)
	if err != nil {
		return err
	}
	return b.plan.dyndepsLoaded(b.scan, node, ddf)
}

// cleanup stops running commands and removes what they may have left
// half-written: outputs whose mtime changed (always, if the edge has a
// depfile) and depfiles. A port of Ninja's Builder::Cleanup.
func (b *Builder) cleanup() {
	if b.runner == nil {
		return
	}
	active := b.runner.activeEdges()
	b.runner.abort()
	for _, e := range active {
		df, _ := e.UnescapedBinding("depfile")
		for _, o := range e.Outputs {
			m, _ := RealDisk{}.Stat(o.Path)
			if df != "" || o.Mtime() != m {
				os.Remove(o.Path)
			}
		}
		if df != "" {
			os.Remove(df)
		}
	}
}
