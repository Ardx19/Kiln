// Package compat is the differential test harness: it applies the same
// edit scripts to the same snapshot of a build tree under two Ninja-style
// runners and compares what each would run (dry run) and what each ran
// (changed .ninja_log entries).
//
// Until kiln's engine exists, both runners are real Ninja (the self-test).
// The tests need the build tag "compat" and the tools from scripts/tools.sh.
package compat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Tool is an executable plus the version it reports.
type Tool struct {
	Path    string
	Version string
}

// Tools is the tool matrix a test run uses.
type Tools struct {
	Ninjas []Tool // one per .ninja_log format, oldest first
	CMake  string // "" if unavailable
	Meson  string // "" if unavailable
}

// RepoRoot returns the repository root, found from the test's directory.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// WorkDir is where the harness keeps fixture trees: KILN_WORK, or
// <repo>/.work. Runs inside the CI image use a separate directory, because
// trees bake in absolute paths (/src/... there) that don't exist outside.
func WorkDir(repo string) string {
	if w := os.Getenv("KILN_WORK"); w != "" {
		return w
	}
	return filepath.Join(repo, ".work")
}

// FindTools looks for tools in this order:
//   - KILN_COMPAT_NINJAS (colon-separated paths), KILN_COMPAT_CMAKE, KILN_COMPAT_MESON
//   - KILN_TOOLS (default <repo>/.tools), laid out by scripts/tools.sh
//   - ninja, cmake and meson on PATH
func FindTools(repo string) (Tools, error) {
	var t Tools
	toolsDir := os.Getenv("KILN_TOOLS")
	if toolsDir == "" {
		toolsDir = filepath.Join(repo, ".tools")
	}

	var ninjas []string
	if env := os.Getenv("KILN_COMPAT_NINJAS"); env != "" {
		ninjas = strings.Split(env, ":")
	} else {
		found, _ := filepath.Glob(filepath.Join(toolsDir, "ninja", "*", "ninja"))
		ninjas = append(ninjas, found...)
		if p, err := exec.LookPath("ninja"); err == nil {
			ninjas = append(ninjas, p)
		}
	}
	seen := map[string]bool{}
	for _, p := range ninjas {
		v, err := version(p)
		if err != nil {
			return t, err
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		t.Ninjas = append(t.Ninjas, Tool{Path: p, Version: v})
	}
	sort.Slice(t.Ninjas, func(i, j int) bool { return versionLess(t.Ninjas[i].Version, t.Ninjas[j].Version) })

	t.CMake = pick(os.Getenv("KILN_COMPAT_CMAKE"), newest(filepath.Join(toolsDir, "cmake", "*", "bin", "cmake")), "cmake")
	t.Meson = pick(os.Getenv("KILN_COMPAT_MESON"), filepath.Join(toolsDir, "meson", "bin", "meson"), "meson")
	return t, nil
}

// pick returns the first candidate that exists as a path or on PATH.
func pick(candidates ...string) string {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

func newest(glob string) string {
	found, _ := filepath.Glob(glob)
	best := ""
	for _, p := range found {
		v := filepath.Base(filepath.Dir(filepath.Dir(p)))
		if best == "" || versionLess(filepath.Base(filepath.Dir(filepath.Dir(best))), v) {
			best = p
		}
	}
	return best
}

func version(path string) (string, error) {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", path, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// versionLess compares dotted numeric versions ("1.10.1" < "1.11.0").
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		var x, y int
		fmt.Sscan(as[i], &x)
		fmt.Sscan(bs[i], &y)
		if x != y {
			return x < y
		}
	}
	return len(as) < len(bs)
}
