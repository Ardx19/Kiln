// Package build decides what to rebuild and runs it. The scan (which
// outputs are dirty) and the plan (which edges to run, in what order) are
// ports of Ninja's DependencyScan, ImplicitDepLoader, DyndepLoader and
// Plan (src/graph.cc, src/dyndep.cc, src/build.cc in Ninja 1.13), so that
// kiln and Ninja agree on every decision.
package build

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/Ardx19/kiln/internal/depfile"
	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/manifest"
	"github.com/Ardx19/kiln/internal/metrics"
	"github.com/Ardx19/kiln/internal/state"
)

// Disk is the file system as a build sees it. Paths are relative to the
// build directory.
type Disk interface {
	graph.StatFS
	// ReadFile returns an error satisfying errors.Is(err, fs.ErrNotExist)
	// for a missing file.
	ReadFile(path string) ([]byte, error)
}

// RealDisk is the real file system, relative to the working directory.
type RealDisk struct{}

// Stat returns the mtime in ns like Ninja's RealDiskInterface::Stat: 0
// for a missing file (ENOENT or ENOTDIR), and 1 for a file whose mtime is
// the epoch, so that it doesn't look missing.
func (RealDisk) Stat(path string) (int64, error) {
	defer metrics.Record("node stat")()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat(%s): %v", path, err)
	}
	if st.Mtim.Sec == 0 {
		return 1, nil
	}
	return st.Mtim.Sec*1e9 + st.Mtim.Nsec, nil
}

// ReadFile reads a file.
func (RealDisk) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// nodeDeps is a deps log entry, mapped onto graph nodes.
type nodeDeps struct {
	mtime  int64
	inputs []*graph.Node
}

// Scanner computes which nodes are dirty.
type Scanner struct {
	state   *graph.State
	disk    Disk
	log     *state.BuildLog
	deps    map[*graph.Node]*nodeDeps
	explain func(msg string)
	walked  []bool // by edge ID: edges prefetch has walked
	// EnvChanged, if set, reports whether output was built under a
	// different declared environment than the current one; the edge is
	// then dirty.
	EnvChanged func(output string) bool
}

// NewScanner prepares a scan of st. blog and dlog may be nil (no log
// yet). Every path in the deps log becomes a node, as when Ninja loads
// it. explain, if not nil, receives Ninja-style explanations.
func NewScanner(st *graph.State, disk Disk, blog *state.BuildLog, dlog *state.DepsLog, explain func(string)) *Scanner {
	if blog == nil {
		blog = &state.BuildLog{Entries: map[string]*state.LogEntry{}}
	}
	s := &Scanner{state: st, disk: disk, log: blog, deps: map[*graph.Node]*nodeDeps{}, explain: explain}
	if dlog != nil {
		nodes := make([]*graph.Node, len(dlog.Paths))
		for id, p := range dlog.Paths {
			nodes[id] = st.DepNode(p)
		}
		for id, d := range dlog.Deps {
			if d == nil || id >= len(nodes) {
				continue
			}
			nd := &nodeDeps{mtime: d.Mtime, inputs: make([]*graph.Node, len(d.Inputs))}
			for i, in := range d.Inputs {
				nd.inputs[i] = nodes[in]
			}
			s.deps[nodes[id]] = nd
		}
	}
	return s
}

func (s *Scanner) explainf(format string, args ...any) {
	if s.explain != nil {
		s.explain(fmt.Sprintf(format, args...))
	}
}

// RecomputeDirty updates the dirty state of node and everything it
// depends on. It returns the validation nodes it found, which callers add
// as extra targets.
func (s *Scanner) RecomputeDirty(node *graph.Node) ([]*graph.Node, error) {
	s.prefetch(node)
	var all []*graph.Node
	queue := []*graph.Node{node}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		var stack, validations []*graph.Node
		if err := s.recomputeNodeDirty(n, &stack, &validations); err != nil {
			return nil, err
		}
		queue = append(queue, validations...)
		all = append(all, validations...)
	}
	return all, nil
}

func (s *Scanner) recomputeNodeDirty(node *graph.Node, stack, validations *[]*graph.Node) error {
	edge := node.InEdge
	if edge == nil {
		// A source file (or a node only dep loaders know): dirty if missing.
		if node.StatusKnown() {
			return nil
		}
		if err := node.StatIfNecessary(s.disk); err != nil {
			return err
		}
		if !node.Exists() {
			s.explainf("%s has no in-edge and is missing", node.Path)
		}
		node.Dirty = !node.Exists()
		return nil
	}
	if edge.Mark == graph.VisitDone {
		return nil
	}
	if err := verifyDAG(node, *stack); err != nil {
		return err
	}
	edge.Mark = graph.VisitInStack
	*stack = append(*stack, node)

	dirty := false
	edge.OutputsReady = true
	edge.DepsMissing = false

	if !edge.DepsLoaded && edge.Dyndep != nil && edge.Dyndep.DyndepPending {
		// Visit a pending dyndep file first, and load it if it's ready.
		if err := s.recomputeNodeDirty(edge.Dyndep, stack, validations); err != nil {
			return err
		}
		if edge.Dyndep.InEdge == nil || edge.Dyndep.InEdge.OutputsReady {
			if _, err := s.LoadDyndeps(edge.Dyndep); err != nil {
				return err
			}
		}
	}

	for _, o := range edge.Outputs {
		if err := o.StatIfNecessary(s.disk); err != nil {
			return err
		}
	}

	if !edge.DepsLoaded {
		edge.DepsLoaded = true
		ok, err := s.loadDeps(edge)
		if err != nil {
			return err
		}
		if !ok {
			// No usable dependency info: rebuild to regenerate it.
			dirty = true
			edge.DepsMissing = true
		}
	}

	// Validations become extra targets later, not inputs, so a validation
	// that depends on this node doesn't look like a cycle.
	*validations = append(*validations, edge.Validations...)

	var mostRecent *graph.Node
	for i := 0; i < len(edge.Inputs); i++ {
		in := edge.Inputs[i]
		if err := s.recomputeNodeDirty(in, stack, validations); err != nil {
			return err
		}
		if in.InEdge != nil && !in.InEdge.OutputsReady {
			edge.OutputsReady = false
		}
		if !edge.IsOrderOnly(i) {
			if in.Dirty {
				s.explainf("%s is dirty", in.Path)
				dirty = true
			} else if mostRecent == nil || in.Mtime() > mostRecent.Mtime() {
				mostRecent = in
			}
		}
	}

	if !dirty {
		var err error
		if dirty, err = s.RecomputeOutputsDirty(edge, mostRecent); err != nil {
			return err
		}
	}
	if dirty {
		for _, o := range edge.Outputs {
			o.Dirty = true
		}
		// Phony edges with no inputs have nothing to do, so stay ready.
		if !(edge.IsPhony() && len(edge.Inputs) == 0) {
			edge.OutputsReady = false
		}
	}

	edge.Mark = graph.VisitDone
	*stack = (*stack)[:len(*stack)-1]
	return nil
}

// verifyDAG reports a dependency cycle if node's edge is already on the
// stack, naming the cycle from the node that closes it.
func verifyDAG(node *graph.Node, stack []*graph.Node) error {
	edge := node.InEdge
	if edge.Mark != graph.VisitInStack {
		return nil
	}
	start := 0
	for start < len(stack) && stack[start].InEdge != edge {
		start++
	}
	cycle := append([]*graph.Node{node}, stack[start+1:]...)
	var b strings.Builder
	b.WriteString("dependency cycle: ")
	for _, n := range cycle {
		b.WriteString(n.Path + " -> ")
	}
	b.WriteString(node.Path)
	if start+1 == len(stack) && edge.IsPhony() && len(edge.Outputs) == 1 && edge.ImplicitOuts == 0 && edge.ImplicitDeps == 0 {
		b.WriteString(" [-w phonycycle=err]")
	}
	return errors.New(b.String())
}

// RecomputeOutputsDirty reports whether any output of edge is dirty given
// its newest non-order-only input.
func (s *Scanner) RecomputeOutputsDirty(edge *graph.Edge, mostRecent *graph.Node) (bool, error) {
	command, err := edge.Command(true)
	if err != nil {
		return false, err
	}
	for _, o := range edge.Outputs {
		dirty, err := s.recomputeOutputDirty(edge, mostRecent, command, o)
		if err != nil || dirty {
			return dirty, err
		}
	}
	return false, nil
}

func (s *Scanner) recomputeOutputDirty(edge *graph.Edge, mostRecent *graph.Node, command string, out *graph.Node) (bool, error) {
	if edge.IsPhony() {
		// Phony outputs are only dirty if there are no inputs and the
		// output is missing.
		if len(edge.Inputs) == 0 && !out.Exists() {
			s.explainf("output %s of phony edge with no inputs doesn't exist", out.Path)
			return true, nil
		}
		if mostRecent != nil {
			out.UpdatePhonyMtime(mostRecent.Mtime())
		}
		return false, nil
	}
	if !out.Exists() {
		s.explainf("output %s doesn't exist", out.Path)
		return true, nil
	}
	if s.EnvChanged != nil && s.EnvChanged(out.Path) {
		s.explainf("the declared environment changed since %s was built", out.Path)
		return true, nil
	}

	restat, err := edge.Binding("restat")
	if err != nil {
		return false, err
	}
	entry := s.log.Entries[out.Path]
	// A restat edge may have left its output untouched last time; its
	// log entry, not the file, says when it last ran.
	usedRestat := restat != "" && entry != nil
	if !usedRestat && mostRecent != nil && out.Mtime() < mostRecent.Mtime() {
		s.explainf("output %s older than most recent input %s (%d vs %d)", out.Path, mostRecent.Path, out.Mtime(), mostRecent.Mtime())
		return true, nil
	}

	generator, err := edge.Binding("generator")
	if err != nil {
		return false, err
	}
	if entry != nil {
		if generator == "" && state.HashCommand(s.log.Version, command) != entry.Hash {
			s.explainf("command line changed for %s", out.Path)
			return true, nil
		}
		if mostRecent != nil && entry.Mtime < mostRecent.Mtime() {
			s.explainf("recorded mtime of %s older than most recent input %s (%d vs %d)", out.Path, mostRecent.Path, entry.Mtime, mostRecent.Mtime())
			return true, nil
		}
	} else if generator == "" {
		s.explainf("command line not found in log for %s", out.Path)
		return true, nil
	}
	return false, nil
}

// loadDeps adds discovered dependencies to edge's inputs. It reports
// false (with no error) when they're missing or out of date, which makes
// the edge dirty.
func (s *Scanner) loadDeps(edge *graph.Edge) (bool, error) {
	depsType, err := edge.Binding("deps")
	if err != nil {
		return false, err
	}
	if depsType != "" {
		return s.loadDepsFromLog(edge), nil
	}
	path, err := edge.UnescapedBinding("depfile")
	if err != nil || path == "" {
		return true, err
	}
	return s.loadDepfile(edge, path)
}

func (s *Scanner) loadDepsFromLog(edge *graph.Edge) bool {
	// Deps are only supported for single-output edges.
	out := edge.Outputs[0]
	d := s.deps[out]
	if d == nil {
		s.explainf("deps for '%s' are missing", out.Path)
		return false
	}
	if out.Mtime() > d.mtime {
		s.explainf("stored deps info out of date for '%s' (%d vs %d)", out.Path, d.mtime, out.Mtime())
		return false
	}
	edge.InsertImplicitInputs(d.inputs)
	return true
}

func (s *Scanner) loadDepfile(edge *graph.Edge, path string) (bool, error) {
	defer metrics.Record("depfile load")()
	content, err := s.disk.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("loading '%s': %v", path, err)
	}
	first := edge.Outputs[0]
	if len(content) == 0 {
		s.explainf("depfile '%s' is missing", path)
		return false, nil
	}
	outs, ins, err := depfile.ParseDialect(content, depfile.DialectForLog(s.log.Version))
	if err != nil {
		return false, fmt.Errorf("%s: %v", path, err)
	}
	if len(outs) == 0 {
		return false, fmt.Errorf("%s: no outputs declared", path)
	}
	// Only the first target is canonicalised before the checks, as in Ninja.
	outs[0] = graph.CanonicalizePath(outs[0])
	if outs[0] != first.Path {
		s.explainf("expected depfile '%s' to mention '%s', got '%s'", path, first.Path, outs[0])
		return false, nil
	}
	for _, o := range outs {
		if !slices.ContainsFunc(edge.Outputs, func(n *graph.Node) bool { return n.Path == o }) {
			return false, fmt.Errorf("%s: depfile mentions '%s' as an output, but no such output was declared", path, o)
		}
	}
	nodes := make([]*graph.Node, len(ins))
	for i, in := range ins {
		nodes[i] = s.state.DepNode(graph.CanonicalizePath(in))
	}
	edge.InsertImplicitInputs(nodes)
	return true, nil
}

// LoadDyndeps reads the dyndep file node names and applies it to every
// edge that uses it, following Ninja's DyndepLoader.
func (s *Scanner) LoadDyndeps(node *graph.Node) (manifest.DyndepFile, error) {
	node.DyndepPending = false
	s.explainf("loading dyndep file '%s'", node.Path)
	content, err := s.disk.ReadFile(node.Path)
	if err != nil {
		return nil, fmt.Errorf("loading '%s': %s", node.Path, describe(err))
	}
	ddf, err := manifest.ParseDyndep(s.state, node.Path, content)
	if err != nil {
		return nil, err
	}
	for _, edge := range node.OutEdges {
		if edge.Dyndep != node {
			continue
		}
		dd := ddf[edge]
		if dd == nil {
			return nil, fmt.Errorf("'%s' not mentioned in its dyndep file '%s'", edge.Outputs[0].Path, node.Path)
		}
		dd.Used = true
		if err := updateEdge(edge, dd); err != nil {
			return nil, err
		}
	}
	for edge, dd := range ddf {
		if !dd.Used {
			return nil, fmt.Errorf("dyndep file '%s' mentions output '%s' whose build statement does not have a dyndep binding for the file", node.Path, edge.Outputs[0].Path)
		}
	}
	return ddf, nil
}

func updateEdge(edge *graph.Edge, dd *manifest.Dyndeps) error {
	if dd.Restat {
		// Like Ninja, this sets the binding in the edge's scope, which is
		// the file scope for an edge without bindings of its own.
		edge.Env.AddBinding("restat", "1")
	}
	edge.Outputs = append(edge.Outputs, dd.ImplicitOutputs...)
	edge.ImplicitOuts += len(dd.ImplicitOutputs)
	for _, n := range dd.ImplicitOutputs {
		if n.InEdge != nil {
			return fmt.Errorf("multiple rules generate %s", n.Path)
		}
		n.InEdge = edge
	}
	edge.InsertImplicitInputs(dd.ImplicitInputs)
	return nil
}

// describe renders a file error the way strerror would.
func describe(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "No such file or directory"
	case errors.Is(err, fs.ErrPermission):
		return "Permission denied"
	}
	return err.Error()
}
