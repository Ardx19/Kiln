package memprobe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// buildMemhog compiles tools/memhog for the test.
func buildMemhog(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "memhog")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/Ardx19/kiln/tools/memhog").CombinedOutput()
	if err != nil {
		t.Fatalf("building memhog: %v\n%s", err, out)
	}
	return bin
}

// TestSamplerSumsTheGroup runs two memhogs at once in one process group and
// checks the sampler sees their combined peak.
func TestSamplerSumsTheGroup(t *testing.T) {
	hog := buildMemhog(t)
	cmd := exec.Command("/bin/sh", "-c", hog+" -mib 120 -secs 0.5 -hold 0.6 & "+hog+" -mib 60 -secs 0.5 -hold 0.6; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s := New()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s.Track(cmd.Process.Pid)
	s.Start(50*time.Millisecond, nil)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	j := s.Untrack(cmd.Process.Pid)
	const mib = 1 << 20
	if j.Peak < 170*mib || j.Peak > 260*mib {
		t.Errorf("peak = %d MiB, want about 180", j.Peak/mib)
	}
	maxrss := uint64(cmd.ProcessState.SysUsage().(*syscall.Rusage).Maxrss) * 1024
	if maxrss < 115*mib || maxrss > j.Peak {
		t.Errorf("ru_maxrss = %d MiB: want the larger memhog alone (about 125), below the group's %d", maxrss/mib, j.Peak/mib)
	}
	if j.Backend {
		t.Error("no compiler ran, but Backend is set")
	}
}

// TestSamplerSpotsBackends checks that a real compile is recognized.
func TestSamplerSpotsBackends(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no cc")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "x.c")
	// Enough code that cc1 lives for a few ticks.
	var code []byte
	for i := range 3000 {
		code = append(code, []byte("int f"+strconv.Itoa(i)+"(int x) { return x * "+strconv.Itoa(i)+" + (x >> 3); }\n")...)
	}
	os.WriteFile(src, code, 0o644)
	cmd := exec.Command(cc, "-O2", "-c", src, "-o", filepath.Join(dir, "x.o"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s := New()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s.Track(cmd.Process.Pid)
	s.Start(10*time.Millisecond, nil)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	if j := s.Untrack(cmd.Process.Pid); !j.Backend {
		t.Errorf("cc -c ran cc1 but Backend is unset (peak %d KiB)", j.Peak>>10)
	}
}

// TestSamplerOverhead keeps 14 jobs running and checks sampling at the
// default interval costs under 1% of one CPU.
func TestSamplerOverhead(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("takes 3 s, and the race detector would be what's measured")
	}
	s := New()
	var cmds []*exec.Cmd
	for range 14 {
		cmd := exec.Command("/bin/sh", "-c", "sleep 30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		s.Track(cmd.Process.Pid)
		cmds = append(cmds, cmd)
	}
	defer func() {
		for _, c := range cmds {
			syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			c.Wait()
		}
	}()
	before := cpuTime()
	s.Start(DefaultInterval, nil)
	const window = 3 * time.Second
	time.Sleep(window)
	s.Stop()
	used := cpuTime() - before
	if share := float64(used) / float64(window); share > 0.01 {
		t.Errorf("sampling used %v of CPU in %v (%.2f%% of a core)", used, window, share*100)
	}
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
