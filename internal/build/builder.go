package build

import (
	"errors"

	"github.com/Ardx19/kiln/internal/graph"
)

// Builder brings targets up to date. So far it only does dry runs.
type Builder struct {
	state  *graph.State
	scan   *Scanner
	plan   *Plan
	status *Status
	// DryRun pretends every command succeeds without running it, like
	// ninja -n.
	DryRun bool
}

// NewBuilder returns a builder for st that scans with scan and reports to
// status.
func NewBuilder(st *graph.State, scan *Scanner, status *Status) *Builder {
	b := &Builder{state: st, scan: scan, status: status}
	b.plan = newPlan(b)
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

// Build runs the plan. Only dry runs are implemented: every edge is
// reported and treated as succeeding at once.
func (b *Builder) Build() error {
	if !b.DryRun {
		return errors.New("running commands isn't implemented yet; use -n for a dry run")
	}
	b.plan.scheduleInitialEdges()
	var pending []*graph.Edge
	for b.plan.moreToDo() {
		if e := b.plan.findWork(); e != nil {
			if e.IsPhony() {
				if err := b.plan.edgeFinished(e, true); err != nil {
					return err
				}
			} else {
				b.status.edgeStarted(e)
				pending = append(pending, e)
			}
			continue
		}
		if len(pending) > 0 {
			e := pending[0]
			pending = pending[1:]
			b.status.edgeFinished(e)
			if err := b.plan.edgeFinished(e, true); err != nil {
				return err
			}
			continue
		}
		return errors.New("stuck [this is a bug]")
	}
	return nil
}

func (b *Builder) loadDyndeps(node *graph.Node) error {
	ddf, err := b.scan.LoadDyndeps(node)
	if err != nil {
		return err
	}
	return b.plan.dyndepsLoaded(b.scan, node, ddf)
}
