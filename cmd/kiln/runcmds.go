package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Ardx19/kiln/internal/build"
	runstate "github.com/Ardx19/kiln/internal/run"
)

// sessionObserver feeds a build's progress to its run directory.
type sessionObserver struct{ s *runstate.Session }

func (o sessionObserver) Progress(p build.Progress, force bool) {
	rp := runstate.Progress{Total: p.Total, Done: p.Done, BudgetMiB: p.BudgetMiB, ReservMiB: p.ReservedMiB}
	for _, j := range p.Jobs {
		rp.Jobs = append(rp.Jobs, runstate.Job{Output: j.Output, RSSMiB: j.RSSMiB, Seconds: j.Seconds})
	}
	o.s.Update(rp, force)
}

func (o sessionObserver) Failed(output string, exit int, command, text string) {
	o.s.Fail(output, exit, command, text)
}

// cdFlag handles a leading "-C DIR" (or "-CDIR") on a subcommand.
func cdFlag(p program, args []string, stderr io.Writer) ([]string, bool) {
	if len(args) == 0 || !strings.HasPrefix(args[0], "-C") {
		return args, true
	}
	dir, rest := strings.TrimPrefix(args[0], "-C"), args[1:]
	if dir == "" {
		if len(rest) == 0 {
			fmt.Fprintf(stderr, "%s: option requires an argument -- 'C'\n", p.name)
			return nil, false
		}
		dir, rest = rest[0], rest[1:]
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintf(stderr, "%s: fatal: chdir to '%s' - %s\n", p.name, dir, strerror(err))
		return nil, false
	}
	return rest, true
}

// runBuildCmd is `kiln build [--detach] [ninja options] [targets]`.
func runBuildCmd(p program, args []string, stdout, stderr io.Writer) int {
	detached := false
	var rest []string
	for i, a := range args {
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if a == "--detach" {
			detached = true
			continue
		}
		rest = append(rest, a)
	}
	if !detached {
		return runNinja(p, rest, stdout, stderr)
	}
	return detach(p, rest, stdout, stderr)
}

// stripDir removes -C DIR from a command line, returning the directory.
func stripDir(args []string) (string, []string) {
	var out []string
	dir := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return dir, append(out, args[i:]...)
		case a == "-C" && i+1 < len(args):
			dir = args[i+1]
			i++
		case strings.HasPrefix(a, "-C") && len(a) > 2:
			dir = a[2:]
		default:
			out = append(out, a)
		}
	}
	return dir, out
}

// detach starts the build in a daemon: a new session, no terminal, its
// output in the run's output.log. It returns once the daemon holds the
// build lock and has begun (or has already finished), so a bad manifest or
// a busy tree is reported here, with the build's exit code.
func detach(p program, args []string, stdout, stderr io.Writer) int {
	o, e := parseArgs(p, args, io.Discard)
	if e != nil || o.dryRun || o.tool != "" {
		fmt.Fprintf(stderr, "%s: error: --detach needs a real build (not -n or -t)\n", p.name)
		return 1
	}
	dir, daemonArgs := stripDir(args)
	if dir != "" {
		if err := os.Chdir(dir); err != nil {
			fmt.Fprintf(stderr, "%s: fatal: chdir to '%s' - %s\n", p.name, dir, strerror(err))
			return 1
		}
	}
	if runstate.Locked() {
		_, err := runstate.Open("", nil, false) // to build the message naming the running build
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	id := runstate.NewID()
	rdir := runstate.RunDir(id)
	if err := os.MkdirAll(rdir, 0o777); err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	logf, err := os.Create(filepath.Join(rdir, "output.log"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	defer logf.Close()
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	cmd := exec.Command(self, append([]string{"__daemon"}, daemonArgs...)...)
	cmd.Env = append(os.Environ(), "KILN_RUN_ID="+id)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "%s: error: starting the build: %v\n", p.name, err)
		return 1
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	relay := func() {
		b, _ := os.ReadFile(filepath.Join(rdir, "output.log"))
		stderr.Write(b)
	}
	deadline := time.After(60 * time.Second)
	for {
		select {
		case err := <-exited:
			// The daemon is gone before it said it was running: an early
			// failure such as a bad manifest.
			relay()
			code := 1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
			return code
		case <-deadline:
			fmt.Fprintf(stderr, "%s: error: the build didn't start within a minute; see %s\n", p.name, filepath.Join(rdir, "output.log"))
			return 1
		case <-time.After(50 * time.Millisecond):
		}
		st, err := runstate.Load(id)
		if err != nil {
			continue
		}
		switch st.State {
		case runstate.Running:
			fmt.Fprintf(stdout, "build %s started in the background (pid %d)\n"+
				"  %s status | %s wait | %s log -f | %s stop\n", id, st.PID, p.name, p.name, p.name, p.name)
			return 0
		case runstate.Finished:
			// Done within the handshake: show how it went, with its exit code.
			relay()
			if st.ExitCode != nil {
				return *st.ExitCode
			}
			return 1
		}
	}
}

// runDaemon is the hidden `kiln __daemon ARGS`: the detached build itself.
func runDaemon(p program, args []string, stdout, stderr io.Writer) int {
	signalIgnoreHUP()
	return runNinja(p, args, stdout, stderr)
}

func signalIgnoreHUP() { signalIgnore(syscall.SIGHUP) }

// runStatus is `kiln status [-C DIR] [--json] [ID]`.
func runStatus(p program, args []string, stdout, stderr io.Writer) int {
	args, ok := cdFlag(p, args, stderr)
	if !ok {
		return 1
	}
	asJSON, id := false, ""
	for _, a := range args {
		switch {
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "-") || id != "":
			fmt.Fprintf(stderr, "usage: %s status [-C DIR] [--json] [RUN]\n", p.name)
			return 1
		default:
			id = a
		}
	}
	st, err := runstate.Load(id)
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	if asJSON {
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Fprintf(stdout, "%s\n", b)
		return 0
	}
	fmt.Fprint(stdout, describe(st))
	return 0
}

func describe(st runstate.Status) string {
	var b strings.Builder
	switch st.State {
	case runstate.Starting:
		fmt.Fprintf(&b, "run %s: starting (pid %d)\n", st.ID, st.PID)
	case runstate.Running:
		fmt.Fprintf(&b, "run %s: running (pid %d), %s so far\n", st.ID, st.PID, time.Since(st.Started).Round(time.Second))
		fmt.Fprintf(&b, "  %d of %d done, %d running\n", st.Done, st.Total, len(st.Jobs))
		if st.BudgetMiB > 0 {
			fmt.Fprintf(&b, "  memory: %.0f of %.0f MiB reserved\n", st.ReservMiB, st.BudgetMiB)
		}
		jobs := append([]runstate.Job(nil), st.Jobs...)
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].Seconds > jobs[j].Seconds })
		for _, j := range jobs[:min(len(jobs), 8)] {
			if j.RSSMiB > 0 {
				fmt.Fprintf(&b, "    %s (%.0fs, %.0f MiB)\n", j.Output, j.Seconds, j.RSSMiB)
			} else {
				fmt.Fprintf(&b, "    %s (%.0fs)\n", j.Output, j.Seconds)
			}
		}
	case runstate.Finished:
		code := -1
		if st.ExitCode != nil {
			code = *st.ExitCode
		}
		fmt.Fprintf(&b, "run %s: finished with exit code %d after %s\n", st.ID, code, st.Finished.Sub(st.Started).Round(time.Second))
		fmt.Fprintf(&b, "  %d of %d done\n", st.Done, st.Total)
	case runstate.Died:
		fmt.Fprintf(&b, "run %s: died; the build stopped without finishing (last seen %s ago, %d of %d done)\n",
			st.ID, time.Since(st.Updated).Round(time.Second), st.Done, st.Total)
	}
	for _, f := range st.Failures {
		fmt.Fprintf(&b, "  failed: %s (exit %d)\n", f.Output, f.Exit)
	}
	return b.String()
}

// runWait is `kiln wait [-C DIR] [--timeout DURATION]`: block until the
// build finishes, then exit with its exit code.
func runWait(p program, args []string, stdout, stderr io.Writer) int {
	args, ok := cdFlag(p, args, stderr)
	if !ok {
		return 1
	}
	var timeout time.Duration
	for i := 0; i < len(args); i++ {
		if args[i] != "--timeout" || i+1 == len(args) {
			fmt.Fprintf(stderr, "usage: %s wait [-C DIR] [--timeout DURATION]\n", p.name)
			return 1
		}
		d, err := time.ParseDuration(args[i+1])
		if err != nil {
			if n, e2 := strconv.Atoi(args[i+1]); e2 == nil {
				d, err = time.Duration(n)*time.Second, nil
			}
		}
		if err != nil || d < 0 {
			fmt.Fprintf(stderr, "%s: error: invalid timeout %q\n", p.name, args[i+1])
			return 1
		}
		timeout = d
		i++
	}
	if !runstate.WaitUnlocked(timeout) {
		fmt.Fprintf(stderr, "%s: the build is still running after %s\n", p.name, timeout)
		return 124
	}
	st, err := runstate.Latest()
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	switch {
	case st.State == runstate.Died:
		fmt.Fprintf(stderr, "%s: run %s died without finishing\n", p.name, st.ID)
		return 1
	case st.ExitCode != nil:
		return *st.ExitCode
	}
	return 1
}

// runLog is `kiln log [-C DIR] [-f] [--failures] [RUN]`.
func runLog(p program, args []string, stdout, stderr io.Writer) int {
	args, ok := cdFlag(p, args, stderr)
	if !ok {
		return 1
	}
	follow, failures, id := false, false, ""
	for _, a := range args {
		switch {
		case a == "-f":
			follow = true
		case a == "--failures":
			failures = true
		case strings.HasPrefix(a, "-") || id != "":
			fmt.Fprintf(stderr, "usage: %s log [-C DIR] [-f] [--failures] [RUN]\n", p.name)
			return 1
		default:
			id = a
		}
	}
	rdir := runstate.RunDir(id)
	if _, err := runstate.Load(id); err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	if failures {
		files, _ := filepath.Glob(filepath.Join(rdir, "failures", "*.log"))
		sort.Slice(files, func(i, j int) bool { return numLess(files[i], files[j]) })
		for _, f := range files {
			b, _ := os.ReadFile(f)
			stdout.Write(b)
		}
		return 0
	}
	logPath := filepath.Join(rdir, "output.log")
	f, err := os.Open(logPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: run %s has no output log (only detached builds keep one); try '%s log --failures'\n", p.name, filepath.Base(mustEval(rdir)), p.name)
		return 1
	}
	defer f.Close()
	io.Copy(stdout, f)
	for follow && runstate.Locked() {
		time.Sleep(200 * time.Millisecond)
		io.Copy(stdout, f)
	}
	io.Copy(stdout, f) // what was written between the last read and the end
	return 0
}

func mustEval(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func numLess(a, b string) bool {
	na, _ := strconv.Atoi(strings.TrimSuffix(filepath.Base(a), ".log"))
	nb, _ := strconv.Atoi(strings.TrimSuffix(filepath.Base(b), ".log"))
	return na < nb
}

// runStop is `kiln stop [-C DIR]`: ask the running build to stop, as
// Ctrl-C would.
func runStop(p program, args []string, stdout, stderr io.Writer) int {
	args, ok := cdFlag(p, args, stderr)
	if !ok || len(args) > 0 {
		if ok {
			fmt.Fprintf(stderr, "usage: %s stop [-C DIR]\n", p.name)
		}
		return 1
	}
	st, err := runstate.Latest()
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	if st.State != runstate.Running && st.State != runstate.Starting {
		fmt.Fprintf(stderr, "%s: no build is running here (the last run, %s, %s)\n", p.name, st.ID, st.State)
		return 1
	}
	if err := runstate.Stop(st); err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	if !runstate.WaitUnlocked(30 * time.Second) {
		fmt.Fprintf(stderr, "%s: asked run %s to stop, but it's still running after 30s\n", p.name, st.ID)
		return 1
	}
	fmt.Fprintf(stdout, "stopped run %s\n", st.ID)
	return 0
}
