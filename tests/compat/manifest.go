package compat

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/manifest"
)

// ManifestReport is the outcome of comparing kiln's reading of a manifest
// with Ninja's.
type ManifestReport struct {
	Dir      string
	KilnErr  error // kiln rejected the manifest
	NinjaErr string
	Edges    int
	Nodes    int
	Commands int // commands compared
	Parse    time.Duration
	Diffs    []string
}

// CompareManifest loads build.ninja in dir with kiln and asks Ninja about
// the same manifest:
//   - `-t targets all` lists every output with its rule, edge by edge
//   - `-t commands -s` prints the evaluated command of each edge
//
// Both tools run right after loading the manifest, before Ninja opens
// .ninja_log or .ninja_deps, so they never touch a tree's state, whatever
// Ninja version wrote it.
func CompareManifest(dir string, ninja Tool) ManifestReport {
	r := ManifestReport{Dir: dir}
	start := time.Now()
	state, err := manifest.Load("build.ninja", manifest.Options{Dir: dir})
	r.Parse = time.Since(start)

	targets, nerr := runNinja(ninja, dir, "-t", "targets", "all")
	if err != nil || nerr != nil {
		r.KilnErr = err
		if nerr != nil {
			r.NinjaErr = nerr.Error()
		}
		if (err == nil) != (nerr == nil) {
			r.Diffs = append(r.Diffs, fmt.Sprintf("only one tool rejected the manifest\n  kiln:  %v\n  ninja: %s", err, r.NinjaErr))
		} else if mine, theirs := firstLine(err.Error()), ninjaMessage(r.NinjaErr); mine != theirs {
			r.Diffs = append(r.Diffs, fmt.Sprintf("both rejected the manifest, with different messages\n  kiln:  %s\n  ninja: %s", mine, theirs))
		}
		return r
	}
	r.Edges, r.Nodes = len(state.Edges), len(state.Paths)

	// Outputs and rules, in edge order.
	var mine []string
	for _, e := range state.Edges {
		for _, o := range e.Outputs {
			mine = append(mine, o.Path+": "+e.Rule.Name)
		}
	}
	theirs := splitLines(targets)
	if d := firstDifference(mine, theirs); d != "" {
		r.Diffs = append(r.Diffs, "targets (-t targets all) differ: "+d)
	}

	// Commands of every non-phony edge, in edge order.
	var edges []*graph.Edge
	args := []string{"-t", "commands", "-s", "--"}
	for _, e := range state.Edges {
		if !e.IsPhony() {
			edges = append(edges, e)
			args = append(args, e.Outputs[0].Path)
		}
	}
	if len(edges) == 0 {
		return r
	}
	var cmds []string
	for _, e := range edges {
		cmd, err := e.Command(false)
		if err != nil {
			cmd = "<error: " + err.Error() + ">"
		}
		cmds = append(cmds, cmd)
	}
	out, nerr := runNinja(ninja, dir, args...)
	if nerr != nil {
		// Ninja exits on errors such as a cycle between rule variables;
		// kiln must have failed on the same edge.
		if !strings.Contains(strings.Join(cmds, "\n"), "<error: ") {
			r.Diffs = append(r.Diffs, "ninja failed to print commands but kiln evaluated them all: "+nerr.Error())
		}
		return r
	}
	// Commands can't contain newlines unless they use $in_newline, so
	// compare whole text if the line counts don't line up.
	theirs = splitLines(out)
	if len(theirs) != len(cmds) {
		if strings.Join(cmds, "\n") != strings.TrimSuffix(string(out), "\n") {
			r.Diffs = append(r.Diffs, fmt.Sprintf("commands differ (%d from kiln, %d lines from ninja)", len(cmds), len(theirs)))
		}
		r.Commands = len(cmds)
		return r
	}
	for i := range cmds {
		if cmds[i] != theirs[i] {
			r.Diffs = append(r.Diffs, fmt.Sprintf("command for %s differs\n  kiln:  %q\n  ninja: %q", edges[i].Outputs[0].Path, cmds[i], theirs[i]))
			if len(r.Diffs) > 10 {
				break
			}
		}
	}
	r.Commands = len(cmds)
	return r
}

func runNinja(ninja Tool, dir string, args ...string) ([]byte, error) {
	cmd := exec.Command(ninja.Path, append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("%s", strings.TrimSpace(stderr.String()+stdout.String()))
		}
		return nil, err
	}
	// Ninja prints "ninja: Entering directory" for -C on stdout.
	out := stdout.Bytes()
	if i := bytes.IndexByte(out, '\n'); i >= 0 && bytes.HasPrefix(out, []byte("ninja: Entering directory")) {
		out = out[i+1:]
	}
	return out, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// ninjaMessage strips Ninja's "ninja: error: " or "ninja: fatal: " prefix
// from the first line of its error output.
func ninjaMessage(s string) string {
	for _, line := range strings.Split(s, "\n") {
		for _, prefix := range []string{"ninja: error: ", "ninja: fatal: "} {
			if m, ok := strings.CutPrefix(line, prefix); ok {
				return m
			}
		}
	}
	return firstLine(s)
}

func splitLines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func firstDifference(a, b []string) string {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return fmt.Sprintf("entry %d: kiln %q, ninja %q", i, a[i], b[i])
		}
	}
	if len(a) != len(b) {
		return fmt.Sprintf("kiln lists %d outputs, ninja %d", len(a), len(b))
	}
	return ""
}
