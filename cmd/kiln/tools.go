package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/Ardx19/kiln/internal/build"
	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/manifest"
	"github.com/Ardx19/kiln/internal/state"
	"github.com/Ardx19/kiln/internal/tools"
)

// toolPhase is when a subtool runs, as in Ninja.
type toolPhase int

const (
	afterFlags toolPhase = iota // before the manifest is loaded
	afterLoad                   // after the manifest, before the logs
	afterLogs                   // after the logs are opened
)

// subtool is one of Ninja 1.13's -t tools. kiln runs the ones generators
// call itself; the rest go to a real Ninja, which reads the same state.
type subtool struct {
	name, desc string
	phase      toolPhase
	run        func(c *toolContext) int // nil: handed to a real Ninja
}

var subtools = []subtool{
	{"browse", "browse dependency graph in a web browser", afterLoad, nil},
	{"clean", "clean built files", afterLoad, toolClean},
	{"commands", "list all commands required to rebuild given targets", afterLoad, nil},
	{"inputs", "list all inputs required to rebuild given targets", afterLoad, nil},
	{"multi-inputs", "print one or more sets of inputs required to build targets", afterLoad, nil},
	{"deps", "show dependencies stored in the deps log", afterLogs, nil},
	{"missingdeps", "check deps log dependencies on generated files", afterLogs, nil},
	{"graph", "output graphviz dot file for targets", afterLoad, nil},
	{"query", "show inputs/outputs for a path", afterLogs, nil},
	{"targets", "list targets by their rule or depth in the DAG", afterLoad, nil},
	{"compdb", "dump JSON compilation database to stdout", afterLoad, toolCompdb},
	{"compdb-targets", "dump JSON compilation database for a given list of targets to stdout", afterLoad, nil},
	{"recompact", "recompacts ninja-internal data structures", afterLoad, toolRecompact},
	{"restat", "restats all outputs in the build log", afterFlags, toolRestat},
	{"rules", "list all rules", afterLoad, nil},
	{"cleandead", "clean built files that are no longer produced by the manifest", afterLogs, toolCleanDead},
	{"urtle", "", afterFlags, nil},
}

// toolContext is what a native subtool works with.
type toolContext struct {
	p              program
	o              options
	args           []string // after the tool name
	stdout, stderr io.Writer
	st             *graph.State // from afterLoad
	tree           *tree        // from afterLogs
}

func (c *toolContext) errorf(format string, args ...any) int {
	fmt.Fprintf(c.stderr, "%s: error: %s\n", c.p.name, fmt.Sprintf(format, args...))
	return 1
}

// runTool runs `-t name`, following Ninja's order: change directory, run
// tools that need nothing else, load the manifest, run tools that need
// only that, open the logs, run the rest. rawArgs is the whole command
// line, for handing a tool to a real Ninja.
func runTool(p program, o options, rawArgs []string, stdout, stderr io.Writer) int {
	if o.tool == "list" {
		fmt.Fprintf(stdout, "%s subtools:\n", p.name)
		for _, t := range subtools {
			if t.desc != "" {
				fmt.Fprintf(stdout, "%11s  %s\n", t.name, t.desc)
			}
		}
		return 0
	}
	var tool *subtool
	for i := range subtools {
		if subtools[i].name == o.tool {
			tool = &subtools[i]
		}
	}
	if tool == nil {
		names := make([]string, len(subtools))
		for i, t := range subtools {
			names[i] = t.name
		}
		msg := fmt.Sprintf("unknown tool '%s'", o.tool)
		if s := spellcheck(o.tool, names...); s != "" {
			msg += fmt.Sprintf(", did you mean '%s'?", s)
		}
		fmt.Fprintf(stderr, "%s: fatal: %s\n", p.name, msg)
		return 1
	}
	if tool.run == nil {
		return passThrough(p, o, *tool, rawArgs, stderr)
	}
	c := &toolContext{p: p, o: o, args: o.targets, stdout: stdout, stderr: stderr}
	if o.dir != "" {
		// Ninja doesn't print "Entering directory" for tools, so their
		// output can be piped.
		if err := os.Chdir(o.dir); err != nil {
			fmt.Fprintf(stderr, "%s: fatal: chdir to '%s' - %s\n", p.name, o.dir, strerror(err))
			return 1
		}
	}
	if tool.phase == afterFlags {
		return tool.run(c)
	}
	warn := func(msg string) { fmt.Fprintf(stderr, "%s: warning: %s\n", p.name, msg) }
	st, err := manifest.Load(o.file, manifest.Options{Warn: warn, PhonyCycleErr: o.phonyCycleErr})
	if err != nil {
		return c.errorf("%v", err)
	}
	c.st = st
	if tool.phase == afterLoad {
		return tool.run(c)
	}
	if err := ensureBuildDir(st, o.dryRun); err != nil {
		return c.errorf("%v", err)
	}
	t, err := openTree(st, o.dryRun, warn)
	if err != nil {
		return c.errorf("%v", err)
	}
	c.tree = t
	return tool.run(c)
}

// ensureBuildDir creates builddir, as Ninja does before opening the logs.
func ensureBuildDir(st *graph.State, dryRun bool) error {
	dir := st.Bindings.LookupVariable("builddir")
	if dir == "" || dryRun {
		return nil
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return fmt.Errorf("creating build directory %s: %s", dir, strerror(err))
	}
	return nil
}

// toolFlags parses a tool's own flags like getopt with the given letters,
// GNU-style (flags may follow operands until "--"). It returns the flags
// seen, the operands, and the first unknown flag (0 if none).
func toolFlags(args []string, letters string) (map[byte]bool, []string, byte) {
	flags := map[byte]bool{}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			rest = append(rest, a)
			continue
		}
		for j := 1; j < len(a); j++ {
			if !strings.ContainsRune(letters, rune(a[j])) {
				return flags, rest, a[j]
			}
			flags[a[j]] = true
		}
	}
	return flags, rest, 0
}

func toolClean(c *toolContext) int {
	flags, targets, bad := toolFlags(c.args, "hgr")
	if bad != 0 || flags['h'] {
		fmt.Fprint(c.stdout, "usage: ninja -t clean [options] [targets]\n\n"+
			"options:\n"+
			"  -g     also clean files marked as ninja generator output\n"+
			"  -r     interpret targets as a list of rules to clean instead\n")
		return 1
	}
	if flags['r'] && len(targets) == 0 {
		return c.errorf("expected a rule to clean")
	}
	cl := c.cleaner()
	switch {
	case len(targets) > 0 && flags['r']:
		return cl.CleanRules(targets)
	case len(targets) > 0:
		return cl.CleanTargets(targets)
	}
	return cl.CleanAll(flags['g'])
}

func toolCleanDead(c *toolContext) int {
	logged := make([]string, 0, len(c.tree.log.Entries))
	for out := range c.tree.log.Entries {
		logged = append(logged, out)
	}
	return c.cleaner().CleanDead(logged)
}

func (c *toolContext) cleaner() *tools.Cleaner {
	scan := build.NewScanner(c.st, build.RealDisk{}, nil, nil, nil)
	return &tools.Cleaner{
		State: c.st, DryRun: c.o.dryRun, Verbose: c.o.verbose, Quiet: c.o.quiet,
		Out: c.stdout, Err: c.stderr, Program: c.p.name,
		LoadDyndep: func(n *graph.Node) { scan.LoadDyndeps(n) },
	}
}

func toolCompdb(c *toolContext) int {
	flags, rules, bad := toolFlags(c.args, "hx")
	if bad != 0 || flags['h'] {
		fmt.Fprint(c.stdout, "usage: ninja -t compdb [options] [rules]\n\n"+
			"options:\n"+
			"  -x     expand @rspfile style response file invocations\n")
		return 1
	}
	// getcwd(3), not $PWD: Ninja reports the physical directory.
	dir, err := unix.Getwd()
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: fatal: cannot determine working directory: %s\n", c.p.name, strerror(err))
		return 1
	}
	if err := tools.Compdb(c.stdout, c.st, dir, rules, flags['x']); err != nil {
		return c.errorf("%v", err)
	}
	return 0
}

// toolRecompact rewrites both logs without dead entries, in the tree's
// own versions. Unlike Ninja 1.12+, it won't discard a log of another
// version.
func toolRecompact(c *toolContext) int {
	if err := ensureBuildDir(c.st, c.o.dryRun); err != nil {
		return c.errorf("%v", err)
	}
	dir := c.st.Bindings.LookupVariable("builddir")
	logPath, depsPath := filepath.Join(dir, ".ninja_log"), filepath.Join(dir, ".ninja_deps")
	if _, err := os.Stat(logPath); err == nil {
		log, err := state.ReadBuildLog(logPath)
		if err != nil {
			return c.errorf("loading build log %s: %v", logPath, err)
		}
		if err := state.NewLogWriter(logPath, log, log.Version).Recompact(logDead(c.st)); err != nil {
			return c.errorf("failed recompaction: %v", err)
		}
	}
	if _, err := os.Stat(depsPath); err == nil {
		deps, err := state.ReadDepsLog(depsPath)
		if err != nil {
			return c.errorf("loading deps log %s: %v", depsPath, err)
		}
		if err := state.NewDepsWriter(depsPath, deps).Recompact(depsLive(c.st)); err != nil {
			return c.errorf("failed recompaction: %v", err)
		}
	}
	return 0
}

// toolRestat rewrites .ninja_log with each output's current mtime (all
// outputs, or the ones named). Like Ninja, it runs before the manifest is
// read, so it uses ./.ninja_log even when the manifest sets builddir.
func toolRestat(c *toolContext) int {
	flags, outputs, bad := toolFlags(c.args, "h")
	if bad != 0 || flags['h'] {
		fmt.Fprint(c.stdout, "usage: ninja -t restat [outputs]\n")
		return 1
	}
	const logPath = ".ninja_log"
	if _, err := os.Stat(logPath); errors.Is(err, fs.ErrNotExist) {
		return 0 // nothing to restat
	}
	log, err := state.ReadBuildLog(logPath)
	if err != nil {
		return c.errorf("loading build log %s: %v", logPath, err)
	}
	want := map[string]bool{}
	for _, o := range outputs {
		want[o] = true
	}
	for out, e := range log.Entries {
		if len(want) > 0 && !want[out] {
			continue
		}
		m, err := build.RealDisk{}.Stat(out)
		if err != nil {
			return c.errorf("failed recompaction: %v", err)
		}
		e.Mtime = m
	}
	noneDead := func(string) bool { return false }
	if err := state.NewLogWriter(logPath, log, log.Version).Recompact(noneDead); err != nil {
		return c.errorf("failed recompaction: %v", err)
	}
	return 0
}

// logDead is Ninja's BuildLogUser::IsPathDead for recompacting
// .ninja_log: an output no edge produces any more, whose file is gone.
func logDead(st *graph.State) func(string) bool {
	return func(out string) bool {
		if n := st.LookupNode(out); n != nil && n.InEdge != nil {
			return false
		}
		m, _ := build.RealDisk{}.Stat(out)
		return m == 0
	}
}

// depsLive is Ninja's DepsLog::IsDepsEntryLiveFor: the output's edge
// still exists and still uses deps.
func depsLive(st *graph.State) func(string) bool {
	return func(out string) bool {
		n := st.LookupNode(out)
		if n == nil || n.InEdge == nil {
			return false
		}
		d, _ := n.InEdge.Binding("deps")
		return d != ""
	}
}

// passThrough replaces kiln with a real Ninja running the same command
// line. Tools that open the logs are only handed to a Ninja that writes
// the tree's log version, because Ninja 1.12+ deletes a log of any other
// version when it loads it.
func passThrough(p program, o options, tool subtool, rawArgs []string, stderr io.Writer) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "%s: error: %s\n", p.name, fmt.Sprintf(format, args...))
		return 1
	}
	if os.Getenv("KILN_PASSTHROUGH") != "" {
		return fail("-t %s: the Ninja kiln handed it to is kiln itself; set KILN_REAL_NINJA to a real Ninja", tool.name)
	}
	real := realNinja()
	if real == "" {
		return fail("-t %s needs a real Ninja, and none was found on PATH (kiln shares Ninja's state files, so set KILN_REAL_NINJA or put ninja on PATH)", tool.name)
	}
	if tool.phase == afterLogs {
		if msg := logVersionClash(o, real); msg != "" {
			return fail("-t %s: %s", tool.name, msg)
		}
	}
	env := append(os.Environ(), "KILN_PASSTHROUGH=1")
	err := execReal(real, append([]string{real}, rawArgs...), env)
	return fail("running %s: %v", real, err)
}

// execReal is syscall.Exec, replaced in tests.
var execReal = syscall.Exec

// logVersionClash explains why real must not open this tree's logs, or
// returns "".
func logVersionClash(o options, real string) string {
	out, err := exec.Command(real, "--version").Output()
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(out))
	want := build.ProfileForNinja(v).LogVersion
	dir := o.dir
	st, err := manifest.Load(o.file, manifest.Options{Dir: dir})
	if err != nil {
		return "" // let Ninja report it
	}
	logPath := filepath.Join(dir, st.Bindings.LookupVariable("builddir"), ".ninja_log")
	log, err := state.ReadBuildLog(logPath)
	if err != nil || log.Version == 0 || log.Version == want {
		return ""
	}
	return fmt.Sprintf("%s is Ninja %s, which writes .ninja_log v%d and would discard this tree's v%d log; "+
		"set KILN_REAL_NINJA to a Ninja that writes v%d", real, v, want, log.Version, log.Version)
}

// realNinja finds a Ninja that isn't kiln: $KILN_REAL_NINJA, then ninja
// or ninja-build on PATH. "" means none.
func realNinja() string {
	var candidates []string
	if r := os.Getenv("KILN_REAL_NINJA"); r != "" {
		candidates = append(candidates, r)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		candidates = append(candidates, filepath.Join(dir, "ninja"), filepath.Join(dir, "ninja-build"))
	}
	for _, c := range candidates {
		fi, err := os.Stat(c)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 || isKiln(c) {
			continue
		}
		return c
	}
	return ""
}

// isKiln reports whether path is this kiln binary (through any symlink).
func isKiln(path string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	a, err1 := os.Stat(path)
	b, err2 := os.Stat(self)
	return err1 == nil && err2 == nil && os.SameFile(a, b)
}

// strerror spells an error's errno like C's strerror, which Ninja prints.
func strerror(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if s := errno.Error(); s != "" {
			return strings.ToUpper(s[:1]) + s[1:]
		}
	}
	return err.Error()
}
