//go:build compat

package compat

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Ardx19/kiln/internal/fixture"
)

// TestEditDuringBuild edits an input while the command reading it runs.
// Ninja 1.10–1.11 log the output's mtime, written after the edit, so the
// next build sees nothing to do. Ninja 1.12+ log when the command
// started, before the edit, so the output is rebuilt. kiln must do
// whatever the tree's Ninja does.
func TestEditDuringBuild(t *testing.T) {
	repo, tools, ninjas, _ := matrix(t)
	kiln, err := KilnBinary(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ninjas {
		t.Run("ninja-"+n.Version, func(t *testing.T) {
			t.Parallel()
			tree := setupTree(t, repo, Kind{Name: "ninja-edit", Spec: fixture.Spec{Generator: fixture.Ninja}}, n, tools)
			wantRebuild := logVersionWrittenBy(n.Version) >= 6
			for _, r := range []Runner{{"ninja", n.Path}, {"kiln", kiln}} {
				for attempt := 1; ; attempt++ {
					rebuild, stepped := editDuringBuild(t, tree, r, Runner{"ninja", n.Path})
					if stepped && attempt < attempts {
						continue // a clock step makes the mtimes meaningless
					}
					if rebuild != wantRebuild {
						t.Errorf("%s: after editing the slow edge's input mid-build, rebuild = %v, want %v", r.Name, rebuild, wantRebuild)
					}
					break
				}
			}
		})
	}
}

// editDuringBuild builds with r, appending to the slow edge's input half
// a second into its one-second command, and reports whether a dry run by
// judge then wants to run that command again.
func editDuringBuild(t *testing.T, tree *Tree, r, judge Runner) (rebuild, stepped bool) {
	t.Helper()
	clock := watchClock()
	if err := tree.Restore(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(editDelay)
	slow := filepath.Join(tree.SrcDir, tree.Info.SlowInput)
	if err := touch(slow); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		code, out, err := tree.exec(r, nil)
		if err == nil && code != 0 {
			t.Errorf("%s build failed:\n%s", r.Name, out)
		}
		done <- err
	}()
	time.Sleep(500 * time.Millisecond)
	if err := appendLine(slow, "edited mid-build"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, cmds, err := tree.dryRun(judge)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(cmds, func(c string) bool { return strings.Contains(c, "slow") }), clock.Stop()
}

// TestLayouts builds the CMake fixture with kiln in the directory layouts
// real projects use. After each build, kiln and the Ninja the tree was
// configured with must both find nothing left to do.
func TestLayouts(t *testing.T) {
	repo, tools, ninjas, _ := matrix(t)
	kiln, err := KilnBinary(repo)
	if err != nil {
		t.Fatal(err)
	}
	if tools.CMake == "" {
		t.Skip("cmake not found")
	}
	ninja := ninjas[len(ninjas)-1]
	base := filepath.Join(WorkDir(repo), "layouts")
	os.RemoveAll(base)

	// Hand-written manifests have no configured Ninja, so kiln asks the one
	// on PATH which log version to write.
	env := withoutWindowsPath(os.Environ())
	for i, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			env[i] = "PATH=" + filepath.Dir(ninja.Path) + string(filepath.ListSeparator) + v
		}
	}
	run := func(t *testing.T, dir string, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, tail(string(out), 1500))
		}
		return string(out)
	}
	generate := func(t *testing.T, src string) {
		t.Helper()
		if _, err := fixture.Write(src, fixture.Spec{Generator: fixture.CMake, Seed: 5}); err != nil {
			t.Fatal(err)
		}
	}
	configure := func(t *testing.T, src, build string) {
		t.Helper()
		run(t, "/", tools.CMake, "-S", src, "-B", build, "-G", "Ninja", "-DCMAKE_MAKE_PROGRAM="+ninja.Path)
	}
	// built checks that kiln built buildArg (as passed to -C, from cwd)
	// and that neither tool finds anything left to do.
	built := func(t *testing.T, cwd, buildArg string, extra ...string) {
		t.Helper()
		args := append([]string{"-C", buildArg}, extra...)
		run(t, cwd, kiln, args...)
		for _, tool := range []string{kiln, ninja.Path} {
			out := run(t, cwd, tool, append(args, "-n")...)
			if !strings.Contains(out, "no work to do") {
				t.Errorf("%s still has work after kiln's build:\n%s", filepath.Base(tool), tail(out, 800))
			}
		}
	}

	t.Run("build dir outside the source tree", func(t *testing.T) {
		src, build := filepath.Join(base, "a", "src"), filepath.Join(base, "elsewhere", "build")
		generate(t, src)
		configure(t, src, build)
		built(t, "/", build)
	})
	t.Run("two build dirs for one source tree", func(t *testing.T) {
		src := filepath.Join(base, "b", "src")
		generate(t, src)
		for _, b := range []string{"build-1", "build-2"} {
			configure(t, src, filepath.Join(base, "b", b))
			built(t, "/", filepath.Join(base, "b", b))
		}
	})
	t.Run("git worktree", func(t *testing.T) {
		src := filepath.Join(base, "c", "main")
		generate(t, src)
		run(t, src, "git", "init", "-q")
		run(t, src, "git", "add", ".")
		run(t, src, "git", "-c", "user.name=kiln", "-c", "user.email=kiln@example.invalid", "commit", "-qm", "fixture")
		wt := filepath.Join(base, "c", "feature")
		run(t, src, "git", "worktree", "add", "-q", wt)
		configure(t, wt, filepath.Join(wt, "build"))
		built(t, "/", filepath.Join(wt, "build"))
	})
	t.Run("relative -C", func(t *testing.T) {
		src := filepath.Join(base, "d", "src")
		generate(t, src)
		configure(t, src, filepath.Join(base, "d", "out", "build"))
		built(t, filepath.Join(base, "d"), "out/build")
	})
	t.Run("paths with spaces", func(t *testing.T) {
		src, build := filepath.Join(base, "e with space", "src dir"), filepath.Join(base, "e with space", "build dir")
		generate(t, src)
		configure(t, src, build)
		built(t, "/", build)
	})
	t.Run("symlinked build dir", func(t *testing.T) {
		src, build := filepath.Join(base, "f", "src"), filepath.Join(base, "f", "real-build")
		generate(t, src)
		configure(t, src, build)
		link := filepath.Join(base, "f", "link-build")
		if err := os.Symlink(build, link); err != nil {
			t.Fatal(err)
		}
		built(t, "/", link)
	})
	t.Run("read-only source tree", func(t *testing.T) {
		src, build := filepath.Join(base, "g", "src"), filepath.Join(base, "g", "build")
		generate(t, src)
		configure(t, src, build)
		run(t, "/", "chmod", "-R", "a-w", src)
		defer run(t, "/", "chmod", "-R", "u+w", src)
		built(t, "/", build)
	})
	t.Run("in-source manifest, builddir and -f", func(t *testing.T) {
		src := filepath.Join(base, "h")
		if _, err := fixture.Write(src, fixture.Spec{Generator: fixture.Ninja, Seed: 5}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(src, "build.ninja"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "other.ninja"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		built(t, "/", src, "-f", "other.ninja")
	})
}
