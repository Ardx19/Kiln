package manifest

import (
	"github.com/Ardx19/kiln/internal/graph"
)

// Dyndeps is what a dyndep file says about one edge: extra implicit
// inputs and outputs (for C++20 modules, the module files it reads and
// writes) and whether it should be restat.
type Dyndeps struct {
	Restat          bool
	ImplicitInputs  []*graph.Node
	ImplicitOutputs []*graph.Node
	Used            bool // set when applied to its edge
}

// DyndepFile maps each edge a dyndep file mentions to its information.
type DyndepFile map[*graph.Edge]*Dyndeps

// ParseDyndep parses a dyndep file, following Ninja's dyndep_parser.cc.
// Every output it mentions must already have a build statement; new
// paths become dep-loader nodes.
func ParseDyndep(state *graph.State, filename string, input []byte) (DyndepFile, error) {
	d := &dyndepParser{state: state, lex: newLexer(filename, input), file: DyndepFile{}, scope: graph.NewScope(nil)}
	return d.file, d.parse()
}

type dyndepParser struct {
	state *graph.State
	lex   *lexer
	file  DyndepFile
	scope *graph.Scope // dyndep files can't set variables, so always empty
}

func (d *dyndepParser) parse() error {
	haveVersion := false
	for {
		switch tok := d.lex.readToken(); tok {
		case tokBuild:
			if !haveVersion {
				return d.lex.error("expected 'ninja_dyndep_version = ...'")
			}
			if err := d.parseEdge(); err != nil {
				return err
			}
		case tokIdent:
			d.lex.unreadToken()
			if haveVersion {
				return d.lex.error("unexpected " + tok.String())
			}
			if err := d.parseVersion(); err != nil {
				return err
			}
			haveVersion = true
		case tokError:
			return d.lex.error(d.lex.describeLastError())
		case tokEOF:
			if !haveVersion {
				return d.lex.error("expected 'ninja_dyndep_version = ...'")
			}
			return nil
		case tokNewline:
		default:
			return d.lex.error("unexpected " + tok.String())
		}
	}
}

func (d *dyndepParser) parseLet() (string, *graph.EvalString, error) {
	name, ok := d.lex.readIdent()
	if !ok {
		return "", nil, d.lex.error("expected variable name")
	}
	p := parser{lex: d.lex}
	if err := p.expectToken(tokEquals); err != nil {
		return "", nil, err
	}
	value := &graph.EvalString{}
	return name, value, d.lex.readVarValue(value)
}

func (d *dyndepParser) parseVersion() error {
	name, value, err := d.parseLet()
	if err != nil {
		return err
	}
	if name != "ninja_dyndep_version" {
		return d.lex.error("expected 'ninja_dyndep_version = ...'")
	}
	v := value.Evaluate(d.scope)
	if major, minor := parseVersion(v); major != 1 || minor != 0 {
		return d.lex.error("unsupported 'ninja_dyndep_version = " + v + "'")
	}
	return nil
}

func (d *dyndepParser) readPath() (*graph.EvalString, error) {
	e := &graph.EvalString{}
	return e, d.lex.readPath(e)
}

func (d *dyndepParser) parseEdge() error {
	p := parser{lex: d.lex}
	out0, err := d.readPath()
	if err != nil {
		return err
	}
	if out0.Empty() {
		return d.lex.error("expected path")
	}
	path := out0.Evaluate(d.scope)
	if path == "" {
		return d.lex.error("empty path")
	}
	path = graph.CanonicalizePath(path)
	node := d.state.LookupNode(path)
	if node == nil || node.InEdge == nil {
		return d.lex.error("no build statement exists for '" + path + "'")
	}
	if d.file[node.InEdge] != nil {
		return d.lex.error("multiple statements for '" + path + "'")
	}
	dd := &Dyndeps{}
	d.file[node.InEdge] = dd

	if out, err := d.readPath(); err != nil {
		return err
	} else if !out.Empty() {
		return d.lex.error("explicit outputs not supported")
	}
	var outs []*graph.EvalString
	if d.lex.peekToken(tokPipe) {
		if outs, _, err = p.readPaths(nil); err != nil {
			return err
		}
	}
	if err := p.expectToken(tokColon); err != nil {
		return err
	}
	if rule, ok := d.lex.readIdent(); !ok || rule != "dyndep" {
		return d.lex.error("expected build command name 'dyndep'")
	}
	if in, err := d.readPath(); err != nil {
		return err
	} else if !in.Empty() {
		return d.lex.error("explicit inputs not supported")
	}
	var ins []*graph.EvalString
	if d.lex.peekToken(tokPipe) {
		if ins, _, err = p.readPaths(nil); err != nil {
			return err
		}
	}
	if d.lex.peekToken(tokPipe2) {
		return d.lex.error("order-only inputs not supported")
	}
	if err := p.expectToken(tokNewline); err != nil {
		return err
	}
	if d.lex.peekToken(tokIndent) {
		key, value, err := d.parseLet()
		if err != nil {
			return err
		}
		if key != "restat" {
			return d.lex.error("binding is not 'restat'")
		}
		dd.Restat = value.Evaluate(d.scope) != ""
	}
	for _, in := range ins {
		path := in.Evaluate(d.scope)
		if path == "" {
			return d.lex.error("empty path")
		}
		dd.ImplicitInputs = append(dd.ImplicitInputs, d.state.DepNode(graph.CanonicalizePath(path)))
	}
	for _, out := range outs {
		path := out.Evaluate(d.scope)
		if path == "" {
			return d.lex.error("empty path")
		}
		dd.ImplicitOutputs = append(dd.ImplicitOutputs, d.state.DepNode(graph.CanonicalizePath(path)))
	}
	return nil
}
