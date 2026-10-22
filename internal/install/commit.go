package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

var syscallENOTDIR = syscall.ENOTDIR

// SavePlan writes a staged plan to path, for a later --commit.
func SavePlan(path string, p *Plan) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

// LoadPlan reads a plan written by SavePlan.
func LoadPlan(path string) (*Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return &p, nil
}

// Journal states.
const (
	stateCommitting = "committing"
	stateCommitted  = "committed"
)

// journal is <prefix>/.kiln/txn/<id>/journal.json. Until its state is
// "committed", recovery rolls the prefix back to what it was.
type journal struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Plan  *Plan  `json:"plan"`
}

func txnDir(prefix, id string) string { return filepath.Join(prefix, Dir, "txn", id) }

func writeFileSync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeAtomic replaces path with data: write a temporary file, sync it,
// rename it over path, and sync the directory.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (j *journal) save(dir string) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "journal.json"), b)
}

// crashAt kills the process (SIGKILL, so nothing runs) at the n-th time
// step is reached, if $KILN_TEST_CRASH_AT is "step:n". Tests use it to
// check recovery at every point of a commit.
var crashCount = map[string]int{}

// crashHook, if set, is called at each crash point; tests make it panic.
var crashHook func(step string)

func crashAt(step string) {
	if crashHook != nil {
		crashHook(step)
	}
	spec := os.Getenv("KILN_TEST_CRASH_AT")
	name, nstr, ok := strings.Cut(spec, ":")
	if !ok || name != step {
		return
	}
	n, _ := strconv.Atoi(nstr)
	crashCount[step]++
	if crashCount[step] == n {
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
}

// lockPrefix takes <prefix>/.kiln/owners.lock; the returned func releases
// it.
func lockPrefix(prefix string) (func(), error) {
	if err := os.MkdirAll(filepath.Join(prefix, Dir), 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(prefix, Dir, "owners.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// Commit carries out plan: it moves the staged files into the prefix,
// removes what's stale, writes the new records, and only then marks the
// transaction committed. A crash before that is rolled back by Recover.
// It runs no build commands, so it can be run by root on a staged tree.
func Commit(plan *Plan, id string) error {
	if len(plan.Conflicts) > 0 {
		return fmt.Errorf("the plan has conflicts:\n%s", plan.ConflictReport())
	}
	if err := validate(plan); err != nil {
		return err
	}
	unlock, err := lockPrefix(plan.Prefix)
	if err != nil {
		return err
	}
	defer unlock()
	if err := recoverLocked(plan.Prefix); err != nil {
		return err
	}
	// The plan was made against these records; if they've changed since,
	// it may no longer be safe.
	_, raw, err := LoadOwners(plan.Prefix)
	if err != nil {
		return err
	}
	if string(raw) != string(plan.OwnersOld) {
		return errors.New("the prefix changed since this install was planned; stage it again")
	}
	dir := txnDir(plan.Prefix, id)
	backup := filepath.Join(dir, "backup")
	if err := os.MkdirAll(backup, 0o777); err != nil {
		return err
	}
	j := &journal{ID: id, State: stateCommitting, Plan: plan}
	if err := j.save(dir); err != nil {
		return err
	}
	for _, d := range plan.NewDirs { // parents first
		if err := os.MkdirAll(filepath.Join(plan.Prefix, d), 0o777); err != nil {
			return rollbackAfter(plan, id, err)
		}
	}
	for _, op := range plan.Ops {
		if err := apply(plan, backup, op); err != nil {
			return rollbackAfter(plan, id, err)
		}
		crashAt("move")
	}
	b, err := json.MarshalIndent(plan.OwnersNew, "", "  ")
	if err != nil {
		return rollbackAfter(plan, id, err)
	}
	if err := os.MkdirAll(filepath.Join(plan.Prefix, Dir), 0o777); err != nil {
		return rollbackAfter(plan, id, err)
	}
	if err := writeAtomic(OwnersPath(plan.Prefix), append(b, '\n')); err != nil {
		return rollbackAfter(plan, id, err)
	}
	crashAt("owners")
	j.State = stateCommitted
	if err := j.save(dir); err != nil {
		return rollbackAfter(plan, id, err)
	}
	crashAt("committed")
	finish(plan, dir)
	crashAt("cleanup")
	return nil
}

// rollbackAfter undoes a failed commit and reports the failure.
func rollbackAfter(plan *Plan, id string, cause error) error {
	if err := rollback(plan, txnDir(plan.Prefix, id)); err != nil {
		return fmt.Errorf("%v (and rolling back failed: %v; run 'kiln install --recover')", cause, err)
	}
	return cause
}

// apply carries out one op: the file it replaces goes to backup first.
func apply(plan *Plan, backup string, op Op) error {
	dest := filepath.Join(plan.Prefix, op.Path)
	if _, err := os.Lstat(dest); err == nil {
		b := filepath.Join(backup, op.Path)
		if err := os.MkdirAll(filepath.Dir(b), 0o777); err != nil {
			return err
		}
		if err := os.Rename(dest, b); err != nil {
			return err
		}
	}
	if op.Kind == "remove" {
		return nil
	}
	return place(filepath.Join(plan.Stage, op.Path), dest, op.Entry)
}

// place moves a staged file or symlink to dest: a rename when they share
// a file system, else a copy next to dest and a rename. When run as root,
// the installed file belongs to root, not to whoever staged it: otherwise
// an unprivileged user could change files in a root-owned prefix.
func place(src, dest string, e Entry) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return err
	}
	if err := placeFile(src, dest, e); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return os.Lchown(dest, 0, 0)
	}
	return nil
}

func placeFile(src, dest string, e Entry) error {
	err := os.Rename(src, dest)
	if err == nil {
		return nil
	}
	var le *os.LinkError
	if !errors.As(err, &le) || !errors.Is(le.Err, syscall.EXDEV) {
		return err
	}
	tmp := dest + ".kiln-new"
	os.Remove(tmp)
	if e.Type == "symlink" {
		if err := os.Symlink(e.Link, tmp); err != nil {
			return err
		}
	} else {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(e.Mode))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		if err := errors.Join(out.Chmod(fs.FileMode(e.Mode)), out.Sync(), out.Close()); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	return os.Remove(src)
}

// finish cleans up a committed transaction: empty directories that
// removals left behind, and the transaction directory itself.
func finish(plan *Plan, dir string) {
	var dirs []string
	for _, op := range plan.Ops {
		if op.Kind == "remove" {
			for d := filepath.Dir(op.Path); d != "." && d != "/"; d = filepath.Dir(d) {
				dirs = append(dirs, d)
			}
		}
	}
	removeEmptyDirs(plan.Prefix, dirs)
	os.RemoveAll(dir)
	if plan.StageDir != "" {
		os.RemoveAll(plan.StageDir)
	}
}

// removeEmptyDirs removes the listed prefix-relative directories that are
// empty, deepest first. Directories are never removed otherwise.
func removeEmptyDirs(prefix string, dirs []string) {
	seen := map[string]bool{}
	var uniq []string
	for _, d := range dirs {
		if !seen[d] {
			seen[d] = true
			uniq = append(uniq, d)
		}
	}
	sort.Slice(uniq, func(i, j int) bool { return len(uniq[i]) > len(uniq[j]) })
	for _, d := range uniq {
		os.Remove(filepath.Join(prefix, d)) // fails unless empty
	}
}

// rollback puts the prefix back as it was before the transaction.
func rollback(plan *Plan, dir string) error {
	backup := filepath.Join(dir, "backup")
	for i := len(plan.Ops) - 1; i >= 0; i-- {
		op := plan.Ops[i]
		dest := filepath.Join(plan.Prefix, op.Path)
		b := filepath.Join(backup, op.Path)
		_, hadBackup := os.Lstat(b)
		switch {
		case hadBackup == nil:
			// Whatever is at dest now is ours (or a partial copy).
			os.Remove(dest)
			if err := os.Rename(b, dest); err != nil {
				return err
			}
		case op.Kind == "install":
			// No backup: either nothing happened yet (the staged file is
			// still there) or the file was new and has been placed.
			if _, err := os.Lstat(filepath.Join(plan.Stage, op.Path)); err != nil {
				os.Remove(dest)
			}
			os.Remove(dest + ".kiln-new")
		}
	}
	// The records go back too.
	if plan.OwnersOld == nil {
		os.Remove(OwnersPath(plan.Prefix))
	} else if err := writeAtomic(OwnersPath(plan.Prefix), plan.OwnersOld); err != nil {
		return err
	}
	removeEmptyDirs(plan.Prefix, plan.NewDirs)
	if plan.StageDir != "" {
		os.RemoveAll(plan.StageDir)
	}
	return os.RemoveAll(dir)
}

// Recover finishes or undoes transactions a crash left in prefix: a
// committed one is cleaned up, any other is rolled back.
func Recover(prefix string) error {
	unlock, err := lockPrefix(prefix)
	if err != nil {
		return err
	}
	defer unlock()
	return recoverLocked(prefix)
}

func recoverLocked(prefix string) error {
	ents, err := os.ReadDir(filepath.Join(prefix, Dir, "txn"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		dir := filepath.Join(prefix, Dir, "txn", e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "journal.json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue // staged, never committed
		}
		if err != nil {
			return err
		}
		var j journal
		if err := json.Unmarshal(b, &j); err != nil || j.Plan == nil {
			return fmt.Errorf("%s: unreadable journal: %v", dir, err)
		}
		if j.State == stateCommitted {
			finish(j.Plan, dir)
		} else if err := rollback(j.Plan, dir); err != nil {
			return fmt.Errorf("rolling back %s: %v", e.Name(), err)
		}
	}
	return nil
}

// validate checks a plan before anything is changed. A staged plan comes
// from a directory its owner controls, and the commit may run as root, so
// nothing in it is trusted: every path must stay inside the prefix, and
// every staged file must still be what was planned.
func validate(plan *Plan) error {
	if !filepath.IsAbs(plan.Prefix) {
		return fmt.Errorf("the plan's prefix %q isn't absolute", plan.Prefix)
	}
	realPrefix, err := filepath.EvalSymlinks(plan.Prefix)
	if err != nil {
		return err
	}
	for _, op := range plan.Ops {
		p := op.Path
		if p == "" || filepath.IsAbs(p) || filepath.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
			return fmt.Errorf("the plan has an unsafe path %q", p)
		}
		if p == Dir || strings.HasPrefix(p, Dir+"/") {
			return fmt.Errorf("the plan would change kiln's own directory (%q)", p)
		}
		if os.Geteuid() == 0 {
			// The directories above the file must not lead out of the
			// prefix, even through symlinks.
			dir := filepath.Dir(filepath.Join(plan.Prefix, p))
			for dir != plan.Prefix && dir != "/" {
				if real, err := filepath.EvalSymlinks(dir); err == nil {
					if real != realPrefix && !strings.HasPrefix(real, realPrefix+"/") {
						return fmt.Errorf("%q would be written outside the prefix, through a symlink", p)
					}
					break
				}
				dir = filepath.Dir(dir)
			}
		}
		if op.Kind == "install" {
			got, ok, err := Stat(filepath.Join(plan.Stage, p))
			if err != nil || !ok || !got.Same(op.Entry) {
				return fmt.Errorf("the staged file %q is missing or changed since it was staged; stage the install again", p)
			}
		}
	}
	return nil
}
