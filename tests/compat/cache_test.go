//go:build compat

package compat

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ardx19/kiln/internal/cache"
	"github.com/Ardx19/kiln/internal/fixture"
)

// worktrees makes two checkouts of the same fixture next to each other
// (base/wt1, base/wt2), each with its own build directory, as git
// worktrees of one project would be. launcher says whether CMake wires
// ccache in as the compiler launcher.
func (e *installEnv) worktrees(base string, launcher bool) (builds [2]string) {
	e.t.Helper()
	if e.tools.CMake == "" {
		e.t.Skip("cmake not found")
	}
	if _, err := exec.LookPath("ccache"); err != nil {
		e.t.Skip("ccache not installed")
	}
	for i := range builds {
		wt := filepath.Join(base, "wt"+string(rune('1'+i)))
		src, build := filepath.Join(wt, "src"), filepath.Join(wt, "build")
		if _, err := fixture.Write(src, fixture.Spec{Generator: fixture.CMake, Seed: 11, Libs: 4, Sources: 5}); err != nil {
			e.t.Fatal(err)
		}
		args := []string{"-S", src, "-B", build, "-G", "Ninja", "-DCMAKE_MAKE_PROGRAM=" + e.ninja.Path}
		if launcher {
			args = append(args, "-DCMAKE_C_COMPILER_LAUNCHER=ccache", "-DCMAKE_CXX_COMPILER_LAUNCHER=ccache")
		}
		cmd := exec.Command(e.tools.CMake, args...)
		cmd.Env = e.env
		if out, err := cmd.CombinedOutput(); err != nil {
			e.t.Fatalf("configure %s: %v\n%s", wt, err, tail(string(out), 800))
		}
		builds[i] = build
	}
	return builds
}

// kilnBuild builds with kiln; the ccache directory is always the given
// one, so runs never touch the user's own cache.
func (e *installEnv) kilnBuild(build, cacheDir string, extra ...string) (string, int) {
	e.t.Helper()
	cmd := exec.Command(e.kiln, append([]string{"-C", build, "--mem=off"}, extra...)...)
	cmd.Env = append(append([]string(nil), e.env...), "CCACHE_DIR="+cacheDir)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

func (e *installEnv) cacheConfig(build, toml string) {
	writeFile(e.t, filepath.Join(build, ".kiln", "config.toml"), toml)
}

// rate reads the hit rate of the cache in dir.
func rateOf(t *testing.T, dir string) cache.Counters {
	t.Helper()
	c, err := cache.ReadStats([]string{"CCACHE_DIR=" + dir})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestCacheSecondWorktree builds a second checkout of a project in
// another directory, first with a plain ccache launcher and then with
// kiln's managed settings (a common base_dir, no directory in the hash).
func TestCacheSecondWorktree(t *testing.T) {
	e := newInstallEnv(t, "cache-wt")
	// Plain ccache: absolute paths differ between the checkouts, so the
	// second build mostly misses.
	plain := e.worktrees(filepath.Join(e.base, "plain"), true)
	plainDir := filepath.Join(e.base, "ccache-plain")
	if out, code := e.kilnBuild(plain[0], plainDir); code != 0 {
		t.Fatalf("plain wt1: exit %d\n%s", code, tail(out, 800))
	}
	before := rateOf(t, plainDir)
	if out, code := e.kilnBuild(plain[1], plainDir); code != 0 {
		t.Fatalf("plain wt2: exit %d\n%s", code, tail(out, 800))
	}
	plainRate := rateOf(t, plainDir).Since(before)
	if plainRate.Misses() == 0 || plainRate.Rate() > 40 {
		t.Logf("plain ccache hit %.0f%% of %d on the second worktree", plainRate.Rate(), plainRate.Hits()+plainRate.Misses())
	}

	// kiln-managed: the same layout, with the settings in each build's
	// config.
	managed := e.worktrees(filepath.Join(e.base, "managed"), true)
	dir := filepath.Join(e.base, "ccache-managed")
	toml := "[cache]\ndir = \"" + dir + "\"\nbase_dir = \"" + filepath.Join(e.base, "managed") + "\"\nhash_dir = false\n"
	for _, b := range managed {
		e.cacheConfig(b, toml)
	}
	out, code := e.kilnBuild(managed[0], dir)
	if code != 0 {
		t.Fatalf("managed wt1: exit %d\n%s", code, tail(out, 800))
	}
	out, code = e.kilnBuild(managed[1], dir)
	if code != 0 {
		t.Fatalf("managed wt2: exit %d\n%s", code, tail(out, 800))
	}
	if !strings.Contains(out, "ccache:") || !strings.Contains(out, "hit rate") {
		t.Errorf("kiln didn't report the hit rate:\n%s", tail(out, 600))
	}
	total := rateOf(t, dir)
	if total.Hits() < 10 {
		t.Errorf("the second worktree should hit the cache, but there were only %d hits (%d misses)", total.Hits(), total.Misses())
	}
	if plainRate.Rate() >= 100*float64(total.Hits())/float64(total.Hits()+total.Misses()) && plainRate.Hits() > 0 {
		t.Errorf("managed settings should beat plain ccache: plain %.0f%%", plainRate.Rate())
	}
	t.Logf("second worktree: plain ccache %d hits / %d misses; kiln-managed %d hits in total / %d misses",
		plainRate.Hits(), plainRate.Misses(), total.Hits(), total.Misses())
}

// TestCacheInjection builds a project configured without any launcher:
// with inject on, kiln puts ccache in front of the compilers, the second
// worktree hits, and Ninja still sees the tree as up to date.
func TestCacheInjection(t *testing.T) {
	e := newInstallEnv(t, "cache-inject")
	trees := e.worktrees(filepath.Join(e.base, "t"), false)
	dir := filepath.Join(e.base, "ccache")
	toml := "[cache]\ninject = true\ndir = \"" + dir + "\"\nbase_dir = \"" + filepath.Join(e.base, "t") + "\"\nhash_dir = false\n"
	for _, b := range trees {
		e.cacheConfig(b, toml)
	}
	out, code := e.kilnBuild(trees[0], dir)
	if code != 0 || !strings.Contains(out, "ccache added to") {
		t.Fatalf("first worktree: exit %d\n%s", code, tail(out, 800))
	}
	first := rateOf(t, dir)
	if first.Misses() < 10 {
		t.Fatalf("injection didn't put ccache in front of the compiles: %d misses", first.Misses())
	}
	out, code = e.kilnBuild(trees[1], dir)
	if code != 0 {
		t.Fatalf("second worktree: exit %d\n%s", code, tail(out, 800))
	}
	if d := rateOf(t, dir).Since(first); d.Hits() < 10 || d.Rate() < 80 {
		t.Errorf("second worktree: %d hits, %d misses (%.0f%%)", d.Hits(), d.Misses(), d.Rate())
	}
	for _, b := range trees {
		o, err := exec.Command(e.ninja.Path, "-C", b, "-n").CombinedOutput()
		if err != nil || !strings.Contains(string(o), "no work to do") {
			t.Errorf("ninja %s still has work in %s after the injected build:\n%s", e.ninja.Version, b, tail(string(o), 600))
		}
	}
}

// TestCacheChurn gives the cache a limit far too small for the build:
// `kiln cache stats` shows it cleaning up.
func TestCacheChurn(t *testing.T) {
	e := newInstallEnv(t, "cache-churn")
	trees := e.worktrees(filepath.Join(e.base, "t"), false)
	dir := filepath.Join(e.base, "ccache")
	toml := "[cache]\ninject = true\ndir = \"" + dir + "\"\nmax_size = \"20K\"\n"
	e.cacheConfig(trees[0], toml)
	if out, code := e.kilnBuild(trees[0], dir); code != 0 {
		t.Fatalf("build: exit %d\n%s", code, tail(out, 800))
	}
	cmd := exec.Command(e.kiln, "cache", "-C", trees[0], "stats")
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cache stats: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "cache directory: "+dir) || !strings.Contains(s, "hit rate") || !strings.Contains(s, "cleanups performed") {
		t.Errorf("stats of an undersized cache:\n%s", s)
	}
	// trim cleans up to the configured limit.
	cmd = exec.Command(e.kiln, "cache", "-C", trees[0], "trim")
	cmd.Env = e.env
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "cleaned up") || !strings.Contains(string(out), "of 19 KiB") {
		t.Errorf("trim: %v\n%s", err, out)
	}
}
