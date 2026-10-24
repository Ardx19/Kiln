package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runners(tuned ...int) []Run {
	return config{tuned: tuned}.runners()
}

func TestScheduleRotatesAndKeepsPairs(t *testing.T) {
	sched := schedule(runners(5, 6), 3)
	if len(sched) != 3 {
		t.Fatalf("%d rounds, want 3", len(sched))
	}
	var firsts []string
	for i, round := range sched {
		seen := map[string]int{}
		for j, r := range round {
			if r.Round != i+1 {
				t.Errorf("round %d holds a run of round %d", i+1, r.Round)
			}
			seen[label(r.Runner, r.Jobs)] = j
		}
		if len(seen) != 5 || len(round) != 5 {
			t.Errorf("round %d: %v, want each of the 5 runners once", i+1, round)
		}
		if seen["kiln-learned"] != seen["kiln-first"]+1 {
			t.Errorf("round %d: kiln-learned doesn't follow kiln-first: %v", i+1, round)
		}
		firsts = append(firsts, label(round[0].Runner, round[0].Jobs))
	}
	want := []string{"ninja-default", "ninja-tuned (-j5)", "ninja-tuned (-j6)"}
	if strings.Join(firsts, ",") != strings.Join(want, ",") {
		t.Errorf("rounds start with %v, want %v", firsts, want)
	}
	// The kiln pair rotates as one: in round 4 it comes first.
	if r := schedule(runners(5, 6), 4)[3]; r[0].Runner != "kiln-first" || r[1].Runner != "kiln-learned" {
		t.Errorf("round 4 = %v, want the kiln pair first", r)
	}
}

func TestPendingSkipsRecordedRuns(t *testing.T) {
	s := Series{"p", "laptop", "rounds"}
	rows := []Result{
		{Project: "p", Profile: "laptop", Runner: "ninja-tuned", Jobs: 5},                                      // before rounds
		{Project: "p", Profile: "laptop", Series: "rounds", Round: 1, Runner: "ninja-tuned", Jobs: 5},          // done
		{Project: "p", Profile: "laptop", Series: "rounds", Round: 1, Runner: "kiln-first", Aborted: true},     // failed, still done
		{Project: "p", Profile: "container", Series: "rounds", Round: 1, Runner: "ninja-tuned", Jobs: 6},       // other profile
		{Project: "p", Profile: "laptop", Series: "other", Round: 1, Runner: "kiln-learned"},                   // other series
		{Project: "p", Profile: "laptop", Series: "rounds", Round: 2, Runner: "ninja-tuned", Jobs: 6, Exit: 1}, // failed, still done
	}
	pend := pending(s, schedule(runners(5, 6), 2), done(s, rows))
	var got []string
	for _, r := range pend {
		got = append(got, strconv.Itoa(r.Round)+" "+label(r.Runner, r.Jobs))
	}
	want := []string{
		"1 ninja-default", "1 ninja-tuned (-j6)", "1 kiln-learned",
		"2 ninja-tuned (-j5)", "2 kiln-first", "2 kiln-learned", "2 ninja-default",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("pending =\n%v\nwant\n%v", got, want)
	}
}

func TestEstimateAndChunks(t *testing.T) {
	s := Series{"p", "laptop", "rounds"}
	rows := []Result{
		{Project: "p", Profile: "laptop", Runner: "ninja-tuned", Jobs: 5, WallS: 100},
		{Project: "p", Profile: "laptop", Series: "x", Round: 1, Runner: "ninja-tuned", Jobs: 5, WallS: 140},
		{Project: "p", Profile: "laptop", Series: "x", Round: 2, Runner: "ninja-tuned", Jobs: 5, WallS: 120},
		{Project: "p", Profile: "container", Runner: "kiln-first", WallS: 900},
		{Project: "p", Profile: "laptop", Runner: "ninja-default", WallS: 1108},
	}
	for _, tc := range []struct {
		run    Run
		runCap time.Duration
		want   time.Duration
	}{
		{Run{Runner: "ninja-tuned", Jobs: 5}, 0, 120 * time.Second},      // median of any series
		{Run{Runner: "ninja-default"}, 6 * time.Minute, 6 * time.Minute}, // at most the cap
		{Run{Runner: "kiln-first"}, 6 * time.Minute, 6 * time.Minute},    // no rows: the cap
		{Run{Runner: "kiln-first"}, 0, 5 * time.Minute},                  // no rows, no cap
	} {
		if got := estimate(s, rows, tc.run, tc.runCap); got != tc.want {
			t.Errorf("estimate(%v, cap %v) = %v, want %v", tc.run, tc.runCap, got, tc.want)
		}
	}

	pend := []Run{{Round: 1, Runner: "a"}, {Round: 1, Runner: "b"}, {Round: 1, Runner: "c"}}
	est := func(Run) time.Duration { return 100 * time.Second }
	for _, tc := range []struct {
		budget time.Duration
		want   int
	}{{250 * time.Second, 2}, {300 * time.Second, 3}, {50 * time.Second, 1}, {0, 3}} {
		if got := nextChunk(pend, est, tc.budget); len(got) != tc.want {
			t.Errorf("budget %v: chunk of %d, want %d", tc.budget, len(got), tc.want)
		}
	}
}

func TestSummary(t *testing.T) {
	s := Series{"p", "laptop", "rounds"}
	row := func(round int, runner string, jobs int, wall, peak float64) Result {
		return Result{Project: "p", Profile: "laptop", Series: "rounds", Round: round, Runner: runner, Jobs: jobs, WallS: wall, PeakMiB: peak}
	}
	aborted := row(2, "ninja-default", 0, 28, 10240)
	aborted.Aborted, aborted.Exit = true, -1
	capped := row(1, "ninja-tuned", 7, 360, 9000)
	capped.Capped, capped.Exit = true, -1
	rows := []Result{
		row(1, "kiln-learned", 0, 135, 9216), row(1, "ninja-tuned", 6, 160, 8192), row(1, "kiln-first", 0, 147, 7680),
		row(2, "ninja-tuned", 6, 150, 8704), aborted, capped, row(2, "ninja-tuned", 7, 155, 8000),
		row(3, "ninja-tuned", 6, 171, 8500),
		{Project: "p", Profile: "laptop", Runner: "ninja-tuned", Jobs: 6, WallS: 999}, // before rounds: left out
	}
	got := Summary(s, rows)
	for _, want := range []string{
		"| ninja-default | 0/1 | – | – | – | 10.0 GiB | 0 | round 2 stopped by the guard at 28 s |",
		"| ninja-tuned (-j6) | 3/3 | 160 s | 150–171 s | 8.3 GiB | 8.5 GiB | 0 | none |",
		"| ninja-tuned (-j7) | 1/2 | 155 s | 155–155 s | 7.8 GiB | 8.8 GiB | 0 | round 1 capped at 360 s |",
		"| kiln-first | 1/1 | 147 s |",
		"| kiln-learned | 1/1 | 135 s |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "ninja-default"), strings.Index(got, "kiln-learned"); i > j {
		t.Errorf("runners out of order:\n%s", got)
	}
	if median([]float64{4, 1, 3, 2}) != 2.5 {
		t.Error("median of an even count is the mean of the middle two")
	}
}

// sleeper runs a shell that leaves a background sleep, the way ninja leaves
// compilers in their own process groups, and returns its pid file.
func sleeper(t *testing.T, runCap time.Duration) (Result, string) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	return run("/bin/sh", []string{"-c", "sleep 60 & echo $! >" + pidFile + "; sleep 60"}, 0, runCap, false), pidFile
}

func TestTreePeak(t *testing.T) {
	// A shell that holds about 64 MiB in a child: the tree's peak should
	// see it, and not the gigabytes the rest of the machine uses.
	memhog := filepath.Join(t.TempDir(), "memhog")
	if out, err := exec.Command("go", "build", "-o", memhog, "../memhog").CombinedOutput(); err != nil {
		t.Fatalf("building memhog: %v\n%s", err, out)
	}
	res := run("/bin/sh", []string{"-c", memhog + " -mib 64 -secs 0.5 -hold 1.5"}, 0, 0, true)
	if !res.OK() || res.Peak != "tree" {
		t.Fatalf("run = %+v, want an ok run with a tree peak", res)
	}
	if res.PeakMiB < 60 || res.PeakMiB > 400 {
		t.Errorf("tree peak %.0f MiB, want about 64 MiB", res.PeakMiB)
	}
}

// waitGone fails unless the process in pidFile dies within a few seconds.
func waitGone(t *testing.T, pidFile string) {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	child := strings.TrimSpace(string(b))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		stat, err := os.ReadFile("/proc/" + child + "/stat")
		if err != nil || strings.Contains(string(stat), ") Z ") {
			return // gone, or dead and waiting to be reaped
		}
		if time.Now().After(deadline) {
			t.Fatalf("the background sleep (pid %s) outlived the run", child)
		}
	}
}

func TestCapKillsTheTree(t *testing.T) {
	res, pidFile := sleeper(t, 300*time.Millisecond)
	if !res.Capped || res.OK() || res.WallS > 10 {
		t.Fatalf("run = %+v, want capped soon after 300 ms", res)
	}
	waitGone(t, pidFile)
}

func TestInterruptKillsTheTree(t *testing.T) {
	timer := time.AfterFunc(300*time.Millisecond, func() { stop <- os.Interrupt })
	defer timer.Stop()
	res, pidFile := sleeper(t, 0)
	if !res.interrupted || res.WallS > 10 {
		t.Fatalf("run = %+v, want interrupted soon after 300 ms", res)
	}
	waitGone(t, pidFile)
}

func TestHistorySnapshots(t *testing.T) {
	dir := t.TempDir()
	s := Series{"p", "laptop", "rounds"}
	write := func(text string) {
		os.MkdirAll(filepath.Dir(historyPath(dir)), 0o755)
		if err := os.WriteFile(historyPath(dir), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		b, err := os.ReadFile(historyPath(dir))
		if err != nil {
			return "<none>"
		}
		return string(b)
	}
	write("old")
	if err := clearHistory(dir); err != nil || read() != "<none>" {
		t.Fatalf("clearHistory: %v, history %q", err, read())
	}
	write("first") // what round 1's kiln-first left
	snap := snapshotPath(dir, s, 1)
	if err := snapshotHistory(dir, snap); err != nil {
		t.Fatal(err)
	}
	write("first\nmore") // another run in between
	if err := restoreHistory(dir, snap); err != nil || read() != "first" {
		t.Fatalf("restoreHistory: %v, history %q, want round 1's", err, read())
	}
	if err := restoreHistory(dir, snapshotPath(dir, s, 2)); err == nil || !strings.Contains(err.Error(), "kiln-first row") {
		t.Errorf("restoring a missing snapshot: %v, want an error saying what to do", err)
	}
	// A kiln-first that left no history hands its kiln-learned none.
	clearHistory(dir)
	if err := snapshotHistory(dir, snap); err != nil {
		t.Fatal(err)
	}
	write("stale")
	if err := restoreHistory(dir, snap); err != nil || read() != "<none>" {
		t.Errorf("restoring an empty snapshot: %v, history %q", err, read())
	}
}

// TestResumeInChunks runs a two-round series one run per invocation, with
// fake ninja and kiln, and meddles with the history between chunks: no run
// repeats, and every kiln-learned starts from its own kiln-first's history.
func TestResumeInChunks(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "build")
	os.MkdirAll(dir, 0o755)
	ninja := filepath.Join(tmp, "ninja")
	kiln := filepath.Join(tmp, "kiln")
	os.WriteFile(ninja, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	// kiln notes how many history lines it started with, then adds one.
	os.WriteFile(kiln, []byte(`#!/bin/sh
h="$2/.kiln/mem.log"
n=0
if [ -f "$h" ]; then n=$(wc -l <"$h"); fi
echo $n >>"$2/seen"
mkdir -p "$2/.kiln"
echo line >>"$h"
`), 0o755)
	c := config{
		dir: dir, ninja: ninja, kiln: kiln, series: Series{"p", "test", "rounds"},
		tuned: []int{2}, rounds: 2, budget: time.Nanosecond, out: filepath.Join(tmp, "results.jsonl"),
		targets: []string{"all"},
	}
	for i := 0; i < 8; i++ {
		rows, err := readRows(c.out)
		if err != nil {
			t.Fatal(err)
		}
		if err := runChunk(c, rows, io.Discard, io.Discard); err != nil {
			t.Fatalf("chunk %d: %v", i+1, err)
		}
		f, _ := os.OpenFile(historyPath(dir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		f.WriteString("meddling\nmeddling\n")
		f.Close()
	}
	rows, err := readRows(c.out)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, r := range rows {
		k := c.series.key(r.Round, r.Runner, r.Jobs)
		if keys[k] || !r.OK() || r.Start == "" {
			t.Errorf("row %+v: repeated, failed or undated", r)
		}
		keys[k] = true
	}
	if len(rows) != 8 {
		t.Errorf("%d rows, want 8 (2 rounds of 4 runners)", len(rows))
	}
	if err := runChunk(c, rows, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if again, _ := readRows(c.out); len(again) != 8 {
		t.Errorf("a finished series ran again: %d rows", len(again))
	}
	seen, _ := os.ReadFile(filepath.Join(dir, "seen"))
	if got := strings.Fields(string(seen)); strings.Join(got, ",") != "0,1,0,1" {
		t.Errorf("kiln started with %v history lines, want 0,1,0,1 (first, learned, per round)", got)
	}
}

func TestParsePhony(t *testing.T) {
	query := `all:
  input: phony
    libfmt.a
  outputs:
    CMakeFiles/install.util
CMakeFiles/fmt.dir/src/os.cc.o:
  input: CXX_COMPILER__fmt_unscanned_Release
    ../src/src/os.cc
    || cmake_object_order_depends_target_fmt
  outputs:
    libfmt.a
test/all:
  input: phony
`
	if got := parsePhony(query); strings.Join(got, ",") != "all,test/all" {
		t.Errorf("parsePhony = %v, want [all test/all]", got)
	}
}

// TestCleanLeavesInputsOfMissingOutputs is the case of a run killed before
// its outputs were written: cleaning must not remove what they are built
// from (ninja -t clean on a file target removes its inputs' outputs too).
func TestCleanLeavesInputsOfMissingOutputs(t *testing.T) {
	tools := os.Getenv("KILN_TOOLS") // laid out by scripts/tools.sh
	if tools == "" {
		tools = filepath.Join("..", "..", ".tools")
	}
	ninja, err := filepath.Abs(filepath.Join(tools, "ninja", "1.13.2", "ninja"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ninja); err != nil {
		if ninja, err = exec.LookPath("ninja"); err != nil {
			t.Skip("no ninja in KILN_TOOLS or on PATH")
		}
	}
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("build.ninja", `rule cp
  command = cp $in $out
build gen.h: cp src.h
build out.o: cp src.c || gen.h
build all: phony out.o
`)
	write("src.h", "h")
	write("src.c", "c")
	if out, err := exec.Command(ninja, "-C", dir).CombinedOutput(); err != nil {
		t.Fatalf("ninja: %v\n%s", err, out)
	}
	os.Remove(filepath.Join(dir, "out.o")) // as a killed run leaves it
	if err := clean(ninja, dir, []string{"out.o"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gen.h")); err != nil {
		t.Errorf("cleaning a missing output removed an input built for it: %v", err)
	}
	// A phony target is still cleaned the cold way, with its inputs.
	if err := clean(ninja, dir, []string{"all"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gen.h")); err == nil {
		t.Error("cleaning the phony target all left gen.h")
	}
}

// TestSetupFailureIsNotRecorded: a build that fails at once (here every
// command is missing, as when an image lacks the compiler launcher) is a
// broken setup; recording it would make resume skip the run for good.
func TestSetupFailureIsNotRecorded(t *testing.T) {
	tmp := t.TempDir()
	ninja := filepath.Join(tmp, "ninja")
	os.WriteFile(ninja, []byte("#!/bin/sh\n[ \"$3\" = -t ] && exit 0\nexit 127\n"), 0o755)
	c := config{
		dir: tmp, ninja: ninja, kiln: ninja, series: Series{"p", "test", "rounds"},
		rounds: 1, out: filepath.Join(tmp, "results.jsonl"), targets: []string{"all"},
	}
	err := runChunk(c, nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "broken setup") {
		t.Errorf("runChunk = %v, want a broken-setup error", err)
	}
	if rows, _ := readRows(c.out); len(rows) != 0 {
		t.Errorf("recorded %d rows of a broken setup", len(rows))
	}
}
