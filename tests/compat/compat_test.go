//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// attempts is how often a script is rerun when the wall clock steps
// during it (see clock.go).
const attempts = 3

// matrix returns the Ninja versions and the number of random steps to use.
// -short keeps only the newest Ninja.
func matrix(t *testing.T) (string, Tools, []Tool, int) {
	t.Helper()
	repo, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	tools, err := FindTools(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Ninjas) == 0 {
		t.Skip("no ninja found; run scripts/tools.sh")
	}
	ninjas, extra := tools.Ninjas, 8
	if testing.Short() {
		ninjas, extra = ninjas[len(ninjas)-1:], 3
	}
	return repo, tools, ninjas, extra
}

func setupTree(t *testing.T, repo string, k Kind, n Tool, tools Tools) *Tree {
	t.Helper()
	root := filepath.Join(repo, ".work", "compat", k.Name+"-"+n.Version)
	tree, err := Setup(root, k, n, tools)
	var skip skipError
	if errors.As(err, &skip) {
		t.Skip(skip)
	}
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// TestSelf runs every script twice with the same Ninja. Any difference is
// a bug in the harness or a nondeterministic fixture, not in a runner.
func TestSelf(t *testing.T) {
	repo, tools, ninjas, extra := matrix(t)
	steps := Script(1, extra)
	for _, n := range ninjas {
		for _, k := range Kinds {
			t.Run(k.Name+"/ninja-"+n.Version, func(t *testing.T) {
				t.Parallel()
				tree := setupTree(t, repo, k, n, tools)
				var a, b []Record
				for attempt := 1; ; attempt++ {
					var err error
					if a, err = RunScript(tree, Runner{"A", n.Path}, steps, 1); err != nil {
						t.Fatal(err)
					}
					if b, err = RunScript(tree, Runner{"B", n.Path}, steps, 1); err != nil {
						t.Fatal(err)
					}
					if c := Comparable(a, b); c == len(steps) || attempt == attempts {
						break
					} else {
						t.Logf("attempt %d: wall clock stepped at step %d; retrying", attempt, c)
					}
				}
				report, _ := json.MarshalIndent(map[string][]Record{"a": a, "b": b}, "", "  ")
				if err := os.WriteFile(tree.Root+".report.json", report, 0o644); err != nil {
					t.Error(err)
				}
				c := Comparable(a, b)
				if c == 0 {
					t.Skip("wall clock stepped at the first step of every attempt; nothing comparable")
				}
				if c < len(steps) {
					t.Logf("wall clock stepped in every attempt; compared only the first %d of %d steps", c, len(steps))
				}
				for _, d := range Compare(a[:c], b[:c]) {
					t.Error(d)
				}
				checkExpectations(t, a[:c], n.Version)
			})
		}
	}
}

// knownQuirk names a documented Ninja behaviour that explains leftover
// work after a build (see docs/ninja-quirks.md), or returns "".
func knownQuirk(ninjaVersion string, r Record) string {
	if versionLess(ninjaVersion, "1.12") &&
		strings.Contains(r.Explain, "recorded mtime of") &&
		strings.Contains(r.Explain, "gen_version.h older than most recent input") {
		return "Ninja < 1.12 records a restat output's mtime without its depfile-only inputs"
	}
	return ""
}

// checkExpectations makes sure the script really exercises rebuilds, so a
// broken edit can't make the comparison pass vacuously.
func checkExpectations(t *testing.T, recs []Record, ninjaVersion string) {
	t.Helper()
	for i, r := range recs {
		fail := func(format string, args ...any) {
			t.Errorf("step %d (%s): "+format, append([]any{i, r.Step}, args...)...)
		}
		switch r.Step {
		case "noop":
			if len(r.Ran) > 0 || len(r.Dry) > 0 {
				fail("expected no work, ran %q", r.Ran)
			}
		case "restat_noop":
			if len(r.Ran) != 1 || !strings.Contains(r.Ran[0], "gen.sh") {
				fail("expected only the generator to run (restat), ran %s", short(r.Ran))
			}
		case "edit_codegen_dep":
			if !slices.ContainsFunc(r.Ran, func(c string) bool { return strings.Contains(c, "main.cpp") }) {
				fail("depfile-only input change didn't recompile main.cpp, ran %s", short(r.Ran))
			}
		case "inject_error":
			if r.Exit == 0 {
				fail("expected the build to fail")
			}
		case "fix_error", "mass_touch", "change_flag":
			if r.Exit != 0 || len(r.Ran) == 0 {
				fail("expected a successful rebuild, exit %d, ran %q", r.Exit, r.Ran)
			}
		}
		if r.Step != "inject_error" {
			if r.Exit != 0 {
				fail("build failed with exit %d", r.Exit)
			}
			if q := knownQuirk(ninjaVersion, r); q != "" {
				t.Logf("step %d (%s): leftover work is a known quirk: %s", i, r.Step, q)
			} else if len(r.Leftover) > 0 {
				fail("build didn't converge; a dry run still wants %s", short(r.Leftover))
			}
		}
	}
}

// TestInterruptResume checks an invariant rather than a diff, because
// where SIGINT lands depends on timing: after an interrupted build and one
// resumed build, nothing is left to do.
func TestInterruptResume(t *testing.T) {
	repo, tools, ninjas, _ := matrix(t)
	for _, n := range ninjas {
		for _, k := range Kinds {
			t.Run(k.Name+"/ninja-"+n.Version, func(t *testing.T) {
				t.Parallel()
				k.Name += "-interrupt"
				tree := setupTree(t, repo, k, n, tools)
				var res InterruptResult
				for attempt := 1; ; attempt++ {
					var err error
					if res, err = Interrupt(tree, Runner{"A", n.Path}); err != nil {
						t.Fatal(err)
					}
					if !res.ClockStepped {
						break
					}
					if attempt == attempts {
						t.Skip("wall clock stepped during every attempt")
					}
				}
				if !res.Interrupted {
					t.Log("build finished before SIGINT; only the resume path was checked")
				}
				if len(res.Leftover) > 0 {
					t.Errorf("after interrupt and resume, a dry run still wants %s", short(res.Leftover))
				}
			})
		}
	}
}
