//go:build compat

package compat

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Ardx19/kiln/internal/manifest"
	"github.com/Ardx19/kiln/internal/state"
)

// TestToolsMatchNinja runs the subtools kiln implements itself (clean,
// cleandead, compdb, recompact, restat) on fixture trees: once with each
// Ninja and once with kiln as a drop-in ninja, from the same snapshot.
// What they print, which files remain and what the logs hold must match.
func TestToolsMatchNinja(t *testing.T) {
	repo, tools, ninjas, _ := matrix(t)
	kiln, err := KilnBinary(repo)
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(WorkDir(repo), "shim", "ninja")
	os.MkdirAll(filepath.Dir(shim), 0o755)
	os.Remove(shim)
	if err := os.Symlink(kiln, shim); err != nil {
		t.Fatal(err)
	}
	for _, n := range ninjas {
		for _, k := range Kinds {
			t.Run(k.Name+"/ninja-"+n.Version, func(t *testing.T) {
				t.Parallel()
				k.Name += "-tools"
				tree := setupTree(t, repo, k, n, tools)
				for _, sc := range toolScenarios(t, tree) {
					a := runToolScenario(t, tree, Runner{"ninja", n.Path}, sc)
					b := runToolScenario(t, tree, Runner{"kiln", shim}, sc)
					if sc.name == "cleandead" && versionLess(n.Version, "1.12") {
						a = a.allowOldCleandead(t, b, tree, sc, n.Version)
					}
					for _, d := range a.diff(b) {
						t.Errorf("%s: %s", sc.name, d)
					}
				}
			})
		}
	}
}

type toolScenario struct {
	name    string
	prepare func(tree *Tree) error
	args    []string
	input   string // cleandead: an input path the log also lists
}

func toolScenarios(t *testing.T, tree *Tree) []toolScenario {
	t.Helper()
	st, err := manifest.Load("build.ninja", manifest.Options{Dir: tree.BuildDir})
	if err != nil {
		t.Fatalf("loading %s/build.ninja: %v", tree.BuildDir, err)
	}
	// A compile rule, and a source as the manifest names it.
	var rule, input string
	for _, e := range st.Edges {
		if strings.HasSuffix(e.Outputs[0].Path, ".o") && rule == "" {
			rule = e.Rule.Name
		}
	}
	for path, node := range st.Paths {
		if node.InEdge == nil && strings.HasSuffix(path, tree.Info.Sources[0]) {
			input = path
		}
	}
	if rule == "" || input == "" {
		t.Fatalf("no compile rule (%q) or source (%q) in the manifest", rule, input)
	}
	_, entries, err := tree.LogEntries()
	if err != nil {
		t.Fatal(err)
	}
	outputs := make([]string, 0, len(entries))
	for o := range entries {
		outputs = append(outputs, o)
	}
	sort.Strings(outputs)
	touchTwo := func(tree *Tree) error {
		for _, o := range outputs[:2] {
			if err := touch(tree.path(o)); err != nil {
				return err
			}
		}
		return nil
	}
	// staleEntries adds log entries for a file nothing builds any more
	// (still on disk) and one that's gone.
	staleEntries := func(tree *Tree, extra ...string) error {
		if err := os.WriteFile(filepath.Join(tree.BuildDir, "stale-exists.o"), nil, 0o644); err != nil {
			return err
		}
		lines := []string{"1\t2\t3\tstale-exists.o\tabc", "1\t2\t3\tstale-gone.o\tabc"}
		for _, e := range extra {
			lines = append(lines, "1\t2\t3\t"+e+"\tabc")
		}
		return appendLine(tree.LogPath, strings.Join(lines, "\n"))
	}
	return []toolScenario{
		{name: "restat all", prepare: touchTwo, args: []string{"-t", "restat"}},
		{name: "restat one", prepare: touchTwo, args: []string{"-t", "restat", outputs[0]}},
		{name: "recompact", args: []string{"-t", "recompact"}, prepare: func(tree *Tree) error {
			// Duplicate every entry, as repeated builds do.
			b, err := os.ReadFile(tree.LogPath)
			if err != nil {
				return err
			}
			_, body, _ := strings.Cut(string(b), "\n")
			if err := appendLine(tree.LogPath, strings.TrimSuffix(body, "\n")); err != nil {
				return err
			}
			return staleEntries(tree)
		}},
		{name: "cleandead", args: []string{"-t", "cleandead"}, input: input,
			prepare: func(tree *Tree) error { return staleEntries(tree, input) }},
		{name: "clean", args: []string{"-t", "clean"}},
		{name: "clean -g", args: []string{"-t", "clean", "-g"}},
		{name: "clean all", args: []string{"-t", "clean", "all"}},
		{name: "clean -r", args: []string{"-t", "clean", "-r", rule}},
		{name: "dry clean", args: []string{"-n", "-t", "clean"}},
		{name: "verbose clean", args: []string{"-v", "-t", "clean", "all"}},
		{name: "compdb", args: []string{"-t", "compdb"}},
		{name: "compdb -x", args: []string{"-t", "compdb", "-x", rule}},
	}
}

type toolResult struct {
	code  int
	out   string
	files []string
	log   []string
	deps  []string
	// inputKept is whether the cleandead scenario's input file survived.
	inputKept bool
}

// allowOldCleandead accepts where an older Ninja's cleandead removes more
// than kiln (docs/ninja-quirks.md): before 1.12 it doesn't load dyndep
// files first, so outputs only a dyndep file declares look dead; 1.10
// also removes logged files that have since become inputs. Each removal
// it allows must have happened, and nothing else may differ.
func (a toolResult) allowOldCleandead(t *testing.T, b toolResult, tree *Tree, sc toolScenario, version string) toolResult {
	t.Helper()
	allowed := dyndepOutputs(tree)
	if versionLess(version, "1.11") {
		if a.inputKept || !b.inputKept {
			t.Errorf("cleandead: expected Ninja %s to remove the logged input %s and kiln to keep it", version, sc.input)
		}
		a.inputKept = b.inputKept
		allowed = append(allowed, sc.input)
	}
	byOutput := func(lines []string) map[string]string {
		m := map[string]string{}
		for _, l := range lines {
			if f := strings.Split(l, "\t"); len(f) == 5 {
				m[f[3]] = l
			}
		}
		return m
	}
	aLog, bLog := byOutput(a.log), byOutput(b.log)
	for _, p := range allowed {
		rel := p
		if r, err := filepath.Rel(tree.BuildDir, tree.path(p)); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		if !slices.Contains(a.files, rel) && slices.Contains(b.files, rel) {
			a.files = append(a.files, rel)
		}
		if l, ok := bLog[p]; ok && aLog[p] != "" {
			for i := range a.log {
				if a.log[i] == aLog[p] {
					a.log[i] = l
				}
			}
		}
	}
	sort.Strings(a.files)
	sort.Strings(a.log[1:])
	a.out = b.out // the counts differ
	return a
}

// dyndepOutputs lists the implicit outputs that dyndep files in the build
// dir declare ("build out | extra... : dyndep").
func dyndepOutputs(tree *Tree) []string {
	var outs []string
	filepath.WalkDir(tree.BuildDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".dd") {
			return nil
		}
		b, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(b), "\n") {
			head, _, ok := strings.Cut(line, ":")
			if !ok || !strings.HasPrefix(head, "build ") {
				continue
			}
			if _, implicit, ok := strings.Cut(head, "|"); ok {
				outs = append(outs, strings.Fields(implicit)...)
			}
		}
		return nil
	})
	return outs
}

func runToolScenario(t *testing.T, tree *Tree, r Runner, sc toolScenario) toolResult {
	t.Helper()
	if err := tree.Restore(); err != nil {
		t.Fatal(err)
	}
	if sc.prepare != nil {
		if err := sc.prepare(tree); err != nil {
			t.Fatal(err)
		}
	}
	code, out, err := tree.exec(r, nil, sc.args...)
	if err != nil {
		t.Fatal(err)
	}
	res := toolResult{code: code, out: string(out)}
	filepath.WalkDir(tree.BuildDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(tree.BuildDir, path)
			res.files = append(res.files, rel)
		}
		return nil
	})
	sort.Strings(res.files)
	res.log = normalizedLog(tree)
	if sc.input != "" {
		_, err := os.Stat(tree.path(sc.input))
		res.inputKept = err == nil
	}
	res.deps = depsDump(filepath.Join(filepath.Dir(tree.LogPath), ".ninja_deps"))
	return res
}

// normalizedLog is the log's version and its lines, sorted, with an mtime
// that matches the output on disk shown as "disk": the two runs touch
// files at different moments.
func normalizedLog(tree *Tree) []string {
	b, err := os.ReadFile(tree.LogPath)
	if err != nil {
		return []string{"(" + err.Error() + ")"}
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	for i, l := range lines[1:] {
		f := strings.Split(l, "\t")
		if len(f) != 5 {
			continue
		}
		var st unixStat
		if m, err := strconv.ParseInt(f[2], 10, 64); err == nil && statNs(tree.path(f[3]), &st) && st.ns == m {
			f[2] = "disk"
		}
		lines[i+1] = strings.Join(f, "\t")
	}
	sort.Strings(lines[1:])
	return lines
}

// path resolves a manifest or log path: relative to the build dir unless
// absolute (CMake logs some outputs both ways).
func (t *Tree) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(t.BuildDir, p)
}

type unixStat struct{ ns int64 }

func statNs(path string, st *unixStat) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	st.ns = fi.ModTime().UnixNano()
	return true
}

// depsDump lists each deps record as "output mtime: inputs".
func depsDump(path string) []string {
	log, err := state.ReadDepsLog(path)
	if err != nil {
		return []string{"(" + err.Error() + ")"}
	}
	var out []string
	for id, d := range log.Deps {
		if d == nil || id >= len(log.Paths) {
			continue
		}
		ins := make([]string, len(d.Inputs))
		for i, in := range d.Inputs {
			ins[i] = log.Paths[in]
		}
		out = append(out, fmt.Sprintf("%s %d: %s", log.Paths[id], d.Mtime, strings.Join(ins, " ")))
	}
	sort.Strings(out)
	return out
}

func (a toolResult) diff(b toolResult) []string {
	var d []string
	if a.inputKept != b.inputKept {
		d = append(d, fmt.Sprintf("input kept: %v (ninja) vs %v (kiln)", a.inputKept, b.inputKept))
	}
	if a.code != b.code {
		d = append(d, fmt.Sprintf("exit %d (ninja) vs %d (kiln)", a.code, b.code))
	}
	if a.out != b.out {
		d = append(d, fmt.Sprintf("output differs:\n--- ninja\n%s\n--- kiln\n%s", tail(a.out, 1500), tail(b.out, 1500)))
	}
	if !reflect.DeepEqual(a.files, b.files) {
		d = append(d, fmt.Sprintf("files left: ninja only %v, kiln only %v", setDiff(a.files, b.files), setDiff(b.files, a.files)))
	}
	if !reflect.DeepEqual(a.log, b.log) {
		d = append(d, fmt.Sprintf(".ninja_log: ninja only %v, kiln only %v", short(setDiff(a.log, b.log)), short(setDiff(b.log, a.log))))
	}
	if !reflect.DeepEqual(a.deps, b.deps) {
		d = append(d, fmt.Sprintf(".ninja_deps: ninja only %v, kiln only %v", short(setDiff(a.deps, b.deps)), short(setDiff(b.deps, a.deps))))
	}
	return d
}
