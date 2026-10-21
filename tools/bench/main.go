// Command bench times builds with plain Ninja and with kiln and records
// wall time, peak memory and OOM kills, for the tables in the README.
//
// For a build directory and a list of targets it runs, each from clean:
//
//	ninja-default   ninja at its default -j
//	ninja-tuned     ninja -jN (-tuned N): the hand-tuned setting
//	kiln-first      kiln with an empty memory history
//	kiln-learned    kiln again, with the history the first run left
//
// Run it on the host, or inside `docker run --memory=8g --cpus=4 ...` for
// the small-runner profile: memory is read from the container's cgroup
// when there is one. A watchdog kills a run that leaves less than -guard
// of memory, so a runaway Ninja can't take the machine down; such a run
// is recorded as aborted.
//
// Usage:
//
//	go run ./tools/bench -dir BUILD -ninja NINJA -kiln KILN -tuned 6 [-profile name] [targets...]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Ardx19/kiln/internal/sysinfo"
)

// Result is one runner's measurements.
type Result struct {
	Project string  `json:"project"`
	Profile string  `json:"profile"`
	Runner  string  `json:"runner"`
	Exit    int     `json:"exit"`
	Aborted bool    `json:"aborted,omitempty"` // killed by the memory guard
	WallS   float64 `json:"wall_s"`
	PeakMiB float64 `json:"peak_mib"`
	OOMKill uint64  `json:"oom_kills"`
	Jobs    int     `json:"jobs,omitempty"`
}

func used(m sysinfo.Memory) uint64 {
	if m.Limit > 0 {
		return m.WorkingSet
	}
	total := totalMem()
	if total > m.Available {
		return total - m.Available
	}
	return 0
}

func totalMem() uint64 {
	b, _ := os.ReadFile("/proc/meminfo")
	for _, l := range strings.Split(string(b), "\n") {
		var kb uint64
		if _, err := fmt.Sscanf(l, "MemTotal: %d kB", &kb); err == nil {
			return kb << 10
		}
	}
	return 0
}

func main() {
	dir := flag.String("dir", "", "build directory (required)")
	ninja := flag.String("ninja", "ninja", "the ninja to compare against")
	kiln := flag.String("kiln", "kiln", "the kiln binary")
	tuned := flag.Int("tuned", 0, "the hand-tuned -j for ninja-tuned (0 skips that runner)")
	project := flag.String("project", "", "project name for the results (default: the build directory's parent)")
	profile := flag.String("profile", "host", "machine profile name for the results")
	guard := flag.Uint64("guard", 400, "abort a run when less than this many MiB of memory remain")
	out := flag.String("out", "", "append JSON results to this file")
	only := flag.String("only", "", "comma-separated runners to run (default: all)")
	flag.Parse()
	if *dir == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *project == "" {
		*project = filepath.Base(filepath.Dir(*dir))
	}
	targets := flag.Args()
	runners := []struct {
		name string
		args []string
		pre  func()
		jobs int
	}{
		{"ninja-default", nil, nil, 0},
		{"ninja-tuned", []string{"-j", fmt.Sprint(*tuned)}, nil, *tuned},
		{"kiln-first", nil, func() { os.Remove(filepath.Join(*dir, ".kiln", "mem.log")) }, 0},
		{"kiln-learned", nil, nil, 0},
	}
	var results []Result
	for _, r := range runners {
		if r.name == "ninja-tuned" && *tuned == 0 {
			continue
		}
		if *only != "" && !strings.Contains(","+*only+",", ","+r.name+",") {
			continue
		}
		clean := exec.Command(*ninja, append([]string{"-C", *dir, "-t", "clean"}, targets...)...)
		if o, err := clean.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "bench: cleaning failed: %v\n%s", err, o)
			os.Exit(1)
		}
		if r.pre != nil {
			r.pre()
		}
		tool := *ninja
		if strings.HasPrefix(r.name, "kiln") {
			tool = *kiln
		}
		res := run(tool, append(append([]string{"-C", *dir}, r.args...), targets...), *guard<<20)
		res.Project, res.Profile, res.Runner, res.Jobs = *project, *profile, r.name, r.jobs
		results = append(results, res)
		fmt.Fprintf(os.Stderr, "bench: %-14s exit %d, %6.1f s, peak %6.0f MiB, %d OOM kills%s\n",
			r.name, res.Exit, res.WallS, res.PeakMiB, res.OOMKill, map[bool]string{true: " (aborted by the guard)", false: ""}[res.Aborted])
	}
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bench:", err)
			os.Exit(1)
		}
		defer f.Close()
		for _, r := range results {
			b, _ := json.Marshal(r)
			f.Write(append(b, '\n'))
		}
	}
	fmt.Println(Table(results))
}

// run times one command, sampling memory twice a second.
func run(tool string, args []string, guard uint64) Result {
	cmd := exec.Command(tool, args...)
	cmd.Env = append(os.Environ(), "CCACHE_DISABLE=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = nil, nil
	before := sysinfo.ReadMemory()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{Exit: 127}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	peak, aborted := uint64(0), false
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	var err error
loop:
	for {
		select {
		case err = <-done:
			break loop
		case <-tick.C:
			m := sysinfo.ReadMemory()
			peak = max(peak, used(m))
			if m.Headroom() < guard && !aborted {
				aborted = true
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}
	}
	after := sysinfo.ReadMemory()
	res := Result{WallS: time.Since(start).Seconds(), PeakMiB: float64(peak) / (1 << 20), Aborted: aborted}
	if after.OOMKills >= before.OOMKills {
		res.OOMKill = after.OOMKills - before.OOMKills
	}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Exit = ee.ExitCode()
	} else if err != nil {
		res.Exit = 1
	}
	return res
}

// Table renders results as a Markdown table.
func Table(rs []Result) string {
	var b strings.Builder
	b.WriteString("| Project | Profile | Runner | Wall time | Peak RAM | OOM kills | Exit |\n|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		runner := r.Runner
		if r.Jobs > 0 {
			runner += fmt.Sprintf(" (-j%d)", r.Jobs)
		}
		exit := fmt.Sprint(r.Exit)
		if r.Aborted {
			exit = "aborted (out of memory)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %.0f s | %.1f GiB | %d | %s |\n", r.Project, r.Profile, runner, r.WallS, r.PeakMiB/1024, r.OOMKill, exit)
	}
	return b.String()
}
