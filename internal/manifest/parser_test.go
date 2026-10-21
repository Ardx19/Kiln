package manifest

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/Ardx19/kiln/internal/graph"
)

// load parses build.ninja from an in-memory file set.
func load(t *testing.T, files map[string]string) (*graph.State, []string, error) {
	t.Helper()
	var warnings []string
	state, err := Load("build.ninja", Options{
		ReadFile: func(path string) ([]byte, error) {
			if s, ok := files[path]; ok {
				return []byte(s), nil
			}
			return nil, fs.ErrNotExist
		},
		Warn: func(msg string) { warnings = append(warnings, msg) },
	})
	return state, warnings, err
}

func mustLoad(t *testing.T, manifest string) *graph.State {
	t.Helper()
	state, _, err := load(t, map[string]string{"build.ninja": manifest})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func command(t *testing.T, s *graph.State, output string) string {
	t.Helper()
	n := s.LookupNode(output)
	if n == nil || n.InEdge == nil {
		t.Fatalf("no edge builds %s", output)
	}
	cmd, err := n.InEdge.Command(false)
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestBasics(t *testing.T) {
	s := mustLoad(t, `# a comment
cflags = -O2

rule cc
  command = cc $cflags -c $in -o $out
  description = CC $out

build foo.o: cc foo.c
build bar.o: cc bar.c
  cflags = -O0
default foo.o
`)
	if got := command(t, s, "foo.o"); got != "cc -O2 -c foo.c -o foo.o" {
		t.Errorf("foo.o: %q", got)
	}
	if got := command(t, s, "bar.o"); got != "cc -O0 -c bar.c -o bar.o" {
		t.Errorf("bar.o: %q", got)
	}
	if len(s.Defaults) != 1 || s.Defaults[0].Path != "foo.o" {
		t.Errorf("defaults = %v", s.Defaults)
	}
	if len(s.Edges) != 2 {
		t.Errorf("%d edges, want 2", len(s.Edges))
	}
}

func TestEdgeStructure(t *testing.T) {
	s := mustLoad(t, `rule r
  command = r $in > $out
build a b | c d: r e f | g || h i |@ j
`)
	e := s.LookupNode("a").InEdge
	paths := func(ns []*graph.Node) string {
		var p []string
		for _, n := range ns {
			p = append(p, n.Path)
		}
		return strings.Join(p, " ")
	}
	if got := paths(e.Outputs); got != "a b c d" || e.ImplicitOuts != 2 {
		t.Errorf("outputs %q (implicit %d)", got, e.ImplicitOuts)
	}
	if got := paths(e.Inputs); got != "e f g h i" || e.ImplicitDeps != 1 || e.OrderOnlyDeps != 2 {
		t.Errorf("inputs %q (implicit %d, order-only %d)", got, e.ImplicitDeps, e.OrderOnlyDeps)
	}
	if got := paths(e.Validations); got != "j" {
		t.Errorf("validations %q", got)
	}
	if got := command(t, s, "a"); got != "r e f > a b" {
		t.Errorf("command %q: $in and $out must only list explicit paths", got)
	}
}

func TestScoping(t *testing.T) {
	files := map[string]string{
		"build.ninja": `x = top
rule show
  command = echo $x $y
include inc.ninja
subninja sub.ninja
build top.txt: show
`,
		"inc.ninja": "y = from-include\n",
		"sub.ninja": `x = sub
rule show
  command = sub-rule $x
build sub.txt: show
`,
	}
	s, _, err := load(t, files)
	if err != nil {
		t.Fatal(err)
	}
	// include shares the parent scope; subninja gets a child scope whose
	// variables and rules don't leak back out.
	if got := command(t, s, "top.txt"); got != "echo top from-include" {
		t.Errorf("top.txt: %q", got)
	}
	if got := command(t, s, "sub.txt"); got != "sub-rule sub" {
		t.Errorf("sub.txt: %q", got)
	}
}

func TestEdgeBindingsEvaluateInEnclosingScope(t *testing.T) {
	s := mustLoad(t, `a = outer
rule r
  command = r $b
build x: r
  a = inner
  b = $a
`)
	// b is evaluated in the file scope, so it sees the outer a.
	if got := command(t, s, "x"); got != "r outer" {
		t.Errorf("got %q", got)
	}
}

func TestFileScopeShadowsRuleBinding(t *testing.T) {
	s := mustLoad(t, `rule r
  command = r $description
  description = from-rule
description = from-file
build x: r
build y: r
  other = 1
`)
	// Without edge bindings the edge resolves in the file scope, whose
	// bindings win over the rule's (Ninja's BindingEnv::LookupWithFallback).
	if got := command(t, s, "x"); got != "r from-file" {
		t.Errorf("x: %q", got)
	}
	// With edge bindings, the rule binding wins over the file scope.
	if got := command(t, s, "y"); got != "r from-rule" {
		t.Errorf("y: %q", got)
	}
}

func TestEscapes(t *testing.T) {
	s := mustLoad(t, "rule r\n  command = r $in $$HOME a$:b $\n    continued\n"+
		"build out$ file: r in$ file it's\n")
	if got := command(t, s, "out file"); got != `r 'in file' 'it'\''s' $HOME a:b continued` {
		t.Errorf("got %q", got)
	}
}

func TestInNewlineAndRspfile(t *testing.T) {
	s := mustLoad(t, `rule link
  command = link @$out.rsp
  rspfile = $out.rsp
  rspfile_content = $in_newline
build app: link a.o b.o
`)
	e := s.LookupNode("app").InEdge
	full, err := e.Command(true)
	if err != nil {
		t.Fatal(err)
	}
	if full != "link @app.rsp;rspfile=a.o\nb.o" {
		t.Errorf("command with rspfile: %q", full)
	}
	rsp, _ := e.UnescapedBinding("rspfile")
	if rsp != "app.rsp" {
		t.Errorf("rspfile %q", rsp)
	}
}

func TestPools(t *testing.T) {
	s := mustLoad(t, `pool link_pool
  depth = 2
rule r
  command = r
  pool = link_pool
build a: r
build b: r
  pool = console
build c: r
  pool =
`)
	for out, want := range map[string]string{"a": "link_pool", "b": "console", "c": ""} {
		if got := s.LookupNode(out).InEdge.Pool.Name; got != want {
			t.Errorf("%s: pool %q, want %q", out, got, want)
		}
	}
	if s.Pools["link_pool"].Depth != 2 {
		t.Errorf("depth %d", s.Pools["link_pool"].Depth)
	}
}

func TestPhonySelfReference(t *testing.T) {
	s, warnings, err := load(t, map[string]string{"build.ninja": "build a: phony a b\n"})
	if err != nil {
		t.Fatal(err)
	}
	e := s.LookupNode("a").InEdge
	if len(e.Inputs) != 1 || e.Inputs[0].Path != "b" || len(warnings) != 1 {
		t.Errorf("inputs %v, warnings %q", e.Inputs, warnings)
	}
}

func TestCanonicalPaths(t *testing.T) {
	s := mustLoad(t, "rule r\n  command = r $in\nbuild ./out/../x.o: r ./src//a.c\n")
	if got := command(t, s, "x.o"); got != "r src/a.c" {
		t.Errorf("got %q", got)
	}
}

func TestVariableCycle(t *testing.T) {
	s := mustLoad(t, `rule r
  command = $a
  description = $b
build x: r
`)
	e := s.LookupNode("x").InEdge
	e.Rule.Bindings["a"] = e.Rule.Bindings["description"] // a -> $b, b unset: no cycle
	if _, err := e.Command(false); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	cyc := mustLoad(t, `rule r
  command = $description
  description = $command
build x: r
`)
	if _, err := cyc.LookupNode("x").InEdge.Command(false); err == nil || !strings.Contains(err.Error(), "cycle in rule variables") {
		t.Errorf("want a cycle error, got %v", err)
	}
}

func TestErrors(t *testing.T) {
	for _, c := range []struct{ manifest, want string }{
		{"build x: nope\n", "unknown build rule 'nope'"},
		{"rule r\n  command = a\nrule r\n  command = b\n", "duplicate rule 'r'"},
		{"rule r\n  description = d\n", "expected 'command =' line"},
		{"rule r\n  command = c\n  rspfile = x\n", "rspfile and rspfile_content need to be both specified"},
		{"rule r\n  command = c\n  bogus = 1\n", "unexpected variable 'bogus'"},
		{"rule r\n  command = c\nbuild a: r\nbuild a: r\n", "multiple rules generate a"},
		{"rule r\n  command = c\nbuild a a: r\n", "a is defined as an output multiple times"},
		{"\tx = 1\n", "tabs are not allowed, use spaces"},
		// Inside a rule block a tab just ends the block.
		{"rule r\n\tcommand = c\n", "expected 'command =' line"},
		{"x = $!\n", "bad $-escape (literal $ must be written as $$)"},
		{"x = ${y\n", "bad $-escape (literal $ must be written as $$)"},
		{"x = y", "unexpected EOF"},
		{"# comment without newline", "lexing error"},
		{"default nothing\n", "unknown target 'nothing'"},
		{"pool p\n", "expected 'depth =' line"},
		{"pool p\n  depth = -1\n", "invalid pool depth"},
		{"rule r\n  command = c\n  pool = nope\nbuild a: r\n", "unknown pool name 'nope'"},
		{"rule r\n  command = c\n  dyndep = dd\nbuild a: r b\n", "dyndep 'dd' is not an input"},
		{"build a b\n", "expected ':', got newline ($ also escapes ':')"},
		{"ninja_required_version = 1.99\n", "incompatible with build file ninja_required_version"},
		{"include missing.ninja\n", "loading 'missing.ninja': No such file or directory"},
		{"  x = 1\n", "unexpected indent"},
	} {
		_, _, err := load(t, map[string]string{"build.ninja": c.manifest})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got error %v, want %q", c.manifest, err, c.want)
		}
	}
}

func TestErrorLocation(t *testing.T) {
	_, _, err := load(t, map[string]string{"build.ninja": "rule r\n  command = c\nbuild out: r in $!\n"})
	perr, ok := err.(*Error)
	if !ok {
		t.Fatalf("got %T %v", err, err)
	}
	if perr.File != "build.ninja" || perr.Line != 3 || !strings.Contains(perr.Context, "^ near here") {
		t.Errorf("got %+v", perr)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"rule r\n  command = c $in\nbuild a: r b | c || d |@ e\n  x = $y\n",
		"x = ${a.b} $$ $: $\n  y\n",
		"pool p\n  depth = 3\ndefault a\n",
		"subninja x\ninclude y\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, manifest string) {
		// Must never panic; errors are fine.
		state := graph.NewState()
		err := Parse(state, "build.ninja", []byte(manifest), Options{
			ReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
		})
		if err != nil {
			return
		}
		for _, e := range state.Edges {
			e.Command(true) // evaluation must not panic either
		}
	})
}
