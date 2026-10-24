// Command benchq runs the server benchmarks as a queue of steps, one at a
// time, and keeps them from bothering the machine's other users. It is
// the supervisor that bench/server/start.sh leaves running on the shared
// kiln-bench server (docs/benchmarks.md, "Server profile").
//
// The queue is ROOT/run/queue.txt, a step per line ('#' starts a comment):
//
//	NAME | WATCH-DIR | COMMAND
//
// COMMAND runs under sh -c in ROOT/kiln, its output appended to
// ROOT/results/logs/NAME.log. WATCH-DIR, under ROOT (or -), is the build
// tree whose .ninja_log shows progress. Steps run in order. Their states
// are kept in run/state.json, so a supervisor started again carries on
// where the last one stopped, rerunning a step it left unfinished (bench
// skips the runs already recorded). The queue is read again before each
// step, so steps can be added while it runs.
//
// A watchdog checks the machine every -tick:
//   - available memory: under -mem-floor the running step is stopped, and
//     no step starts until -mem-resume is available;
//   - the memory of benchq's own session, everything it started: over
//     -own-max the step is stopped and counts as failed;
//   - the CPUs left to other users (-reserved): when they stay busier than
//     -busy-hold for -busy-patience the step is stopped, and none starts
//     until they are under -busy-resume;
//   - free disk space under ROOT: under -disk-floor the step is stopped;
//   - the system's I/O pressure: no step starts while it is over -io-hold;
//   - progress: an alert when WATCH-DIR's .ninja_log stops growing for -stall.
//
// A stopped step records nothing (bench drops an interrupted run). It runs
// again once the machine has been quiet for -settle.
//
// Events go to run/events.log, a line each, "TIME LEVEL STEP MESSAGE".
// LEVEL is one of STARTED START DONE FAIL HALT PAUSE RESUME PREEMPT ALERT
// STOPPED FINISHED REFUSED. run/heartbeat holds the latest sample, and
// results/load.log gets one a minute. Two failed steps in a row halt the
// queue. The file run/STOP stops it, killing the running step; run/PAUSE
// holds it before the next step; run/retry/NAME queues a failed step again.
//
// Usage:
//
//	benchq -root ROOT run           the supervisor (see bench/server/start.sh)
//	benchq -root ROOT status
//	benchq -root ROOT pause|resume|stop
//	benchq -root ROOT retry STEP
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Ardx19/kiln/internal/proctree"
	"github.com/Ardx19/kiln/internal/sysinfo"
)

const gib = 1 << 30

// limits are the watchdog's thresholds.
type limits struct {
	memFloor, memResume, ownMax, diskFloor uint64  // bytes
	busyHold, busyResume, ioHold           float64 // percent
	busyPatience, stall, settle            time.Duration
	tick, grace                            time.Duration
}

// probe reads the machine. The real one reads /proc; tests fake it.
type probe interface {
	avail() uint64             // memory available before something gives
	own() uint64               // RSS of benchq's session
	cpu() (busy, total uint64) // cumulative jiffies of the reserved CPUs
	ioPressure() float64       // percent of the last minute some task waited on I/O
	diskFree() uint64          // bytes free under ROOT
	load() float64             // 1-minute load average
}

type step struct{ Name, Watch, Command string }

type stepState struct {
	Status   string  `json:"status"` // pending, running, done or failed
	Attempts int     `json:"attempts,omitempty"`
	Start    string  `json:"start,omitempty"`
	End      string  `json:"end,omitempty"`
	Exit     int     `json:"exit,omitempty"`
	WallS    float64 `json:"wall_s,omitempty"`
}

// sample is one reading of the machine, for run/heartbeat and load.log.
type sample struct {
	Time     string  `json:"time"`
	State    string  `json:"state"`
	Step     string  `json:"step,omitempty"`
	AvailGiB float64 `json:"avail_gib"`
	OwnGiB   float64 `json:"own_gib"`
	Busy1m   float64 `json:"reserved_busy_1m"`
	Busy5m   float64 `json:"reserved_busy_5m"`
	IO       float64 `json:"io_some_avg60"`
	DiskGiB  float64 `json:"disk_free_gib"`
	Load1    float64 `json:"load1"`
}

type cpuSample struct {
	at          time.Time
	busy, total uint64
}

// sup is the supervisor.
type sup struct {
	root   string
	lim    limits
	p      probe
	sid    int // benchq's session, whose strays are killed; 0 in tests
	events io.Writer

	states  map[string]*stepState
	cpuHist []cpuSample
	lastBad time.Time // when the gate last found the machine busy
	strict  bool      // after a busy stop, wait for -busy-resume rather than -busy-hold
	lastLog time.Time
	failed  int // failed steps in a row
}

func (s *sup) path(elem ...string) string { return filepath.Join(append([]string{s.root}, elem...)...) }

func (s *sup) event(level, name, msg string) {
	if name == "" {
		name = "-"
	}
	line := fmt.Sprintf("%s %s %s %s\n", time.Now().Format(time.RFC3339), level, name, strings.ReplaceAll(msg, "\n", " / "))
	if f, err := os.OpenFile(s.path("run", "events.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.WriteString(line)
		f.Close()
	}
	if s.events != nil {
		io.WriteString(s.events, line)
	}
}

// parseQueue reads queue.txt's steps.
func parseQueue(text string) ([]step, error) {
	var steps []step
	seen := map[string]bool{}
	for i, line := range strings.Split(text, "\n") {
		if j := strings.Index(line, "#"); j >= 0 {
			line = line[:j]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.SplitN(line, "|", 3)
		if len(f) != 3 {
			return nil, fmt.Errorf("queue line %d: want NAME | WATCH-DIR | COMMAND", i+1)
		}
		st := step{strings.TrimSpace(f[0]), strings.TrimSpace(f[1]), strings.TrimSpace(f[2])}
		if st.Name == "" || strings.ContainsAny(st.Name, " /") || st.Watch == "" || st.Command == "" {
			return nil, fmt.Errorf("queue line %d: a step needs a name without spaces or slashes, a watch dir (or -) and a command", i+1)
		}
		if seen[st.Name] {
			return nil, fmt.Errorf("queue line %d: step %s is listed twice", i+1, st.Name)
		}
		seen[st.Name] = true
		steps = append(steps, st)
	}
	return steps, nil
}

func (s *sup) readQueue() ([]step, error) {
	b, err := os.ReadFile(s.path("run", "queue.txt"))
	if err != nil {
		return nil, err
	}
	return parseQueue(string(b))
}

func (s *sup) loadState() error {
	s.states = map[string]*stepState{}
	b, err := os.ReadFile(s.path("run", "state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.states)
}

func (s *sup) saveState() error {
	b, _ := json.MarshalIndent(s.states, "", "  ")
	return writeAtomic(s.path("run", "state.json"), append(b, '\n'))
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *sup) state(name string) *stepState {
	st := s.states[name]
	if st == nil {
		st = &stepState{Status: "pending"}
		s.states[name] = st
	}
	return st
}

// takeRetries queues again the failed steps named in run/retry.
func (s *sup) takeRetries() {
	ents, _ := os.ReadDir(s.path("run", "retry"))
	for _, e := range ents {
		os.Remove(s.path("run", "retry", e.Name()))
		if st := s.states[e.Name()]; st != nil && st.Status == "failed" {
			st.Status = "pending"
			s.failed = 0
			s.event("RESUME", e.Name(), "queued again by retry")
		}
	}
}

func (s *sup) flag(name string) bool {
	_, err := os.Stat(s.path("run", name))
	return err == nil
}

// sample reads the machine and keeps the reserved CPUs' history.
func (s *sup) sample(now time.Time, state, name string) sample {
	busy, total := s.p.cpu()
	s.cpuHist = append(s.cpuHist, cpuSample{now, busy, total})
	// Keep a little over the longest window.
	for len(s.cpuHist) > 2 && now.Sub(s.cpuHist[1].at) > 5*time.Minute {
		s.cpuHist = s.cpuHist[1:]
	}
	return sample{
		Time: now.Format(time.RFC3339), State: state, Step: name,
		AvailGiB: float64(s.p.avail()) / gib, OwnGiB: float64(s.p.own()) / gib,
		Busy1m: s.busy(now, time.Minute), Busy5m: s.busy(now, 5*time.Minute),
		IO: s.p.ioPressure(), DiskGiB: float64(s.p.diskFree()) / gib, Load1: s.p.load(),
	}
}

// busy is the reserved CPUs' busy share over the last window, in percent,
// from the oldest sample inside it (all of the history while it's shorter).
func (s *sup) busy(now time.Time, window time.Duration) float64 {
	if len(s.cpuHist) < 2 {
		return 0
	}
	last := s.cpuHist[len(s.cpuHist)-1]
	first := last
	for _, c := range s.cpuHist {
		if now.Sub(c.at) <= window {
			first = c
			break
		}
	}
	if last.total <= first.total {
		return 0
	}
	return 100 * float64(last.busy-first.busy) / float64(last.total-first.total)
}

func (s *sup) record(m sample) {
	b, _ := json.Marshal(m)
	writeAtomic(s.path("run", "heartbeat"), append(b, '\n'))
	if t, _ := time.Parse(time.RFC3339, m.Time); t.Sub(s.lastLog) >= time.Minute {
		s.lastLog = t
		if f, err := os.OpenFile(s.path("results", "load.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
}

// blocked says why no step may start now: a short kind ("" when none)
// and a message.
func (s *sup) blocked(m sample) (string, string) {
	hold := s.lim.busyHold
	if s.strict {
		hold = s.lim.busyResume
	}
	switch {
	case s.flag("PAUSE"):
		return "paused", "paused by run/PAUSE"
	case m.AvailGiB*gib < float64(s.lim.memResume):
		return "memory", fmt.Sprintf("%.0f GiB of memory available, waiting for %d", m.AvailGiB, s.lim.memResume/gib)
	case m.Busy5m > hold:
		return "busy", fmt.Sprintf("the reserved CPUs are %.0f%% busy, waiting for under %.0f%%", m.Busy5m, hold)
	case m.DiskGiB*gib < float64(s.lim.diskFloor):
		return "disk", fmt.Sprintf("%.0f GiB of disk free, under the %d GiB floor", m.DiskGiB, s.lim.diskFloor/gib)
	case m.IO > s.lim.ioHold:
		return "io", fmt.Sprintf("I/O pressure %.0f%%, waiting for under %.0f%%", m.IO, s.lim.ioHold)
	}
	return "", ""
}

// wait holds until a step may start. It reports false on run/STOP.
func (s *sup) wait() bool {
	held := ""
	for {
		if s.flag("STOP") {
			return false
		}
		now := time.Now()
		m := s.sample(now, "waiting", "")
		kind, why := s.blocked(m)
		if kind != "" && kind != "paused" { // a pause by hand needs no settling
			s.lastBad = now
		} else if kind == "" && !s.lastBad.IsZero() && now.Sub(s.lastBad) < s.lim.settle {
			kind, why = "settling", fmt.Sprintf("the machine must stay quiet for %v", s.lim.settle)
		}
		if kind == "" {
			if held != "" {
				s.event("RESUME", "", "the machine is quiet again")
			}
			s.strict = false
			return true
		}
		m.State = "waiting: " + why
		s.record(m)
		if kind != held && kind != "settling" {
			s.event("PAUSE", "", why)
		}
		if held == "" || kind != "settling" {
			held = kind
		}
		time.Sleep(s.lim.tick)
	}
}

// outcome is how a step's run ended.
type outcome int

const (
	finished  outcome = iota // the command exited by itself
	preempted                // stopped for the machine's sake; it runs again
	stopped                  // stopped by run/STOP
	overgrown                // stopped for using too much memory itself
)

// runStep runs one step under the watchdog.
func (s *sup) runStep(st step) (outcome, int, time.Duration) {
	logf, err := os.OpenFile(s.path("results", "logs", st.Name+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		s.event("ALERT", st.Name, err.Error())
		return finished, -1, 0
	}
	defer logf.Close()
	fmt.Fprintf(logf, "\n=== benchq %s: %s\n", time.Now().Format(time.RFC3339), st.Command)
	cmd := exec.Command("/bin/sh", "-c", st.Command)
	cmd.Dir = s.path("kiln")
	// Tests fake benchq's memory readings; the step must see the real ones.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "KILN_TEST_MEMORY=") })
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		s.event("ALERT", st.Name, err.Error())
		return finished, -1, 0
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(s.lim.tick)
	defer tick.Stop()
	var busySince time.Time
	alerted := false
	how, why := finished, ""
	var werr error
loop:
	for {
		select {
		case werr = <-done:
			break loop
		case <-tick.C:
		}
		now := time.Now()
		m := s.sample(now, "running", st.Name)
		s.record(m)
		if m.Busy1m <= s.lim.busyHold {
			busySince = time.Time{}
		} else if busySince.IsZero() {
			busySince = now
		}
		switch {
		case s.flag("STOP"):
			how, why = stopped, "run/STOP"
		case m.AvailGiB*gib < float64(s.lim.memFloor):
			how, why = preempted, fmt.Sprintf("only %.0f GiB of memory available (floor %d)", m.AvailGiB, s.lim.memFloor/gib)
		case m.OwnGiB*gib > float64(s.lim.ownMax):
			how, why = overgrown, fmt.Sprintf("our processes use %.0f GiB (at most %d)", m.OwnGiB, s.lim.ownMax/gib)
		case !busySince.IsZero() && now.Sub(busySince) >= s.lim.busyPatience:
			how, why = preempted, fmt.Sprintf("the reserved CPUs stayed over %.0f%% busy for %v", s.lim.busyHold, s.lim.busyPatience)
			s.strict = true
		case m.DiskGiB*gib < float64(s.lim.diskFloor):
			how, why = preempted, fmt.Sprintf("%.0f GiB of disk free (floor %d)", m.DiskGiB, s.lim.diskFloor/gib)
		}
		if how != finished {
			s.terminate(cmd.Process.Pid, done)
			break loop
		}
		if !alerted && st.Watch != "-" && now.Sub(start) > s.lim.stall {
			fi, err := os.Stat(s.path(st.Watch, ".ninja_log"))
			if err != nil || now.Sub(maxTime(fi.ModTime(), start)) > s.lim.stall {
				s.event("ALERT", st.Name, fmt.Sprintf("no progress in %s/.ninja_log for %v", st.Watch, s.lim.stall))
				alerted = true
			}
		}
	}
	wall := time.Since(start)
	s.sweep(st.Name)
	switch how {
	case preempted:
		s.lastBad = time.Now()
		s.event("PREEMPT", st.Name, why+"; it runs again when the machine is quiet")
		return preempted, 0, wall
	case stopped:
		return stopped, 0, wall
	case overgrown:
		s.event("ALERT", st.Name, why)
		return overgrown, -1, wall
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		code = ee.ExitCode()
	} else if werr != nil {
		code = -1
	}
	return finished, code, wall
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// terminate stops a step: SIGTERM to its process group (bench then kills
// its run's tree and records nothing), and after -grace SIGKILL to
// whatever is left of its tree. What the step started in other process
// groups (ninja's jobs, timeout(1) and its child) can outlive it, so the
// tree seen when it was stopped is killed too; sweep catches the rest.
func (s *sup) terminate(pid int, done chan error) {
	tree := proctree.Descendants(pid)
	syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(s.lim.grace):
		proctree.Kill(pid)
		<-done
	}
	for _, p := range tree[1:] {
		proctree.Kill(p)
	}
}

// sweep kills anything a step left running in benchq's session.
func (s *sup) sweep(name string) {
	if s.sid == 0 {
		return
	}
	var strays []int
	for _, pid := range proctree.InSession(s.sid) {
		if pid != os.Getpid() {
			strays = append(strays, pid)
		}
	}
	if len(strays) == 0 {
		return
	}
	for _, pid := range strays {
		proctree.Kill(pid)
	}
	s.event("ALERT", name, fmt.Sprintf("killed %d processes the step left behind: %v", len(strays), strays))
}

// loop runs the queue and returns the process's exit code.
func (s *sup) loop() int {
	for name, st := range s.states {
		if st.Status == "running" {
			st.Status = "pending"
			s.event("RESUME", name, "left unfinished by an earlier supervisor; it runs again")
		}
	}
	s.saveState()
	for {
		s.takeRetries()
		steps, err := s.readQueue()
		if err != nil {
			s.event("HALT", "", "reading the queue: "+err.Error())
			return 1
		}
		var next *step
		for i := range steps {
			if s.state(steps[i].Name).Status == "pending" {
				next = &steps[i]
				break
			}
		}
		if next == nil {
			s.saveState()
			s.event("FINISHED", "", "no pending steps")
			return 0
		}
		if !s.wait() {
			s.event("STOPPED", "", "run/STOP")
			return 0
		}
		st := s.state(next.Name)
		st.Status, st.Attempts, st.Start, st.End = "running", st.Attempts+1, time.Now().Format(time.RFC3339), ""
		s.saveState()
		s.event("START", next.Name, fmt.Sprintf("attempt %d: %s", st.Attempts, next.Command))
		how, code, wall := s.runStep(*next)
		st.End, st.WallS = time.Now().Format(time.RFC3339), wall.Seconds()
		switch {
		case how == preempted:
			st.Status = "pending"
		case how == stopped:
			st.Status = "pending"
			s.saveState()
			s.event("STOPPED", next.Name, "run/STOP; the step runs again when the queue is started")
			return 0
		case how == finished && code == 0:
			st.Status, st.Exit = "done", 0
			s.failed = 0
			s.event("DONE", next.Name, fmt.Sprintf("in %.0f s; %s", wall.Seconds(), s.tail(next.Name, 1)))
		default:
			st.Status, st.Exit = "failed", code
			s.failed++
			s.event("FAIL", next.Name, fmt.Sprintf("exit %d after %.0f s; log tail: %s", code, wall.Seconds(), s.tail(next.Name, 6)))
		}
		s.saveState()
		if s.failed >= 2 {
			s.event("HALT", "", "two steps failed in a row")
			return 1
		}
	}
}

// tail returns the last n non-empty lines of a step's log.
func (s *sup) tail(name string, n int) string {
	b, _ := os.ReadFile(s.path("results", "logs", name+".log"))
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// realProbe reads /proc.
type realProbe struct {
	root     string
	reserved []int
}

func (realProbe) avail() uint64 { return sysinfo.ReadMemory().Headroom() }

func (realProbe) own() uint64 {
	sid, _ := session(os.Getpid())
	return proctree.RSS(proctree.InSession(sid))
}

func (p realProbe) cpu() (busy, total uint64) { return reservedCPU(readFile("/proc/stat"), p.reserved) }

func (realProbe) ioPressure() float64 {
	// "some avg10=0.00 avg60=1.23 avg300=0.50 total=…"
	for _, l := range strings.Split(readFile("/proc/pressure/io"), "\n") {
		if !strings.HasPrefix(l, "some ") {
			continue
		}
		for _, f := range strings.Fields(l) {
			if v, ok := strings.CutPrefix(f, "avg60="); ok {
				x, _ := strconv.ParseFloat(v, 64)
				return x
			}
		}
	}
	return 0
}

func (p realProbe) diskFree() uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p.root, &st); err != nil {
		return 0
	}
	return st.Bavail * uint64(st.Bsize)
}

func (realProbe) load() float64 {
	f := strings.Fields(readFile("/proc/loadavg"))
	if len(f) == 0 {
		return 0
	}
	x, _ := strconv.ParseFloat(f[0], 64)
	return x
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// reservedCPU sums the busy and total jiffies of cpus in /proc/stat.
func reservedCPU(stat string, cpus []int) (busy, total uint64) {
	want := map[string]bool{}
	for _, c := range cpus {
		want["cpu"+strconv.Itoa(c)] = true
	}
	for _, l := range strings.Split(stat, "\n") {
		f := strings.Fields(l)
		if len(f) < 9 || !want[f[0]] {
			continue
		}
		var v [8]uint64
		for i := range v {
			v[i], _ = strconv.ParseUint(f[i+1], 10, 64)
		}
		// user nice system idle iowait irq softirq steal
		b := v[0] + v[1] + v[2] + v[5] + v[6] + v[7]
		busy += b
		total += b + v[3] + v[4]
	}
	return busy, total
}

// parseCPUList reads a list like "0-11,16-27".
func parseCPUList(s string) ([]int, error) {
	var cpus []int
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(lo)
		b, err2 := a, error(nil)
		if isRange {
			b, err2 = strconv.Atoi(hi)
		}
		if err1 != nil || err2 != nil || b < a {
			return nil, fmt.Errorf("bad CPU list %q", s)
		}
		for c := a; c <= b; c++ {
			cpus = append(cpus, c)
		}
	}
	return cpus, nil
}

// session returns pid's session id and nice value, from /proc/PID/stat.
func session(pid int) (sid, nice int) {
	b := readFile("/proc/" + strconv.Itoa(pid) + "/stat")
	i := strings.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0
	}
	// After the command name, f[0] is field 3 (state): the session is
	// field 6 and nice field 19.
	f := strings.Fields(b[i+1:])
	if len(f) > 16 {
		sid, _ = strconv.Atoi(f[3])
		nice, _ = strconv.Atoi(f[16])
	}
	return sid, nice
}

// interlock refuses to supervise unless benchq runs the way start.sh
// starts it: on exactly the CPU mask, at the lowest priority, as the
// leader of its own session, with HOME and TMPDIR inside ROOT.
func interlock(root, mask string, minNice int) error {
	want, err := parseCPUList(mask)
	if err != nil {
		return err
	}
	allowed := ""
	for _, l := range strings.Split(readFile("/proc/self/status"), "\n") {
		if v, ok := strings.CutPrefix(l, "Cpus_allowed_list:"); ok {
			allowed = strings.TrimSpace(v)
		}
	}
	if have, err := parseCPUList(allowed); err != nil || !slices.Equal(have, want) {
		return fmt.Errorf("CPU affinity is %q, want %q (start it with taskset)", allowed, mask)
	}
	sid, nice := session(os.Getpid())
	if nice < minNice {
		return fmt.Errorf("nice is %d, want at least %d", nice, minNice)
	}
	if sid != os.Getpid() {
		return fmt.Errorf("not a session leader (session %d), so its processes can't be told apart; start it with setsid", sid)
	}
	for _, v := range []string{"HOME", "TMPDIR"} {
		if p := os.Getenv(v); !strings.HasPrefix(filepath.Clean(p)+"/", filepath.Clean(root)+"/") {
			return fmt.Errorf("%s is %q, outside %s (source activate.sh)", v, p, root)
		}
	}
	return nil
}

func status(root string, w io.Writer) error {
	s := &sup{root: root}
	if err := s.loadState(); err != nil {
		return err
	}
	if b, err := os.ReadFile(s.path("run", "heartbeat")); err == nil {
		fmt.Fprintf(w, "heartbeat: %s", b)
	}
	steps, err := s.readQueue()
	if err != nil {
		return err
	}
	for _, st := range steps {
		ss := s.state(st.Name)
		fmt.Fprintf(w, "%-8s %-22s attempts %d  %s  %.0f s\n", ss.Status, st.Name, ss.Attempts, ss.End, ss.WallS)
	}
	f, err := os.Open(s.path("run", "events.log"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var last []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if last = append(last, sc.Text()); len(last) > 12 {
			last = last[1:]
		}
	}
	fmt.Fprintln(w, "recent events:")
	for _, l := range last {
		fmt.Fprintln(w, " ", l)
	}
	return nil
}

func main() {
	root := flag.String("root", "", "the benchmark directory, holding run/, results/ and kiln/ (required)")
	mask := flag.String("mask", "0-11,16-27", "the CPU affinity benchq must run with")
	reserved := flag.String("reserved", "12-15,28-31", "the CPUs left to other users, watched for how busy they are")
	minNice := flag.Int("nice", 19, "the lowest niceness benchq may run at")
	memFloor := flag.Uint64("mem-floor", 64, "stop the running step when less than this many GiB of memory are available")
	memResume := flag.Uint64("mem-resume", 96, "start a step only when this many GiB are available")
	ownMax := flag.Uint64("own-max", 96, "stop a step whose processes use more than this many GiB")
	diskFloor := flag.Uint64("disk-floor", 200, "stop when less than this many GiB are free under -root")
	busyHold := flag.Float64("busy-hold", 85, "busy percent of the reserved CPUs that holds the queue")
	busyResume := flag.Float64("busy-resume", 50, "busy percent under which a queue stopped for busy CPUs resumes")
	busyPatience := flag.Duration("busy-patience", 15*time.Minute, "how long the reserved CPUs may stay over -busy-hold before the step is stopped")
	ioHold := flag.Float64("io-hold", 40, "I/O pressure (percent, avg60) that holds the next step")
	stall := flag.Duration("stall", 30*time.Minute, "alert when the step's .ninja_log hasn't grown for this long")
	settle := flag.Duration("settle", 10*time.Minute, "how long the machine must stay quiet after a busy spell")
	tick := flag.Duration("tick", 15*time.Second, "how often the watchdog samples")
	grace := flag.Duration("grace", time.Minute, "how long a stopped step gets to exit before it is killed")
	flag.Parse()
	if *root == "" || flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: benchq -root ROOT run|status|pause|resume|stop|retry STEP")
		os.Exit(2)
	}
	r, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchq:", err)
		os.Exit(2)
	}
	s := &sup{root: r}
	switch cmd := flag.Arg(0); cmd {
	case "status":
		err = status(r, os.Stdout)
	case "pause":
		err = os.WriteFile(s.path("run", "PAUSE"), nil, 0o644)
	case "resume":
		err = os.Remove(s.path("run", "PAUSE"))
	case "stop":
		err = os.WriteFile(s.path("run", "STOP"), nil, 0o644)
	case "retry":
		if flag.NArg() != 2 {
			err = errors.New("retry needs a step name")
			break
		}
		if err = os.MkdirAll(s.path("run", "retry"), 0o755); err == nil {
			err = os.WriteFile(s.path("run", "retry", filepath.Base(flag.Arg(1))), nil, 0o644)
		}
	case "run":
		os.Exit(supervise(s, *mask, *reserved, *minNice, limits{
			memFloor: *memFloor * gib, memResume: *memResume * gib, ownMax: *ownMax * gib, diskFloor: *diskFloor * gib,
			busyHold: *busyHold, busyResume: *busyResume, ioHold: *ioHold,
			busyPatience: *busyPatience, stall: *stall, settle: *settle, tick: *tick, grace: *grace,
		}))
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchq:", err)
		os.Exit(1)
	}
}

// supervise checks how benchq was started, takes the lock and runs the
// queue. It returns the exit code.
func supervise(s *sup, mask, reserved string, minNice int, lim limits) int {
	os.MkdirAll(s.path("results", "logs"), 0o755)
	os.MkdirAll(s.path("run"), 0o755)
	s.events = os.Stderr
	cpus, err := parseCPUList(reserved)
	if err == nil {
		err = interlock(s.root, mask, minNice)
	}
	if err != nil {
		s.event("REFUSED", "", err.Error())
		return 2
	}
	lock, err := os.OpenFile(s.path("run", "benchq.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err == nil {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchq: another supervisor holds run/benchq.lock")
		return 2
	}
	if err := s.loadState(); err != nil {
		s.event("REFUSED", "", "reading run/state.json: "+err.Error())
		return 2
	}
	s.lim, s.p, s.sid = lim, realProbe{root: s.root, reserved: cpus}, os.Getpid()
	os.Remove(s.path("run", "STOP"))
	s.event("STARTED", "", fmt.Sprintf("pid %d, CPUs %s, nice %d", os.Getpid(), mask, minNice))
	return s.loop()
}
