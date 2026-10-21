// Package graph is kiln's build graph: nodes (files), edges (build
// statements), rules, pools and variable scopes, with Ninja's exact
// variable-evaluation semantics.
package graph

import (
	"fmt"
	"strings"
)

// Pool limits how many of its edges run at once. Depth 0 means no limit.
type Pool struct {
	Name  string
	Depth int
}

// The two built-in pools.
var (
	DefaultPool = &Pool{Name: "", Depth: 0}
	ConsolePool = &Pool{Name: "console", Depth: 1}
)

// Rule is a named command template.
type Rule struct {
	Name     string
	Bindings map[string]*EvalString
}

// PhonyRule is the built-in rule for aliases and grouping.
var PhonyRule = &Rule{Name: "phony", Bindings: map[string]*EvalString{}}

// IsReservedBinding reports whether name may be set inside a rule block.
func IsReservedBinding(name string) bool {
	switch name {
	case "command", "depfile", "dyndep", "description", "deps", "generator",
		"pool", "restat", "rspfile", "rspfile_content", "msvc_deps_prefix":
		return true
	}
	return false
}

// Node is a file, or a phony target, in the build graph.
type Node struct {
	Path string
	// InEdge produces this node; nil for source files.
	InEdge *Edge
	// OutEdges consume this node as an input.
	OutEdges []*Edge
	// ValidationOutEdges list this node as a validation.
	ValidationOutEdges []*Edge
	// DyndepPending is set when some edge names this node as its dyndep
	// file and it hasn't been loaded yet.
	DyndepPending bool
	// FromDepLoader marks nodes that only a deps log, depfile or dyndep
	// file mentions. If one is missing, the edges using it rebuild; it
	// isn't an error the way a missing manifest input is.
	FromDepLoader bool

	// Scan state, maintained while computing what's dirty.
	mtime int64 // ns; 0 when missing; -1 before the first stat
	Dirty bool
	// Prefetched is set once the scan's parallel stat pass has seen it.
	Prefetched bool
}

// StatFS stats files for the dependency scan.
type StatFS interface {
	// Stat returns the mtime in ns, 0 if the file doesn't exist.
	Stat(path string) (int64, error)
}

// StatIfNecessary stats the node unless that's already been done.
func (n *Node) StatIfNecessary(fs StatFS) error {
	if n.StatusKnown() {
		return nil
	}
	return n.Stat(fs)
}

// Stat (re)reads the node's mtime.
func (n *Node) Stat(fs StatFS) error {
	m, err := fs.Stat(n.Path)
	if err != nil {
		return err
	}
	n.mtime = m
	return nil
}

// StatusKnown reports whether the node has been statted.
func (n *Node) StatusKnown() bool { return n.mtime != -1 }

// Exists reports whether the file was there when last statted.
func (n *Node) Exists() bool { return n.mtime > 0 }

// Mtime returns the mtime in ns from the last stat, 0 if missing.
func (n *Node) Mtime() int64 { return max(n.mtime, 0) }

// UpdatePhonyMtime lets a missing phony output carry its newest input's
// mtime, so its dependents compare against that.
func (n *Node) UpdatePhonyMtime(m int64) {
	if !n.Exists() {
		n.mtime = max(n.mtime, m)
	}
}

// ResetState forgets the scan state, as before a fresh scan.
func (n *Node) ResetState() {
	n.mtime = -1
	n.Dirty = false
	n.Prefetched = false
}

// VisitMark tracks an edge's progress through a dependency scan.
type VisitMark int

// Visit marks.
const (
	VisitNone VisitMark = iota
	VisitInStack
	VisitDone
)

// Edge is one build statement.
type Edge struct {
	// ID is the edge's position in State.Edges; it breaks ties between
	// equally urgent edges, as in Ninja.
	ID   int
	Rule *Rule
	Pool *Pool
	// Env is the scope the edge's variables resolve in: a child scope if
	// the edge has its own bindings, otherwise the file scope.
	Env *Scope

	// Inputs holds explicit inputs, then implicit (|), then order-only (||).
	Inputs        []*Node
	ImplicitDeps  int
	OrderOnlyDeps int
	// Outputs holds explicit outputs, then implicit outputs (|).
	Outputs      []*Node
	ImplicitOuts int
	Validations  []*Node
	Dyndep       *Node

	// CriticalPath is the edge's scheduling weight: its own expected cost
	// plus that of the heaviest chain of edges waiting on it.
	CriticalPath int64

	// Scan state.
	Mark         VisitMark
	OutputsReady bool // every output is up to date, or will be once inputs are
	DepsLoaded   bool // deps log or depfile entries have been added to Inputs
	DepsMissing  bool // they couldn't be loaded, so the edge must rerun
}

// IsOrderOnly reports whether Inputs[i] is an order-only input.
func (e *Edge) IsOrderOnly(i int) bool { return i >= len(e.Inputs)-e.OrderOnlyDeps }

// AllInputsReady reports whether every input's producing edge is done.
func (e *Edge) AllInputsReady() bool {
	for _, in := range e.Inputs {
		if in.InEdge != nil && !in.InEdge.OutputsReady {
			return false
		}
	}
	return true
}

// InsertImplicitInputs adds discovered dependencies after the existing
// implicit inputs (before the order-only ones), as Ninja's dep loaders do.
func (e *Edge) InsertImplicitInputs(nodes []*Node) {
	at := len(e.Inputs) - e.OrderOnlyDeps
	ins := make([]*Node, 0, len(e.Inputs)+len(nodes))
	ins = append(ins, e.Inputs[:at]...)
	ins = append(ins, nodes...)
	e.Inputs = append(ins, e.Inputs[at:]...)
	e.ImplicitDeps += len(nodes)
	for _, n := range nodes {
		n.OutEdges = append(n.OutEdges, e)
	}
}

// ResetState forgets the scan state, as before a fresh scan.
func (e *Edge) ResetState() {
	e.Mark = VisitNone
	e.OutputsReady = false
	e.DepsLoaded = false
	e.DepsMissing = false
}

// IsPhony reports whether the edge uses the phony rule.
func (e *Edge) IsPhony() bool { return e.Rule == PhonyRule }

// ExplicitInputs returns the inputs that $in expands to.
func (e *Edge) ExplicitInputs() []*Node {
	return e.Inputs[:len(e.Inputs)-e.ImplicitDeps-e.OrderOnlyDeps]
}

// ExplicitOutputs returns the outputs that $out expands to.
func (e *Edge) ExplicitOutputs() []*Node {
	return e.Outputs[:len(e.Outputs)-e.ImplicitOuts]
}

// Binding evaluates one of the edge's variables, with $in and $out
// quoted for the shell. It fails only on a cycle between rule variables.
func (e *Edge) Binding(name string) (string, error) {
	return e.binding(name, true)
}

// UnescapedBinding is Binding without shell quoting, for values used as
// paths (depfile, rspfile, dyndep).
func (e *Edge) UnescapedBinding(name string) (string, error) {
	return e.binding(name, false)
}

func (e *Edge) binding(name string, escape bool) (string, error) {
	switch name {
	case "in", "in_newline", "out":
	default:
		if e.Rule.Bindings[name] == nil {
			// Nothing to evaluate: what's left of the lookup order is the
			// edge's scope and its parents.
			return e.Env.LookupVariable(name), nil
		}
	}
	env := &edgeEnv{edge: e, escape: escape}
	v := env.LookupVariable(name)
	return v, env.err
}

// Command returns the command line. With inclRspfile the response file
// content is appended as ";rspfile=<content>", which is what Ninja hashes
// for the build log.
func (e *Edge) Command(inclRspfile bool) (string, error) {
	cmd, err := e.Binding("command")
	if err != nil || !inclRspfile {
		return cmd, err
	}
	rsp, err := e.Binding("rspfile_content")
	if rsp != "" {
		cmd += ";rspfile=" + rsp
	}
	return cmd, err
}

// edgeEnv resolves variables for one edge, like Ninja's EdgeEnv.
type edgeEnv struct {
	edge      *Edge
	escape    bool
	recursive bool
	lookups   []string
	err       error
}

func (env *edgeEnv) LookupVariable(name string) string {
	if env.err != nil {
		return ""
	}
	switch name {
	case "in", "in_newline":
		sep := " "
		if name == "in_newline" {
			sep = "\n"
		}
		return env.pathList(env.edge.ExplicitInputs(), sep)
	case "out":
		return env.pathList(env.edge.ExplicitOutputs(), " ")
	}

	if env.recursive {
		for i, l := range env.lookups {
			if l == name {
				env.err = fmt.Errorf("cycle in rule variables: %s -> %s",
					strings.Join(env.lookups[i:], " -> "), name)
				return ""
			}
		}
	}
	eval := env.edge.Rule.Bindings[name]
	record := env.recursive && eval != nil
	if record {
		env.lookups = append(env.lookups, name)
	}
	// Rule variables rarely refer to other rule variables, so cycle
	// tracking only starts after the first lookup, as in Ninja.
	env.recursive = true
	v := env.edge.Env.lookupWithFallback(name, eval, env)
	if record {
		env.lookups = env.lookups[:len(env.lookups)-1]
	}
	return v
}

func (env *edgeEnv) pathList(nodes []*Node, sep string) string {
	var b strings.Builder
	for i, n := range nodes {
		if i > 0 {
			b.WriteString(sep)
		}
		if env.escape {
			b.WriteString(ShellEscape(n.Path))
		} else {
			b.WriteString(n.Path)
		}
	}
	return b.String()
}

// State is a loaded build graph.
type State struct {
	Paths    map[string]*Node
	Edges    []*Edge
	Pools    map[string]*Pool
	Defaults []*Node
	// Bindings is the top-level file scope; it holds the phony rule.
	Bindings *Scope
}

// NewState returns an empty graph with the built-in rule and pools.
func NewState() *State {
	s := &State{
		Paths:    map[string]*Node{},
		Pools:    map[string]*Pool{},
		Bindings: NewScope(nil),
	}
	s.Bindings.AddRule(PhonyRule)
	s.Pools[DefaultPool.Name] = DefaultPool
	s.Pools[ConsolePool.Name] = ConsolePool
	return s
}

// LookupNode returns the node for a canonical path, or nil.
func (s *State) LookupNode(path string) *Node { return s.Paths[path] }

// Node returns the node for a canonical path, creating it if needed.
func (s *State) Node(path string) *Node {
	if n := s.Paths[path]; n != nil {
		return n
	}
	n := &Node{Path: path, mtime: -1}
	s.Paths[path] = n
	return n
}

// DepNode is Node for paths found by a dep loader (deps log, depfile or
// dyndep file): a node it creates is marked FromDepLoader.
func (s *State) DepNode(path string) *Node {
	if n := s.Paths[path]; n != nil {
		return n
	}
	n := s.Node(path)
	n.FromDepLoader = true
	return n
}

// Reset forgets all scan state, as Ninja does before rescanning after
// checking the manifest.
func (s *State) Reset() {
	for _, n := range s.Paths {
		n.ResetState()
	}
	for _, e := range s.Edges {
		e.ResetState()
	}
}

// RootNodes returns outputs that nothing consumes.
func (s *State) RootNodes() ([]*Node, error) {
	var roots []*Node
	for _, e := range s.Edges {
		for _, o := range e.Outputs {
			if len(o.OutEdges) == 0 {
				roots = append(roots, o)
			}
		}
	}
	if len(s.Edges) > 0 && len(roots) == 0 {
		return nil, fmt.Errorf("could not determine root nodes of build graph")
	}
	return roots, nil
}

// DefaultNodes returns the default targets, or the root nodes if the
// manifest names none.
func (s *State) DefaultNodes() ([]*Node, error) {
	if len(s.Defaults) > 0 {
		return s.Defaults, nil
	}
	return s.RootNodes()
}

// AddEdge appends a new edge using rule, in the default pool.
func (s *State) AddEdge(rule *Rule) *Edge {
	e := &Edge{ID: len(s.Edges), Rule: rule, Pool: DefaultPool, Env: s.Bindings}
	s.Edges = append(s.Edges, e)
	return e
}

// AddOut makes path an output of edge. A path can have only one producer.
func (s *State) AddOut(e *Edge, path string) error {
	n := s.Node(path)
	if n.InEdge != nil {
		if n.InEdge == e {
			return fmt.Errorf("%s is defined as an output multiple times", path)
		}
		return fmt.Errorf("multiple rules generate %s", path)
	}
	e.Outputs = append(e.Outputs, n)
	n.InEdge = e
	return nil
}

// AddIn makes path an input of edge.
func (s *State) AddIn(e *Edge, path string) {
	n := s.Node(path)
	e.Inputs = append(e.Inputs, n)
	n.OutEdges = append(n.OutEdges, e)
}

// AddValidation makes path a validation of edge.
func (s *State) AddValidation(e *Edge, path string) {
	n := s.Node(path)
	e.Validations = append(e.Validations, n)
	n.ValidationOutEdges = append(n.ValidationOutEdges, e)
}

// AddDefault adds an existing node to the default targets.
func (s *State) AddDefault(path string) error {
	n := s.LookupNode(path)
	if n == nil {
		return fmt.Errorf("unknown target '%s'", path)
	}
	s.Defaults = append(s.Defaults, n)
	return nil
}
