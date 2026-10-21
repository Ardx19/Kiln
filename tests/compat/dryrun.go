package compat

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/Ardx19/kiln/internal/manifest"
	"github.com/Ardx19/kiln/internal/state"
)

var (
	kilnOnce sync.Once
	kilnPath string
	kilnErr  error
)

// KilnBinary builds cmd/kiln once per test run and returns its path.
func KilnBinary(repo string) (string, error) {
	kilnOnce.Do(func() {
		dir, err := os.MkdirTemp("", "kiln-bin-")
		if err != nil {
			kilnErr = err
			return
		}
		kilnPath = filepath.Join(dir, "kiln")
		cmd := exec.Command("go", "build", "-o", kilnPath, "./cmd/kiln")
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			kilnErr = fmt.Errorf("go build ./cmd/kiln: %v\n%s", err, out)
		}
	})
	return kilnPath, kilnErr
}

// DryRunReport compares `kiln -n` with `ninja -n` on one tree.
type DryRunReport struct {
	Dir      string
	Ninja    string
	Skipped  string
	Commands int // commands both would run
	Diffs    []string
}

// CompareDryRun runs `ninja -n -v` and `kiln -n -v` on a real tree and
// compares the commands each would run. Ninja runs under the same rules
// as CompareState: a version that writes the tree's log format, only
// after checking the deps log, with the state files fingerprinted.
func CompareDryRun(dir string, tools Tools, kiln string) DryRunReport {
	r := DryRunReport{Dir: dir}
	g, err := manifest.Load("build.ninja", manifest.Options{Dir: dir})
	if err != nil {
		r.Skipped = "manifest: " + err.Error()
		return r
	}
	logDir := dir
	if bd := g.Bindings.LookupVariable("builddir"); bd != "" {
		logDir = filepath.Join(dir, bd)
	}
	logPath, depsPath := filepath.Join(logDir, ".ninja_log"), filepath.Join(logDir, ".ninja_deps")
	blog, err := state.ReadBuildLog(logPath)
	var verr *state.VersionError
	if errors.As(err, &verr) {
		r.Skipped = verr.Error()
		return r
	}
	if err != nil {
		r.Diffs = append(r.Diffs, err.Error())
		return r
	}
	if dlog, err := state.ReadDepsLog(depsPath); err != nil || dlog.Corrupt != "" {
		r.Skipped = fmt.Sprintf("deps log unreadable or corrupt (%v)", err)
		return r
	}
	ninja, ok := ninjaForLog(tools, blog.Version)
	if !ok {
		r.Skipped = fmt.Sprintf("no Ninja here writes log v%d", blog.Version)
		return r
	}
	r.Ninja = ninja.Version

	before := fingerprint(logPath, depsPath)
	defer func() {
		if after := fingerprint(logPath, depsPath); after != before {
			r.Diffs = append(r.Diffs, "SAFETY: the state files changed: "+before+" -> "+after)
		}
	}()

	tree := &Tree{BuildDir: dir, env: withoutWindowsPath(os.Environ())}
	nExit, nCmds, err := tree.dryRun(Runner{"ninja", ninja.Path})
	if err != nil {
		r.Diffs = append(r.Diffs, err.Error())
		return r
	}
	kExit, kCmds, err := tree.dryRun(Runner{"kiln", kiln})
	if err != nil {
		r.Diffs = append(r.Diffs, err.Error())
		return r
	}
	if nExit != kExit {
		r.Diffs = append(r.Diffs, fmt.Sprintf("exit codes differ: ninja %d, kiln %d", nExit, kExit))
	}
	if onlyN, onlyK := setDiff(nCmds, kCmds), setDiff(kCmds, nCmds); len(onlyN)+len(onlyK) > 0 {
		r.Diffs = append(r.Diffs, fmt.Sprintf("dry runs differ\n  only ninja: %s\n  only kiln:  %s", short(onlyN), short(onlyK)))
	}
	r.Commands = len(nCmds)
	return r
}
