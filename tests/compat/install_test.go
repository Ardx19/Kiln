//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Ardx19/kiln/internal/fixture"
)

// installEnv drives `kiln install` on fixture trees.
type installEnv struct {
	t      *testing.T
	repo   string
	tools  Tools
	kiln   string
	ninja  Tool
	base   string
	env    []string
	prefix string
}

func newInstallEnv(t *testing.T, name string) *installEnv {
	t.Helper()
	repo, tools, ninjas, _ := matrix(t)
	kiln, err := KilnBinary(repo)
	if err != nil {
		t.Fatal(err)
	}
	e := &installEnv{t: t, repo: repo, tools: tools, kiln: kiln, ninja: ninjas[len(ninjas)-1]}
	e.base = filepath.Join(WorkDir(repo), "install", name)
	os.RemoveAll(e.base)
	if err := os.MkdirAll(e.base, 0o755); err != nil {
		t.Fatal(err)
	}
	e.env = withoutWindowsPath(os.Environ())
	for i, kv := range e.env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			e.env[i] = "PATH=" + filepath.Dir(e.ninja.Path) + string(filepath.ListSeparator) + v
		}
	}
	e.env = append(e.env, "NINJA="+e.ninja.Path, "KILN_CONFIG="+filepath.Join(e.base, "none.toml"), "MESON="+tools.Meson)
	e.prefix = filepath.Join(e.base, "prefix")
	return e
}

// configure makes a build tree of the fixture (variant "a" or "b") and
// returns its build directory. mutate, if set, edits the build file
// before configuring.
func (e *installEnv) configure(gen, name, variant string, prefix string) (build string) {
	e.t.Helper()
	src := filepath.Join(e.base, name, "src")
	build = filepath.Join(e.base, name, "build")
	g := fixture.CMake
	if gen == "meson" {
		g = fixture.Meson
	}
	if _, err := fixture.Write(src, fixture.Spec{Generator: g, Seed: 4, Variant: variant}); err != nil {
		e.t.Fatal(err)
	}
	var cmd *exec.Cmd
	if gen == "meson" {
		if e.tools.Meson == "" {
			e.t.Skip("meson not found")
		}
		cmd = exec.Command(e.tools.Meson, "setup", build, src, "--prefix="+prefix, "-Dbackend_max_links=1")
	} else {
		if e.tools.CMake == "" {
			e.t.Skip("cmake not found")
		}
		cmd = exec.Command(e.tools.CMake, "-S", src, "-B", build, "-G", "Ninja", "-DCMAKE_MAKE_PROGRAM="+e.ninja.Path, "-DCMAKE_INSTALL_PREFIX="+prefix)
	}
	cmd.Env = e.env
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("configure %s: %v\n%s", name, err, tail(string(out), 1200))
	}
	return build
}

func (e *installEnv) kilnInstall(build string, extra []string, env ...string) (string, int) {
	e.t.Helper()
	cmd := exec.Command(e.kiln, append([]string{"install", "-C", build}, extra...)...)
	cmd.Env = append(append([]string(nil), e.env...), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
		if code == -1 { // killed by a signal
			code = 128 + int(ee.Sys().(syscall.WaitStatus).Signal())
		}
	} else if err != nil {
		e.t.Fatal(err)
	}
	return string(out), code
}

// files lists every file under prefix (not kiln's directory) with its
// content hash or link target.
func prefixFiles(t *testing.T, prefix string) map[string]string {
	t.Helper()
	m := map[string]string{}
	filepath.Walk(prefix, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(prefix, path)
		if rel == ".kiln" || strings.HasPrefix(rel, ".kiln/") || fi.IsDir() {
			return nil
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			l, _ := os.Readlink(path)
			m[rel] = "-> " + l
			return nil
		}
		b, _ := os.ReadFile(path)
		m[rel] = string(b)
		return nil
	})
	return m
}

func ownedFiles(t *testing.T, prefix, treeSuffix string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(prefix, ".kiln", "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		Sets []struct {
			BuildDir string                    `json:"build_dir"`
			Files    map[string]map[string]any `json:"files"`
		} `json:"sets"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range o.Sets {
		if strings.HasSuffix(s.BuildDir, treeSuffix) {
			for f := range s.Files {
				out = append(out, f)
			}
		}
	}
	sort.Strings(out)
	return out
}

func diffMaps(a, b map[string]string) []string {
	var d []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			d = append(d, "missing after: "+k)
		} else if v != w {
			d = append(d, "changed: "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d = append(d, "new: "+k)
		}
	}
	sort.Strings(d)
	return d
}

func TestInstallVariantsAndSwap(t *testing.T) {
	for _, gen := range []string{"cmake", "meson"} {
		t.Run(gen, func(t *testing.T) {
			e := newInstallEnv(t, "variants-"+gen)
			a := e.configure(gen, "treeA", "a", e.prefix)
			b := e.configure(gen, "treeB", "b", e.prefix)

			out, code := e.kilnInstall(a, []string{"--dry-run"})
			if code != 0 || !strings.Contains(out, "new") {
				t.Fatalf("dry run: exit %d\n%s", code, tail(out, 800))
			}
			if _, err := os.Stat(e.prefix); err == nil && len(prefixFiles(t, e.prefix)) > 0 {
				t.Fatalf("--dry-run changed the prefix")
			}

			if out, code := e.kilnInstall(a, nil); code != 0 {
				t.Fatalf("install A: exit %d\n%s", code, tail(out, 1500))
			}
			afterA := prefixFiles(t, e.prefix)
			if len(afterA) < 5 {
				t.Fatalf("A installed only %d files", len(afterA))
			}
			var extraA bool
			for f := range afterA {
				extraA = extraA || strings.HasSuffix(f, "extra_a.txt")
			}
			if !extraA {
				t.Errorf("variant A's extra_a.txt wasn't installed")
			}

			// Installing B over A is refused, and says how to override.
			out, code = e.kilnInstall(b, nil)
			if code == 0 || !strings.Contains(out, "--swap") || !strings.Contains(out, "another tree") {
				t.Errorf("B over A: exit %d\n%s", code, tail(out, 800))
			}
			if d := diffMaps(afterA, prefixFiles(t, e.prefix)); len(d) > 0 {
				t.Errorf("a refused install changed the prefix: %v", d)
			}

			// --swap leaves exactly B's set.
			if out, code := e.kilnInstall(b, []string{"--swap"}); code != 0 {
				t.Fatalf("install B --swap: exit %d\n%s", code, tail(out, 1500))
			}
			afterB := prefixFiles(t, e.prefix)
			want := ownedFiles(t, e.prefix, "treeB/build")
			var got []string
			for f := range afterB {
				got = append(got, f)
			}
			sort.Strings(got)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("after the swap the prefix holds %d files but B's set is %d:\n%v", len(got), len(want), diffMaps(afterB, toMap(want)))
			}
			for f := range afterA {
				if strings.HasSuffix(f, "extra_a.txt") {
					if _, still := afterB[f]; still {
						t.Errorf("A's %s survived the swap", f)
					}
				}
			}
			var extraB bool
			for f := range afterB {
				extraB = extraB || strings.HasSuffix(f, "extra_b.txt")
			}
			if !extraB {
				t.Errorf("variant B's extra_b.txt is missing after the swap")
			}
		})
	}
}

func toMap(keys []string) map[string]string {
	m := map[string]string{}
	for _, k := range keys {
		m[k] = ""
	}
	return m
}

func TestInstallRenamedLibrary(t *testing.T) {
	for _, gen := range []string{"cmake", "meson"} {
		t.Run(gen, func(t *testing.T) {
			e := newInstallEnv(t, "rename-"+gen)
			build := e.configure(gen, "tree", "a", e.prefix)
			if out, code := e.kilnInstall(build, nil); code != 0 {
				t.Fatalf("install: exit %d\n%s", code, tail(out, 1500))
			}
			before := prefixFiles(t, e.prefix)
			var old []string
			for f := range before {
				if strings.Contains(f, "libk1.so") {
					old = append(old, f)
				}
			}
			if len(old) == 0 {
				t.Fatalf("no libk1 was installed: %v", before)
			}
			// Rename the shared library in the build file; the build
			// directory regenerates itself on the next build.
			file := filepath.Join(e.base, "tree", "src", map[string]string{"cmake": "CMakeLists.txt", "meson": "meson.build"}[gen])
			text, _ := os.ReadFile(file)
			renamed := strings.Replace(string(text), "OUTPUT_NAME k1", "OUTPUT_NAME k1renamed", 1)
			renamed = strings.Replace(renamed, "shared_library('k1'", "shared_library('k1renamed'", 1)
			if renamed == string(text) {
				t.Fatal("the fixture has no libk1 to rename")
			}
			if err := os.WriteFile(file, []byte(renamed), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, code := e.kilnInstall(build, nil); code != 0 {
				t.Fatalf("reinstall: exit %d\n%s", code, tail(out, 1500))
			}
			after := prefixFiles(t, e.prefix)
			for _, f := range old {
				if _, still := after[f]; still {
					t.Errorf("the old %s is still installed", f)
				}
			}
			var fresh bool
			for f := range after {
				fresh = fresh || strings.Contains(f, "libk1renamed")
			}
			if !fresh {
				t.Errorf("the renamed library wasn't installed: %v", after)
			}
		})
	}
}

func TestInstallProjectsShareAPrefix(t *testing.T) {
	e := newInstallEnv(t, "projects")
	a := e.configure("cmake", "one", "a", e.prefix)
	b := e.configure("cmake", "two", "a", e.prefix)
	if out, code := e.kilnInstall(a, []string{"--project", "one"}); code != 0 {
		t.Fatalf("install one: exit %d\n%s", code, tail(out, 1200))
	}
	afterOne := prefixFiles(t, e.prefix)
	out, code := e.kilnInstall(b, []string{"--project", "two"})
	if code == 0 || !strings.Contains(out, "belongs to project one") || !strings.Contains(out, "--force") {
		t.Errorf("a second project over the first: exit %d\n%s", code, tail(out, 800))
	}
	if d := diffMaps(afterOne, prefixFiles(t, e.prefix)); len(d) > 0 {
		t.Errorf("the refused install changed the prefix: %v", d)
	}
}

func TestInstallCrashRecovery(t *testing.T) {
	e := newInstallEnv(t, "crash")
	a := e.configure("cmake", "treeA", "a", e.prefix)
	b := e.configure("cmake", "treeB", "b", e.prefix)
	if out, code := e.kilnInstall(a, nil); code != 0 {
		t.Fatalf("install A: exit %d\n%s", code, tail(out, 1200))
	}
	oldState := prefixFiles(t, e.prefix)
	ownersOld, _ := os.ReadFile(filepath.Join(e.prefix, ".kiln", "owners.json"))
	snap := filepath.Join(e.base, "prefix.snap")
	if out, err := exec.Command("cp", "-a", e.prefix, snap).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// Learn B's final state from an uninterrupted swap, then put A back.
	if out, code := e.kilnInstall(b, []string{"--swap"}); code != 0 {
		t.Fatalf("install B: exit %d\n%s", code, tail(out, 1200))
	}
	newState := prefixFiles(t, e.prefix)
	reset := func() {
		os.RemoveAll(e.prefix)
		if out, err := exec.Command("cp", "-a", snap, e.prefix).CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	moves := len(diffMaps(oldState, newState))
	points := []string{"owners:1", "committed:1", "cleanup:1"}
	for i := 1; i <= moves; i += max(1, moves/12) {
		points = append(points, "move:"+itoa(i))
	}
	for _, point := range points {
		reset()
		out, code := e.kilnInstall(b, []string{"--swap", "--no-build"}, "KILN_TEST_CRASH_AT="+point)
		if code != 128+int(syscall.SIGKILL) {
			t.Errorf("%s: kiln wasn't killed (exit %d)\n%s", point, code, tail(out, 400))
			continue
		}
		if out, code := e.kilnInstall(b, []string{"--recover"}); code != 0 {
			t.Errorf("%s: recover: exit %d\n%s", point, code, tail(out, 600))
			continue
		}
		got := prefixFiles(t, e.prefix)
		owners, _ := os.ReadFile(filepath.Join(e.prefix, ".kiln", "owners.json"))
		switch {
		case string(owners) == string(ownersOld):
			if d := diffMaps(oldState, got); len(d) > 0 {
				t.Errorf("%s: rolled back, but the prefix isn't the old state: %v", point, d)
			}
		default:
			if d := diffMaps(newState, got); len(d) > 0 {
				t.Errorf("%s: committed, but the prefix isn't the new state: %v", point, d)
			}
		}
		if ents, _ := os.ReadDir(filepath.Join(e.prefix, ".kiln", "txn")); len(ents) > 0 {
			t.Errorf("%s: a transaction is left after recovery", point)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestInstallStageThenCommit(t *testing.T) {
	e := newInstallEnv(t, "stage")
	a := e.configure("cmake", "tree", "a", e.prefix)
	out, code := e.kilnInstall(a, []string{"--stage"})
	if code != 0 || !strings.Contains(out, "staged as") {
		t.Fatalf("--stage: exit %d\n%s", code, tail(out, 800))
	}
	if n := len(prefixFiles(t, e.prefix)); n != 0 {
		t.Fatalf("--stage installed %d files", n)
	}
	var dir string
	for _, f := range strings.Fields(out) {
		if strings.Contains(f, "/.kiln/stage/") {
			dir = f
		}
	}
	if dir == "" {
		t.Fatalf("no staging directory in the output:\n%s", out)
	}
	cmd := exec.Command(e.kiln, "install", "--commit", dir)
	cmd.Env = e.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("--commit: %v\n%s", err, tail(string(out), 800))
	}
	if n := len(prefixFiles(t, e.prefix)); n < 5 {
		t.Errorf("--commit installed %d files", n)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("the staging directory was left behind")
	}
	// A prefix you can't write to is staged, with instructions.
	e2 := newInstallEnv(t, "stage-ro")
	ro := filepath.Join(e2.base, "ro-prefix")
	os.MkdirAll(ro, 0o755)
	b := e2.configure("cmake", "tree", "a", ro)
	os.Chmod(ro, 0o555)
	defer os.Chmod(ro, 0o755)
	if os.Geteuid() != 0 {
		out, code := e2.kilnInstall(b, nil)
		if code != 0 || !strings.Contains(out, "isn't writable") || !strings.Contains(out, "install --commit") {
			t.Errorf("read-only prefix: exit %d\n%s", code, tail(out, 800))
		}
		if n := len(prefixFiles(t, ro)); n != 0 {
			t.Errorf("a read-only prefix got %d files", n)
		}
	}
}

func TestInstallOtherGenerators(t *testing.T) {
	e := newInstallEnv(t, "tier2")
	build := filepath.Join(e.base, "tree")
	prefix := filepath.Join(e.base, "pfx")
	os.MkdirAll(filepath.Join(build, ".kiln"), 0o755)
	os.WriteFile(filepath.Join(build, "build.ninja"), []byte("rule t\n  command = touch $out\nbuild tool: t\ndefault tool\n"), 0o644)
	conf := func(cmd string) {
		toml := "[install]\nproject = \"tier2\"\nprefix = \"" + prefix + "\"\ncommand = '" + cmd + "'\n"
		os.WriteFile(filepath.Join(build, ".kiln", "config.toml"), []byte(toml), 0o644)
	}
	conf(`mkdir -p "$DESTDIR` + prefix + `/bin" && cp tool "$DESTDIR` + prefix + `/bin/tool"`)
	out, code := e.kilnInstall(build, nil)
	if code != 0 {
		t.Fatalf("install: exit %d\n%s", code, tail(out, 800))
	}
	if _, err := os.Stat(filepath.Join(prefix, "bin", "tool")); err != nil {
		t.Errorf("tool wasn't installed: %v", err)
	}
	// A script that ignores DESTDIR is noticed.
	conf(`mkdir -p "$DESTDIR` + prefix + `/bin" && cp tool "$DESTDIR` + prefix + `/bin/tool" && echo x > ` + prefix + `/stray`)
	out, code = e.kilnInstall(build, nil)
	if code != 0 || !strings.Contains(out, "ignores DESTDIR") {
		t.Errorf("a script writing outside DESTDIR: exit %d\n%s", code, tail(out, 800))
	}
}

// TestInstallAbsoluteDestinations: an install whose destinations are
// absolute (RDKit's in-tree install writes into the source directory) puts
// its files under stage/<that directory>, not under the prefix. kiln finds
// them, says so, and warns about the few that fall outside a real prefix.
func TestInstallAbsoluteDestinations(t *testing.T) {
	e := newInstallEnv(t, "absolute")
	build := filepath.Join(e.base, "tree")
	nominal := filepath.Join(e.base, "nominal")
	real := filepath.Join(e.base, "checkout")
	os.MkdirAll(filepath.Join(build, ".kiln"), 0o755)
	os.MkdirAll(real, 0o755)
	writeFile(t, filepath.Join(build, "build.ninja"), "rule t\n  command = touch $out\nbuild tool: t\ndefault tool\n")
	conf := func(cmd string) {
		writeFile(t, filepath.Join(build, ".kiln", "config.toml"),
			"[install]\nproject = \"abs\"\nprefix = \""+nominal+"\"\ncommand = '"+cmd+"'\n")
	}
	conf(`mkdir -p "$DESTDIR` + real + `/bin" "$DESTDIR` + real + `/lib" && cp tool "$DESTDIR` + real + `/bin/tool" && echo x > "$DESTDIR` + real + `/lib/libx.so"`)
	out, code := e.kilnInstall(build, nil)
	if code != 0 || !strings.Contains(out, "treating "+real) {
		t.Fatalf("install: exit %d\n%s", code, tail(out, 800))
	}
	for _, f := range []string{"bin/tool", "lib/libx.so"} {
		if _, err := os.Stat(filepath.Join(real, f)); err != nil {
			t.Errorf("%s wasn't installed: %v", f, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(real, ".kiln", "owners.json")); !strings.Contains(string(b), "bin/tool") {
		t.Errorf("ownership wasn't recorded in the real prefix")
	}
	// Some files under the prefix and some outside: the outside ones are
	// reported.
	conf(`mkdir -p "$DESTDIR` + nominal + `/bin" "$DESTDIR` + real + `/lib" && cp tool "$DESTDIR` + nominal + `/bin/tool" && echo x > "$DESTDIR` + real + `/lib/stray.so"`)
	out, code = e.kilnInstall(build, nil)
	if code != 0 || !strings.Contains(out, "outside "+nominal) {
		t.Errorf("mixed destinations: exit %d\n%s", code, tail(out, 800))
	}
}
