//go:build compat

package compat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// detachStatus is the part of `kiln status --json` the tests rely on.
type detachStatus struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	PID      int    `json:"pid"`
	PIDStart uint64 `json:"pid_start"`
	Dir      string `json:"dir"`
	Total    int    `json:"total"`
	Done     int    `json:"done"`
	ExitCode *int   `json:"exit_code"`
	Failures []struct {
		Output string `json:"output"`
		Exit   int    `json:"exit"`
	} `json:"failures"`
}

func (e *memEnv) status() detachStatus {
	e.t.Helper()
	out, code := e.runKiln([]string{"status", "--json"})
	if code != 0 {
		e.t.Fatalf("status --json: exit %d\n%s", code, out)
	}
	var st detachStatus
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &st); err != nil {
		e.t.Fatalf("status --json isn't JSON: %v\n%s", err, out)
	}
	return st
}

// startDetached runs `kiln build --detach` and returns its status.
func (e *memEnv) startDetached(args ...string) detachStatus {
	e.t.Helper()
	out, code := e.runKiln(append([]string{"build", "--detach"}, args...))
	if code != 0 || !strings.Contains(out, "started in the background") {
		e.t.Fatalf("build --detach: exit %d\n%s", code, out)
	}
	return e.status()
}

// settle kills any daemon left by a failed test.
func (e *memEnv) settle() {
	if b, err := os.ReadFile(filepath.Join(e.dir, ".kiln", "runs", "latest", "status.json")); err == nil {
		var st detachStatus
		if json.Unmarshal(b, &st) == nil && st.PID > 0 {
			syscall.Kill(-st.PID, syscall.SIGKILL)
		}
	}
}

func TestDetachLifecycle(t *testing.T) {
	e := newMemEnv(t, "detach-life")
	defer e.settle()
	e.manifest("sleep 2 && touch $out", "a", "b", "c")
	st := e.startDetached("-j3")
	if st.State != "running" && st.State != "finished" {
		t.Errorf("state = %q right after starting", st.State)
	}
	if st.ID == "" || st.PID == 0 || st.PIDStart == 0 || st.Dir == "" {
		t.Errorf("status.json lacks identity fields: %+v", st)
	}
	if st.PID == os.Getpid() {
		t.Errorf("the build runs in the caller's process")
	}
	// A second build in the same tree fails fast and names the first.
	out, code := e.runKiln([]string{"build"})
	if code == 0 || !strings.Contains(out, "another build is running") || !strings.Contains(out, st.ID) {
		t.Errorf("second build: exit %d\n%s", code, out)
	}
	// -n takes no lock.
	if out, code := e.runKiln([]string{"-n"}); code != 0 {
		t.Errorf("a dry run during a build: exit %d\n%s", code, out)
	}
	// log -f follows until the build ends.
	done := make(chan string, 1)
	go func() {
		o, _ := e.runKiln([]string{"log", "-f"})
		done <- o
	}()
	select {
	case o := <-done:
		if !strings.Contains(o, "[3/3]") {
			t.Errorf("log -f returned without the last status line:\n%s", o)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("log -f never returned")
	}
	if out, code := e.runKiln([]string{"wait"}); code != 0 {
		t.Errorf("wait: exit %d\n%s", code, out)
	}
	st = e.status()
	if st.State != "finished" || st.ExitCode == nil || *st.ExitCode != 0 || st.Done != 3 || st.Total != 3 {
		t.Errorf("final status: %+v", st)
	}
	out, _ = e.runKiln([]string{"status"})
	if !strings.Contains(out, "finished with exit code 0") {
		t.Errorf("status text:\n%s", out)
	}
	e.requireClean()
}

func TestDetachExitCodes(t *testing.T) {
	e := newMemEnv(t, "detach-fail")
	defer e.settle()
	e.manifest("sleep 1; echo oops-from-$out >&2; exit 3", "bad")
	e.startDetached()
	out, code := e.runKiln([]string{"wait", "--timeout", "60s"})
	st := e.status()
	if code == 0 || st.ExitCode == nil || code != *st.ExitCode {
		t.Errorf("wait exited %d, status.json says %v\n%s", code, st.ExitCode, out)
	}
	if len(st.Failures) != 1 || st.Failures[0].Output != "bad" {
		t.Errorf("failures: %+v", st.Failures)
	}
	out, _ = e.runKiln([]string{"log", "--failures"})
	if !strings.Contains(out, "oops-from-bad") || !strings.Contains(out, "FAILED") {
		t.Errorf("log --failures:\n%s", out)
	}

	// A bad manifest is reported by the command itself, with a failing exit.
	if err := os.WriteFile(filepath.Join(e.dir, "build.ninja"), []byte("build x: nosuchrule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = e.runKiln([]string{"build", "--detach"})
	if code == 0 || !strings.Contains(out, "nosuchrule") {
		t.Errorf("bad manifest: exit %d\n%s", code, out)
	}
}

func TestDetachSurvivesHUP(t *testing.T) {
	e := newMemEnv(t, "detach-hup")
	defer e.settle()
	e.manifest("sleep 3 && touch $out", "a", "b")
	st := e.startDetached("-j2")
	// The terminal closing sends SIGHUP to the session.
	if err := syscall.Kill(-st.PID, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if out, code := e.runKiln([]string{"wait", "--timeout", "60s"}); code != 0 {
		t.Fatalf("the build didn't survive SIGHUP: wait exit %d\n%s", code, out)
	}
	for _, o := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(e.dir, o)); err != nil {
			t.Errorf("output %s missing after SIGHUP: %v", o, err)
		}
	}
}

func TestDetachKilled(t *testing.T) {
	e := newMemEnv(t, "detach-kill")
	defer e.settle()
	e.manifest("sleep 60 && touch $out", "a")
	st := e.startDetached()
	if err := syscall.Kill(-st.PID, syscall.SIGKILL); err != nil { // the daemon and its job
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.status().State != "died" {
		if time.Now().After(deadline) {
			t.Fatalf("status never said died: %+v", e.status())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if out, code := e.runKiln([]string{"wait"}); code == 0 {
		t.Errorf("wait on a dead build exited 0\n%s", out)
	}
	// The next build starts normally.
	e.manifest("touch $out", "a")
	if out, code := e.runKiln([]string{"build", "-j1"}); code != 0 {
		t.Fatalf("build after a death: exit %d\n%s", code, out)
	}
	e.requireClean()
}

func TestDetachStop(t *testing.T) {
	e := newMemEnv(t, "detach-stop")
	defer e.settle()
	e.manifest("sleep 60 && touch $out", "a", "b")
	e.startDetached("-j2")
	out, code := e.runKiln([]string{"stop"})
	if code != 0 || !strings.Contains(out, "stopped run") {
		t.Fatalf("stop: exit %d\n%s", code, out)
	}
	st := e.status()
	if st.State != "finished" || st.ExitCode == nil || *st.ExitCode == 0 {
		t.Errorf("after stop: %+v", st)
	}
	if out, code := e.runKiln([]string{"stop"}); code == 0 {
		t.Errorf("stop with nothing running exited 0\n%s", out)
	}
	for _, o := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(e.dir, o)); err == nil {
			t.Errorf("a stopped build left output %s", o)
		}
	}
}
