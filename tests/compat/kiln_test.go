//go:build compat

package compat

import (
	"testing"
)

// TestKilnBuild runs every edit script twice from the same snapshot:
// Ninja does the builds in run A, kiln in run B. At every step they must
// run the same commands, exit the same way and leave the same work. In
// run B, Ninja also dry-runs kiln's tree before and after each kiln
// build: Ninja must see exactly what kiln sees, so switching between the
// two never rebuilds anything.
func TestKilnBuild(t *testing.T) {
	repo, tools, ninjas, extra := matrix(t)
	kiln, err := KilnBinary(repo)
	if err != nil {
		t.Fatal(err)
	}
	steps := Script(3, extra)
	for _, n := range ninjas {
		for _, k := range Kinds {
			t.Run(k.Name+"/ninja-"+n.Version, func(t *testing.T) {
				t.Parallel()
				k.Name += "-kiln"
				tree := setupTree(t, repo, k, n, tools)
				ninja := Runner{"ninja", n.Path}
				var a, b []Record
				for attempt := 1; ; attempt++ {
					var err error
					if a, err = RunScript(tree, ninja, nil, steps, 3); err != nil {
						t.Fatal(err)
					}
					if b, err = RunScript(tree, Runner{"kiln", kiln}, &ninja, steps, 3); err != nil {
						t.Fatal(err)
					}
					if c := Comparable(a, b); c == len(steps) || attempt == attempts {
						break
					} else {
						t.Logf("attempt %d: wall clock stepped at step %d; retrying", attempt, c)
					}
				}
				c := Comparable(a, b)
				if c == 0 {
					t.Skip("wall clock stepped at the first step of every attempt")
				}
				for _, d := range Compare(a[:c], b[:c]) {
					t.Error("ninja (A) vs kiln (B): " + d.String())
				}
				for i, r := range b[:c] {
					for _, x := range []struct {
						when       string
						kiln, ninj []string
					}{{"before kiln's build", r.Dry, r.CheckDry}, {"after kiln's build", r.Leftover, r.CheckLeftover}} {
						if onlyK, onlyN := setDiff(x.kiln, x.ninj), setDiff(x.ninj, x.kiln); len(onlyK)+len(onlyN) > 0 {
							t.Errorf("step %d (%s), %s: Ninja and kiln see different work\n  only kiln:  %s\n  only ninja: %s",
								i, r.Step, x.when, short(onlyK), short(onlyN))
						}
					}
				}
				checkExpectations(t, b[:c], n.Version)
				ran := 0
				for _, r := range b[:c] {
					ran += len(r.Ran)
				}
				t.Logf("%d of %d steps compared; kiln ran %d commands", c, len(steps), ran)
			})
		}
	}
}

// TestKilnInterruptResume interrupts a kiln build with SIGINT, resumes it
// with kiln, and checks that neither kiln nor Ninja then sees work left.
func TestKilnInterruptResume(t *testing.T) {
	repo, tools, ninjas, _ := matrix(t)
	kiln, err := KilnBinary(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ninjas {
		for _, k := range Kinds {
			t.Run(k.Name+"/ninja-"+n.Version, func(t *testing.T) {
				t.Parallel()
				k.Name += "-kiln-interrupt"
				tree := setupTree(t, repo, k, n, tools)
				var res InterruptResult
				for attempt := 1; ; attempt++ {
					var err error
					if res, err = Interrupt(tree, Runner{"kiln", kiln}); err != nil {
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
					t.Errorf("after interrupt and resume, kiln still wants %s", short(res.Leftover))
				}
				if _, left, err := tree.dryRun(Runner{"ninja", n.Path}); err != nil {
					t.Fatal(err)
				} else if len(left) > 0 {
					t.Errorf("after kiln's interrupted and resumed build, Ninja still wants %s", short(left))
				}
			})
		}
	}
}
