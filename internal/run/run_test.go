package run

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func inTempDir(t *testing.T) {
	t.Helper()
	old, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestLifecycle(t *testing.T) {
	inTempDir(t)
	if Locked() {
		t.Fatal("locked before any build")
	}
	s, err := Open("", []string{"all"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !Locked() {
		t.Error("not locked while a build runs")
	}
	var busy *ErrBusy
	if _, err := Open("", nil, false); !errors.As(err, &busy) || busy.ID != s.ID() || busy.PID != os.Getpid() {
		t.Errorf("second build: %v", err)
	} else if !strings.Contains(busy.Error(), s.ID()) {
		t.Errorf("message doesn't name the run: %v", busy)
	}
	if st, _ := Latest(); st.State != Starting {
		t.Errorf("state = %q, want starting", st.State)
	}
	s.Update(Progress{Total: 10, Done: 3, Jobs: []Job{{Output: "a.o", Seconds: 1.5}}}, true)
	s.Fail("b.o", 1, "cc b.c", "error: oops\n")
	st, err := Latest()
	if err != nil || st.State != Running || st.Done != 3 || st.Total != 10 || len(st.Jobs) != 1 || len(st.Failures) != 1 || st.PID != os.Getpid() {
		t.Fatalf("running status: %+v, %v", st, err)
	}
	if b, _ := os.ReadFile(RunDir("") + "/failures/" + st.Failures[0].Log); !strings.Contains(string(b), "error: oops") || !strings.Contains(string(b), "cc b.c") {
		t.Errorf("failure log: %q", b)
	}
	s.Finish(1, "subcommand failed")
	if Locked() {
		t.Error("still locked after Finish")
	}
	st, _ = Latest()
	if st.State != Finished || st.ExitCode == nil || *st.ExitCode != 1 || st.Message != "subcommand failed" {
		t.Errorf("finished status: %+v", st)
	}
	if !WaitUnlocked(time.Second) {
		t.Error("WaitUnlocked timed out on a free lock")
	}
}

func TestDied(t *testing.T) {
	inTempDir(t)
	s, err := Open("", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	s.Update(Progress{Total: 2}, true)
	s.lock.Close() // the process went away without finishing
	if st, _ := Latest(); st.State != Died {
		t.Errorf("state = %q, want died", st.State)
	}
	if !WaitUnlocked(time.Second) {
		t.Error("lock should be free")
	}
	if s2, err := Open("", nil, false); err != nil {
		t.Errorf("next build after a death: %v", err)
	} else {
		s2.Finish(0, "")
	}
}

func TestWaitUnlockedTimesOut(t *testing.T) {
	inTempDir(t)
	s, _ := Open("", nil, false)
	defer s.Finish(0, "")
	if WaitUnlocked(200 * time.Millisecond) {
		t.Error("WaitUnlocked returned while the lock was held")
	}
}

func TestStopChecksTheProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	start, err := ProcStart(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := Stop(Status{ID: "x", PID: cmd.Process.Pid, PIDStart: start + 1}); err == nil {
		t.Error("Stop signalled a process with a different start time")
	}
	if err := Stop(Status{ID: "x", PID: cmd.Process.Pid, PIDStart: start}); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Errorf("sleep wasn't terminated: %v", err)
	}
}

func TestPrune(t *testing.T) {
	inTempDir(t)
	for i := range keepRuns + 5 {
		s, err := Open("2026010"+string(rune('a'+i%26))+string(rune('a'+i/26)), nil, false)
		if err != nil {
			t.Fatal(err)
		}
		s.Finish(0, "")
	}
	ents, _ := os.ReadDir(Dir + "/runs")
	n := 0
	for _, e := range ents {
		if e.IsDir() {
			n++
		}
	}
	if n != keepRuns {
		t.Errorf("%d run dirs kept, want %d", n, keepRuns)
	}
}
