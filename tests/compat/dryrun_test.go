//go:build compat

package compat

import (
	"testing"
)

// TestKilnDryRun runs every edit script with Ninja doing the builds, and
// at each step compares `kiln -n` with `ninja -n` on the same tree state,
// before and after the build.
func TestKilnDryRun(t *testing.T) {
	repo, tools, ninjas, extra := matrix(t)
	kiln, err := KilnBinary(repo)
	if err != nil {
		t.Fatal(err)
	}
	steps := Script(2, extra)
	for _, n := range ninjas {
		for _, k := range Kinds {
			t.Run(k.Name+"/ninja-"+n.Version, func(t *testing.T) {
				t.Parallel()
				k.Name += "-dryrun"
				tree := setupTree(t, repo, k, n, tools)
				recs, err := RunScript(tree, Runner{"ninja", n.Path}, &Runner{"kiln", kiln}, steps, 2)
				if err != nil {
					t.Fatal(err)
				}
				compared := 0
				for i, r := range recs {
					for _, c := range []struct {
						when         string
						ninja, kiln_ []string
					}{{"before the build", r.Dry, r.CheckDry}, {"after the build", r.Leftover, r.CheckLeftover}} {
						onlyN, onlyK := setDiff(c.ninja, c.kiln_), setDiff(c.kiln_, c.ninja)
						if len(onlyN)+len(onlyK) > 0 {
							t.Errorf("step %d (%s), %s: dry runs differ\n  only ninja: %s\n  only kiln:  %s",
								i, r.Step, c.when, short(onlyN), short(onlyK))
						}
						compared += len(c.ninja)
					}
					if r.DryExit != r.CheckDryExit {
						t.Errorf("step %d (%s): dry-run exit codes differ: ninja %d, kiln %d", i, r.Step, r.DryExit, r.CheckDryExit)
					}
				}
				t.Logf("%d steps, %d dry-run commands compared", len(recs), compared)
			})
		}
	}
}

// TestRealDryRun compares `kiln -n` with `ninja -n` on real build trees.
func TestRealDryRun(t *testing.T) {
	repo, tools, _, _ := matrix(t)
	kiln, err := KilnBinary(repo)
	if err != nil {
		t.Fatal(err)
	}
	names, trees := realTrees(t)
	for i, dir := range trees {
		t.Run(names[i], func(t *testing.T) {
			r := CompareDryRun(dir, tools, kiln)
			for _, d := range r.Diffs {
				t.Error(d)
			}
			if r.Skipped != "" {
				t.Logf("skipped: %s", r.Skipped)
			}
			t.Logf("ninja %s: both would run %d commands", r.Ninja, r.Commands)
		})
	}
}
