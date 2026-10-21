package compat

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Runner is a Ninja-compatible executable under test.
type Runner struct {
	Name string
	Path string
}

// Record is what one runner did for one step.
type Record struct {
	Step    string   `json:"step"`
	DryExit int      `json:"dry_exit"`
	Dry     []string `json:"dry"` // commands a dry run (-n -v) would run
	Exit    int      `json:"exit"`
	Ran     []string `json:"ran"` // commands the build ran (-v), sorted
	// LogChanged lists outputs whose .ninja_log entry changed. It's only
	// informational: generators rewrite entries without running anything
	// (Meson's regeneration runs `ninja -t restat`).
	LogChanged []string `json:"log_changed"`
	LogVersion string   `json:"log_version"`
	// Leftover is what a dry run still wants right after the build. A
	// successful build must leave nothing, or the next step's results
	// depend on timing.
	Leftover []string `json:"leftover"`
	// Explain is `-n -d explain` output, captured only when there is
	// leftover work.
	Explain string `json:"explain,omitempty"`
	// Wall-clock times in Unix nanoseconds, to line up with mtimes.
	EditedAt   int64 `json:"edited_at"`
	BuildStart int64 `json:"build_start"`
	BuildEnd   int64 `json:"build_end"`
	// CheckDry and CheckLeftover are a second runner's dry runs, taken
	// right after Dry and Leftover on the same files. They must match.
	CheckDryExit  int      `json:"check_dry_exit,omitempty"`
	CheckDry      []string `json:"check_dry,omitempty"`
	CheckLeftover []string `json:"check_leftover,omitempty"`
	// ClockStepped is set if the wall clock jumped during this step. This
	// step's results, and every later step's, then depend on timing.
	ClockStepped bool `json:"clock_stepped,omitempty"`
}

// Comparable returns the number of leading steps that ran without a clock
// step in both runs.
func Comparable(a, b []Record) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i].ClockStepped || b[i].ClockStepped {
			return i
		}
	}
	return n
}

// editDelay separates an edit from the previous build. File timestamps
// come from a coarse kernel clock, so an edit made within a few
// milliseconds of a build could otherwise share its mtime.
const editDelay = 30 * time.Millisecond

// RunScript restores the snapshot and runs every step with r. If check
// is not nil, it also does a dry run with check before and after each
// build, on the same tree state as r's dry runs (see Record.CheckDry).
func RunScript(t *Tree, r Runner, check *Runner, steps []string, seed int64) ([]Record, error) {
	clock := watchClock()
	defer clock.Stop()
	if err := t.Restore(); err != nil {
		return nil, err
	}
	var recs []Record
	for i, step := range steps {
		// A step between the previous build and this edit matters as much
		// as one during the build: the edit's mtime can land in the past.
		stepsBefore := clock.Steps()
		time.Sleep(editDelay)
		rec := Record{Step: step, EditedAt: time.Now().UnixNano()}
		if err := t.Apply(step, rand.New(rand.NewSource(seed*1000+int64(i)))); err != nil {
			return nil, fmt.Errorf("step %d %s: %w", i, step, err)
		}
		var err error
		if rec.DryExit, rec.Dry, err = t.dryRun(r); err != nil {
			return nil, err
		}
		if check != nil {
			if rec.CheckDryExit, rec.CheckDry, err = t.dryRun(*check); err != nil {
				return nil, err
			}
		}
		_, before, err := t.LogEntries()
		if err != nil {
			return nil, err
		}
		args := []string{"-v"}
		if step == "inject_error" {
			args = append(args, "-k", "0") // keep going, so the set that runs doesn't depend on scheduling order
		}
		rec.BuildStart = time.Now().UnixNano()
		var out []byte
		if rec.Exit, out, err = t.exec(r, nil, args...); err != nil {
			return nil, err
		}
		rec.BuildEnd = time.Now().UnixNano()
		if rec.Ran, err = statusLines(out); err != nil {
			return nil, err
		}
		var after map[string]string
		if rec.LogVersion, after, err = t.LogEntries(); err != nil {
			return nil, err
		}
		rec.LogChanged = changed(before, after)
		if _, rec.Leftover, err = t.dryRun(r); err != nil {
			return nil, err
		}
		if check != nil {
			if _, rec.CheckLeftover, err = t.dryRun(*check); err != nil {
				return nil, err
			}
		}
		if len(rec.Leftover) > 0 {
			_, out, err := t.exec(r, nil, "-n", "-d", "explain")
			if err != nil {
				return nil, err
			}
			rec.Explain = string(out)
		}
		rec.ClockStepped = clock.Steps() != stepsBefore
		recs = append(recs, rec)
	}
	return recs, nil
}

// dryRun returns the commands `-n -v` would run, sorted.
func (t *Tree) dryRun(r Runner) (int, []string, error) {
	code, out, err := t.exec(r, nil, "-n", "-v")
	if err != nil {
		return 0, nil, err
	}
	cmds, err := statusLines(out)
	return code, cmds, err
}

// statusLines returns, sorted, the commands a -v run printed after the
// status prefix.
func statusLines(out []byte) ([]string, error) {
	var cmds []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), statusPrefix); ok {
			cmds = append(cmds, line)
		}
	}
	sort.Strings(cmds)
	return cmds, sc.Err()
}

func (t *Tree) build(r Runner, args ...string) (int, []byte, error) {
	return t.exec(r, nil, args...)
}

// statusPrefix replaces Ninja's "[n/m] " status so dry-run commands can be
// picked out of the output.
const statusPrefix = "@@kiln@@ "

// exec runs r in the build dir and returns its exit code. If started is
// non-nil it receives the process as soon as it starts.
func (t *Tree) exec(r Runner, started chan<- *exec.Cmd, args ...string) (int, []byte, error) {
	cmd := exec.Command(r.Path, append([]string{"-C", t.BuildDir}, args...)...)
	cmd.Env = append(append([]string(nil), t.env...), "NINJA_STATUS="+statusPrefix)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	if started != nil {
		started <- cmd
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return 0, nil, err
	}
	return cmd.ProcessState.ExitCode(), out.Bytes(), nil
}

// InterruptResult is the outcome of Interrupt.
type InterruptResult struct {
	Interrupted  bool     // SIGINT landed mid-build
	Leftover     []string // what a dry run wants after the resumed build; must be empty
	ClockStepped bool     // the wall clock jumped, so Leftover proves nothing
}

// Interrupt touches every source, starts a build, sends SIGINT once the
// first edge has been logged, then resumes.
func Interrupt(t *Tree, r Runner) (InterruptResult, error) {
	clock := watchClock()
	interrupted, leftover, err := interrupt(t, r)
	return InterruptResult{interrupted, leftover, clock.Stop()}, err
}

func interrupt(t *Tree, r Runner) (interrupted bool, leftover []string, err error) {
	if err := t.Restore(); err != nil {
		return false, nil, err
	}
	time.Sleep(editDelay)
	if err := t.Apply("mass_touch", rand.New(rand.NewSource(0))); err != nil {
		return false, nil, err
	}
	_, before, err := t.LogEntries()
	if err != nil {
		return false, nil, err
	}
	started := make(chan *exec.Cmd, 1)
	done := make(chan error, 1)
	go func() {
		_, _, err := t.exec(r, started)
		done <- err
	}()
	cmd := <-started
wait:
	for {
		select {
		case err := <-done:
			if err != nil {
				return false, nil, err
			}
			break wait // finished before the first edge was logged
		case <-time.After(5 * time.Millisecond):
		}
		_, now, err := t.LogEntries()
		if err != nil {
			return false, nil, err
		}
		if len(changed(before, now)) > 0 {
			interrupted = cmd.Process.Signal(syscall.SIGINT) == nil
			if err := <-done; err != nil {
				return false, nil, err
			}
			break wait
		}
	}
	if code, out, err := t.build(r); err != nil || code != 0 {
		return interrupted, nil, fmt.Errorf("resume build: exit %d, %v\n%s", code, err, tail(string(out), 2000))
	}
	_, leftover, err = t.dryRun(r)
	return interrupted, leftover, err
}

// Diff describes one disagreement between two runners.
type Diff struct {
	Index int
	Step  string
	Field string
	OnlyA []string
	OnlyB []string
}

func (d Diff) String() string {
	return fmt.Sprintf("step %d (%s): %s differs\n  only A: %s\n  only B: %s",
		d.Index, d.Step, d.Field, short(d.OnlyA), short(d.OnlyB))
}

// Compare returns every disagreement between two runs of one script.
func Compare(a, b []Record) []Diff {
	var diffs []Diff
	for i := range min(len(a), len(b)) {
		x, y := a[i], b[i]
		add := func(field string, xs, ys []string) {
			if onlyX, onlyY := setDiff(xs, ys), setDiff(ys, xs); len(onlyX)+len(onlyY) > 0 {
				diffs = append(diffs, Diff{i, x.Step, field, onlyX, onlyY})
			}
		}
		add("dry-run commands", x.Dry, y.Dry)
		add("commands run", x.Ran, y.Ran)
		add("leftover work", x.Leftover, y.Leftover)
		if x.Exit != y.Exit || x.DryExit != y.DryExit {
			add("exit codes", []string{fmt.Sprint(x.DryExit, x.Exit)}, []string{fmt.Sprint(y.DryExit, y.Exit)})
		}
		if x.LogVersion != y.LogVersion {
			add("log version", []string{x.LogVersion}, []string{y.LogVersion})
		}
	}
	if len(a) != len(b) {
		diffs = append(diffs, Diff{Field: fmt.Sprintf("step count (%d vs %d)", len(a), len(b))})
	}
	return diffs
}

func setDiff(xs, ys []string) []string {
	var out []string
	for _, x := range xs {
		if !slices.Contains(ys, x) {
			out = append(out, x)
		}
	}
	return out
}

func short(xs []string) string {
	if len(xs) > 5 {
		return fmt.Sprintf("%q … (%d more)", xs[:5], len(xs)-5)
	}
	return fmt.Sprintf("%q", xs)
}
