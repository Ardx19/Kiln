//go:build compat

package compat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ruleR = "rule r\n  command = r $in > $out\n"

// manifestCases are tricky manifests that kiln must read exactly as Ninja
// does, including which ones are rejected. Keys of extra are additional
// files next to build.ninja.
var manifestCases = []struct {
	name     string
	manifest string
	extra    map[string]string
}{
	{"basic", ruleR + "build a.o: r a.c\ndefault a.o\n", nil},
	{"escapes", "x = 1\nrule r\n  command = r $$HOME $ space a$:b ${x}y $x.suffix $\n    continued\nbuild a: r\n", nil},
	{"dotted variable", "a.b = dotted\na = plain\nrule r\n  command = ${a.b} $a.b\nbuild a: r\n", nil},
	{"dashed variable", "a-b = 1\nrule r\n  command = $a-b\nbuild a: r\n", nil},
	{"keyword prefixes", "builddir = out\nrules = 1\ndefault_x = 2\nrule r\n  command = $builddir $rules $default_x\nbuild build.ninja.d: r\n", nil},
	{"quoted paths", ruleR + "build out$ file 'q': r in$ file it's a$:b\n", nil},
	{"unicode paths", ruleR + "build ünï.o: r ünï.c\n", nil},
	{"tab inside path", ruleR + "build a\tb: r c\n", nil},
	{"canonical paths", ruleR + "build ./a/../b/c x//y/./z ../up /abs/./p: r ./in//put\ndefault ./b/c\n", nil},
	{"dotdot past start", ruleR + "build a/../../b: r ./x/..\n", nil},
	// Ninja keeps ".." that climbs above "/"; filepath.Clean would drop it.
	{"dotdot above root", ruleR + "build /.. /a/../../b /../x: r\n", nil},
	{"implicit and order-only", ruleR + "build a b | c: r d e | f || g h |@ v\nbuild v: r\n", nil},
	{"in_newline and rspfile", "rule link\n  command = link @$out.rsp $in_newline\n  rspfile = $out.rsp\n  rspfile_content = $in_newline\nbuild app: link a.o b.o\n", nil},
	{"edge binding evaluates in outer scope", "a = outer\nrule r\n  command = r $b\nbuild x: r\n  a = inner\n  b = $a\n", nil},
	{"file scope shadows rule binding", "rule r\n  command = r $description\n  description = rule\ndescription = file\nbuild x: r\nbuild y: r\n  z = 1\n", nil},
	{"rule variable chain", "rule r\n  command = $description!\n  description = d $out\nbuild x: r\n", nil},
	{"rule variable cycle", "rule r\n  command = $description\n  description = $command\nbuild x: r\n", nil},
	{"pools", "pool p\n  depth = 3abc\nrule r\n  command = r\n  pool = p\nbuild a: r\nbuild b: r\n  pool = console\n", nil},
	{"pool depth variable", "d = 2\npool p\n  depth = $d\nrule r\n  command = r\nbuild a: r\n  pool = p\n", nil},
	{"phony self reference", "build a: phony a b\nbuild b: phony\n", nil},
	{"dyndep input", "rule r\n  command = r\n  dyndep = dd\nbuild a: r dd\nbuild dd: phony\n", nil},
	{"comments", "# top\n  # indented\nrule r\n  # in rule\n  command = r\nbuild a: r\n", nil},
	{"crlf", "rule r\r\n  command = r $in\r\nbuild a: r b\r\n", nil},
	{"continuation in build line", ruleR + "build a: r $\n    b $\n    c\n", nil},
	{"include and subninja scopes", "x = top\nrule show\n  command = echo $x $y\ninc = inc.ninja\ninclude $inc\nsubninja sub.ninja\nbuild top.txt: show\n",
		map[string]string{"inc.ninja": "y = included\n", "sub.ninja": "x = sub\nrule show\n  command = sub $x\nbuild sub.txt: show\n"}},
	{"required version ok", "ninja_required_version = 1.3\n" + ruleR + "build a: r\n", nil},
	{"no inputs", ruleR + "build a: r\n", nil},
	{"multiple outputs", ruleR + "build a b c: r\n", nil},

	// Rejected by both.
	{"unknown rule", "build x: nope\n", nil},
	{"duplicate rule", "rule r\n  command = a\nrule r\n  command = b\n", nil},
	{"missing command", "rule r\n  description = d\n", nil},
	{"empty command", "rule r\n  command =\n", nil},
	{"rspfile without content", "rule r\n  command = c\n  rspfile = x\n", nil},
	{"unknown rule variable", "rule r\n  command = c\n  bogus = 1\n", nil},
	{"duplicate output", ruleR + "build a: r\nbuild a: r\n", nil},
	{"output listed twice", ruleR + "build a a: r\n", nil},
	{"tab at top level", "\tx = 1\n", nil},
	{"tab in rule block", "rule r\n\tcommand = c\n", nil},
	{"bad escape", "x = $!\n", nil},
	{"unterminated brace", "x = ${y\n", nil},
	{"value at eof", "x = y", nil},
	{"comment at eof", "x = 1\n# no newline", nil},
	{"lone carriage return", "x = a\rb\n", nil},
	{"unknown default", "default nothing\n", nil},
	{"pool without depth", "pool p\n", nil},
	{"negative pool depth", "pool p\n  depth = -1\n", nil},
	{"duplicate pool", "pool p\n  depth = 1\npool p\n  depth = 2\n", nil},
	{"unknown pool", "rule r\n  command = c\n  pool = nope\nbuild a: r\n", nil},
	{"dyndep not an input", "rule r\n  command = c\n  dyndep = dd\nbuild a: r b\n", nil},
	{"missing colon", "build a b\n", nil},
	{"empty path", ruleR + "build $empty: r\n", nil},
	{"validations before order-only", ruleR + "build a: r |@ v || o\n", nil},
	{"required version too new", "ninja_required_version = 1.99\n", nil},
	{"missing include", "include missing.ninja\n", nil},
	{"indent at top level", "  x = 1\n", nil},
	{"subninja rule doesn't leak", "subninja sub.ninja\nbuild a: subrule\n", map[string]string{"sub.ninja": "rule subrule\n  command = s\n"}},
}

func newestNinja(t *testing.T) Tool {
	t.Helper()
	_, tools, _, _ := matrix(t)
	return tools.Ninjas[len(tools.Ninjas)-1]
}

// TestManifestCases compares kiln with the newest Ninja on each case.
func TestManifestCases(t *testing.T) {
	ninja := newestNinja(t)
	for _, c := range manifestCases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{"build.ninja": c.manifest}
			for name, content := range c.extra {
				files[name] = content
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r := CompareManifest(dir, ninja)
			for _, d := range r.Diffs {
				t.Error(d)
			}
			if r.KilnErr != nil {
				t.Logf("both rejected it; kiln: %v", r.KilnErr)
			}
		})
	}
}

// realTrees lists real build trees: the compat fixtures and corpus
// projects under .work, plus any build dirs in KILN_TREES
// (colon-separated), such as local RDKit trees. It returns test names too.
func realTrees(t *testing.T) (names, trees []string) {
	t.Helper()
	repo, _, _, _ := matrix(t)
	var dirs []string
	// Fixture trees as the last compat run left them (built and clean),
	// not the .snap copies, whose absolute paths point at the live trees.
	work := WorkDir(repo)
	for _, glob := range []string{"corpus/*/build", "compat/*/build", "compat/ninja-*/src"} {
		found, _ := filepath.Glob(filepath.Join(work, glob))
		for _, d := range found {
			if !strings.HasSuffix(filepath.Dir(d), ".snap") {
				dirs = append(dirs, d)
			}
		}
	}
	if env := os.Getenv("KILN_TREES"); env != "" {
		dirs = append(dirs, strings.Split(env, ":")...)
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "build.ninja")); err != nil {
			continue
		}
		name, _ := filepath.Rel(repo, d)
		if strings.HasPrefix(name, "..") {
			name = d
		}
		names, trees = append(names, name), append(trees, d)
	}
	if len(trees) == 0 {
		t.Skip("no build trees found; run make compat or corpus/corpus.sh first")
	}
	return names, trees
}

// TestRealManifests compares kiln's parser with Ninja on real build trees.
func TestRealManifests(t *testing.T) {
	ninja := newestNinja(t)
	names, trees := realTrees(t)
	for i, dir := range trees {
		t.Run(names[i], func(t *testing.T) {
			r := CompareManifest(dir, ninja)
			if r.KilnErr != nil {
				t.Errorf("kiln couldn't read it: %v", r.KilnErr)
			}
			for _, d := range r.Diffs {
				t.Error(d)
			}
			t.Logf("%d edges, %d nodes, %d commands compared, parsed in %v", r.Edges, r.Nodes, r.Commands, r.Parse.Round(100_000))
		})
	}
}
