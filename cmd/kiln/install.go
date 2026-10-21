package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Ardx19/kiln/internal/config"
	"github.com/Ardx19/kiln/internal/generator"
	"github.com/Ardx19/kiln/internal/install"
)

const installUsage = `usage: %s install [-C DIR] [options]

Installs the build into its prefix, recording which tree owns each file.

  --dry-run      show what would change, change nothing
  --swap         replace the install of another tree of this project
  --adopt        take over existing files nobody owns
  --force        also take files of other projects, modified files and tracked files
  --no-build     don't build first
  --project NAME the project's name for ownership (default: from CMake or Meson)
  --config NAME  the configuration of a multi-config CMake build
  --stage        build and stage only; finish with: kiln install --commit STAGEDIR
  --commit DIR   commit a staged install (runs no build commands; fit for sudo)
  --recover      finish or undo an install a crash interrupted
`

type installFlags struct {
	dryRun, swap, adopt, force, noBuild, stage, recoverOnly bool
	project, config, commit                                 string
}

func runInstall(p program, args []string, stdout, stderr io.Writer) int {
	args, ok := cdFlag(p, args, stderr)
	if !ok {
		return 1
	}
	var f installFlags
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 == len(args) {
				fmt.Fprintf(stderr, "%s: error: %s needs a value\n", p.name, a)
				return ""
			}
			i++
			return args[i]
		}
		switch a {
		case "--dry-run", "-n":
			f.dryRun = true
		case "--swap":
			f.swap = true
		case "--adopt":
			f.adopt = true
		case "--force":
			f.force = true
		case "--no-build":
			f.noBuild = true
		case "--stage":
			f.stage = true
		case "--recover":
			f.recoverOnly = true
		case "--project":
			if f.project = next(); f.project == "" {
				return 1
			}
		case "--config":
			if f.config = next(); f.config == "" {
				return 1
			}
		case "--commit":
			if f.commit = next(); f.commit == "" {
				return 1
			}
		default:
			fmt.Fprintf(stderr, installUsage, p.name)
			return 1
		}
	}
	if err := doInstall(p, f, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	return 0
}

func doInstall(p program, f installFlags, stdout, stderr io.Writer) error {
	if f.commit != "" {
		plan, err := install.LoadPlan(filepath.Join(f.commit, "plan.json"))
		if err != nil {
			return err
		}
		if err := install.Commit(plan, filepath.Base(f.commit)); err != nil {
			return err
		}
		fmt.Fprint(stdout, plan.Describe())
		return nil
	}

	cwd, _ := os.Getwd()
	info, err := generator.Detect(".")
	if err != nil {
		return err
	}
	cfg, err := config.Load(filepath.Join(".kiln", "config.toml"))
	if err != nil {
		return err
	}
	if cfg.Install.Project != "" && f.project == "" {
		f.project = cfg.Install.Project
	}
	if f.project == "" {
		f.project = info.Project
	}
	if f.project == "" {
		return fmt.Errorf("can't tell this project's name; pass --project NAME or set [install] project in .kiln/config.toml")
	}
	prefix := info.Prefix
	if cfg.Install.Prefix != "" {
		prefix = cfg.Install.Prefix
	}
	if prefix == "" {
		return fmt.Errorf("can't tell where this project installs; set [install] prefix in .kiln/config.toml")
	}
	prefix, err = filepath.Abs(prefix)
	if err != nil {
		return err
	}
	if f.recoverOnly {
		if err := install.Recover(prefix); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s is consistent\n", prefix)
		return nil
	}

	if !f.noBuild {
		if code := runNinja(p, nil, stdout, stderr); code != 0 {
			return fmt.Errorf("the build failed (exit %d), so nothing was installed", code)
		}
	}
	tree, err := treeID(cwd)
	if err != nil {
		return err
	}

	// Stage inside the prefix when we can write there (so the commit is a
	// rename), else next to the build, for a later sudo ... --commit.
	id := time.Now().Format("20060102-150405") + "-" + tree[:8]
	writable := os.MkdirAll(prefix, 0o777) == nil && unix.Access(prefix, unix.W_OK) == nil
	stageDir := filepath.Join(cwd, ".kiln", "stage", id)
	if writable {
		stageDir = filepath.Join(prefix, install.Dir, "stage", id)
	}
	if err := os.MkdirAll(stageDir, 0o777); err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(stageDir)
		}
	}()
	before := snapshot(prefix)
	var cmd *exec.Cmd
	if cfg.Install.Command != "" {
		cmd = exec.Command("/bin/sh", "-c", cfg.Install.Command)
	} else if cmd, err = info.StageCommand(".", f.config); err != nil {
		return err
	}
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "DESTDIR="+stageDir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("staging the install (%s): %v\n%s", strings.Join(cmd.Args, " "), err, tailString(out.String(), 1500))
	}
	if after := snapshot(prefix); before != nil && after != nil {
		if changed := diffSnapshots(before, after); len(changed) > 0 {
			fmt.Fprintf(stderr, "%s: warning: the install wrote %d file(s) straight into %s instead of the staging directory, "+
				"so they bypass ownership tracking (the install script ignores DESTDIR), for example %s\n", p.name, len(changed), prefix, changed[0])
		}
	}

	stageRoot := filepath.Join(stageDir, prefix)
	files, err := install.Scan(stageRoot)
	if err != nil {
		return fmt.Errorf("the install staged nothing under %s: %v", prefix, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("the install staged no files for %s", prefix)
	}
	tracked := gitTracked(prefix)
	plan, err := install.MakePlan(install.Options{
		Prefix: prefix, Project: f.project, Tree: tree, BuildDir: cwd,
		Swap: f.swap, Force: f.force, Adopt: f.adopt, Tracked: tracked,
	}, files, stageRoot)
	if err != nil {
		return err
	}
	plan.StageDir = stageDir
	if len(plan.Conflicts) > 0 {
		return fmt.Errorf("won't install into %s:\n%s", prefix, plan.ConflictReport())
	}
	if f.dryRun {
		fmt.Fprint(stdout, plan.Describe())
		return nil
	}
	if f.stage || !writable {
		if err := install.SavePlan(filepath.Join(stageDir, "plan.json"), plan); err != nil {
			return err
		}
		keep = true
		if !writable {
			fmt.Fprintf(stdout, "%s isn't writable by you, so the install is staged only.\n", prefix)
		}
		fmt.Fprint(stdout, plan.Describe())
		fmt.Fprintf(stdout, "staged as %s\nto install it: %s install --commit %s\n", stageDir, p.name, stageDir)
		return nil
	}
	if err := install.Commit(plan, id); err != nil {
		return err
	}
	excludeFromGit(prefix)
	fmt.Fprint(stdout, plan.Describe())
	fmt.Fprintf(stdout, "installed %d files into %s\n", len(plan.OwnersNew.Sets[len(plan.OwnersNew.Sets)-1].Files), prefix)
	return nil
}

func tailString(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// treeID is this build directory's identity, kept in .kiln/tree-id with
// the directory it was made for, so a copy of the directory gets its own.
func treeID(dir string) (string, error) {
	path := filepath.Join(dir, ".kiln", "tree-id")
	if b, err := os.ReadFile(path); err == nil {
		if id, d, ok := strings.Cut(strings.TrimSpace(string(b)), " "); ok && d == dir && len(id) >= 16 {
			return id, nil
		}
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", err
	}
	return id, os.WriteFile(path, []byte(id+" "+dir+"\n"), 0o644)
}

type fileStamp struct {
	size  int64
	mtime int64
}

// snapshot records every file under prefix (sizes and mtimes, not
// contents), skipping .git and kiln's own directory. A prefix that
// doesn't exist, or is too big to walk quickly, gives nil.
func snapshot(prefix string) map[string]fileStamp {
	m := map[string]fileStamp{}
	deadline := time.Now().Add(20 * time.Second)
	err := filepath.WalkDir(prefix, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == install.Dir) {
			return filepath.SkipDir
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("slow")
		}
		if !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				m[path] = fileStamp{fi.Size(), fi.ModTime().UnixNano()}
			}
		}
		return nil
	})
	if err != nil {
		return nil
	}
	return m
}

func diffSnapshots(before, after map[string]fileStamp) []string {
	var changed []string
	for path, s := range after {
		if b, ok := before[path]; !ok || b != s {
			changed = append(changed, path)
		}
	}
	return changed
}

// gitTracked returns a func reporting whether a prefix-relative path is
// tracked by git, if the prefix is inside a repository; else nil.
func gitTracked(prefix string) func(string) bool {
	out, err := exec.Command("git", "-C", prefix, "ls-files", "-z").Output()
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			set[f] = true
		}
	}
	return func(p string) bool { return set[p] }
}

// excludeFromGit keeps .kiln/ out of `git status` when the prefix is a
// checkout.
func excludeFromGit(prefix string) {
	out, err := exec.Command("git", "-C", prefix, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude").Output()
	if err != nil {
		return
	}
	path := strings.TrimSpace(string(out))
	if b, _ := os.ReadFile(path); bytes.Contains(b, []byte("\n.kiln/")) || bytes.HasPrefix(b, []byte(".kiln/")) {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0o777)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, "\n.kiln/")
}
