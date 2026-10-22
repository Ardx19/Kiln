// Command bench times builds with plain Ninja and with kiln and records
// wall time, peak memory and OOM kills, for the tables in the README.
//
// For a build directory and a list of targets it runs, each from clean:
//
//	ninja-default   ninja at its default -j
//	ninja-tuned     ninja -jN for each N in -tuned: the hand-tuned settings
//	kiln-first      kiln with an empty memory history
//	kiln-learned    kiln again, with the history its kiln-first left
//
// -rounds repeats every runner, rotating the order each round (a learned
// run always follows its first run). Each row is appended to -out when its
// run ends, and runs already there are skipped, so a series can be
// stopped and resumed. -budget runs only the next chunk: the pending runs
// that earlier runs say will fit in that time. -plan prints the chunks and
// -summary the median and range over rounds, without running anything.
//
// Run it on the host, or inside `docker run --memory=8g --cpus=4 ...` for
// the small-runner profile: memory is read from the container's cgroup
// when there is one. A watchdog kills a run that leaves less than -guard
// of memory, so a runaway Ninja can't take the machine down; such a run
// is recorded as aborted. -cap kills a run that takes too long.
//
// Usage:
//
//	go run ./tools/bench -dir BUILD -ninja NINJA -kiln KILN -tuned 5,6 [-rounds 3 -out FILE -budget 15m] [targets...]
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Ardx19/kiln/internal/sysinfo"
)

// Result is one run's measurements: a line of the results file.
type Result struct {
	Project string  `json:"project"`
	Profile string  `json:"profile"`
	Series  string  `json:"series,omitempty"`
	Round   int     `json:"round,omitempty"` // 0 in rows from before rounds
	Runner  string  `json:"runner"`
	Exit    int     `json:"exit"`
	Aborted bool    `json:"aborted,omitempty"` // killed by the memory guard
	Capped  bool    `json:"capped,omitempty"`  // killed by the time cap
	WallS   float64 `json:"wall_s"`
	PeakMiB float64 `json:"peak_mib"`
	OOMKill uint64  `json:"oom_kills"`
	Jobs    int     `json:"jobs,omitempty"`
	Start   string  `json:"start,omitempty"` // RFC 3339

	interrupted bool // stopped by a signal to bench: not a result
}

// OK reports whether the run finished and succeeded.
func (r Result) OK() bool { return r.Exit == 0 && !r.Aborted && !r.Capped }

// Run is one scheduled run of a runner.
type Run struct {
	Round  int
	Runner string
	Jobs   int // ninja-tuned's -j
}

func label(runner string, jobs int) string {
	if jobs > 0 {
		return fmt.Sprintf("%s (-j%d)", runner, jobs)
	}
	return runner
}

// Series names the rows that belong together: the same targets on the
// same machine profile, repeated over rounds.
type Series struct{ Project, Profile, Name string }

func (s Series) key(round int, runner string, jobs int) string {
	return fmt.Sprintf("%s|%s|%s|%d|%s|%d", s.Project, s.Profile, s.Name, round, runner, jobs)
}

// has reports whether r is one of the series' runs. Rows written before
// rounds existed have round 0 and belong to no series.
func (s Series) has(r Result) bool {
	return r.Round > 0 && r.Project == s.Project && r.Profile == s.Profile && r.Series == s.Name
}

type config struct {
	dir, ninja, kiln string
	series           Series
	tuned            []int
	only             string
	rounds           int
	guard            uint64 // bytes
	runCap, budget   time.Duration
	out              string
	targets          []string
}

// runners lists one round's runners in their usual order.
func (c config) runners() []Run {
	all := []Run{{Runner: "ninja-default"}}
	for _, j := range c.tuned {
		all = append(all, Run{Runner: "ninja-tuned", Jobs: j})
	}
	all = append(all, Run{Runner: "kiln-first"}, Run{Runner: "kiln-learned"})
	var rs []Run
	for _, r := range all {
		if c.only == "" || strings.Contains(","+c.only+",", ","+r.Runner+",") {
			rs = append(rs, r)
		}
	}
	return rs
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
	var c config
	flag.StringVar(&c.dir, "dir", "", "build directory (required)")
	flag.StringVar(&c.ninja, "ninja", "ninja", "the ninja to compare against")
	flag.StringVar(&c.kiln, "kiln", "kiln", "the kiln binary")
	tuned := flag.String("tuned", "", "comma-separated hand-tuned -j values, a ninja-tuned runner each (none skips that runner)")
	flag.StringVar(&c.series.Project, "project", "", "project name for the results (default: the build directory's parent)")
	flag.StringVar(&c.series.Profile, "profile", "host", "machine profile name for the results")
	flag.StringVar(&c.series.Name, "series", "", "series name; rows of other series are neither resumed nor summarised")
	guard := flag.Uint64("guard", 400, "abort a run when less than this many MiB of memory remain")
	flag.StringVar(&c.out, "out", "", "append JSON results to this file, skipping runs already in it")
	flag.StringVar(&c.only, "only", "", "comma-separated runners to run (default: all)")
	flag.IntVar(&c.rounds, "rounds", 1, "how many times to run each runner")
	flag.DurationVar(&c.runCap, "cap", 0, "kill a run that takes longer than this (0: no cap)")
	flag.DurationVar(&c.budget, "budget", 0, "run only the next chunk: the pending runs expected to fit in this time (0: all)")
	plan := flag.Bool("plan", false, "print the schedule split into chunks, and run nothing")
	summary := flag.Bool("summary", false, "print the series' medians and ranges from -out, and run nothing")
	flag.Parse()
	if c.dir == "" {
		flag.Usage()
		os.Exit(2)
	}
	if c.series.Project == "" {
		c.series.Project = filepath.Base(filepath.Dir(c.dir))
	}
	var err error
	if c.tuned, err = parseJobs(*tuned); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(2)
	}
	c.guard = *guard << 20
	c.targets = flag.Args()
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	rows, err := readRows(c.out)
	if err == nil {
		switch {
		case *summary:
			fmt.Print(Summary(c.series, rows))
		case *plan:
			fmt.Print(planText(c, rows))
		default:
			err = runChunk(c, rows, os.Stdout, os.Stderr)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func parseJobs(s string) ([]int, error) {
	var js []int
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" || f == "0" {
			continue
		}
		j, err := strconv.Atoi(f)
		if err != nil || j < 0 {
			return nil, fmt.Errorf("bad -tuned value %q", f)
		}
		js = append(js, j)
	}
	return js, nil
}

// schedule lists a series' runs: rounds of the runners, each round
// rotated one place further, so that no runner always goes first (into
// whatever state the machine was left in) or last. kiln-learned stays
// right after its kiln-first.
func schedule(runners []Run, rounds int) [][]Run {
	var units [][]Run
	for i := 0; i < len(runners); i++ {
		if runners[i].Runner == "kiln-first" && i+1 < len(runners) && runners[i+1].Runner == "kiln-learned" {
			units = append(units, runners[i:i+2])
			i++
			continue
		}
		units = append(units, runners[i:i+1])
	}
	var sched [][]Run
	for round := 1; round <= rounds; round++ {
		var rs []Run
		for u := range units {
			for _, r := range units[(u+round-1)%len(units)] {
				r.Round = round
				rs = append(rs, r)
			}
		}
		sched = append(sched, rs)
	}
	return sched
}

// done indexes a series' rows by run.
func done(s Series, rows []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range rows {
		if s.has(r) {
			m[s.key(r.Round, r.Runner, r.Jobs)] = r
		}
	}
	return m
}

// pending returns the scheduled runs that have no row yet, in order.
func pending(s Series, sched [][]Run, done map[string]Result) []Run {
	var p []Run
	for _, round := range sched {
		for _, r := range round {
			if _, ok := done[s.key(r.Round, r.Runner, r.Jobs)]; !ok {
				p = append(p, r)
			}
		}
	}
	return p
}

// estimate guesses a run's wall time from earlier runs of the same runner
// on the same project and profile, in any series: their median, at most
// the cap. With none it is the cap, or 5 minutes.
func estimate(s Series, rows []Result, r Run, runCap time.Duration) time.Duration {
	var walls []float64
	for _, row := range rows {
		if row.Project == s.Project && row.Profile == s.Profile && row.Runner == r.Runner && row.Jobs == r.Jobs {
			walls = append(walls, row.WallS)
		}
	}
	if len(walls) == 0 {
		if runCap > 0 {
			return runCap
		}
		return 5 * time.Minute
	}
	d := time.Duration(median(walls) * float64(time.Second))
	if runCap > 0 && d > runCap {
		return runCap
	}
	return d
}

// nextChunk takes runs from the front of pend while their estimated total
// fits in budget, and always at least one. A zero budget takes them all.
func nextChunk(pend []Run, est func(Run) time.Duration, budget time.Duration) []Run {
	if budget <= 0 {
		return pend
	}
	var total time.Duration
	for i, r := range pend {
		if total += est(r); total > budget && i > 0 {
			return pend[:i]
		}
	}
	return pend
}

// runChunk runs the series' next chunk, appending each row to c.out as
// soon as its run ends.
func runChunk(c config, rows []Result, stdout, stderr io.Writer) error {
	sched := schedule(c.runners(), c.rounds)
	total := len(sched) * len(c.runners())
	pend := pending(c.series, sched, done(c.series, rows))
	if len(pend) == 0 {
		fmt.Fprintf(stderr, "bench: all %d runs of the series are recorded; nothing to do\n", total)
		return nil
	}
	chunk := nextChunk(pend, func(r Run) time.Duration { return estimate(c.series, rows, r, c.runCap) }, c.budget)
	var results []Result
	for _, r := range chunk {
		res, err := c.runOne(r)
		if err != nil {
			return err
		}
		if res.interrupted {
			return fmt.Errorf("interrupted: round %d %s was stopped and not recorded", r.Round, label(r.Runner, r.Jobs))
		}
		results = append(results, res)
		note := ""
		switch {
		case res.Aborted:
			note = " (aborted by the guard)"
		case res.Capped:
			note = fmt.Sprintf(" (capped at %v)", c.runCap)
		}
		fmt.Fprintf(stderr, "bench: round %d %-20s exit %d, %6.1f s, peak %6.0f MiB, %d OOM kills%s\n",
			r.Round, label(r.Runner, r.Jobs), res.Exit, res.WallS, res.PeakMiB, res.OOMKill, note)
		if c.out != "" {
			if err := appendRow(c.out, res); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(stdout, Table(results))
	fmt.Fprintf(stderr, "bench: %d of %d runs recorded, %d left\n", total-len(pend)+len(chunk), total, len(pend)-len(chunk))
	return nil
}

// runOne cleans the targets, sets up kiln's memory history for the runner
// and times the run.
func (c config) runOne(r Run) (Result, error) {
	if err := clean(c.ninja, c.dir, c.targets); err != nil {
		return Result{}, fmt.Errorf("cleaning failed: %v", err)
	}
	snap := snapshotPath(c.dir, c.series, r.Round)
	tool, args := c.ninja, []string{"-C", c.dir}
	switch r.Runner {
	case "ninja-tuned":
		args = append(args, "-j", strconv.Itoa(r.Jobs))
	case "kiln-first":
		tool = c.kiln
		if err := clearHistory(c.dir); err != nil {
			return Result{}, err
		}
	case "kiln-learned":
		tool = c.kiln
		if err := restoreHistory(c.dir, snap); err != nil {
			return Result{}, err
		}
	}
	res := run(tool, append(args, c.targets...), c.guard, c.runCap)
	if r.Runner == "kiln-first" {
		if err := snapshotHistory(c.dir, snap); err != nil {
			return Result{}, err
		}
	}
	res.Project, res.Profile, res.Series, res.Round = c.series.Project, c.series.Profile, c.series.Name, r.Round
	res.Runner, res.Jobs = r.Runner, r.Jobs
	return res, nil
}

// kiln's memory history. Each round's kiln-first starts without one, and
// its kiln-learned starts from a snapshot of what kiln-first left, so
// however a series is split into chunks, nothing run in between changes
// what kiln-learned learned from.

func historyPath(dir string) string { return filepath.Join(dir, ".kiln", "mem.log") }

func snapshotPath(dir string, s Series, round int) string {
	name := strings.ReplaceAll(fmt.Sprintf("%s-%s-%s-r%d.mem.log", s.Project, s.Profile, s.Name, round), "/", "_")
	return filepath.Join(dir, ".kiln", "bench", name)
}

func clearHistory(dir string) error {
	if err := os.Remove(historyPath(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// snapshotHistory copies the history to snap; no history is saved empty.
func snapshotHistory(dir, snap string) error {
	data, err := os.ReadFile(historyPath(dir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(snap), 0o755); err != nil {
		return err
	}
	return os.WriteFile(snap, data, 0o644)
}

func restoreHistory(dir, snap string) error {
	data, err := os.ReadFile(snap)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no memory history from this round's kiln-first (%s); remove that kiln-first row from the results to run the pair again", snap)
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return clearHistory(dir)
	}
	if err := os.MkdirAll(filepath.Dir(historyPath(dir)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(historyPath(dir), data, 0o644)
}

// readRows reads a results file; a missing one (or none) has no rows.
func readRows(path string) ([]Result, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []Result
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r Result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("%s:%d: %v", path, i+1, err)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

func appendRow(path string, r Result) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// clean makes the targets out of date: output files are removed, anything
// else (a phony target) is cleaned by ninja -t clean.
func clean(ninja, dir string, targets []string) error {
	var rest []string
	for _, t := range targets {
		p := filepath.Join(dir, t)
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			if err := os.Remove(p); err != nil {
				return err
			}
			continue
		}
		rest = append(rest, t)
	}
	if len(rest) == 0 {
		return nil
	}
	if out, err := exec.Command(ninja, append([]string{"-C", dir, "-t", "clean"}, rest...)...).CombinedOutput(); err != nil {
		return fmt.Errorf("%v\n%s", err, out)
	}
	return nil
}

// stop receives the signals that interrupt bench. The run in progress is
// killed with its whole tree: ninja puts itself in its own process group,
// so it would otherwise outlive bench and compete with the next chunk.
var stop = make(chan os.Signal, 1)

// run times one command, sampling memory twice a second. It kills the
// command's whole tree when less than guard bytes of memory remain, or
// once it has run for runCap (if not zero).
func run(tool string, args []string, guard uint64, runCap time.Duration) Result {
	cmd := exec.Command(tool, args...)
	cmd.Env = append(os.Environ(), "CCACHE_DISABLE=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = nil, nil
	before := sysinfo.ReadMemory()
	start := time.Now()
	res := Result{Start: start.Format(time.RFC3339)}
	if err := cmd.Start(); err != nil {
		res.Exit = 127
		return res
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var deadline <-chan time.Time
	if runCap > 0 {
		t := time.NewTimer(runCap)
		defer t.Stop()
		deadline = t.C
	}
	peak := uint64(0)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	var err error
loop:
	for {
		select {
		case err = <-done:
			break loop
		case <-stop:
			res.interrupted = true
			killTree(cmd.Process.Pid)
		case <-deadline:
			deadline = nil
			if !res.Aborted {
				res.Capped = true
				killTree(cmd.Process.Pid)
			}
		case <-tick.C:
			m := sysinfo.ReadMemory()
			peak = max(peak, used(m))
			if m.Headroom() < guard && !res.Aborted && !res.Capped {
				res.Aborted = true
				killTree(cmd.Process.Pid)
			}
		}
	}
	after := sysinfo.ReadMemory()
	res.WallS, res.PeakMiB = time.Since(start).Seconds(), float64(peak)/(1<<20)
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

// killTree kills root and everything it started, however the descendants
// regrouped themselves: ninja runs each job in its own process group, so
// killing ninja's group would leave the compilers running.
func killTree(root int) {
	for range 3 { // a few passes, in case something forks meanwhile
		parent := map[int]int{}
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			var pid int
			if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil {
				continue
			}
			b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
			if err != nil {
				continue
			}
			rest := string(b[strings.LastIndexByte(string(b), ')')+1:])
			var state string
			var ppid int
			if _, err := fmt.Sscanf(rest, " %s %d", &state, &ppid); err == nil {
				parent[pid] = ppid
			}
		}
		victims := []int{root}
		for i := 0; i < len(victims); i++ {
			for pid, pp := range parent {
				if pp == victims[i] {
					victims = append(victims, pid)
				}
			}
		}
		for _, pid := range victims {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// Table renders results as a Markdown table.
func Table(rs []Result) string {
	var b strings.Builder
	b.WriteString("| Project | Profile | Round | Runner | Wall time | Peak RAM | OOM kills | Exit |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		exit := fmt.Sprint(r.Exit)
		switch {
		case r.Aborted:
			exit = "aborted (out of memory)"
		case r.Capped:
			exit = "capped (too slow)"
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %.0f s | %.1f GiB | %d | %s |\n", r.Project, r.Profile, r.Round, label(r.Runner, r.Jobs), r.WallS, r.PeakMiB/1024, r.OOMKill, exit)
	}
	return b.String()
}

// Summary renders a series' rows as a Markdown table, a line per runner:
// how many runs succeeded, the median and range of their wall times and
// the median of their memory peaks; the highest peak of any run; and the
// runs that failed, which are listed rather than timed.
func Summary(s Series, rows []Result) string {
	type group struct {
		runner string
		jobs   int
		rs     []Result
	}
	var gs []*group
	byLabel := map[string]*group{}
	for _, r := range rows {
		if !s.has(r) {
			continue
		}
		g := byLabel[label(r.Runner, r.Jobs)]
		if g == nil {
			g = &group{runner: r.Runner, jobs: r.Jobs}
			byLabel[label(r.Runner, r.Jobs)] = g
			gs = append(gs, g)
		}
		g.rs = append(g.rs, r)
	}
	if len(gs) == 0 {
		return fmt.Sprintf("no rows for %s, %s, series %q\n", s.Project, s.Profile, s.Name)
	}
	order := map[string]int{"ninja-default": 0, "ninja-tuned": 1, "kiln-first": 2, "kiln-learned": 3}
	sort.SliceStable(gs, func(i, j int) bool {
		if order[gs[i].runner] != order[gs[j].runner] {
			return order[gs[i].runner] < order[gs[j].runner]
		}
		return gs[i].jobs < gs[j].jobs
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%s, %s, series %q\n\n", s.Project, s.Profile, s.Name)
	b.WriteString("| Runner | OK | Median wall | Range | Median peak | Max peak | OOM kills | Failed runs |\n|---|---|---|---|---|---|---|---|\n")
	for _, g := range gs {
		var walls, peaks []float64
		var failed []string
		maxPeak, ooms := 0.0, uint64(0)
		for _, r := range g.rs {
			maxPeak, ooms = max(maxPeak, r.PeakMiB), ooms+r.OOMKill
			if r.OK() {
				walls, peaks = append(walls, r.WallS), append(peaks, r.PeakMiB)
			} else {
				failed = append(failed, failure(r))
			}
		}
		wall, span, peak, fails := "–", "–", "–", "none"
		if len(walls) > 0 {
			wall = fmt.Sprintf("%.0f s", median(walls))
			span = fmt.Sprintf("%.0f–%.0f s", slices.Min(walls), slices.Max(walls))
			peak = fmt.Sprintf("%.1f GiB", median(peaks)/1024)
		}
		if len(failed) > 0 {
			fails = strings.Join(failed, "; ")
		}
		fmt.Fprintf(&b, "| %s | %d/%d | %s | %s | %s | %.1f GiB | %d | %s |\n",
			label(g.runner, g.jobs), len(walls), len(g.rs), wall, span, peak, maxPeak/1024, ooms, fails)
	}
	return b.String()
}

// failure says how a run failed and when.
func failure(r Result) string {
	why := fmt.Sprintf("exit %d", r.Exit)
	switch {
	case r.Aborted:
		why = "stopped by the guard"
	case r.Capped:
		why = "capped"
	}
	return fmt.Sprintf("round %d %s at %.0f s", r.Round, why, r.WallS)
}

// planText prints a series' schedule: the runs already recorded, and the
// pending ones split into the chunks that successive -budget runs take.
func planText(c config, rows []Result) string {
	sched := schedule(c.runners(), c.rounds)
	d := done(c.series, rows)
	pend := pending(c.series, sched, d)
	est := func(r Run) time.Duration { return estimate(c.series, rows, r, c.runCap) }
	chunkOf := map[string]int{}
	n := 0
	for rest := pend; len(rest) > 0; n++ {
		chunk := nextChunk(rest, est, c.budget)
		for _, r := range chunk {
			chunkOf[c.series.key(r.Round, r.Runner, r.Jobs)] = n + 1
		}
		rest = rest[len(chunk):]
	}
	var b strings.Builder
	for _, round := range sched {
		for _, r := range round {
			k := c.series.key(r.Round, r.Runner, r.Jobs)
			if row, ok := d[k]; ok {
				fmt.Fprintf(&b, "done      round %d  %-20s %5.0f s\n", r.Round, label(r.Runner, r.Jobs), row.WallS)
			} else {
				fmt.Fprintf(&b, "chunk %-3d round %d  %-20s ~%4.0f s\n", chunkOf[k], r.Round, label(r.Runner, r.Jobs), est(r).Seconds())
			}
		}
	}
	total := len(sched) * len(c.runners())
	fmt.Fprintf(&b, "%d runs: %d recorded, %d left in %d chunks\n", total, total-len(pend), len(pend), n)
	return b.String()
}

func median(xs []float64) float64 {
	s := slices.Sorted(slices.Values(xs))
	switch n := len(s); {
	case n == 0:
		return 0
	case n%2 == 1:
		return s[n/2]
	default:
		return (s[n/2-1] + s[n/2]) / 2
	}
}
