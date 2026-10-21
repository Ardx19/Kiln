package compat

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/manifest"
	"github.com/Ardx19/kiln/internal/state"
)

// StateReport compares kiln's reading of a tree's .ninja_log and
// .ninja_deps with Ninja's.
type StateReport struct {
	Dir        string
	LogVersion int
	Ninja      string
	Skipped    string // why the tree, or its hash check, wasn't compared
	// HashesCompared counts log entries of edges Ninja considers clean,
	// whose hash Ninja has therefore verified against the command.
	HashesCompared int
	HashesDirty    int // entries of dirty edges, which prove nothing
	DepsOutputs    int // outputs listed by `ninja -t deps`
	Diffs          []string
}

// logVersionWrittenBy returns the .ninja_log version a Ninja release writes.
func logVersionWrittenBy(ninjaVersion string) int {
	switch {
	case versionLess(ninjaVersion, "1.12"):
		return 5
	case versionLess(ninjaVersion, "1.13"):
		return 6
	}
	return 7
}

// ninjaForLog picks a Ninja that writes this log version. Running any
// other version is unsafe: Ninja 1.12+ deletes a log of another version as
// soon as it loads it, even for a dry run.
func ninjaForLog(tools Tools, version int) (Tool, bool) {
	for i := len(tools.Ninjas) - 1; i >= 0; i-- {
		if version == 0 || logVersionWrittenBy(tools.Ninjas[i].Version) == version {
			return tools.Ninjas[i], true
		}
	}
	return Tool{}, false
}

// CompareState checks kiln's state readers against Ninja on one tree.
// Ninja only ever runs in dry-run mode (-n), which reads the logs but
// never writes, recompacts or truncates them. The two cases where loading
// alone would modify them (a log version Ninja doesn't write, and a
// corrupt deps-log tail) are detected first and skipped, and the files
// are fingerprinted before and after as a final guard.
func CompareState(dir string, tools Tools) StateReport {
	r := StateReport{Dir: dir}
	g, err := manifest.Load("build.ninja", manifest.Options{Dir: dir})
	if err != nil {
		r.Skipped = "manifest: " + err.Error()
		return r
	}
	logDir := dir
	if bd := g.Bindings.LookupVariable("builddir"); bd != "" {
		logDir = filepath.Join(dir, bd)
	}
	logPath, depsPath := filepath.Join(logDir, ".ninja_log"), filepath.Join(logDir, ".ninja_deps")

	blog, err := state.ReadBuildLog(logPath)
	var verr *state.VersionError
	if errors.As(err, &verr) {
		r.Skipped = verr.Error()
		return r
	}
	if err != nil {
		r.Diffs = append(r.Diffs, err.Error())
		return r
	}
	r.LogVersion = blog.Version
	dlog, err := state.ReadDepsLog(depsPath)
	if errors.As(err, &verr) {
		r.Skipped = verr.Error()
		return r
	}
	if err != nil {
		r.Diffs = append(r.Diffs, err.Error())
		return r
	}
	if dlog.Corrupt != "" {
		r.Skipped = "deps log has a corrupt tail (" + dlog.Corrupt + "); Ninja would truncate it"
		return r
	}
	ninja, ok := ninjaForLog(tools, blog.Version)
	if !ok {
		r.Skipped = fmt.Sprintf("no Ninja here writes log v%d", blog.Version)
		return r
	}
	r.Ninja = ninja.Version

	before := fingerprint(logPath, depsPath)
	defer func() {
		if after := fingerprint(logPath, depsPath); after != before {
			r.Diffs = append(r.Diffs, "SAFETY: Ninja modified the state files: "+before+" -> "+after)
		}
	}()

	r.compareHashes(g, blog, dir, ninja)
	r.compareDeps(g, dlog, dir, ninja)
	return r
}

func (r *StateReport) compareHashes(g *graph.State, blog *state.BuildLog, dir string, ninja Tool) {
	dirty, err := dryRunCommands(dir, ninja)
	if err != nil {
		// Ninja can't plan a build here (say, sources it needs are
		// missing), so it can't say which edges are clean.
		r.Skipped = "hash check: ninja -n failed: " + err.Error()
		return
	}
	for _, e := range g.Edges {
		if gen, _ := e.Binding("generator"); gen != "" && dirty[mustCommand(e, false)] {
			r.Skipped = "the manifest is out of date, so a dry run stops at regenerating it"
			return
		}
	}
	for _, e := range g.Edges {
		// Ninja ignores command changes for generator edges, so their
		// hashes can be stale while the edge is clean.
		if gen, _ := e.Binding("generator"); e.IsPhony() || gen != "" {
			continue
		}
		cmd, err := e.Command(true)
		if err != nil {
			r.Diffs = append(r.Diffs, err.Error())
			continue
		}
		isDirty := dirty[mustCommand(e, false)]
		for _, o := range e.Outputs {
			entry := blog.Entries[o.Path]
			if entry == nil {
				continue
			}
			if isDirty {
				r.HashesDirty++
				continue
			}
			r.HashesCompared++
			if got := state.HashCommand(blog.Version, cmd); got != entry.Hash {
				r.Diffs = append(r.Diffs, fmt.Sprintf("hash for clean output %s: kiln %x, log %x", o.Path, got, entry.Hash))
			}
		}
	}
}

func (r *StateReport) compareDeps(g *graph.State, dlog *state.DepsLog, dir string, ninja Tool) {
	cmd := exec.Command(ninja.Path, "-C", dir, "-n", "-t", "deps")
	out, err := cmd.Output()
	if err != nil {
		r.Diffs = append(r.Diffs, "ninja -t deps: "+err.Error())
		return
	}
	var b strings.Builder
	for id, p := range dlog.Paths {
		n := g.LookupNode(p)
		if n == nil || n.InEdge == nil {
			continue
		}
		if d, _ := n.InEdge.Binding("deps"); d == "" {
			continue // Ninja only lists outputs of edges that use deps
		}
		r.DepsOutputs++
		var deps *state.Deps
		if id < len(dlog.Deps) {
			deps = dlog.Deps[id]
		}
		if deps == nil {
			fmt.Fprintf(&b, "%s: deps not found\n", p)
			continue
		}
		mtime := statMtime(dir, p)
		status := "VALID"
		if mtime == 0 || mtime > deps.Mtime {
			status = "STALE"
		}
		fmt.Fprintf(&b, "%s: #deps %d, deps mtime %d (%s)\n", p, len(deps.Inputs), deps.Mtime, status)
		for _, in := range deps.Inputs {
			fmt.Fprintf(&b, "    %s\n", dlog.Paths[in])
		}
		b.WriteString("\n")
	}
	if d := firstDifference(splitLines([]byte(b.String())), splitLines(out)); d != "" {
		r.Diffs = append(r.Diffs, "ninja -t deps differs: "+d)
	}
}

// dryRunCommands returns the commands `ninja -n -v` would run.
func dryRunCommands(dir string, ninja Tool) (map[string]bool, error) {
	cmd := exec.Command(ninja.Path, "-C", dir, "-n", "-v")
	cmd.Env = append(withoutWindowsPath(os.Environ()), "NINJA_STATUS="+statusPrefix)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, tail(string(out), 500))
	}
	cmds, err := statusLines(out)
	set := map[string]bool{}
	for _, c := range cmds {
		set[c] = true
	}
	return set, err
}

func mustCommand(e *graph.Edge, rsp bool) string {
	c, _ := e.Command(rsp)
	return c
}

// statMtime returns a file's mtime in ns, or 0 if it doesn't exist, like
// Ninja's RealDiskInterface::Stat.
func statMtime(dir, p string) int64 {
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

func fingerprint(paths ...string) string {
	var parts []string
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			parts = append(parts, "absent")
			continue
		}
		parts = append(parts, fmt.Sprintf("%d@%d", fi.Size(), fi.ModTime().UnixNano()))
	}
	return strings.Join(parts, ",")
}
