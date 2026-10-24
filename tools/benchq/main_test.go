package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeProbe is a machine whose readings the test sets.
type fakeProbe struct {
	mu                      sync.Mutex
	availB, ownB, diskB     uint64
	busyJ, totalJ, busyStep uint64 // jiffies; each cpu() call adds busyStep of 100
	io                      float64
}

func (p *fakeProbe) set(f func(p *fakeProbe)) { p.mu.Lock(); f(p); p.mu.Unlock() }
func (p *fakeProbe) avail() uint64            { p.mu.Lock(); defer p.mu.Unlock(); return p.availB }
func (p *fakeProbe) own() uint64              { p.mu.Lock(); defer p.mu.Unlock(); return p.ownB }
func (p *fakeProbe) diskFree() uint64         { p.mu.Lock(); defer p.mu.Unlock(); return p.diskB }
func (p *fakeProbe) ioPressure() float64      { p.mu.Lock(); defer p.mu.Unlock(); return p.io }
func (p *fakeProbe) load() float64            { return 1 }
func (p *fakeProbe) cpu() (uint64, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.busyJ += p.busyStep
	p.totalJ += 100
	return p.busyJ, p.totalJ
}

// newSup makes a ROOT with the queue and a quiet machine.
func newSup(t *testing.T, queue string) (*sup, *fakeProbe) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"run", "results/logs", "kiln"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "run", "queue.txt"), []byte(queue), 0o644)
	p := &fakeProbe{availB: 200 * gib, diskB: 1000 * gib, busyStep: 10}
	s := &sup{
		root: root, p: p,
		lim: limits{
			memFloor: 64 * gib, memResume: 96 * gib, ownMax: 96 * gib, diskFloor: 200 * gib,
			busyHold: 85, busyResume: 50, ioHold: 40,
			busyPatience: time.Hour, stall: time.Hour, settle: 0,
			tick: 20 * time.Millisecond, grace: 2 * time.Second,
		},
	}
	if err := s.loadState(); err != nil {
		t.Fatal(err)
	}
	return s, p
}

func (s *sup) eventLog(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(s.path("run", "events.log"))
	return string(b)
}

// levels lists the events' levels and steps, in order.
func levels(log string) []string {
	var ls []string
	for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
		if f := strings.Fields(l); len(f) >= 3 {
			ls = append(ls, f[1]+" "+f[2])
		}
	}
	return ls
}

func wantStatus(t *testing.T, s *sup, want map[string]string) {
	t.Helper()
	b, err := os.ReadFile(s.path("run", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]stepState
	json.Unmarshal(b, &got)
	for name, w := range want {
		if st, ok := got[name]; !ok && w == "pending" {
			continue // a step never reached has no state yet
		} else if st.Status != w {
			t.Errorf("step %s is %q, want %q", name, got[name].Status, w)
		}
	}
}

func TestQueueRunsStepsInOrder(t *testing.T) {
	s, _ := newSup(t, "# a comment\na | - | echo one\nb | - | echo two; exit 3\nc | - | echo three\n")
	if code := s.loop(); code != 0 {
		t.Fatalf("loop = %d, want 0", code)
	}
	got := strings.Join(levels(s.eventLog(t)), ",")
	want := "START a,DONE a,START b,FAIL b,START c,DONE c,FINISHED -"
	if got != want {
		t.Errorf("events %s, want %s", got, want)
	}
	wantStatus(t, s, map[string]string{"a": "done", "b": "failed", "c": "done"})
	if log := s.eventLog(t); !strings.Contains(log, "exit 3") || !strings.Contains(log, "two") {
		t.Errorf("the FAIL event lacks the exit code or the log's tail:\n%s", log)
	}
	// A second supervisor finds nothing to do, and a retry runs b again.
	os.MkdirAll(s.path("run", "retry"), 0o755)
	os.WriteFile(s.path("run", "retry", "b"), nil, 0o644)
	s.loadState()
	s.loop()
	if n := strings.Count(s.eventLog(t), "START b"); n != 2 {
		t.Errorf("b started %d times after a retry, want 2", n)
	}
}

func TestTwoFailuresHalt(t *testing.T) {
	s, _ := newSup(t, "a | - | exit 1\nb | - | exit 2\nc | - | true\n")
	if code := s.loop(); code != 1 {
		t.Fatalf("loop = %d, want 1 (halted)", code)
	}
	if log := s.eventLog(t); !strings.Contains(log, "HALT") || strings.Contains(log, "START c") {
		t.Errorf("want a halt before c:\n%s", log)
	}
	wantStatus(t, s, map[string]string{"c": "pending"})
}

func TestUnfinishedStepRunsAgain(t *testing.T) {
	s, _ := newSup(t, "a | - | true\n")
	os.WriteFile(s.path("run", "state.json"), []byte(`{"a": {"status": "running", "attempts": 1}}`), 0o644)
	s.loadState()
	s.loop()
	if log := s.eventLog(t); !strings.Contains(log, "RESUME a") || !strings.Contains(log, "DONE a") {
		t.Errorf("an unfinished step should run again:\n%s", log)
	}
	if st := s.states["a"]; st.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", st.Attempts)
	}
}

// treeStep is a command that leaves a background sleep in its own process
// group, as ninja does with its jobs, writes the sleep's pid to child,
// then waits. Once again exists it exits at once instead.
func treeStep(dir string) string {
	return "if [ -f " + dir + "/again ]; then exit 0; fi; touch " + dir + "/again; set -m; sleep 60 & echo $! >" + dir + "/child; sleep 60"
}

// waitFile waits for a file holding a pid. It is safe in a goroutine:
// on a timeout it reports an error and returns 0.
func waitFile(t *testing.T, path string) int {
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
			n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			return n
		}
		if time.Now().After(deadline) {
			t.Errorf("%s never appeared", path)
			return 0
		}
	}
}

// gone waits for pid to die and reports whether it did.
func gone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil || strings.Contains(string(b), ") Z ") {
			return true
		}
	}
	return false
}

// killChild kills a treeStep's background sleep when the test ends.
func killChild(t *testing.T, dir string) {
	t.Cleanup(func() {
		if b, err := os.ReadFile(filepath.Join(dir, "child")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

func TestLowMemoryStopsTheStepAndRunsItAgain(t *testing.T) {
	dir := t.TempDir()
	s, p := newSup(t, "a | - | "+treeStep(dir)+"\n")
	killChild(t, dir)
	go func() {
		child := waitFile(t, filepath.Join(dir, "child"))
		p.set(func(p *fakeProbe) { p.availB = 10 * gib })
		if !gone(child) {
			t.Errorf("the step's background sleep %d outlived the preempt", child)
		}
		p.set(func(p *fakeProbe) { p.availB = 200 * gib })
	}()
	if code := s.loop(); code != 0 {
		t.Fatalf("loop = %d, want 0", code)
	}
	got := strings.Join(levels(s.eventLog(t)), ",")
	if !strings.Contains(got, "START a,PREEMPT a") || !strings.HasSuffix(got, "START a,DONE a,FINISHED -") {
		t.Errorf("events %s, want a preempt and a second, successful run", got)
	}
	if st := s.states["a"]; st.Status != "done" || st.Attempts != 2 {
		t.Errorf("state %+v, want done in 2 attempts", st)
	}
}

func TestStopKillsTheTree(t *testing.T) {
	dir := t.TempDir()
	s, _ := newSup(t, "a | - | "+treeStep(dir)+"\nb | - | true\n")
	killChild(t, dir)
	childc := make(chan int, 1)
	go func() {
		childc <- waitFile(t, filepath.Join(dir, "child"))
		os.WriteFile(s.path("run", "STOP"), nil, 0o644)
	}()
	if code := s.loop(); code != 0 {
		t.Fatalf("loop = %d, want 0", code)
	}
	if child := <-childc; !gone(child) {
		t.Errorf("the step's background sleep %d outlived the stop", child)
	}
	if log := s.eventLog(t); !strings.Contains(log, "STOPPED a") || strings.Contains(log, "START b") {
		t.Errorf("want a stop during a, before b:\n%s", log)
	}
	wantStatus(t, s, map[string]string{"a": "pending", "b": "pending"})
}

func TestOwnMemoryFailsTheStep(t *testing.T) {
	dir := t.TempDir()
	s, p := newSup(t, "a | - | "+treeStep(dir)+"\n")
	killChild(t, dir)
	p.set(func(p *fakeProbe) { p.ownB = 100 * gib })
	s.loop()
	if log := s.eventLog(t); !strings.Contains(log, "ALERT a our processes use 100 GiB") || !strings.Contains(log, "FAIL a") {
		t.Errorf("want an alert and a failure:\n%s", log)
	}
}

func TestBusyReservedCPUsHoldAndStop(t *testing.T) {
	dir := t.TempDir()
	s, p := newSup(t, "a | - | "+treeStep(dir)+"\n")
	s.lim.busyPatience = 100 * time.Millisecond
	killChild(t, dir)
	go func() {
		waitFile(t, filepath.Join(dir, "child"))
		p.set(func(p *fakeProbe) { p.busyStep = 95 })
		for deadline := time.Now().Add(20 * time.Second); !strings.Contains(s.eventLogRaced(), "PAUSE"); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Error("the queue never paused for the busy CPUs")
				break
			}
		}
		p.set(func(p *fakeProbe) { p.busyStep = 10 })
	}()
	s.loop()
	log := s.eventLog(t)
	for _, want := range []string{"PREEMPT a the reserved CPUs stayed over 85% busy", "PAUSE - the reserved CPUs are", "RESUME -", "DONE a"} {
		if !strings.Contains(log, want) {
			t.Errorf("events lack %q:\n%s", want, log)
		}
	}
}

// eventLogRaced reads the event log from another goroutine than the loop.
func (s *sup) eventLogRaced() string {
	b, _ := os.ReadFile(filepath.Join(s.root, "run", "events.log"))
	return string(b)
}

func TestStallAlerts(t *testing.T) {
	s, _ := newSup(t, "a | tree | sleep 0.5\n")
	s.lim.stall = 100 * time.Millisecond
	os.MkdirAll(s.path("tree"), 0o755)
	s.loop()
	if log := s.eventLog(t); !strings.Contains(log, "ALERT a no progress in tree/.ninja_log") || !strings.Contains(log, "DONE a") {
		t.Errorf("want a stall alert, then the step done:\n%s", log)
	}
}

func TestGateHoldsForMemoryDiskAndPause(t *testing.T) {
	s, p := newSup(t, "")
	m := func() sample { return s.sample(time.Now(), "", "") }
	if kind, _ := s.blocked(m()); kind != "" {
		t.Errorf("a quiet machine is blocked by %s", kind)
	}
	p.set(func(p *fakeProbe) { p.availB = 90 * gib })
	if kind, _ := s.blocked(m()); kind != "memory" {
		t.Errorf("blocked by %q, want memory", kind)
	}
	p.set(func(p *fakeProbe) { p.availB, p.diskB = 200*gib, 100*gib })
	if kind, _ := s.blocked(m()); kind != "disk" {
		t.Errorf("blocked by %q, want disk", kind)
	}
	p.set(func(p *fakeProbe) { p.diskB, p.io = 1000*gib, 60 })
	if kind, _ := s.blocked(m()); kind != "io" {
		t.Errorf("blocked by %q, want io", kind)
	}
	p.set(func(p *fakeProbe) { p.io = 0 })
	os.WriteFile(s.path("run", "PAUSE"), nil, 0o644)
	if kind, _ := s.blocked(m()); kind != "paused" {
		t.Errorf("blocked by %q, want paused", kind)
	}
}

func TestBusyWindows(t *testing.T) {
	s := &sup{lim: limits{tick: time.Second}}
	t0 := time.Now()
	// 6 minutes of samples: idle for the first 3, then fully busy.
	for i := 0; i <= 360; i += 15 {
		busy := uint64(0)
		if i > 180 {
			busy = uint64(i-180) * 100
		}
		s.cpuHist = append(s.cpuHist, cpuSample{t0.Add(time.Duration(i) * time.Second), busy, uint64(i) * 100})
	}
	now := t0.Add(360 * time.Second)
	if b := s.busy(now, time.Minute); b < 99 {
		t.Errorf("1-minute busy %.0f%%, want 100", b)
	}
	if b := s.busy(now, 5*time.Minute); b < 55 || b > 65 {
		t.Errorf("5-minute busy %.0f%%, want about 60", b)
	}
}

func TestReservedCPU(t *testing.T) {
	stat := "cpu  1 2 3 4 5 6 7 8 0 0\ncpu0 10 0 10 80 0 0 0 0 0 0\ncpu12 50 0 0 40 10 0 0 0 0 0\ncpu28 0 0 0 100 0 0 0 0 0 0\nintr 1\n"
	busy, total := reservedCPU(stat, []int{12, 28})
	if busy != 50 || total != 200 {
		t.Errorf("reservedCPU = %d/%d, want 50/200", busy, total)
	}
}

func TestParse(t *testing.T) {
	if _, err := parseQueue("a | - | true\na | - | true\n"); err == nil {
		t.Error("a duplicate step parsed")
	}
	if _, err := parseQueue("a b | - | true\n"); err == nil {
		t.Error("a name with a space parsed")
	}
	if _, err := parseQueue("a | true\n"); err == nil {
		t.Error("a line without a watch dir parsed")
	}
	steps, err := parseQueue("w | duckdb/build | bench/heavy.sh duckdb-full server -cap 30m | tee x\n")
	if err != nil || len(steps) != 1 || steps[0].Command != "bench/heavy.sh duckdb-full server -cap 30m | tee x" {
		t.Errorf("parseQueue = %+v, %v; the command keeps its own |", steps, err)
	}
	cpus, err := parseCPUList("0-2,16-17")
	if err != nil || len(cpus) != 5 || cpus[3] != 16 {
		t.Errorf("parseCPUList = %v, %v", cpus, err)
	}
	if _, err := parseCPUList("3-1"); err == nil {
		t.Error("a backwards range parsed")
	}
}

func TestInterlockRefuses(t *testing.T) {
	// The test runs on every CPU, unpinned and not a session leader.
	err := interlock(t.TempDir(), "0-0", 19)
	if err == nil {
		t.Fatal("interlock accepted an unpinned test process")
	}
	if !strings.Contains(err.Error(), "affinity") && !strings.Contains(err.Error(), "nice") {
		t.Errorf("interlock = %v, want an affinity or nice refusal", err)
	}
}

func TestServerQueueParses(t *testing.T) {
	b, err := os.ReadFile("../../bench/server/queue.txt")
	if err != nil {
		t.Fatal(err)
	}
	steps, err := parseQueue(string(b))
	if err != nil || len(steps) != 11 {
		t.Fatalf("bench/server/queue.txt: %d steps, %v; want 11", len(steps), err)
	}
}
