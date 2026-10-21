package build

import (
	"container/heap"
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
// a port of Ninja's Plan.
type Plan struct {
	builder *Builder
	want    map[*graph.Edge]want
	order   []*graph.Edge // edges in the order they joined want, for determinism
	targets []*graph.Node
	ready   edgeQueue
	// wanted counts edges still wanted. commands counts non-phony edges
	// wanted and not cleaned by restat; Ninja's more_to_do needs both.
	wanted, commands int
	pools            map[*graph.Pool]*poolState
}

// poolState is a pool's use during one build: edges count against its
// depth from when they're scheduled until they finish.
type poolState struct {
	use     int
	delayed edgeQueue
}

// edgeQueue is a priority queue of edges: the heaviest critical path
// first, then the lowest ID, as in Ninja.
type edgeQueue []*graph.Edge

func (q edgeQueue) Len() int { return len(q) }
func (q edgeQueue) Less(i, j int) bool {
	if q[i].CriticalPath != q[j].CriticalPath {
		return q[i].CriticalPath > q[j].CriticalPath
	}
	return q[i].ID < q[j].ID
}
func (q edgeQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *edgeQueue) Push(x any)   { *q = append(*q, x.(*graph.Edge)) }
func (q *edgeQueue) Pop() any {
	old := *q
	e := old[len(old)-1]
	*q = old[:len(old)-1]
	return e
}

func newPlan(b *Builder) *Plan {
	return &Plan{builder: b, want: map[*graph.Edge]want{}, pools: map[*graph.Pool]*poolState{}}
}

func (p *Plan) pool(e *graph.Edge) *poolState {
	ps := p.pools[e.Pool]
	if ps == nil {
		ps = &poolState{}
		p.pools[e.Pool] = ps
	}
	return ps
}

// retrieveReadyEdges moves delayed edges into the ready queue while the
// pool has room.
func (p *Plan) retrieveReadyEdges(e *graph.Edge) {
	if e.Pool.Depth == 0 {
		return
	}
	ps := p.pool(e)
	for ps.delayed.Len() > 0 && ps.use+1 <= e.Pool.Depth {
		heap.Push(&p.ready, heap.Pop(&ps.delayed))
		ps.use++
	}
}

func (p *Plan) moreToDo() bool { return p.wanted > 0 && p.commands > 0 }

// AddTarget adds what's needed to bring target up to date.
func (p *Plan) AddTarget(target *graph.Node) error {
	p.targets = append(p.targets, target)
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
		p.builder.status.edgeAdded(e)
	}
}

func (p *Plan) scheduleWork(e *graph.Edge) {
	if p.want[e] == wantToFinish {
		return // already scheduled
	}
	p.want[e] = wantToFinish
	if e.Pool.Depth != 0 {
		heap.Push(&p.pool(e).delayed, e)
		p.retrieveReadyEdges(e)
		return
	}
	heap.Push(&p.ready, e)
}

// computeCriticalPath weights every edge reachable from the targets: its
// own cost (cost returns it) plus the heaviest chain of edges waiting on
// its outputs. Ninja 1.12+ does the same with every edge costing 1; kiln
// uses each edge's last recorded duration where it has one, so long
// compiles start early.
func (p *Plan) computeCriticalPath(cost func(*graph.Edge) int64) {
	// Topological order: every edge after the edges producing its inputs.
	var sorted []*graph.Edge
	visited := map[*graph.Edge]bool{}
	var visit func(e *graph.Edge)
	visit = func(e *graph.Edge) {
		if visited[e] {
			return
		}
		visited[e] = true
		for _, in := range e.Inputs {
			if in.InEdge != nil {
				visit(in.InEdge)
			}
		}
		sorted = append(sorted, e)
	}
	for _, t := range p.targets {
		if t.InEdge != nil {
			visit(t.InEdge)
		}
	}
	for _, e := range sorted {
		e.CriticalPath = cost(e)
	}
	for i := len(sorted) - 1; i >= 0; i-- {
		e := sorted[i]
		for _, in := range e.Inputs {
			if pe := in.InEdge; pe != nil {
				pe.CriticalPath = max(pe.CriticalPath, e.CriticalPath+cost(pe))
			}
		}
	}
}

func (p *Plan) scheduleInitialEdges() {
	for _, e := range p.order {
		if w, ok := p.want[e]; ok && w == wantToStart && e.AllInputsReady() {
			p.scheduleWork(e)
		}
	}
}

func (p *Plan) findWork() *graph.Edge {
	if p.ready.Len() == 0 {
		return nil
	}
	return heap.Pop(&p.ready).(*graph.Edge)
}

// edgeFinished records that e completed (successfully or not).
func (p *Plan) edgeFinished(e *graph.Edge, success bool) error {
	w := p.want[e]
	directlyWanted := w != wantNothing
	if directlyWanted && e.Pool.Depth != 0 {
		p.pool(e).use--
	}
	p.retrieveReadyEdges(e)
	if !success {
		return nil
	}
	if directlyWanted {
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

// cleanNode propagates a restat result: node turned out not to change,
// so edges waiting on it may not need to run after all. A port of Ninja's
// Plan::CleanNode.
func (p *Plan) cleanNode(scan *Scanner, node *graph.Node) error {
	node.Dirty = false
	for i := 0; i < len(node.OutEdges); i++ {
		oe := node.OutEdges[i]
		if w, ok := p.want[oe]; !ok || w == wantNothing {
			continue
		}
		if oe.DepsMissing {
			continue // couldn't load its deps, so it must run anyway
		}
		ins := oe.Inputs[:len(oe.Inputs)-oe.OrderOnlyDeps]
		var mostRecent *graph.Node
		anyDirty := false
		for _, in := range ins {
			if in.Dirty {
				anyDirty = true
				break
			}
			if mostRecent == nil || in.Mtime() > mostRecent.Mtime() {
				mostRecent = in
			}
		}
		if anyDirty {
			continue
		}
		dirty, err := scan.RecomputeOutputsDirty(oe, mostRecent)
		if err != nil {
			return err
		}
		if dirty {
			continue
		}
		for _, o := range oe.Outputs {
			if err := p.cleanNode(scan, o); err != nil {
				return err
			}
		}
		p.want[oe] = wantNothing
		p.wanted--
		if !oe.IsPhony() {
			p.commands--
			p.builder.status.edgeRemoved(oe)
		}
	}
	return nil
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
