package build

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/Ardx19/kiln/internal/graph"
)

// result is a finished command.
type result struct {
	edge   *graph.Edge
	output string
	// exit is the command's exit code, or -1 if a signal ended it.
	exit   int
	signal syscall.Signal
	err    error // the command couldn't be started
	// maxRSS is the largest resident set of the command or any process it
	// waited for (wait4's ru_maxrss), in bytes.
	maxRSS uint64
}

func (r *result) success() bool { return r.err == nil && r.exit == 0 }

// interrupted reports whether the command died of SIGINT, SIGTERM or
// SIGHUP, which Ninja treats as the build being interrupted.
func (r *result) interrupted() bool {
	return r.signal == syscall.SIGINT || r.signal == syscall.SIGTERM || r.signal == syscall.SIGHUP
}

// status is the command's failure status as Ninja 1.13 reports it: the
// exit code, 130 for an interrupting signal, or 128 plus the signal.
func (r *result) status() int {
	switch {
	case r.err != nil:
		return 1
	case r.signal == 0:
		return r.exit
	case r.interrupted():
		return 130
	}
	return 128 + int(r.signal)
}

// runner runs commands like Ninja's SubprocessSet: /bin/sh -c in a new
// process group with stdin from /dev/null and stdout and stderr captured
// together, except console-pool commands, which share kiln's terminal and
// process group.
type runner struct {
	parallelism int     // 0 means no limit
	maxLoad     float64 // -l; 0 means no limit
	running     map[*graph.Edge]*exec.Cmd
	done        chan *result
	signals     chan os.Signal
	caught      os.Signal // the signal that interrupted the build, if any
	// wake, if set, ends a wait early so the builder can look at memory.
	wake chan struct{}
	// env is the environment for commands; nil inherits.
	env []string
}

func newRunner(parallelism int, maxLoad float64, ignoreHUP bool) *runner {
	r := &runner{
		parallelism: parallelism,
		maxLoad:     maxLoad,
		running:     map[*graph.Edge]*exec.Cmd{},
		done:        make(chan *result),
		signals:     make(chan os.Signal, 1),
	}
	if ignoreHUP {
		// A detached build must outlive its terminal.
		signal.Notify(r.signals, syscall.SIGINT, syscall.SIGTERM)
	} else {
		signal.Notify(r.signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	}
	return r
}

// close stops catching signals.
func (r *runner) close() { signal.Stop(r.signals) }

// canRunMore is how many more commands may start, as Ninja 1.13
// computes it: -j minus those running, capped by -l's headroom over the
// load average, but always at least one when nothing runs.
func (r *runner) canRunMore() int {
	capacity := 1 << 30
	if r.parallelism != 0 {
		capacity = r.parallelism - len(r.running)
	}
	if r.maxLoad > 0 {
		if load := int(r.maxLoad - loadAverage()); load < capacity {
			capacity = load
		}
	}
	if capacity <= 0 {
		capacity = 0
		if len(r.running) == 0 {
			capacity = 1 // make progress
		}
	}
	return capacity
}

// loadAverage is the one-minute load average, as getloadavg(3) reports.
func loadAverage() float64 {
	var info unix.Sysinfo_t
	if unix.Sysinfo(&info) != nil {
		return 0
	}
	return float64(info.Loads[0]) / (1 << unix.SI_LOAD_SHIFT)
}

// start runs command for e. It returns the command's process group, or 0
// for a console command, which shares kiln's.
func (r *runner) start(e *graph.Edge, command string) (int, error) {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = r.env
	var out bytes.Buffer
	if e.Pool == graph.ConsolePool {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	} else {
		cmd.Stdout, cmd.Stderr = &out, &out
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	r.running[e] = cmd
	go func() {
		err := cmd.Wait()
		res := &result{edge: e, output: out.String()}
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			res.maxRSS = uint64(ru.Maxrss) * 1024
		}
		var exitErr *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exitErr):
			ws := exitErr.Sys().(syscall.WaitStatus)
			if ws.Signaled() {
				res.exit, res.signal = -1, ws.Signal()
			} else {
				res.exit = ws.ExitStatus()
			}
		default:
			res.err = err
		}
		r.done <- res
	}()
	if e.Pool == graph.ConsolePool {
		return 0, nil
	}
	return cmd.Process.Pid, nil
}

// wait returns the next finished command, or nil if a signal arrived
// first (see caught) or the runner was woken (woke is then true).
func (r *runner) wait() (res *result, woke bool) {
	select {
	case res := <-r.done:
		delete(r.running, res.edge)
		return res, false
	case sig := <-r.signals:
		r.caught = sig
		return nil, false
	case <-r.wake:
		return nil, true
	}
}

// kill stops one running command's process group with SIGKILL.
func (r *runner) kill(e *graph.Edge) {
	if cmd := r.running[e]; cmd != nil && e.Pool != graph.ConsolePool {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// activeEdges lists edges whose commands are still running.
func (r *runner) activeEdges() []*graph.Edge {
	es := make([]*graph.Edge, 0, len(r.running))
	for e := range r.running {
		es = append(es, e)
	}
	return es
}

// abort signals every running command's process group and waits for
// them all. Console commands got any terminal signal directly, so they
// are only waited for.
func (r *runner) abort() {
	sig := syscall.SIGINT
	if s, ok := r.caught.(syscall.Signal); ok {
		sig = s
	}
	for e, cmd := range r.running {
		if e.Pool != graph.ConsolePool {
			syscall.Kill(-cmd.Process.Pid, sig)
		}
	}
	for len(r.running) > 0 {
		res := <-r.done
		delete(r.running, res.edge)
	}
}
