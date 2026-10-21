// Package manifest parses Ninja build files (build.ninja and the files it
// includes) into a graph.State. It follows Ninja's manifest_parser.cc
// statement by statement, so a manifest means the same thing to kiln as it
// does to Ninja, error cases included.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/metrics"
)

// NinjaVersion is the Ninja release whose manifest language kiln
// implements. A ninja_required_version above it is rejected.
const NinjaVersion = "1.13.2"

// ReadFile loads a manifest file; os.ReadFile in production.
type ReadFile func(path string) ([]byte, error)

// Options adjusts parsing.
type Options struct {
	// Dir is the build directory. Manifest paths, including those named by
	// include and subninja, are relative to it, as they are for Ninja run
	// with -C. Empty means the current directory.
	Dir string
	// ReadFile defaults to os.ReadFile.
	ReadFile ReadFile
	// Warn receives warnings, such as a phony target naming itself as an
	// input. Nil discards them.
	Warn func(msg string)
	// PhonyCycleErr keeps a phony target's reference to itself, so the
	// build reports it as a cycle (-w phonycycle=err).
	PhonyCycleErr bool
}

// Load parses the manifest at path, and everything it includes, into a
// new graph.
func Load(path string, opts Options) (*graph.State, error) {
	state := graph.NewState()
	if err := LoadInto(state, path, opts); err != nil {
		return nil, err
	}
	return state, nil
}

// LoadInto parses the manifest at path into an existing graph, in its
// top-level scope.
func LoadInto(state *graph.State, path string, opts Options) error {
	if opts.ReadFile == nil {
		opts.ReadFile = os.ReadFile
	}
	p := &parser{state: state, scope: state.Bindings, opts: opts}
	return p.load(path, nil)
}

// Parse parses manifest text that was already read, for tests.
func Parse(state *graph.State, filename string, input []byte, opts Options) error {
	if opts.ReadFile == nil {
		opts.ReadFile = os.ReadFile
	}
	p := &parser{state: state, scope: state.Bindings, opts: opts}
	return p.parse(filename, input)
}

type parser struct {
	state *graph.State
	scope *graph.Scope
	opts  Options
	lex   *lexer
}

func (p *parser) load(path string, parent *lexer) error {
	defer metrics.Record(".ninja parse")()
	file := path
	if p.opts.Dir != "" && !filepath.IsAbs(path) {
		file = filepath.Join(p.opts.Dir, path)
	}
	input, err := p.opts.ReadFile(file)
	if err != nil {
		msg := fmt.Sprintf("loading '%s': %s", path, describeReadError(err))
		if parent != nil {
			return parent.error(msg)
		}
		return fmt.Errorf("%s", msg)
	}
	return p.parse(path, input)
}

// describeReadError mimics strerror for the common cases.
func describeReadError(err error) string {
	if os.IsNotExist(err) {
		return "No such file or directory"
	}
	if os.IsPermission(err) {
		return "Permission denied"
	}
	return err.Error()
}

func (p *parser) parse(filename string, input []byte) error {
	p.lex = newLexer(filename, input)
	for {
		switch tok := p.lex.readToken(); tok {
		case tokPool:
			if err := p.parsePool(); err != nil {
				return err
			}
		case tokBuild:
			if err := p.parseEdge(); err != nil {
				return err
			}
		case tokRule:
			if err := p.parseRule(); err != nil {
				return err
			}
		case tokDefault:
			if err := p.parseDefault(); err != nil {
				return err
			}
		case tokIdent:
			p.lex.unreadToken()
			name, value, err := p.parseLet()
			if err != nil {
				return err
			}
			v := value.Evaluate(p.scope)
			// Checked straight away, so a too-old binary stops before
			// tripping over syntax it doesn't know.
			if name == "ninja_required_version" {
				if err := checkVersion(v, p.opts.Warn); err != nil {
					return err
				}
			}
			p.scope.AddBinding(name, v)
		case tokInclude, tokSubninja:
			if err := p.parseFileInclude(tok == tokSubninja); err != nil {
				return err
			}
		case tokError:
			return p.lex.error(p.lex.describeLastError())
		case tokEOF:
			return nil
		case tokNewline:
		default:
			return p.lex.error("unexpected " + tok.String())
		}
	}
}

func (p *parser) expectToken(want token) error {
	if got := p.lex.readToken(); got != want {
		return p.lex.error(fmt.Sprintf("expected %s, got %s%s", want, got, want.errorHint()))
	}
	return nil
}

func (p *parser) parseLet() (string, *graph.EvalString, error) {
	name, ok := p.lex.readIdent()
	if !ok {
		return "", nil, p.lex.error("expected variable name")
	}
	if err := p.expectToken(tokEquals); err != nil {
		return "", nil, err
	}
	value := &graph.EvalString{}
	if err := p.lex.readVarValue(value); err != nil {
		return "", nil, err
	}
	return name, value, nil
}

func (p *parser) parsePool() error {
	name, ok := p.lex.readIdent()
	if !ok {
		return p.lex.error("expected pool name")
	}
	if err := p.expectToken(tokNewline); err != nil {
		return err
	}
	if p.state.Pools[name] != nil {
		return p.lex.error("duplicate pool '" + name + "'")
	}
	depth := -1
	for p.lex.peekToken(tokIndent) {
		key, value, err := p.parseLet()
		if err != nil {
			return err
		}
		if key != "depth" {
			return p.lex.error("unexpected variable '" + key + "'")
		}
		if depth = atoi(value.Evaluate(p.scope)); depth < 0 {
			return p.lex.error("invalid pool depth")
		}
	}
	if depth < 0 {
		return p.lex.error("expected 'depth =' line")
	}
	p.state.Pools[name] = &graph.Pool{Name: name, Depth: depth}
	return nil
}

func (p *parser) parseRule() error {
	name, ok := p.lex.readIdent()
	if !ok {
		return p.lex.error("expected rule name")
	}
	if err := p.expectToken(tokNewline); err != nil {
		return err
	}
	if p.scope.LookupRuleCurrentScope(name) != nil {
		return p.lex.error("duplicate rule '" + name + "'")
	}
	rule := &graph.Rule{Name: name, Bindings: map[string]*graph.EvalString{}}
	for p.lex.peekToken(tokIndent) {
		key, value, err := p.parseLet()
		if err != nil {
			return err
		}
		if !graph.IsReservedBinding(key) {
			return p.lex.error("unexpected variable '" + key + "'")
		}
		rule.Bindings[key] = value
	}
	empty := func(key string) bool { v := rule.Bindings[key]; return v == nil || v.Empty() }
	if empty("rspfile") != empty("rspfile_content") {
		return p.lex.error("rspfile and rspfile_content need to be both specified")
	}
	if empty("command") {
		return p.lex.error("expected 'command =' line")
	}
	p.scope.AddRule(rule)
	return nil
}

func (p *parser) parseDefault() error {
	eval := &graph.EvalString{}
	if err := p.lex.readPath(eval); err != nil {
		return err
	}
	if eval.Empty() {
		return p.lex.error("expected target name")
	}
	for !eval.Empty() {
		path := eval.Evaluate(p.scope)
		if path == "" {
			return p.lex.error("empty path")
		}
		if err := p.state.AddDefault(graph.CanonicalizePath(path)); err != nil {
			return p.lex.error(err.Error())
		}
		eval = &graph.EvalString{}
		if err := p.lex.readPath(eval); err != nil {
			return err
		}
	}
	return p.expectToken(tokNewline)
}

// readPaths reads paths until the next non-path token.
func (p *parser) readPaths(into []*graph.EvalString) ([]*graph.EvalString, int, error) {
	n := 0
	for {
		eval := &graph.EvalString{}
		if err := p.lex.readPath(eval); err != nil {
			return nil, 0, err
		}
		if eval.Empty() {
			return into, n, nil
		}
		into = append(into, eval)
		n++
	}
}

func (p *parser) parseEdge() error {
	outs, _, err := p.readPaths(nil)
	if err != nil {
		return err
	}
	implicitOuts := 0
	if p.lex.peekToken(tokPipe) {
		if outs, implicitOuts, err = p.readPaths(outs); err != nil {
			return err
		}
	}
	if len(outs) == 0 {
		return p.lex.error("expected path")
	}
	if err := p.expectToken(tokColon); err != nil {
		return err
	}
	ruleName, ok := p.lex.readIdent()
	if !ok {
		return p.lex.error("expected build command name")
	}
	rule := p.scope.LookupRule(ruleName)
	if rule == nil {
		return p.lex.error("unknown build rule '" + ruleName + "'")
	}

	ins, _, err := p.readPaths(nil)
	if err != nil {
		return err
	}
	implicit, orderOnly := 0, 0
	if p.lex.peekToken(tokPipe) {
		if ins, implicit, err = p.readPaths(ins); err != nil {
			return err
		}
	}
	if p.lex.peekToken(tokPipe2) {
		if ins, orderOnly, err = p.readPaths(ins); err != nil {
			return err
		}
	}
	var validations []*graph.EvalString
	if p.lex.peekToken(tokPipeAt) {
		if validations, _, err = p.readPaths(nil); err != nil {
			return err
		}
	}
	if err := p.expectToken(tokNewline); err != nil {
		return err
	}

	// Edge bindings are evaluated in the enclosing scope, not the edge's
	// own, so one edge variable can't refer to another; Ninja does the same.
	env := p.scope
	if p.lex.peekToken(tokIndent) {
		env = graph.NewScope(p.scope)
		for {
			key, value, err := p.parseLet()
			if err != nil {
				return err
			}
			env.AddBinding(key, value.Evaluate(p.scope))
			if !p.lex.peekToken(tokIndent) {
				break
			}
		}
	}

	edge := p.state.AddEdge(rule)
	edge.Env = env

	poolName, err := edge.Binding("pool")
	if err != nil {
		return p.lex.error(err.Error())
	}
	if poolName != "" {
		pool := p.state.Pools[poolName]
		if pool == nil {
			return p.lex.error("unknown pool name '" + poolName + "'")
		}
		edge.Pool = pool
	}

	for _, out := range outs {
		path := out.Evaluate(env)
		if path == "" {
			return p.lex.error("empty path")
		}
		if err := p.state.AddOut(edge, graph.CanonicalizePath(path)); err != nil {
			return p.lex.error(err.Error())
		}
	}
	edge.ImplicitOuts = implicitOuts

	for _, in := range ins {
		path := in.Evaluate(env)
		if path == "" {
			return p.lex.error("empty path")
		}
		p.state.AddIn(edge, graph.CanonicalizePath(path))
	}
	edge.ImplicitDeps = implicit
	edge.OrderOnlyDeps = orderOnly

	for _, v := range validations {
		path := v.Evaluate(env)
		if path == "" {
			return p.lex.error("empty path")
		}
		p.state.AddValidation(edge, graph.CanonicalizePath(path))
	}

	// CMake 2.8.12 and 3.0 wrote phony edges that list their own output as
	// an input. Ninja drops the self-reference with a warning by default.
	if !p.opts.PhonyCycleErr && edge.IsPhony() && len(edge.Outputs) == 1 && edge.ImplicitOuts == 0 && edge.ImplicitDeps == 0 {
		out := edge.Outputs[0]
		kept := edge.Inputs[:0]
		for _, in := range edge.Inputs {
			if in != out {
				kept = append(kept, in)
			}
		}
		if len(kept) != len(edge.Inputs) {
			// Ninja leaves the node's out-edge list alone; so do we.
			edge.Inputs = kept
			if p.opts.Warn != nil {
				p.opts.Warn(fmt.Sprintf("phony target '%s' names itself as an input; ignoring [-w phonycycle=warn]", out.Path))
			}
		}
	}

	dyndep, err := edge.UnescapedBinding("dyndep")
	if err != nil {
		return p.lex.error(err.Error())
	}
	if dyndep != "" {
		dyndep = graph.CanonicalizePath(dyndep)
		edge.Dyndep = p.state.Node(dyndep)
		edge.Dyndep.DyndepPending = true
		found := false
		for _, in := range edge.Inputs {
			if in == edge.Dyndep {
				found = true
				break
			}
		}
		if !found {
			return p.lex.error("dyndep '" + dyndep + "' is not an input")
		}
	}
	return nil
}

func (p *parser) parseFileInclude(newScope bool) error {
	eval := &graph.EvalString{}
	if err := p.lex.readPath(eval); err != nil {
		return err
	}
	path := eval.Evaluate(p.scope)
	sub := &parser{state: p.state, scope: p.scope, opts: p.opts}
	if newScope {
		sub.scope = graph.NewScope(p.scope)
	}
	if err := sub.load(path, p.lex); err != nil {
		return err
	}
	return p.expectToken(tokNewline)
}

// checkVersion rejects manifests that need a newer Ninja than kiln
// implements, using Ninja's major.minor comparison.
func checkVersion(required string, warn func(string)) error {
	binMajor, binMinor := parseVersion(NinjaVersion)
	fileMajor, fileMinor := parseVersion(required)
	if binMajor > fileMajor {
		if warn != nil {
			warn(fmt.Sprintf("ninja executable version (%s) greater than build file ninja_required_version (%s); versions may be incompatible.", NinjaVersion, required))
		}
		return nil
	}
	if binMajor == fileMajor && binMinor < fileMinor || binMajor < fileMajor {
		//lint:ignore ST1005 the message matches Ninja's word for word
		return fmt.Errorf("ninja version (%s) incompatible with build file ninja_required_version version (%s).", NinjaVersion, required)
	}
	return nil
}

func parseVersion(v string) (major, minor int) {
	majorStr, rest, found := strings.Cut(v, ".")
	major = atoi(majorStr)
	if found {
		minorStr, _, _ := strings.Cut(rest, ".")
		minor = atoi(minorStr)
	}
	return major, minor
}

// atoi behaves like C's atoi: leading whitespace and sign, then digits;
// anything unparsable is 0.
func atoi(s string) int {
	s = strings.TrimLeft(s, " \t\n\v\f\r")
	end := 0
	if end < len(s) && (s[end] == '+' || s[end] == '-') {
		end++
	}
	for end < len(s) && '0' <= s[end] && s[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}
