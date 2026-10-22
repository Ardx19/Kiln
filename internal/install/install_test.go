package install

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// tree maps each path under root to its content ("-> target" for a
// symlink), skipping kiln's own directory.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." || rel == Dir || strings.HasPrefix(rel, Dir+"/") {
			return nil
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			l, _ := os.Readlink(path)
			m[rel] = "-> " + l
		case fi.IsDir():
			m[rel+"/"] = ""
		default:
			b, _ := os.ReadFile(path)
			m[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sameTree(a, b map[string]string) string {
	var diff []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			diff = append(diff, "only before: "+k)
		} else if v != w {
			diff = append(diff, "differs: "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			diff = append(diff, "only after: "+k)
		}
	}
	sort.Strings(diff)
	return strings.Join(diff, "\n")
}

// install stages files ("path": content; "path": "-> target" for symlinks)
// and installs them, returning the plan.
func doInstall(t *testing.T, prefix string, o Options, files map[string]string) (*Plan, error) {
	t.Helper()
	stage := t.TempDir()
	for p, c := range files {
		dest := filepath.Join(stage, p)
		os.MkdirAll(filepath.Dir(dest), 0o755)
		if l, ok := strings.CutPrefix(c, "-> "); ok {
			if err := os.Symlink(l, dest); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(dest, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := Scan(stage)
	if err != nil {
		t.Fatal(err)
	}
	o.Prefix = prefix
	plan, err := MakePlan(o, entries, stage)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) > 0 {
		return plan, fmt.Errorf("conflicts:\n%s", plan.ConflictReport())
	}
	return plan, Commit(plan, "t1")
}

func opts(project, tree string) Options {
	return Options{Project: project, Tree: tree, BuildDir: "/b/" + tree}
}

func TestFreshAndReinstall(t *testing.T) {
	p := t.TempDir()
	if _, err := doInstall(t, p, opts("proj", "A"), map[string]string{
		"bin/app": "v1", "lib/libx.so.1": "x1", "lib/libx.so": "-> libx.so.1", "share/doc/readme": "hi",
	}); err != nil {
		t.Fatal(err)
	}
	got := tree(t, p)
	if got["bin/app"] != "v1" || got["lib/libx.so"] != "-> libx.so.1" {
		t.Fatalf("fresh install: %v", got)
	}
	o, _, err := LoadOwners(p)
	if err != nil || len(o.Sets) != 1 || len(o.Sets[0].Files) != 4 || o.Sets[0].Files["lib/libx.so"].Type != "symlink" {
		t.Fatalf("owners: %+v, %v", o, err)
	}
	// Rename a library and drop a doc: the old files go (rule 1), and so
	// does the directory that held only them.
	plan, err := doInstall(t, p, opts("proj", "A"), map[string]string{
		"bin/app": "v2", "lib/libx.so.2": "x2", "lib/libx.so": "-> libx.so.2",
	})
	if err != nil {
		t.Fatal(err)
	}
	got = tree(t, p)
	want := map[string]string{"bin/": "", "bin/app": "v2", "lib/": "", "lib/libx.so.2": "x2", "lib/libx.so": "-> libx.so.2"}
	if d := sameTree(want, got); d != "" {
		t.Errorf("after reinstall:\n%s", d)
	}
	if !strings.Contains(plan.Describe(), "remove  lib/libx.so.1") || !strings.Contains(plan.Describe(), "2 removed") {
		t.Errorf("describe:\n%s", plan.Describe())
	}
	if _, err := os.Stat(filepath.Join(p, Dir, "txn", "t1")); err == nil {
		t.Errorf("transaction directory left behind")
	}
}

func TestVariantsAndSwap(t *testing.T) {
	p := t.TempDir()
	a := map[string]string{"bin/app": "A", "lib/libk.so": "A", "share/extra_a": "a"}
	b := map[string]string{"bin/app": "B", "lib/libkb.so": "B", "share/extra_b": "b"}
	if _, err := doInstall(t, p, opts("proj", "A"), a); err != nil {
		t.Fatal(err)
	}
	before := tree(t, p)
	// B over A is refused, naming the flag, and nothing changes.
	plan, err := doInstall(t, p, opts("proj", "B"), b)
	if err == nil || len(plan.Conflicts) != 1 || plan.Conflicts[0].Path != "bin/app" || plan.Conflicts[0].Fix != "--swap" {
		t.Fatalf("variant over variant: %v %+v", err, plan.Conflicts)
	}
	if d := sameTree(before, tree(t, p)); d != "" {
		t.Errorf("a refused install changed the prefix:\n%s", d)
	}
	// --swap leaves exactly B's set.
	o := opts("proj", "B")
	o.Swap = true
	if _, err := doInstall(t, p, o, b); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"bin/": "", "bin/app": "B", "lib/": "", "lib/libkb.so": "B", "share/": "", "share/extra_b": "b"}
	if d := sameTree(want, tree(t, p)); d != "" {
		t.Errorf("after swap:\n%s", d)
	}
	owners, _, _ := LoadOwners(p)
	if len(owners.Sets) != 1 || owners.Sets[0].Tree != "B" {
		t.Errorf("owners after swap: %+v", owners.Sets)
	}
}

func TestProjectsCoexist(t *testing.T) {
	p := t.TempDir()
	if _, err := doInstall(t, p, opts("one", "1"), map[string]string{"bin/one": "1", "share/common": "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := doInstall(t, p, opts("two", "2"), map[string]string{"bin/two": "2"}); err != nil {
		t.Fatal(err)
	}
	// A file both would install is refused for the second project...
	plan, err := doInstall(t, p, opts("two", "2"), map[string]string{"bin/two": "2", "share/common": "2"})
	if err == nil || plan.Conflicts[0].Fix != "--force" || !strings.Contains(plan.Conflicts[0].Reason, "project one") {
		t.Fatalf("shared file: %v %+v", err, plan.Conflicts)
	}
	if tree(t, p)["share/common"] != "1" {
		t.Errorf("the file was overwritten")
	}
	// ...and with Force it changes hands.
	o := opts("two", "2")
	o.Force = true
	if _, err := doInstall(t, p, o, map[string]string{"bin/two": "2", "share/common": "2"}); err != nil {
		t.Fatal(err)
	}
	owners, _, _ := LoadOwners(p)
	for _, s := range owners.Sets {
		if _, has := s.Files["share/common"]; has != (s.Project == "two") {
			t.Errorf("share/common is recorded under %s", s.Project)
		}
	}
	if tree(t, p)["bin/one"] != "1" {
		t.Errorf("project one's other file was disturbed")
	}
}

func TestUnownedFiles(t *testing.T) {
	p := t.TempDir()
	os.MkdirAll(filepath.Join(p, "lib"), 0o755)
	os.WriteFile(filepath.Join(p, "lib/same"), []byte("same"), 0o644)
	os.WriteFile(filepath.Join(p, "lib/other"), []byte("theirs"), 0o644)
	files := map[string]string{"lib/same": "same", "lib/other": "ours"}
	plan, err := doInstall(t, p, opts("proj", "A"), files)
	if err == nil || len(plan.Conflicts) != 1 || plan.Conflicts[0].Path != "lib/other" || plan.Conflicts[0].Fix != "--adopt" {
		t.Fatalf("unowned differing file: %v %+v", err, plan.Conflicts)
	}
	o := opts("proj", "A")
	o.Adopt = true
	plan, err = doInstall(t, p, o, files)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(t, p); got["lib/other"] != "ours" || got["lib/same"] != "same" {
		t.Errorf("after adopt: %v", got)
	}
	adopted := 0
	for _, op := range plan.Ops {
		if op.Why == "adopt" {
			adopted++
		}
	}
	if adopted != 1 {
		t.Errorf("the identical file should have been adopted silently: %+v", plan.Ops)
	}
}

func TestModifiedFilesBlock(t *testing.T) {
	p := t.TempDir()
	if _, err := doInstall(t, p, opts("proj", "A"), map[string]string{"bin/app": "1", "bin/old": "old"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p, "bin/old"), []byte("edited by hand"), 0o644)
	plan, err := doInstall(t, p, opts("proj", "A"), map[string]string{"bin/app": "2"})
	if err == nil || plan.Conflicts[0].Path != "bin/old" || plan.Conflicts[0].Fix != "--force" {
		t.Fatalf("removing a modified file: %v %+v", err, plan.Conflicts)
	}
	if tree(t, p)["bin/app"] != "1" {
		t.Errorf("the install went ahead despite the conflict")
	}
	o := opts("proj", "A")
	o.Force = true
	if _, err := doInstall(t, p, o, map[string]string{"bin/app": "2"}); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, p); got["bin/app"] != "2" || got["bin/old"] != "" && got["bin/old"] == "edited by hand" {
		t.Errorf("after force: %v", got)
	}
	if _, ok := tree(t, p)["bin/old"]; ok {
		t.Errorf("bin/old should be gone")
	}
}

func TestGitTrackedFilesAreKept(t *testing.T) {
	p := t.TempDir()
	if _, err := doInstall(t, p, opts("proj", "A"), map[string]string{"py/a.py": "a", "py/b.py": "b"}); err != nil {
		t.Fatal(err)
	}
	o := opts("proj", "A")
	o.Tracked = func(path string) bool { return path == "py/b.py" }
	plan, err := doInstall(t, p, o, map[string]string{"py/a.py": "a2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(t, p); got["py/b.py"] != "b" || got["py/a.py"] != "a2" {
		t.Errorf("tracked file not kept: %v", got)
	}
	if len(plan.Kept) != 1 {
		t.Errorf("kept = %v", plan.Kept)
	}
	// Overwriting a tracked file is refused (as unowned), and still refused
	// with --adopt until --force.
	files := map[string]string{"py/a.py": "a2", "py/new.py": "n", "py/b.py": "b-new"}
	plan, err = doInstall(t, p, o, files)
	if err == nil || plan.Conflicts[0].Path != "py/b.py" || plan.Conflicts[0].Fix != "--adopt" {
		t.Errorf("overwriting a tracked file: %v", err)
	}
	o.Adopt = true
	plan, err = doInstall(t, p, o, files)
	if err == nil || plan.Conflicts[0].Fix != "--force" || !strings.Contains(plan.Conflicts[0].Reason, "tracked by git") {
		t.Errorf("--adopt over a tracked file: %v", err)
	}
	o.Force = true
	if _, err := doInstall(t, p, o, files); err != nil || tree(t, p)["py/b.py"] != "b-new" {
		t.Errorf("--force over a tracked file: %v", err)
	}
}

func TestPlanGoesStale(t *testing.T) {
	p := t.TempDir()
	stage := t.TempDir()
	os.WriteFile(filepath.Join(stage, "f"), []byte("x"), 0o644)
	entries, _ := Scan(stage)
	plan, err := MakePlan(opts2(p), entries, stage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doInstall(t, p, opts("other", "9"), map[string]string{"g": "y"}); err != nil {
		t.Fatal(err)
	}
	if err := Commit(plan, "late"); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Errorf("committing a stale plan: %v", err)
	}
}

func opts2(p string) Options { o := opts("proj", "A"); o.Prefix = p; return o }

// TestCrashAtEveryPoint stops a commit at each of its crash points, then
// recovers: the prefix must be exactly the old state or exactly the new.
func TestCrashAtEveryPoint(t *testing.T) {
	type crash struct{}
	old := map[string]string{"bin/app": "1", "lib/libx.so.1": "x1", "doc/readme": "r", "lib/libx.so": "-> libx.so.1"}
	next := map[string]string{"bin/app": "2", "lib/libx.so.2": "x2", "lib/libx.so": "-> libx.so.2", "share/new/file": "n"}
	// First, count the crash points of an uninterrupted commit.
	points := 0
	crashHook = func(string) { points++ }
	p := t.TempDir()
	doInstall(t, p, opts("proj", "A"), old)
	points = 0
	doInstall(t, p, opts("proj", "A"), next)
	crashHook = nil
	if points < 8 {
		t.Fatalf("only %d crash points", points)
	}
	for k := 1; k <= points; k++ {
		p := t.TempDir()
		if _, err := doInstall(t, p, opts("proj", "A"), old); err != nil {
			t.Fatal(err)
		}
		before := tree(t, p)
		ownersBefore, _ := os.ReadFile(OwnersPath(p))
		n := 0
		crashHook = func(step string) {
			if n++; n == k {
				panic(crash{})
			}
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					if _, ok := r.(crash); !ok {
						panic(r)
					}
				}
			}()
			doInstall(t, p, opts("proj", "A"), next)
		}()
		crashHook = nil
		if err := Recover(p); err != nil {
			t.Fatalf("crash point %d: recover: %v", k, err)
		}
		got := tree(t, p)
		ownersAfter, _ := os.ReadFile(OwnersPath(p))
		committed := string(ownersAfter) != string(ownersBefore)
		if !committed {
			if d := sameTree(before, got); d != "" {
				t.Errorf("crash point %d: rolled back, but the prefix differs from before:\n%s", k, d)
			}
		} else {
			want := map[string]string{"bin/": "", "bin/app": "2", "lib/": "", "lib/libx.so.2": "x2", "lib/libx.so": "-> libx.so.2", "share/": "", "share/new/": "", "share/new/file": "n"}
			if d := sameTree(want, got); d != "" {
				t.Errorf("crash point %d: committed, but the prefix isn't the new state:\n%s", k, d)
			}
		}
		if _, err := os.Stat(filepath.Join(p, Dir, "txn", "t1")); err == nil {
			t.Errorf("crash point %d: transaction left behind after recovery", k)
		}
	}
}

// TestCommitDoesNotTrustThePlan: a plan with paths that leave the prefix,
// or a staged file changed after planning, is refused before anything
// changes.
func TestCommitDoesNotTrustThePlan(t *testing.T) {
	newPlan := func(t *testing.T) (*Plan, string, string) {
		prefix, outside := t.TempDir(), t.TempDir()
		stage := t.TempDir()
		os.WriteFile(filepath.Join(stage, "f"), []byte("x"), 0o644)
		entries, _ := Scan(stage)
		plan, err := MakePlan(opts2(prefix), entries, stage)
		if err != nil {
			t.Fatal(err)
		}
		return plan, prefix, outside
	}
	for _, bad := range []string{"../escape", "/etc/passwd", "a/../../escape", ".kiln/owners.json", ""} {
		plan, prefix, _ := newPlan(t)
		plan.Ops[0].Path = bad
		if err := Commit(plan, "x"); err == nil || !strings.Contains(err.Error(), "unsafe path") && !strings.Contains(err.Error(), "kiln's own") {
			t.Errorf("path %q: %v", bad, err)
		}
		if _, err := os.Stat(filepath.Join(prefix, ".kiln", "txn")); err == nil {
			t.Errorf("path %q: the refused plan left a transaction", bad)
		}
	}
	plan, prefix, _ := newPlan(t)
	os.WriteFile(filepath.Join(plan.Stage, "f"), []byte("swapped after planning"), 0o644)
	if err := Commit(plan, "x"); err == nil || !strings.Contains(err.Error(), "changed since it was staged") {
		t.Errorf("a changed staged file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(prefix, "f")); err == nil {
		t.Error("the swapped file was installed")
	}
}

// TestRootRefusesSymlinkEscape: as root, a path that goes through a symlink
// out of the prefix is refused. It only runs as root (inside a container).
func TestRootRefusesSymlinkEscape(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	prefix, outside, stage := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(prefix, "evil")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(stage, "evil"), 0o755)
	os.WriteFile(filepath.Join(stage, "evil", "x"), []byte("x"), 0o644)
	entries, _ := Scan(stage)
	plan, err := MakePlan(opts2(prefix), entries, stage)
	if err != nil {
		t.Fatal(err)
	}
	if err := Commit(plan, "x"); err == nil || !strings.Contains(err.Error(), "outside the prefix") {
		t.Errorf("Commit = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Error("root wrote outside the prefix")
	}
}
