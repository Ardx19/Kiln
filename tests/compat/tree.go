package compat

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Ardx19/kiln/internal/fixture"
)

// Tree is a configured fixture: a source dir and a build dir under Root.
// Every run restores Root from the same snapshot, at the same absolute
// path, because generators bake absolute paths into the build dir.
type Tree struct {
	Root     string
	SrcDir   string
	BuildDir string
	LogPath  string
	Info     fixture.Info
	env      []string // environment for every build command (Meson's NINJA)
}

// Kind names a fixture flavour for the harness.
type Kind struct {
	Name string
	Spec fixture.Spec
	// MultiConfig configures a CMake fixture with Ninja Multi-Config,
	// building Debug and Release by default.
	MultiConfig bool
}

// Kinds is every fixture flavour the self-test covers.
var Kinds = []Kind{
	{Name: "cmake", Spec: fixture.Spec{Generator: fixture.CMake}},
	{Name: "cmake-rsp", Spec: fixture.Spec{Generator: fixture.CMake, RSP: true}},
	{Name: "cmake-multi", Spec: fixture.Spec{Generator: fixture.CMake}, MultiConfig: true},
	{Name: "meson", Spec: fixture.Spec{Generator: fixture.Meson}},
	{Name: "ninja", Spec: fixture.Spec{Generator: fixture.Ninja}},
}

// Setup generates, configures and builds a fixture with the reference
// Ninja, then snapshots it next to root. If the wall clock steps during
// setup, the snapshot's mtimes can't be trusted (and Meson may refuse to
// configure, reporting clock skew), so it starts over, up to three times.
func Setup(root string, k Kind, ref Tool, tools Tools) (*Tree, error) {
	for attempt := 1; ; attempt++ {
		clock := watchClock()
		t, err := setup(root, k, ref, tools)
		if stepped := clock.Stop(); !stepped || attempt == 3 {
			return t, err
		}
	}
}

func setup(root string, k Kind, ref Tool, tools Tools) (*Tree, error) {
	for _, p := range []string{root, snapshot(root)} {
		if err := os.RemoveAll(p); err != nil {
			return nil, err
		}
	}
	spec := k.Spec
	spec.Seed, spec.Libs, spec.Sources, spec.Headers = 1, 4, 4, 10
	info, err := fixture.Write(filepath.Join(root, "src"), spec)
	if err != nil {
		return nil, err
	}
	t := &Tree{Root: root, SrcDir: filepath.Join(root, "src"), BuildDir: filepath.Join(root, "build"), Info: info}
	t.env = append(withoutWindowsPath(os.Environ()), "NINJA="+ref.Path)
	switch info.Generator {
	case fixture.CMake:
		if tools.CMake == "" {
			return nil, errSkip("cmake not found")
		}
		args := []string{"-S", t.SrcDir, "-B", t.BuildDir, "-G", "Ninja", "-DCMAKE_MAKE_PROGRAM=" + ref.Path}
		if k.MultiConfig {
			args[5] = "Ninja Multi-Config"
			args = append(args, "-DCMAKE_CONFIGURATION_TYPES=Debug;Release",
				"-DCMAKE_CROSS_CONFIGS=all", "-DCMAKE_DEFAULT_CONFIGS=all")
		}
		err = t.run(tools.CMake, args...)
	case fixture.Meson:
		if tools.Meson == "" {
			return nil, errSkip("meson not found")
		}
		err = t.run(tools.Meson, "setup", t.BuildDir, t.SrcDir, "-Dbackend_max_links=1")
	case fixture.Ninja:
		t.BuildDir = t.SrcDir
	}
	if err != nil {
		return nil, err
	}
	t.LogPath = filepath.Join(t.BuildDir, info.LogDir, ".ninja_log")
	if err := t.run(ref.Path, "-C", t.BuildDir); err != nil {
		return nil, fmt.Errorf("initial build: %w", err)
	}
	return t, copyTree(root, snapshot(root))
}

// withoutWindowsPath drops WSL's /mnt/* entries from PATH. Every lookup
// there goes through the slow Windows filesystem bridge: Meson's
// regeneration probes dozens of clang-tidy names along PATH and took 35 s
// instead of a few.
func withoutWindowsPath(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			var keep []string
			for _, dir := range filepath.SplitList(v) {
				if !strings.HasPrefix(dir, "/mnt/") {
					keep = append(keep, dir)
				}
			}
			kv = "PATH=" + strings.Join(keep, string(filepath.ListSeparator))
		}
		out = append(out, kv)
	}
	return out
}

// Restore resets Root to the snapshot taken by Setup.
func (t *Tree) Restore() error {
	if err := os.RemoveAll(t.Root); err != nil {
		return err
	}
	return copyTree(snapshot(t.Root), t.Root)
}

func snapshot(root string) string { return root + ".snap" }

// copyTree copies with cp -a, which keeps nanosecond mtimes and symlinks.
func copyTree(from, to string) error {
	out, err := exec.Command("cp", "-a", from, to).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp -a %s %s: %v: %s", from, to, err, out)
	}
	return nil
}

func (t *Tree) run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = t.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, tail(string(out), 2000))
	}
	return nil
}

// LogEntries maps each output in .ninja_log to its latest line. Ninja
// recompacts the log at startup, so the harness compares latest entries
// rather than appended bytes.
func (t *Tree) LogEntries() (version string, entries map[string]string, err error) {
	entries = map[string]string{}
	f, err := os.Open(t.LogPath)
	if os.IsNotExist(err) {
		return "", entries, nil
	}
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "# ninja log ") {
			version = strings.TrimPrefix(line, "# ninja log ")
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) == 5 {
			entries[fields[3]] = line
		}
	}
	return version, entries, sc.Err()
}

// changed returns the outputs whose log entry is new or different.
func changed(before, after map[string]string) []string {
	var out []string
	for o, line := range after {
		if before[o] != line {
			out = append(out, o)
		}
	}
	sort.Strings(out)
	return out
}

type skipError string

func (e skipError) Error() string { return string(e) }

func errSkip(msg string) error { return skipError(msg) }

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
