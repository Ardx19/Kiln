// Package run records builds: a lock that says whether one is running in a
// build directory, and a run directory per build with a status file that
// other kiln commands (status, wait, log, stop) read.
//
//	.kiln/lock                       flock held by the running build
//	.kiln/runs/<id>/status.json      state, progress, failures; rewritten atomically
//	.kiln/runs/<id>/output.log       what a detached build printed
//	.kiln/runs/<id>/failures/N.log   one file per failed command
//	.kiln/runs/latest                symlink to the newest run
//
// Liveness is the lock: flock is released when the process dies, so a
// lock can't go stale. A status file that says "running" while the lock is
// free means the build died.
package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Dir is where kiln keeps its per-tree state, relative to the build dir.
const Dir = ".kiln"

// keepRuns is how many run directories are kept.
const keepRuns = 20

// State of a run.
const (
	Starting = "starting" // the manifest is loading
	Running  = "running"
	Finished = "finished"
	Died     = "died" // reported by Load, never written: running, but nobody holds the lock
)

// Job is a command that's running.
type Job struct {
	Output  string  `json:"output"`
	RSSMiB  float64 `json:"rss_mib,omitempty"`
	Seconds float64 `json:"seconds"`
}

// Failure is a command that failed.
type Failure struct {
	Output string `json:"output"`
	Exit   int    `json:"exit"`
	Log    string `json:"log"` // file under failures/
}

// Status is status.json.
type Status struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	PID       int       `json:"pid"`
	PIDStart  uint64    `json:"pid_start"` // /proc/<pid>/stat starttime, so stop can't hit a reused pid
	Dir       string    `json:"dir"`
	Targets   []string  `json:"targets,omitempty"`
	Started   time.Time `json:"started"`
	Updated   time.Time `json:"updated"`
	Finished  time.Time `json:"finished,omitzero"`
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	Jobs      []Job     `json:"jobs,omitempty"`
	BudgetMiB float64   `json:"budget_mib,omitempty"`
	ReservMiB float64   `json:"reserved_mib,omitempty"`
	Failures  []Failure `json:"failures,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"` // set when finished
	Message   string    `json:"message,omitempty"`
	Detached  bool      `json:"detached,omitempty"`
	LogPath   string    `json:"log,omitempty"` // output.log, if the build wrote one
}

// ErrBusy is returned when another build holds the lock; it names that build.
type ErrBusy struct {
	ID  string // may be ""
	PID int
}

func (e *ErrBusy) Error() string {
	if e.ID != "" {
		return fmt.Sprintf("another build is running in this directory (run %s, pid %d); use 'kiln wait' or 'kiln status'", e.ID, e.PID)
	}
	return "another build is running in this directory"
}

// Session is one build's run directory, with the lock held.
type Session struct {
	dir    string // .kiln/runs/<id>
	lock   *os.File
	st     Status
	last   time.Time
	nfail  int
	closed bool
}

// NewID makes a run id that sorts by time.
func NewID() string {
	return time.Now().Format("20060102-150405") + "-" + strconv.Itoa(os.Getpid())
}

// Open takes the lock in the current directory's .kiln and creates run id
// (a new id if ""). It fails with *ErrBusy if a build is running.
func Open(id string, targets []string, detached bool) (*Session, error) {
	if err := os.MkdirAll(filepath.Join(Dir, "runs"), 0o777); err != nil {
		return nil, err
	}
	// Keep kiln's directory out of `git status` in an in-source build.
	if _, err := os.Stat(filepath.Join(Dir, ".gitignore")); err != nil {
		os.WriteFile(filepath.Join(Dir, ".gitignore"), []byte("*\n"), 0o644)
	}
	lock, err := os.OpenFile(filepath.Join(Dir, "lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		busy := &ErrBusy{}
		if st, err := Latest(); err == nil && st.State != Finished {
			busy.ID, busy.PID = st.ID, st.PID
		}
		return nil, busy
	}
	if id == "" {
		id = NewID()
	}
	dir := filepath.Join(Dir, "runs", id)
	if err := os.MkdirAll(filepath.Join(dir, "failures"), 0o777); err != nil {
		lock.Close()
		return nil, err
	}
	abs, _ := filepath.Abs(".")
	start, _ := ProcStart(os.Getpid())
	s := &Session{dir: dir, lock: lock, st: Status{
		ID: id, State: Starting, PID: os.Getpid(), PIDStart: start, Dir: abs, Targets: targets,
		Started: time.Now(), Detached: detached,
	}}
	if _, err := os.Stat(filepath.Join(dir, "output.log")); err == nil {
		s.st.LogPath = filepath.Join(dir, "output.log")
	}
	latest := filepath.Join(Dir, "runs", "latest")
	os.Remove(latest)
	os.Symlink(id, latest)
	prune()
	return s, s.write()
}

// ID is the run's id.
func (s *Session) ID() string { return s.st.ID }

// Dir is the run directory.
func (s *Session) Dir() string { return s.dir }

// Progress is what a build reports as it goes.
type Progress struct {
	Total, Done int
	Jobs        []Job
	BudgetMiB   float64
	ReservMiB   float64
}

// minInterval is how often status.json is rewritten at most (unless a
// write is forced).
const minInterval = 400 * time.Millisecond

// Update records progress, rewriting status.json at most every
// minInterval unless force is set.
func (s *Session) Update(p Progress, force bool) {
	if s.closed {
		return
	}
	now := time.Now()
	if !force && now.Sub(s.last) < minInterval && s.st.State == Running {
		return
	}
	s.st.State = Running
	s.st.Total, s.st.Done, s.st.Jobs = p.Total, p.Done, p.Jobs
	s.st.BudgetMiB, s.st.ReservMiB = p.BudgetMiB, p.ReservMiB
	s.write()
}

// Fail records a failed command: its full output goes in failures/N.log.
func (s *Session) Fail(output string, exit int, command, text string) {
	if s.closed {
		return
	}
	s.nfail++
	name := fmt.Sprintf("%d.log", s.nfail)
	body := fmt.Sprintf("FAILED: [exit %d] %s\n%s\n%s", exit, output, command, text)
	os.WriteFile(filepath.Join(s.dir, "failures", name), []byte(body), 0o644)
	s.st.Failures = append(s.st.Failures, Failure{Output: output, Exit: exit, Log: name})
	s.write()
}

// Finish records the exit code and releases the lock.
func (s *Session) Finish(exit int, msg string) {
	if s.closed {
		return
	}
	s.closed = true
	s.st.State = Finished
	s.st.ExitCode = &exit
	s.st.Message = msg
	s.st.Finished = time.Now()
	s.st.Jobs = nil
	s.write()
	syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	s.lock.Close()
}

func (s *Session) write() error {
	s.last = time.Now()
	s.st.Updated = s.last
	b, err := json.MarshalIndent(&s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "status.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "status.json"))
}

// Locked reports whether a build holds the lock in the current directory.
func Locked() bool {
	f, err := os.OpenFile(filepath.Join(Dir, "lock"), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// WaitUnlocked blocks until no build holds the lock, or timeout passes
// (0 means no timeout). It reports whether the lock came free.
func WaitUnlocked(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for Locked() {
		if timeout > 0 && time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// Load reads run id's status ("" or "latest" for the newest). A run that
// says it's running while nobody holds the lock is reported as Died.
func Load(id string) (Status, error) {
	if id == "" {
		id = "latest"
	}
	b, err := os.ReadFile(filepath.Join(Dir, "runs", id, "status.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Status{}, fmt.Errorf("no build has run here yet (no %s/runs/%s)", Dir, id)
		}
		return Status{}, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return Status{}, fmt.Errorf("reading status of run %s: %v", id, err)
	}
	if (st.State == Running || st.State == Starting) && !Locked() {
		st.State = Died
	}
	return st, nil
}

// Latest is Load("latest").
func Latest() (Status, error) { return Load("latest") }

// RunDir is the directory of run id ("" or "latest": the newest).
func RunDir(id string) string {
	if id == "" {
		id = "latest"
	}
	return filepath.Join(Dir, "runs", id)
}

// ProcStart returns a process's start time in clock ticks since boot
// (field 22 of /proc/<pid>/stat), which together with the pid identifies
// the process even if the pid is reused.
func ProcStart(pid int) (uint64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	return strconv.ParseUint(f[19], 10, 64)
}

// Stop sends SIGTERM to the build in st, if that pid is still the same
// process.
func Stop(st Status) error {
	start, err := ProcStart(st.PID)
	if err != nil || start != st.PIDStart {
		return fmt.Errorf("build %s (pid %d) isn't running", st.ID, st.PID)
	}
	return syscall.Kill(st.PID, syscall.SIGTERM)
}

// prune removes all but the newest keepRuns run directories.
func prune() {
	ents, err := os.ReadDir(filepath.Join(Dir, "runs"))
	if err != nil {
		return
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	for len(ids) > keepRuns {
		os.RemoveAll(filepath.Join(Dir, "runs", ids[0]))
		ids = ids[1:]
	}
}
