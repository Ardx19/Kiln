package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// grandchild starts a shell that leaves a background sleep in its own
// process group, and returns the shell and the sleep's pid.
func grandchild(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.Command("/bin/sh", "-c", "set -m; sleep 60 & echo $! >"+pidFile+"; sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Kill(cmd.Process.Pid); cmd.Wait() })
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) != "" {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			return cmd, pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the shell never wrote its child's pid")
		}
	}
}

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	return err == nil && !strings.Contains(string(b), ") Z ")
}

func TestTreeAndSession(t *testing.T) {
	cmd, child := grandchild(t)
	if d := Descendants(cmd.Process.Pid); !slices.Contains(d, child) {
		t.Errorf("Descendants(%d) = %v, lacks the background sleep %d", cmd.Process.Pid, d, child)
	}
	// Setsid made the shell a session leader; its children stay in the session.
	if s := InSession(cmd.Process.Pid); !slices.Contains(s, child) || !slices.Contains(s, cmd.Process.Pid) {
		t.Errorf("InSession(%d) = %v, want the shell and %d", cmd.Process.Pid, s, child)
	}
	if rss := RSS([]int{cmd.Process.Pid, child}); rss == 0 {
		t.Error("RSS of two live processes is 0")
	}
	Kill(cmd.Process.Pid)
	cmd.Wait()
	for deadline := time.Now().Add(5 * time.Second); alive(child); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the background sleep %d outlived Kill", child)
		}
	}
}
