//go:build compat

package compat

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// memEnv is a mem-test tree: a hand-written manifest of memhog jobs.
type memEnv struct {
	t      *testing.T
	dir    string
	kiln   string
	ninja  Tool
	memhog string
	rule   string // the manifest's rule name; kiln classifies jobs by it
}

func newMemEnv(t *testing.T, name string) *memEnv {
	t.Helper()
	repo, _, ninjas, _ := matrix(t)
	kiln, err := KilnBinary(repo)
	if err != nil {
		t.Fatal(err)
	}
	memhog := filepath.Join(WorkDir(repo), "bin", "memhog")
	if out, err := exec.Command("go", "build", "-o", memhog, "github.com/Ardx19/kiln/tools/memhog").CombinedOutput(); err != nil {
		t.Fatalf("building memhog: %v\n%s", err, out)
	}
	dir := filepath.Join(WorkDir(repo), "mem", name)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &memEnv{t: t, dir: dir, kiln: kiln, ninja: ninjas[len(ninjas)-1], memhog: memhog, rule: "hog"}
}

// manifest writes build.ninja: one rule running cmd, and an edge per name.
func (e *memEnv) manifest(cmd string, names ...string) {
	e.t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "rule %s\n  command = %s\n", e.rule, strings.ReplaceAll(cmd, "MEMHOG", e.memhog))
	for _, n := range names {
		fmt.Fprintf(&b, "build %s: %s\n", n, e.rule)
	}
	fmt.Fprintf(&b, "build all: phony %s\ndefault all\n", strings.Join(names, " "))
	if err := os.WriteFile(filepath.Join(e.dir, "build.ninja"), []byte(b.String()), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// env has the test's Ninja first on PATH, so kiln writes that Ninja's log
// version, plus extra variables.
func (e *memEnv) env(extra ...string) []string {
	env := withoutWindowsPath(os.Environ())
	for i, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			env[i] = "PATH=" + filepath.Dir(e.ninja.Path) + string(filepath.ListSeparator) + v
		}
	}
	return append(env, extra...)
}

func (e *memEnv) kilnCmd(args []string, extra ...string) *exec.Cmd {
	cmd := exec.Command(e.kiln, append([]string{"-C", e.dir}, args...)...)
	cmd.Env = e.env(append([]string{"KILN_CONFIG=" + filepath.Join(e.dir, "no-config.toml")}, extra...)...)
	return cmd
}

func (e *memEnv) runKiln(args []string, extra ...string) (string, int) {
	e.t.Helper()
	out, err := e.kilnCmd(args, extra...).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return string(out), code
}

// clean removes build products so the next run rebuilds everything.
func (e *memEnv) clean() {
	e.t.Helper()
	entries, _ := os.ReadDir(e.dir)
	for _, f := range entries {
		if n := f.Name(); n != "build.ninja" && n != ".kiln" && !strings.HasSuffix(n, ".toml") {
			os.RemoveAll(filepath.Join(e.dir, n))
		}
	}
}

// requireClean checks the test's Ninja finds nothing left to do.
func (e *memEnv) requireClean() {
	e.t.Helper()
	out, err := exec.Command(e.ninja.Path, "-C", e.dir, "-n").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "no work to do") {
		e.t.Errorf("ninja %s still has work after kiln's build: %v\n%s", e.ninja.Version, err, tail(string(out), 600))
	}
}

// maxOverlap is the most jobs that ran at once, from .ninja_log.
func (e *memEnv) maxOverlap() int {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, ".ninja_log"))
	if err != nil {
		e.t.Fatal(err)
	}
	type ev struct {
		ms, d int
	}
	var evs []ev
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, "\t")
		if len(f) != 5 {
			continue
		}
		s, _ := strconv.Atoi(f[0])
		t, _ := strconv.Atoi(f[1])
		evs = append(evs, ev{s, 1}, ev{t, -1})
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].ms < evs[j].ms || evs[i].ms == evs[j].ms && evs[i].d < evs[j].d })
	cur, best := 0, 0
	for _, v := range evs {
		cur += v.d
		best = max(best, cur)
	}
	return best
}

func (e *memEnv) runs() []string {
	b, _ := os.ReadFile(filepath.Join(e.dir, "runs.txt"))
	return strings.Fields(string(b))
}

// TestMemBudget runs eight 200 MiB jobs under a 600 MiB budget: kiln
// keeps few running at once, learns each job's peak, and a run with
// --mem=off shows what Ninja would do.
func TestMemBudget(t *testing.T) {
	e := newMemEnv(t, "budget")
	var names []string
	for i := range 8 {
		names = append(names, fmt.Sprintf("o%d", i))
	}
	e.manifest("echo $out >> runs.txt && MEMHOG -mib 200 -secs 1 -hold 0.5 -o $out", names...)

	for _, run := range []string{"first", "learned"} {
		e.clean()
		if out, code := e.runKiln([]string{"-j8", "--mem-budget", "600M"}); code != 0 {
			t.Fatalf("%s run: exit %d\n%s", run, code, tail(out, 800))
		}
		if got := e.maxOverlap(); got > 3 {
			t.Errorf("%s run: %d jobs ran at once under a 600 MiB budget for 200 MiB jobs", run, got)
		}
		e.requireClean()
	}
	out, _ := e.runKiln([]string{"mem", "top", "3"})
	if !strings.Contains(out, "ESTIMATE") || !strings.Contains(out, "o") {
		t.Errorf("kiln mem top shows nothing:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, ".kiln", "mem.log")); !bytes.Contains(b, []byte("# kiln mem log v1")) {
		t.Errorf("no memory history was written")
	}
	rec := parseMemLog(t, filepath.Join(e.dir, ".kiln", "mem.log"))["o3"]
	if rec < 150<<10 || rec > 320<<10 {
		t.Errorf("recorded estimate for a 200 MiB job: %d KiB", rec)
	}

	e.clean()
	if out, code := e.runKiln([]string{"-j8", "--mem=off"}); code != 0 {
		t.Fatalf("--mem=off run: exit %d\n%s", code, tail(out, 800))
	}
	if got := e.maxOverlap(); got < 5 {
		t.Errorf("with --mem=off only %d jobs ran at once; the comparison is meaningless", got)
	}
	e.requireClean()
}

// parseMemLog maps outputs to their estimate in KiB.
func parseMemLog(t *testing.T, path string) map[string]uint64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]uint64{}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, "\t")
		if len(f) == 5 {
			n, _ := strconv.ParseUint(f[0], 10, 64)
			m[f[4]] = n
		}
	}
	return m
}

// syncBuffer collects a command's output while a test watches it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestMemBrake drops the (faked) available memory mid-build: kiln must
// stop the youngest job, run it again once memory comes back, and finish.
func TestMemBrake(t *testing.T) {
	e := newMemEnv(t, "brake")
	e.manifest("echo $out >> runs.txt && MEMHOG -mib 40 -secs 6 -hold 0 -o $out", "a", "b", "c")
	memFile := filepath.Join(e.dir, "memory.txt")
	setAvail := func(bytes uint64) {
		if err := os.WriteFile(memFile, []byte(fmt.Sprintf("available=%d\n", bytes)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setAvail(10 << 30)
	cmd := e.kilnCmd([]string{"-j3", "--mem-budget", "4G", "-d", "sched"}, "KILN_TEST_MEMORY="+memFile)
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Wait for all three jobs to be running, then squeeze memory.
	deadline := time.Now().Add(30 * time.Second)
	for len(e.runs()) < 3 {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("jobs never started:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	setAvail(20 << 20)
	for !strings.Contains(out.String(), "stopped") {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("no job was stopped under memory pressure:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	setAvail(10 << 30)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("build failed after the brake: %v\n%s", err, tail(out.String(), 1500))
		}
	case <-time.After(90 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("build never finished:\n%s", tail(out.String(), 1500))
	}
	if runs := e.runs(); len(runs) < 4 {
		t.Errorf("expected a stopped job to run again (at least 4 runs), got %v", runs)
	}
	for _, o := range []string{"a", "b", "c"} {
		if _, err := os.Stat(filepath.Join(e.dir, o)); err != nil {
			t.Errorf("output %s missing: %v", o, err)
		}
	}
	if !strings.Contains(out.String(), "brake on") {
		t.Errorf("-d sched didn't report the brake:\n%s", tail(out.String(), 1500))
	}
	e.requireClean()
}

// TestMemOOMRetry has a job killed by SIGKILL on its first run, as the
// kernel's OOM killer would: kiln runs it again once, and a job that
// simply fails is not run again.
func TestMemOOMRetry(t *testing.T) {
	e := newMemEnv(t, "oom")
	e.manifest("echo $out >> runs.txt; if [ $out = victim ] && [ ! -e marker ]; then : > marker; kill -9 $$$$; fi; touch $out", "victim", "bystander")
	out, code := e.runKiln([]string{"-j2"})
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, tail(out, 1000))
	}
	if !strings.Contains(out, "victim was killed") {
		t.Errorf("no message about the killed job:\n%s", out)
	}
	count := map[string]int{}
	for _, r := range e.runs() {
		count[r]++
	}
	if count["victim"] != 2 || count["bystander"] != 1 {
		t.Errorf("runs = %v, want the victim twice and the bystander once", count)
	}
	e.requireClean()

	// A real failure isn't retried.
	e2 := newMemEnv(t, "oom-fail")
	e2.manifest("echo $out >> runs.txt; exit 1", "bad")
	out, code = e2.runKiln([]string{"-j1"})
	if code == 0 || len(e2.runs()) != 1 {
		t.Errorf("a failing job: exit %d, %d runs\n%s", code, len(e2.runs()), out)
	}

	// A job killed twice fails the build: the retry is one-shot.
	e3 := newMemEnv(t, "oom-twice")
	e3.manifest("echo $out >> runs.txt; kill -9 $$$$", "doomed")
	out, code = e3.runKiln([]string{"-j1"})
	if code == 0 || len(e3.runs()) != 2 {
		t.Errorf("a job killed every time: exit %d, %d runs, want a failure after 2\n%s", code, len(e3.runs()), tail(out, 800))
	}
}

// TestMemContainer is run inside `docker run --memory=2g` (make
// mem-container): eight 400 MiB "compiles" need 3.2 GiB at once. With no
// flags kiln finds the container's limit by itself and finishes; Ninja at
// -j8 is OOM-killed.
func TestMemContainer(t *testing.T) {
	if os.Getenv("KILN_MEM_CONTAINER") == "" {
		t.Skip("run by make mem-container, inside a memory-limited container")
	}
	e := newMemEnv(t, "container")
	e.rule = "CXX_COMPILER__memhog" // classified as a compile: 768 MiB until learned
	var names []string
	for i := range 8 {
		names = append(names, fmt.Sprintf("o%d.o", i))
	}
	e.manifest("echo $out >> runs.txt && MEMHOG -mib 400 -secs 3 -hold 1 -o $out", names...)

	cmd := exec.Command(e.ninja.Path, "-C", e.dir, "-j8")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("ninja -j8 survived 3.2 GiB of jobs in a 2 GiB container; the test shows nothing:\n%s", tail(string(out), 400))
	}
	e.clean()
	os.RemoveAll(filepath.Join(e.dir, ".kiln"))

	for _, run := range []string{"first", "learned"} {
		e.clean()
		out, code := e.runKiln([]string{"-j8"})
		if code != 0 {
			t.Fatalf("%s kiln run in a 2 GiB container: exit %d\n%s", run, code, tail(out, 1500))
		}
		if strings.Contains(out, "Killed") {
			t.Logf("%s run: the kernel killed something kiln had to retry:\n%s", run, tail(out, 600))
		}
		t.Logf("%s run: %d jobs at once", run, e.maxOverlap())
	}
	e.requireClean()
}
