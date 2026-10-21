// Command memrss is the M0 memory spike. It reruns the slowest compile
// edges of an existing Ninja build tree and measures their peak memory, to
// check that peak RSS per edge is stable enough to learn from past runs.
//
// It never writes into the build tree: each command's -o and -MF arguments
// are redirected to a scratch directory, and ccache is disabled.
//
// Usage:
//
//	go run ./spikes/memrss -C <build dir> [-n 8] [-runs 2] [-out results.jsonl]
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type edge struct {
	output  string
	logged  time.Duration // duration recorded in .ninja_log
	command string
}

type result struct {
	Output      string   `json:"output"`
	Run         int      `json:"run"`
	LoggedSecs  float64  `json:"logged_secs"`
	WallSecs    float64  `json:"wall_secs"`
	TreePeakKiB int64    `json:"tree_peak_kib"` // max over samples of the summed RSS of the process tree
	TreePeakAt  float64  `json:"tree_peak_at"`  // when the tree peak was seen, as a fraction of wall time
	MaxHWMKiB   int64    `json:"max_hwm_kib"`   // largest per-process VmHWM seen
	Wait4KiB    int64    `json:"wait4_maxrss_kib"`
	CPUSecs     float64  `json:"cpu_secs"`
	Samples     int      `json:"samples"`
	Procs       []string `json:"procs"`
	ExitCode    int      `json:"exit_code"`
}

func main() {
	buildDir := flag.String("C", "", "Ninja build directory to read (required)")
	n := flag.Int("n", 8, "number of slowest object files to measure")
	runs := flag.Int("runs", 2, "runs per edge")
	interval := flag.Duration("interval", 50*time.Millisecond, "sampling interval")
	ninja := flag.String("ninja", "ninja", "ninja binary used for -t commands")
	scratch := flag.String("scratch", "spikes/memrss/out/scratch", "directory for redirected outputs")
	out := flag.String("out", "spikes/memrss/out/results.jsonl", "JSON lines output")
	dry := flag.Bool("dry", false, "print the redirected commands and exit")
	flag.Parse()
	if *buildDir == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := spike(*buildDir, *n, *runs, *interval, *ninja, *scratch, *out, *dry); err != nil {
		fmt.Fprintln(os.Stderr, "memrss:", err)
		os.Exit(1)
	}
}

func spike(buildDir string, n, runs int, interval time.Duration, ninja, scratch, out string, dry bool) error {
	buildDir, err := filepath.Abs(buildDir)
	if err != nil {
		return err
	}
	if scratch, err = filepath.Abs(scratch); err != nil {
		return err
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	edges, err := slowestObjects(filepath.Join(buildDir, ".ninja_log"), n)
	if err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	var all []result
	for i := range edges {
		e := &edges[i]
		cmd, err := commandFor(ninja, buildDir, e.output)
		if err != nil {
			return err
		}
		obj := filepath.Join(scratch, fmt.Sprintf("%d.o", i))
		dep := filepath.Join(scratch, fmt.Sprintf("%d.d", i))
		redirected, ok := redirect(cmd, e.output, obj, dep)
		if !ok {
			fmt.Fprintf(os.Stderr, "skip %s: no \"-o %s\" in its command\n", e.output, e.output)
			continue
		}
		e.command = redirected
		if dry {
			fmt.Printf("%s (logged %.0fs)\n  %s\n", e.output, e.logged.Seconds(), e.command)
			continue
		}
		for r := 1; r <= runs; r++ {
			fmt.Fprintf(os.Stderr, "[%d/%d run %d] %s (logged %.0fs)\n", i+1, len(edges), r, e.output, e.logged.Seconds())
			res, err := measure(buildDir, e.command, interval)
			if err != nil {
				return err
			}
			res.Output, res.Run, res.LoggedSecs = e.output, r, e.logged.Seconds()
			if err := enc.Encode(res); err != nil {
				return err
			}
			all = append(all, res)
		}
	}
	printSummary(all)
	return nil
}

// slowestObjects returns the n object files with the longest latest
// duration in a v5 or v6 .ninja_log (v7 has the same line format).
func slowestObjects(logPath string, n int) ([]edge, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	latest := map[string]time.Duration{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 5 || !strings.HasSuffix(fields[3], ".o") {
			continue
		}
		start, err1 := strconv.Atoi(fields[0])
		end, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		latest[fields[3]] = time.Duration(end-start) * time.Millisecond
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	edges := make([]edge, 0, len(latest))
	for out, d := range latest {
		edges = append(edges, edge{output: out, logged: d})
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].logged > edges[j].logged })
	if len(edges) > n {
		edges = edges[:n]
	}
	return edges, nil
}

// commandFor asks Ninja for the command that builds output. -t commands
// only loads the manifest; it doesn't touch the logs or rebuild anything.
func commandFor(ninja, buildDir, output string) (string, error) {
	b, err := exec.Command(ninja, "-C", buildDir, "-t", "commands", output).Output()
	if err != nil {
		return "", fmt.Errorf("ninja -t commands %s: %w", output, err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1], nil
}

// redirect points a compile command's -o and -MF arguments at scratch
// files. It reports false if the command doesn't contain "-o <output>".
func redirect(cmd, output, obj, dep string) (string, bool) {
	o := " -o " + output + " "
	if !strings.Contains(cmd, o) {
		return "", false
	}
	cmd = strings.Replace(cmd, o, " -o "+obj+" ", 1)
	if i := strings.Index(cmd, " -MF "); i >= 0 {
		rest := cmd[i+len(" -MF "):]
		j := strings.IndexByte(rest, ' ')
		if j < 0 {
			j = len(rest)
		}
		cmd = cmd[:i+len(" -MF ")] + dep + rest[j:]
	}
	return cmd, true
}

func measure(dir, command string, interval time.Duration) (result, error) {
	var res result
	var output bytes.Buffer
	c := exec.Command("/bin/sh", "-c", command)
	c.Dir = dir
	c.Env = append(os.Environ(), "CCACHE_DISABLE=1")
	c.Stdout, c.Stderr = &output, &output
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	if err := c.Start(); err != nil {
		return res, err
	}
	done := make(chan struct{})
	sampled := make(chan treeStats)
	go func() {
		var st treeStats
		st.procs = map[string]bool{}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			sum := sampleTree(c.Process.Pid, &st)
			st.samples++
			if sum > st.peak {
				st.peak, st.peakAt = sum, time.Since(start)
			}
			select {
			case <-done:
				sampled <- st
				return
			case <-t.C:
			}
		}
	}()
	err := c.Wait()
	wall := time.Since(start)
	close(done)
	st := <-sampled

	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return res, err
	}
	res.ExitCode = c.ProcessState.ExitCode()
	if res.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "  exit %d: %s\n", res.ExitCode, tail(output.String(), 400))
	}
	if ru, ok := c.ProcessState.SysUsage().(*syscall.Rusage); ok {
		res.Wait4KiB = ru.Maxrss
		res.CPUSecs = time.Duration(ru.Utime.Nano() + ru.Stime.Nano()).Seconds()
	}
	res.WallSecs = wall.Seconds()
	res.TreePeakKiB = st.peak
	res.TreePeakAt = st.peakAt.Seconds() / wall.Seconds()
	res.MaxHWMKiB = st.maxHWM
	res.Samples = st.samples
	for p := range st.procs {
		res.Procs = append(res.Procs, p)
	}
	sort.Strings(res.Procs)
	return res, nil
}

type treeStats struct {
	peak    int64
	peakAt  time.Duration
	maxHWM  int64
	samples int
	procs   map[string]bool
}

var pageKiB = int64(os.Getpagesize() / 1024)

// sampleTree walks the process tree under root through
// /proc/<pid>/task/<tid>/children and returns its summed RSS in KiB.
// Processes that exit mid-walk are skipped.
func sampleTree(root int, st *treeStats) int64 {
	var sum int64
	stack := []int{root}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		proc := "/proc/" + strconv.Itoa(pid)
		if statm, err := os.ReadFile(proc + "/statm"); err == nil {
			if f := strings.Fields(string(statm)); len(f) > 1 {
				pages, _ := strconv.ParseInt(f[1], 10, 64)
				sum += pages * pageKiB
			}
		}
		if hwm := statusField(proc+"/status", "VmHWM:"); hwm > st.maxHWM {
			st.maxHWM = hwm
		}
		if comm, err := os.ReadFile(proc + "/comm"); err == nil {
			st.procs[strings.TrimSpace(string(comm))] = true
		}
		tasks, _ := os.ReadDir(proc + "/task")
		for _, t := range tasks {
			kids, err := os.ReadFile(proc + "/task/" + t.Name() + "/children")
			if err != nil {
				continue
			}
			for _, k := range strings.Fields(string(kids)) {
				if kid, err := strconv.Atoi(k); err == nil {
					stack = append(stack, kid)
				}
			}
		}
	}
	return sum
}

// statusField returns a "<name> <n> kB" field of /proc/<pid>/status in KiB.
func statusField(path, name string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, name) {
			if f := strings.Fields(line); len(f) > 1 {
				v, _ := strconv.ParseInt(f[1], 10, 64)
				return v
			}
		}
	}
	return 0
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func printSummary(all []result) {
	byOutput := map[string][]result{}
	var order []string
	for _, r := range all {
		if _, seen := byOutput[r.Output]; !seen {
			order = append(order, r.Output)
		}
		byOutput[r.Output] = append(byOutput[r.Output], r)
	}
	fmt.Println("| Output | Logged s | Wall s (runs) | Tree peak MiB (runs) | Spread | Max VmHWM MiB | wait4 MiB | Peak at |")
	fmt.Println("|---|---|---|---|---|---|---|---|")
	for _, out := range order {
		rs := byOutput[out]
		var walls, peaks []string
		lo, hi := rs[0].TreePeakKiB, rs[0].TreePeakKiB
		for _, r := range rs {
			walls = append(walls, fmt.Sprintf("%.0f", r.WallSecs))
			peaks = append(peaks, fmt.Sprintf("%d", r.TreePeakKiB/1024))
			lo, hi = min(lo, r.TreePeakKiB), max(hi, r.TreePeakKiB)
		}
		spread := 0.0
		if lo > 0 {
			spread = 100 * float64(hi-lo) / float64(lo)
		}
		last := rs[len(rs)-1]
		fmt.Printf("| %s | %.0f | %s | %s | %.1f%% | %d | %d | %.0f%% |\n",
			filepath.Base(out), last.LoggedSecs, strings.Join(walls, ", "), strings.Join(peaks, ", "),
			spread, last.MaxHWMKiB/1024, last.Wait4KiB/1024, 100*last.TreePeakAt)
	}
}
