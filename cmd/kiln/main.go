// Command kiln is a Ninja-compatible build runner.
//
// It reads Ninja manifests and shares .ninja_log and .ninja_deps with
// whichever Ninja version the build tree uses: kiln [-C dir] [-f file]
// [-j N] [-k N] [-l N] [-n] [-v] [-d MODE] [-t TOOL] [targets...].
// Run under the name "ninja" (see `kiln shim`), it is a drop-in Ninja.
// See docs/plans/2026-10-21-feat-kiln-build-runner-plan.md.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Ardx19/kiln/internal/build"
	"github.com/Ardx19/kiln/internal/config"
	"github.com/Ardx19/kiln/internal/envdecl"
	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/manifest"
	"github.com/Ardx19/kiln/internal/metrics"
	runstate "github.com/Ardx19/kiln/internal/run"
	"github.com/Ardx19/kiln/internal/state"
	"github.com/Ardx19/kiln/internal/sysinfo"
)

// envLogPath records the environment each output was built under.
const envLogPath = ".kiln/env.log"

// version is overridden at link time: -ldflags "-X main.version=v0.1.0".
var version = "0.0.0-dev"

// ninjaVersion is the Ninja that drop-in mode presents itself as: the
// newest whose manifest language, state files and command line kiln
// supports. Generators enable features by this number.
const ninjaVersion = "1.13.2"

func main() {
	delayGC()
	os.Exit(run(invokedAs(os.Args[0]), os.Args[1:], os.Stdout, os.Stderr))
}

// program is how kiln was started. Under the name ninja (a symlink made
// by `kiln shim`) it's a drop-in Ninja: Ninja's messages and version, and
// no subcommands, because generators run "ninja install" and "ninja test"
// as targets. Under any other name it's kiln.
type program struct {
	name   string // message prefix: "kiln" or "ninja"
	dropIn bool
	// arg0 is argv[0] as given, which getopt's messages start with.
	arg0 string
}

func invokedAs(arg0 string) program {
	switch filepath.Base(arg0) {
	case "ninja", "ninja-build":
		return program{name: "ninja", dropIn: true, arg0: arg0}
	}
	return program{name: "kiln", arg0: arg0}
}

// getoptName is how getopt names the program in its messages.
func (p program) getoptName() string {
	if p.arg0 != "" {
		return p.arg0
	}
	return p.name
}

func run(p program, args []string, stdout, stderr io.Writer) int {
	if !p.dropIn {
		if len(args) > 0 && args[0] == "__daemon" {
			return runDaemon(p, args[1:], stdout, stderr)
		}
		if name, rest := splitSubcommand(args); name != "" {
			return runSubcommand(p, name, rest, stdout, stderr)
		}
	}
	return runNinja(p, args, stdout, stderr)
}

// runNinja runs Ninja's command line: a tool, or a build.
func runNinja(p program, args []string, stdout, stderr io.Writer) int {
	o, e := parseArgs(p, args, stderr)
	if e != nil {
		fmt.Fprint(stdout, e.stdout)
		fmt.Fprint(stderr, e.stderr)
		return e.code
	}
	if o.tool != "" {
		return runTool(p, o, args, stdout, stderr)
	}
	return runBuild(p, o, stdout, stderr)
}

// cycleLimit bounds how often the manifest may be regenerated in one run.
const cycleLimit = 100

// runBuild follows Ninja's main loop: load the manifest and logs, bring
// the manifest itself up to date, and start over whenever that
// regenerated it; then build the targets.
func runBuild(p program, o options, stdout, stderr io.Writer) (code int) {
	if o.stats {
		metrics.Enable()
	}
	errorf := func(format string, args ...any) int {
		fmt.Fprintf(stderr, p.name+": error: "+format+"\n", args...)
		return 1
	}
	if o.dir != "" {
		if !o.quiet {
			fmt.Fprintf(stdout, "%s: Entering directory `%s'\n", p.name, o.dir)
		}
		if err := os.Chdir(o.dir); err != nil {
			fmt.Fprintf(stderr, "%s: fatal: chdir to '%s' - %s\n", p.name, o.dir, strerror(err))
			return 1
		}
	}
	warn := func(msg string) { fmt.Fprintf(stderr, "%s: warning: %s\n", p.name, msg) }
	var obs build.Observer
	if !o.dryRun && os.Getenv("KILN_NO_LOCK") == "" {
		// One build at a time per tree, recorded for status, wait and log.
		sess, err := runstate.Open(os.Getenv("KILN_RUN_ID"), o.targets, os.Getenv("KILN_RUN_ID") != "")
		if err != nil {
			fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
			return 1
		}
		obs = sessionObserver{sess}
		defer func() { sess.Finish(code, "") }()
	}
	if fs := sysinfo.RemoteFilesystem("."); fs != "" {
		warn("this build directory is on " + fs + ", which this machine reaches over a network or VM bridge. " +
			"Every file check is slow, and timestamps can disagree with this machine's clock. " +
			"A build directory on a local disk avoids both.")
	}
	var explain func(string)
	if o.explain {
		explain = func(msg string) { fmt.Fprintf(stderr, "%s explain: %s\n", p.name, msg) }
	}
	verbosity := build.Normal
	switch {
	case o.verbose:
		verbosity = build.Verbose
	case o.quiet:
		verbosity = build.Quiet
	}
	status := build.NewStatus(stdout, os.Getenv("NINJA_STATUS"), verbosity, o.parallelism())
	status.SetProgram(p.name)
	if err := status.FormatError(); err != nil {
		fmt.Fprintf(stderr, "%s: fatal: %v\n", p.name, err)
		return 1
	}

	memCfg, err := memoryConfig(p, o)
	if err != nil {
		fmt.Fprintf(stderr, "%s: fatal: %v\n", p.name, err)
		return 1
	}
	// The declared environment: what commands run in, and what makes edges
	// built under a different declaration dirty.
	cfgs, _, err := loadConfigs()
	if err != nil {
		fmt.Fprintf(stderr, "%s: fatal: %v\n", p.name, err)
		return 1
	}
	decl := declared(cfgs)
	envHash := decl.Hash()
	var jobEnv []string
	if !decl.Empty() {
		jobEnv = decl.Apply(nil)
	}
	cc := newCacheRun(cfgs, warn)
	jobEnv = cc.addEnv(jobEnv)
	var envLog *envdecl.Log
	if _, statErr := os.Stat(envLogPath); envHash != "" || statErr == nil {
		if envLog, err = envdecl.OpenLog(envLogPath); err != nil {
			fmt.Fprintf(stderr, "%s: fatal: %v\n", p.name, err)
			return 1
		}
		defer envLog.Close()
	}
	for cycle := 1; cycle <= cycleLimit; cycle++ {
		st, err := manifest.Load(o.file, manifest.Options{Warn: warn, PhonyCycleErr: o.phonyCycleErr})
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
			MaxLoad:         o.maxLoad,
			Mem:             memCfg,
			Observer:        obs,
			Env:             jobEnv,
			Rewrite:         cc.rewrite(),
			IgnoreHUP:       os.Getenv("KILN_RUN_ID") != "",
			Profile:         t.profile,
		}
		scan := build.NewScanner(st, build.RealDisk{}, t.log, t.deps, explain)
		if envLog != nil {
			scan.EnvChanged = func(output string) bool { return envLog.Changed(output, envHash) }
			cfg.OnBuilt = func(outs []string) {
				if err := envLog.Record(outs, envHash); err != nil {
					warn(fmt.Sprintf("recording the environment: %v", err))
				}
			}
		}
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

		cc.start()
		code := buildTargets(p, o, st, newBuilder, status, stderr)
		cc.report(status)
		t.close()
		if o.stats {
			metrics.Report(stdout)
		}
		return code
	}
	return errorf("manifest '%s' still dirty after %d tries, perhaps system time is not set", o.file, cycleLimit)
}

func buildTargets(p program, o options, st *graph.State, newBuilder func() *build.Builder, status *build.Status, stderr io.Writer) int {
	targets, err := collectTargets(st, o.targets)
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	b := newBuilder()
	for _, n := range targets {
		if err := b.AddTarget(n); err != nil {
			fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
			return 1
		}
	}
	if b.AlreadyUpToDate() {
		if !o.quiet {
			status.Info("no work to do.")
		}
		return 0
	}
	if err := b.Build(); err != nil {
		status.Info("build stopped: %v.", err)
		var berr *build.BuildError
		if errors.As(err, &berr) {
			return berr.Exit
		}
		return 1
	}
	return 0
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
	done := metrics.Record(".ninja_log load")
	t.log, err = state.ReadBuildLog(logPath)
	done()
	if err != nil {
		if !dryRun {
			return nil, fmt.Errorf("%v; kiln won't write to it (run ninja once, or remove it)", err)
		}
		warn(fmt.Sprintf("%v; treating it as empty", err))
		t.log = &state.BuildLog{Entries: map[string]*state.LogEntry{}}
	}
	done = metrics.Record(".ninja_deps load")
	t.deps, err = state.ReadDepsLog(depsPath)
	done()
	if err != nil {
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
		if err := t.logWriter.Recompact(logDead(st)); err != nil {
			return nil, fmt.Errorf("recompacting the build log: %v", err)
		}
	}
	if t.deps.NeedsRecompaction() {
		if err := t.depsWriter.Recompact(depsLive(st)); err != nil {
			return nil, fmt.Errorf("recompacting the deps log: %v", err)
		}
	}
	return t, nil
}

// configuredNinjaVersion finds the Ninja this build tree is meant for, to
// start a new log in its format: CMake's CMAKE_MAKE_PROGRAM, then $NINJA
// (Meson), then ninja on PATH, skipping kiln itself. "" means none was
// found.
func configuredNinjaVersion() string {
	var candidates []string
	if cache, err := os.ReadFile("CMakeCache.txt"); err == nil {
		// The entry's type varies: FILEPATH when CMake found the program,
		// UNINITIALIZED when it came from -DCMAKE_MAKE_PROGRAM=...
		for _, line := range strings.Split(string(cache), "\n") {
			if rest, ok := strings.CutPrefix(line, "CMAKE_MAKE_PROGRAM:"); ok {
				if _, v, ok := strings.Cut(rest, "="); ok {
					candidates = append(candidates, strings.TrimSpace(v))
				}
			}
		}
	}
	if n := os.Getenv("NINJA"); n != "" {
		candidates = append(candidates, n)
	}
	candidates = append(candidates, "ninja")
	for _, c := range candidates {
		// kiln itself (say, through a shim) says it's 1.13.2; the tree's
		// format should come from a real Ninja if there is one.
		if path, err := exec.LookPath(c); err != nil || isKiln(path) {
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

// memoryConfig combines the config file, the flags (which win) and
// $KILN_MEM=off, which switches memory scheduling off in any mode,
// drop-in included (there are no kiln flags as ninja).
func memoryConfig(p program, o options) (build.MemConfig, error) {
	c, err := config.Load(config.Path())
	if err != nil {
		return build.MemConfig{}, err
	}
	m := build.MemConfig{Debug: o.schedDebug, MinJobs: c.Memory.MinJobs}
	m.Budget, _ = config.ParseSize(c.Memory.Budget)
	if c.Memory.Enabled != nil && !*c.Memory.Enabled {
		m.Off = true
	}
	if o.memBudget != "" {
		m.Budget, _ = config.ParseSize(o.memBudget)
	}
	if o.minJobs > 0 {
		m.MinJobs = o.minJobs
	}
	if os.Getenv("KILN_MEM") == "off" {
		m.Off = true
	}
	switch o.memMode {
	case "off":
		m.Off = true
	case "on":
		m.Off = false
	}
	return m, nil
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
