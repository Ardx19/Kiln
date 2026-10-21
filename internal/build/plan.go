package build

import (
	"fmt"
	"sort"

	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/manifest"
)

type want int

const (
	// wantNothing: in the plan only to reach its dependencies.
	wantNothing want = iota
	// wantToStart: should run, not yet scheduled.
	wantToStart
	// wantToFinish: scheduled (in the ready queue or running).
	wantToFinish
)

// Plan tracks the edges a build wants and which are ready to run. It is
// a port of Ninja's Plan, minus pools, which only affect order.
type Plan struct {
	builder *Builder
	want    map[*graph.Edge]want
	order   []*graph.Edge // edges in the order they joined want, for determinism
	ready   []*graph.Edge
	// wanted counts edges still wanted. commands counts non-phony edges
	// ever wanted; Ninja's more_to_do needs both to be non-zero.
	wanted, commands int
}

func newPlan(b *Builder) *Plan {
	return &Plan{builder: b, want: map[*graph.Edge]want{}}
}

func (p *Plan) moreToDo() bool { return p.wanted > 0 && p.commands > 0 }

// AddTarget adds what's needed to bring target up to date.
func (p *Plan) AddTarget(target *graph.Node) error {
	_, err := p.addSubTarget(target, nil, nil)
	return err
}

func (p *Plan) addSubTarget(node, dependent *graph.Node, dyndepWalk map[*graph.Edge]bool) (bool, error) {
	edge := node.InEdge
	if edge == nil {
		// A missing manifest input is an error. A missing file that only a
		// dep loader mentioned (a deleted header) just makes its users
		// dirty, which the scan already did.
		if node.Dirty && !node.FromDepLoader {
			ref := ""
			if dependent != nil {
				ref = ", needed by '" + dependent.Path + "',"
			}
			return false, fmt.Errorf("'%s'%s missing and no known rule to make it", node.Path, ref)
		}
		return false, nil
	}
	if edge.OutputsReady {
		return false, nil
	}
	w, seen := p.want[edge]
	if !seen {
		p.want[edge] = wantNothing
		p.order = append(p.order, edge)
	}
	if dyndepWalk != nil && w == wantToFinish {
		return false, nil
	}
	if node.Dirty && w == wantNothing {
		p.want[edge] = wantToStart
		p.edgeWanted(edge)
	}
	if dyndepWalk != nil {
		dyndepWalk[edge] = true
	}
	if seen {
		return true, nil // inputs already processed
	}
	for i := 0; i < len(edge.Inputs); i++ {
		if _, err := p.addSubTarget(edge.Inputs[i], node, dyndepWalk); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (p *Plan) edgeWanted(e *graph.Edge) {
	p.wanted++
	if !e.IsPhony() {
		p.commands++
		p.builder.status.edgeAdded()
	}
}

func (p *Plan) scheduleWork(e *graph.Edge) {
	if p.want[e] == wantToFinish {
		return // already scheduled
	}
	p.want[e] = wantToFinish
	p.ready = append(p.ready, e)
}

func (p *Plan) scheduleInitialEdges() {
	for _, e := range p.order {
		if w, ok := p.want[e]; ok && w == wantToStart && e.AllInputsReady() {
			p.scheduleWork(e)
		}
	}
}

func (p *Plan) findWork() *graph.Edge {
	if len(p.ready) == 0 {
		return nil
	}
	e := p.ready[0]
	p.ready = p.ready[1:]
	return e
}

// edgeFinished records that e completed (successfully or not).
func (p *Plan) edgeFinished(e *graph.Edge, success bool) error {
	w := p.want[e]
	if !success {
		return nil
	}
	if w != wantNothing {
		p.wanted--
	}
	delete(p.want, e)
	e.OutputsReady = true
	for i := 0; i < len(e.Outputs); i++ {
		if err := p.nodeFinished(e.Outputs[i]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plan) nodeFinished(n *graph.Node) error {
	if n.DyndepPending {
		// The dyndep file is now up to date: load it, which can add work.
		return p.builder.loadDyndeps(n)
	}
	for i := 0; i < len(n.OutEdges); i++ {
		if _, ok := p.want[n.OutEdges[i]]; ok {
			if err := p.edgeMaybeReady(n.OutEdges[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Plan) edgeMaybeReady(e *graph.Edge) error {
	if !e.AllInputsReady() {
		return nil
	}
	if p.want[e] != wantNothing {
		p.scheduleWork(e)
		return nil
	}
	// Not needed itself, but its dependents may be.
	return p.edgeFinished(e, true)
}

// dyndepsLoaded updates the plan once a dyndep file has been loaded:
// dependents are rescanned, and newly discovered inputs join the plan.
func (p *Plan) dyndepsLoaded(scan *Scanner, node *graph.Node, ddf manifest.DyndepFile) error {
	if err := p.refreshDyndepDependents(scan, node); err != nil {
		return err
	}
	var roots []*graph.Edge
	for edge := range ddf {
		if edge.OutputsReady {
			continue
		}
		if _, ok := p.want[edge]; ok {
			roots = append(roots, edge)
		}
	}
	sortEdges(roots)
	walk := map[*graph.Edge]bool{}
	for _, edge := range roots {
		for _, in := range ddf[edge].ImplicitInputs {
			if _, err := p.addSubTarget(in, edge.Outputs[0], walk); err != nil {
				return err
			}
		}
	}
	for _, oe := range node.OutEdges {
		if _, ok := p.want[oe]; ok {
			walk[oe] = true
		}
	}
	edges := make([]*graph.Edge, 0, len(walk))
	for e := range walk {
		edges = append(edges, e)
	}
	sortEdges(edges)
	for _, e := range edges {
		if _, ok := p.want[e]; ok {
			if err := p.edgeMaybeReady(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Plan) refreshDyndepDependents(scan *Scanner, node *graph.Node) error {
	var dependents []*graph.Node
	seen := map[*graph.Node]bool{}
	p.unmarkDependents(node, seen, &dependents)
	for _, n := range dependents {
		validations, err := scan.RecomputeDirty(n)
		if err != nil {
			return err
		}
		for _, v := range validations {
			if v.InEdge != nil && !v.InEdge.OutputsReady {
				if err := p.AddTarget(v); err != nil {
					return err
				}
			}
		}
		if !n.Dirty {
			continue
		}
		// With the dyndep information, an output is now known to be
		// dirty, so the edge is wanted after all.
		if e := n.InEdge; p.want[e] == wantNothing {
			p.want[e] = wantToStart
			p.edgeWanted(e)
		}
	}
	return nil
}

func (p *Plan) unmarkDependents(node *graph.Node, seen map[*graph.Node]bool, out *[]*graph.Node) {
	for _, e := range node.OutEdges {
		if _, ok := p.want[e]; !ok || e.Mark == graph.VisitNone {
			continue
		}
		e.Mark = graph.VisitNone
		for _, o := range e.Outputs {
			if !seen[o] {
				seen[o] = true
				*out = append(*out, o)
				p.unmarkDependents(o, seen, out)
			}
		}
	}
}

// sortEdges orders edges by their first output, so map-derived sets are
// processed deterministically.
func sortEdges(es []*graph.Edge) {
	sort.Slice(es, func(i, j int) bool { return es[i].Outputs[0].Path < es[j].Outputs[0].Path })
}
