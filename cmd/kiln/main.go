// Command kiln is a Ninja-compatible build runner.
//
// It reads Ninja manifests and shares .ninja_log and .ninja_deps with
// whichever Ninja version the build tree uses: kiln [-C dir] [-f file]
// [-j N] [-k N] [-n] [-v] [-d explain|keepdepfile|keeprsp] [targets...].
// See docs/plans/2026-10-21-feat-kiln-build-runner-plan.md.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Ardx19/kiln/internal/build"
	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/manifest"
	"github.com/Ardx19/kiln/internal/state"
)

// version is overridden at link time: -ldflags "-X main.version=v0.1.0".
var version = "0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type options struct {
	dir, file    string
	dryRun       bool
	verbose      bool
	explain      bool
	tool         string
	showVersion  bool
	targets      []string
	jobs, failOK int
	jobsSet      bool
	keepGoingSet bool
	keepDepfile  bool
	keepRsp      bool
}

// parseArgs reads Ninja's command line, getopt-style: bundled short
// flags (-nv), attached values (-j8, -Cdir), options after targets, and
// "--" to end options.
func parseArgs(args []string) (options, error) {
	o := options{file: "build.ninja"}
	withValue := "Cfjkldtw"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.targets = append(o.targets, args[i+1:]...)
			return o, nil
		case a == "--version":
			o.showVersion = true
			continue
		case a == "--verbose":
			o.verbose = true
			continue
		case strings.HasPrefix(a, "--"):
			return o, fmt.Errorf("unrecognized option '%s'", a)
		case len(a) < 2 || a[0] != '-':
			o.targets = append(o.targets, a)
			continue
		}
		for j := 1; j < len(a); j++ {
			c := a[j]
			if !strings.ContainsRune(withValue, rune(c)) {
				switch c {
				case 'n':
					o.dryRun = true
				case 'v':
					o.verbose = true
				default:
					return o, fmt.Errorf("invalid option -- '%c'", c)
				}
				continue
			}
			value := a[j+1:]
			if value == "" {
				if i+1 == len(args) {
					return o, fmt.Errorf("option requires an argument -- '%c'", c)
				}
				i++
				value = args[i]
			}
			if err := o.set(c, value); err != nil {
				return o, err
			}
			if c == 't' {
				// Everything after the tool name belongs to the tool.
				o.targets = append(o.targets, args[i+1:]...)
				return o, nil
			}
			break
		}
	}
	return o, nil
}

func (o *options) set(c byte, value string) error {
	switch c {
	case 'C':
		o.dir = value
	case 'f':
		o.file = value
	case 't':
		o.tool = value
	case 'd':
		switch value {
		case "explain":
			o.explain = true
		case "keepdepfile":
			o.keepDepfile = true
		case "keeprsp":
			o.keepRsp = true
		case "stats":
			// Accepted; not implemented yet.
		default:
			return fmt.Errorf("unknown debug setting '%s'", value)
		}
	case 'j', 'k', 'l':
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid -%c parameter", c)
		}
		switch c {
		case 'j':
			o.jobs, o.jobsSet = n, true
		case 'k':
			o.failOK, o.keepGoingSet = n, true
		}
	case 'w':
		// Warning flags are accepted and ignored.
	}
	return nil
}

func run(args []string, stdout, stderr io.Writer) int {
	o, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "kiln: %v\n", err)
		return 2
	}
	if o.showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if o.tool != "" {
		fmt.Fprintf(stderr, "kiln: error: -t %s isn't available yet; run ninja -t %s (kiln shares Ninja's state files)\n", o.tool, o.tool)
		return 1
	}
	return runBuild(o, stdout, stderr)
}

// cycleLimit bounds how often the manifest may be regenerated in one run.
const cycleLimit = 100

// runBuild follows Ninja's main loop: load the manifest and logs, bring
// the manifest itself up to date, and start over whenever that
// regenerated it; then build the targets.
func runBuild(o options, stdout, stderr io.Writer) int {
	errorf := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "kiln: error: "+format+"\n", args...)
		return 1
	}
	if o.dir != "" {
		fmt.Fprintf(stdout, "kiln: Entering directory `%s'\n", o.dir)
		if err := os.Chdir(o.dir); err != nil {
			return errorf("chdir to '%s' - %v", o.dir, err)
		}
	}
	warn := func(msg string) { fmt.Fprintf(stderr, "kiln: warning: %s\n", msg) }
	var explain func(string)
	if o.explain {
		explain = func(msg string) { fmt.Fprintf(stderr, "kiln explain: %s\n", msg) }
	}
	status := build.NewStatus(stdout, os.Getenv("NINJA_STATUS"), o.verbose)

	for cycle := 1; cycle <= cycleLimit; cycle++ {
		st, err := manifest.Load(o.file, manifest.Options{Warn: warn})
		if err != nil {
			return errorf("%v", err)
		}
		t, err := openTree(st, o.dryRun, warn)
		if err != nil {
			return errorf("%v", err)
		}
		cfg := build.Config{
			DryRun:          o.dryRun,
			Parallelism:     o.parallelism(),
			FailuresAllowed: o.failuresAllowed(),
			KeepDepfile:     o.keepDepfile,
			KeepRsp:         o.keepRsp,
			Profile:         t.profile,
		}
		scan := build.NewScanner(st, build.RealDisk{}, t.log, t.deps, explain)
		newBuilder := func() *build.Builder {
			return build.NewBuilder(st, scan, status, cfg, t.logWriter, t.depsWriter)
		}

		// Bring the manifest itself up to date first.
		if node := st.LookupNode(graph.CanonicalizePath(o.file)); node != nil {
			b := newBuilder()
			if err := b.AddTarget(node); err != nil {
				t.close()
				return errorf("rebuilding '%s': %v", o.file, err)
			}
			if !b.AlreadyUpToDate() {
				if err := b.Build(); err != nil {
					t.close()
					return errorf("rebuilding '%s': %v", o.file, err)
				}
				if o.dryRun {
					// The manifest would change, and a dry run can't see how.
					t.close()
					return 0
				}
				if node.Dirty {
					t.close()
					continue // regenerated: start over with the new manifest
				}
				// A restat edge left it unchanged; rescan from scratch.
				st.Reset()
			}
		}

		code := buildTargets(o, st, newBuilder, stdout, stderr)
		t.close()
		return code
	}
	return errorf("manifest '%s' still dirty after %d tries, perhaps system time is not set", o.file, cycleLimit)
}

func buildTargets(o options, st *graph.State, newBuilder func() *build.Builder, stdout, stderr io.Writer) int {
	targets, err := collectTargets(st, o.targets)
	if err != nil {
		fmt.Fprintf(stderr, "kiln: error: %v\n", err)
		return 1
	}
	b := newBuilder()
	for _, n := range targets {
		if err := b.AddTarget(n); err != nil {
			fmt.Fprintf(stderr, "kiln: error: %v\n", err)
			return 1
		}
	}
	if b.AlreadyUpToDate() {
		fmt.Fprintln(stdout, "kiln: no work to do.")
		return 0
	}
	if err := b.Build(); err != nil {
		fmt.Fprintf(stdout, "kiln: build stopped: %v.\n", err)
		var berr *build.BuildError
		if errors.As(err, &berr) {
			return berr.Exit
		}
		return 1
	}
	return 0
}

func (o options) parallelism() int {
	if o.jobsSet {
		return o.jobs // 0 means no limit, as in Ninja
	}
	// Ninja's default: two more than the CPUs we may run on.
	switch n := runtime.NumCPU(); n {
	case 0, 1:
		return 2
	case 2:
		return 3
	default:
		return n + 2
	}
}

func (o options) failuresAllowed() int {
	if !o.keepGoingSet {
		return 1
	}
	if o.failOK <= 0 {
		return 1 << 30 // -k 0: keep going no matter what
	}
	return o.failOK
}

// tree is the state kiln shares with Ninja in a build directory.
type tree struct {
	profile    build.Profile
	log        *state.BuildLog
	deps       *state.DepsLog
	logWriter  *state.LogWriter
	depsWriter *state.DepsWriter
}

func (t *tree) close() {
	if t.logWriter != nil {
		t.logWriter.Close()
	}
	if t.depsWriter != nil {
		t.depsWriter.Close()
	}
}

// openTree reads .ninja_log and .ninja_deps and decides which Ninja to
// behave like. A dry run treats unreadable state as empty, with a
// warning. A real build refuses to append to a format kiln doesn't write;
// it never deletes or rewrites such files the way Ninja would.
func openTree(st *graph.State, dryRun bool, warn func(string)) (*tree, error) {
	dir := st.Bindings.LookupVariable("builddir")
	logPath, depsPath := filepath.Join(dir, ".ninja_log"), filepath.Join(dir, ".ninja_deps")
	t := &tree{}
	var err error
	if t.log, err = state.ReadBuildLog(logPath); err != nil {
		if !dryRun {
			return nil, fmt.Errorf("%v; kiln won't write to it (run ninja once, or remove it)", err)
		}
		warn(fmt.Sprintf("%v; treating it as empty", err))
		t.log = &state.BuildLog{Entries: map[string]*state.LogEntry{}}
	}
	if t.deps, err = state.ReadDepsLog(depsPath); err != nil {
		if !dryRun {
			return nil, fmt.Errorf("%v; kiln won't write to it (run ninja once, or remove it)", err)
		}
		warn(fmt.Sprintf("%v; treating it as empty", err))
		t.deps = &state.DepsLog{}
	} else if t.deps.Corrupt != "" {
		warn(fmt.Sprintf(".ninja_deps: %s; using the records before it", t.deps.Corrupt))
	}
	if t.log.Version != 0 {
		t.profile = build.Profile{LogVersion: t.log.Version}
	} else {
		t.profile = build.ProfileForNinja(configuredNinjaVersion())
	}
	if dryRun {
		return t, nil
	}

	if dir != "" {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return nil, err
		}
	}
	t.logWriter = state.NewLogWriter(logPath, t.log, t.profile.LogVersion)
	t.depsWriter = state.NewDepsWriter(depsPath, t.deps)
	// Recompact as Ninja does when opening the logs for writing.
	if t.log.NeedsRecompaction() {
		dead := func(out string) bool {
			if n := st.LookupNode(out); n != nil && n.InEdge != nil {
				return false
			}
			m, _ := build.RealDisk{}.Stat(out)
			return m == 0
		}
		if err := t.logWriter.Recompact(dead); err != nil {
			return nil, fmt.Errorf("recompacting the build log: %v", err)
		}
	}
	if t.deps.NeedsRecompaction() {
		live := func(out string) bool {
			n := st.LookupNode(out)
			if n == nil || n.InEdge == nil {
				return false
			}
			d, _ := n.InEdge.Binding("deps")
			return d != ""
		}
		if err := t.depsWriter.Recompact(live); err != nil {
			return nil, fmt.Errorf("recompacting the deps log: %v", err)
		}
	}
	return t, nil
}

// configuredNinjaVersion finds the Ninja this build tree is meant for, to
// start a new log in its format: CMake's CMAKE_MAKE_PROGRAM, then $NINJA
// (Meson), then ninja on PATH. "" means none was found.
func configuredNinjaVersion() string {
	var candidates []string
	if cache, err := os.ReadFile("CMakeCache.txt"); err == nil {
		for _, line := range strings.Split(string(cache), "\n") {
			if v, ok := strings.CutPrefix(line, "CMAKE_MAKE_PROGRAM:FILEPATH="); ok {
				candidates = append(candidates, strings.TrimSpace(v))
			}
		}
	}
	if n := os.Getenv("NINJA"); n != "" {
		candidates = append(candidates, n)
	}
	candidates = append(candidates, "ninja")
	for _, c := range candidates {
		if strings.Contains(filepath.Base(c), "kiln") {
			continue
		}
		out, err := exec.Command(c, "--version").Output()
		if err == nil {
			if v := strings.TrimSpace(string(out)); v != "" && v[0] >= '0' && v[0] <= '9' {
				return v
			}
		}
	}
	return ""
}

func collectTargets(st *graph.State, args []string) ([]*graph.Node, error) {
	if len(args) == 0 {
		return st.DefaultNodes()
	}
	var nodes []*graph.Node
	for _, a := range args {
		path := graph.CanonicalizePath(a)
		n := st.LookupNode(path)
		if n == nil {
			msg := fmt.Sprintf("unknown target '%s'", path)
			if path == "clean" {
				msg += ", did you mean 'ninja -t clean'?"
			}
			return nil, errors.New(msg)
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}
