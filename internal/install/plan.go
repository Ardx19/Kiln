// Package install makes installs safe to share a prefix. Every file kiln
// installs is recorded in <prefix>/.kiln/owners.json under the (project,
// build tree) that installed it, and an install is staged first, planned
// against those records, and committed through a journal, so a crash
// leaves the prefix exactly as it was or exactly as it will be.
//
// Rules for tree T of project P installing a staged file set N:
//
//  1. Files T installed last time and N no longer holds are removed.
//  2. Files owned by another tree of P (a variant: Debug vs Release,
//     another worktree) are refused unless Swap; Swap removes that tree's
//     whole set.
//  3. Files owned by a different project are refused unless Force.
//  4. Existing files nobody owns are adopted if their content is
//     identical, and refused otherwise unless Adopt.
//  5. A file about to be removed or overwritten must still match its
//     recorded hash; modified files block the install unless Force.
//
// Git-tracked files are never removed, and not overwritten without Force.
package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Dir is the per-prefix state directory.
const Dir = ".kiln"

// OwnersVersion is the format of owners.json.
const OwnersVersion = 1

// Entry describes one installed path.
type Entry struct {
	Type string `json:"type"` // "file" or "symlink"
	Hash string `json:"hash,omitempty"`
	Mode uint32 `json:"mode,omitempty"`
	Link string `json:"link,omitempty"`
}

// Same reports whether two entries have the same content (modes aside).
func (e Entry) Same(o Entry) bool {
	if e.Type != o.Type {
		return false
	}
	if e.Type == "symlink" {
		return e.Link == o.Link
	}
	return e.Hash == o.Hash
}

// Set is the files one build tree installed.
type Set struct {
	Project  string           `json:"project"`
	Tree     string           `json:"tree"`
	BuildDir string           `json:"build_dir,omitempty"`
	Time     time.Time        `json:"time"`
	Files    map[string]Entry `json:"files"`
}

// Owners is owners.json.
type Owners struct {
	Version int    `json:"version"`
	Sets    []*Set `json:"sets"`
}

// OwnersPath is where prefix keeps its records.
func OwnersPath(prefix string) string { return filepath.Join(prefix, Dir, "owners.json") }

// LoadOwners reads prefix's records; a missing file is empty. The raw
// bytes are returned too, for the journal.
func LoadOwners(prefix string) (Owners, []byte, error) {
	b, err := os.ReadFile(OwnersPath(prefix))
	if errors.Is(err, fs.ErrNotExist) {
		return Owners{Version: OwnersVersion}, nil, nil
	}
	if err != nil {
		return Owners{}, nil, err
	}
	var o Owners
	if err := json.Unmarshal(b, &o); err != nil {
		return Owners{}, nil, fmt.Errorf("%s: %v", OwnersPath(prefix), err)
	}
	if o.Version != OwnersVersion {
		return Owners{}, nil, fmt.Errorf("%s: unknown version %d", OwnersPath(prefix), o.Version)
	}
	return o, b, nil
}

// Stat describes the file or symlink at path; ok is false if it doesn't
// exist. A directory is reported with Type "dir".
func Stat(path string) (e Entry, ok bool, err error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscallENOTDIR) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		l, err := os.Readlink(path)
		return Entry{Type: "symlink", Link: l}, err == nil, err
	case fi.IsDir():
		return Entry{Type: "dir"}, true, nil
	case !fi.Mode().IsRegular():
		return Entry{Type: "special"}, true, nil
	}
	h, err := hashFile(path)
	if err != nil {
		return Entry{}, false, err
	}
	return Entry{Type: "file", Hash: h, Mode: uint32(fi.Mode().Perm())}, true, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Scan lists the files and symlinks under root, by path relative to it.
func Scan(root string) (map[string]Entry, error) {
	files := map[string]Entry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		e, ok, err := Stat(path)
		if err != nil || !ok {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		files[filepath.ToSlash(rel)] = e
		return nil
	})
	return files, err
}

// Options say who is installing and how strict to be.
type Options struct {
	Prefix, Project, Tree, BuildDir string
	Swap, Force, Adopt              bool
	// Tracked reports whether prefix-relative path p is tracked by git.
	Tracked func(p string) bool
}

// Op is one change to the prefix.
type Op struct {
	Kind  string `json:"kind"` // "install" or "remove"
	Path  string `json:"path"` // relative to the prefix
	Entry Entry  `json:"entry"`
	Why   string `json:"why"` // new, replace, adopt, stale, swap
}

// Conflict is something the plan refuses to do.
type Conflict struct {
	Path   string
	Reason string
	Fix    string // the flag that overrides it
}

// Plan is everything an install will do.
type Plan struct {
	Prefix  string `json:"prefix"`
	Project string `json:"project"`
	Tree    string `json:"tree"`
	Stage   string `json:"stage"` // the staged files, laid out as under the prefix
	// StageDir is the staging area to delete when the transaction ends
	// (Stage is somewhere inside it); "" to leave it.
	StageDir  string     `json:"stage_dir,omitempty"`
	Ops       []Op       `json:"ops"`
	Unchanged int        `json:"unchanged"`
	NewDirs   []string   `json:"new_dirs"` // directories the install creates
	Kept      []string   `json:"kept,omitempty"`
	OwnersOld []byte     `json:"owners_old"` // owners.json as planned against (nil: none)
	OwnersNew Owners     `json:"owners_new"`
	Conflicts []Conflict `json:"-"`
}

// MakePlan works out what installing files (staged under stage) into the
// prefix would do.
func MakePlan(o Options, files map[string]Entry, stage string) (*Plan, error) {
	owners, raw, err := LoadOwners(o.Prefix)
	if err != nil {
		return nil, err
	}
	p := &Plan{Prefix: o.Prefix, Project: o.Project, Tree: o.Tree, Stage: stage, OwnersOld: raw}
	owner := map[string]*Set{}
	var ours *Set
	for _, s := range owners.Sets {
		if s.Project == o.Project && s.Tree == o.Tree {
			ours = s
		}
		for path := range s.Files {
			owner[path] = s
		}
	}
	swapped := map[*Set]bool{}
	tracked := o.Tracked
	if tracked == nil {
		tracked = func(string) bool { return false }
	}
	refuse := func(path, reason, fix string) {
		p.Conflicts = append(p.Conflicts, Conflict{Path: path, Reason: reason, Fix: fix})
	}

	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	forced := map[string]*Set{} // files taken over from another project
	for _, path := range paths {
		want := files[path]
		cur, exists, err := Stat(filepath.Join(o.Prefix, path))
		if err != nil {
			return nil, err
		}
		if exists && (cur.Type == "dir" || cur.Type == "special") {
			refuse(path, "is a "+cur.Type+", and the install puts a file there", "")
			continue
		}
		own := owner[path]
		why := "new"
		switch {
		case own != nil && own == ours:
			rec := own.Files[path]
			switch {
			case exists && cur.Same(want):
				p.Unchanged++
				continue
			case exists && !cur.Same(rec) && !o.Force:
				refuse(path, "was modified since it was installed", "--force")
				continue
			}
			why = "replace"
		case own != nil && own.Project == o.Project:
			if !o.Swap {
				refuse(path, fmt.Sprintf("is owned by another tree of %s (%s)", own.Project, own.BuildDir), "--swap")
				continue
			}
			swapped[own] = true
			why = "replace"
		case own != nil:
			if !o.Force {
				refuse(path, fmt.Sprintf("belongs to project %s", own.Project), "--force")
				continue
			}
			forced[path] = own
			why = "replace"
		case exists && cur.Same(want):
			why = "adopt"
		case exists:
			if !o.Adopt && !o.Force {
				refuse(path, "already exists and nothing owns it", "--adopt")
				continue
			}
			why = "replace"
		}
		if exists && why == "replace" && tracked(path) && !o.Force {
			refuse(path, "is tracked by git", "--force")
			continue
		}
		p.Ops = append(p.Ops, Op{Kind: "install", Path: path, Entry: want, Why: why})
	}

	// Rule 1 and Swap: remove what no longer belongs.
	remove := func(s *Set, why string) {
		keys := make([]string, 0, len(s.Files))
		for path := range s.Files {
			keys = append(keys, path)
		}
		sort.Strings(keys)
		for _, path := range keys {
			if _, still := files[path]; still {
				continue
			}
			rec := s.Files[path]
			cur, exists, err := Stat(filepath.Join(o.Prefix, path))
			switch {
			case err != nil || !exists:
				continue // already gone
			case tracked(path):
				p.Kept = append(p.Kept, path)
				continue
			case !cur.Same(rec) && !o.Force:
				refuse(path, "was modified since it was installed, so it won't be removed", "--force")
				continue
			}
			p.Ops = append(p.Ops, Op{Kind: "remove", Path: path, Entry: cur, Why: why})
		}
	}
	if ours != nil {
		remove(ours, "stale")
	}
	for s := range swapped {
		remove(s, "swap")
	}

	// The records afterwards.
	now := Owners{Version: OwnersVersion}
	mine := &Set{Project: o.Project, Tree: o.Tree, BuildDir: o.BuildDir, Time: time.Now().UTC().Truncate(time.Second), Files: files}
	for _, s := range owners.Sets {
		if s == ours || swapped[s] {
			continue
		}
		c := *s
		c.Files = map[string]Entry{}
		for path, e := range s.Files {
			if _, taken := forced[path]; taken {
				continue
			}
			c.Files[path] = e
		}
		if len(c.Files) > 0 {
			now.Sets = append(now.Sets, &c)
		}
	}
	now.Sets = append(now.Sets, mine)
	p.OwnersNew = now

	// Directories the install has to create.
	need := map[string]bool{}
	for _, op := range p.Ops {
		if op.Kind != "install" {
			continue
		}
		for d := filepath.Dir(op.Path); d != "." && d != "/"; d = filepath.Dir(d) {
			if need[d] {
				break
			}
			if _, err := os.Lstat(filepath.Join(o.Prefix, d)); errors.Is(err, fs.ErrNotExist) {
				need[d] = true
			} else {
				break
			}
		}
	}
	for d := range need {
		p.NewDirs = append(p.NewDirs, d)
	}
	sort.Strings(p.NewDirs)
	return p, nil
}

// Describe summarizes the plan for --dry-run and for the install report.
func (p *Plan) Describe() string {
	count := map[string]int{}
	for _, op := range p.Ops {
		count[op.Why]++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "install into %s (project %s):\n", p.Prefix, p.Project)
	fmt.Fprintf(&b, "  %d new, %d replaced, %d adopted, %d unchanged\n", count["new"], count["replace"], count["adopt"], p.Unchanged)
	fmt.Fprintf(&b, "  %d removed (%d no longer installed, %d from the tree being swapped out)\n", count["stale"]+count["swap"], count["stale"], count["swap"])
	if len(p.Kept) > 0 {
		fmt.Fprintf(&b, "  %d git-tracked files kept\n", len(p.Kept))
	}
	for _, op := range p.Ops {
		if op.Kind == "remove" || op.Why == "replace" {
			fmt.Fprintf(&b, "  %s %s\n", map[string]string{"remove": "remove ", "install": "replace"}[op.Kind], op.Path)
		}
	}
	return b.String()
}

// ConflictReport lists the refusals, with the flag that overrides each.
func (p *Plan) ConflictReport() string {
	var b strings.Builder
	for i, c := range p.Conflicts {
		if i == 20 {
			fmt.Fprintf(&b, "  ... and %d more\n", len(p.Conflicts)-20)
			break
		}
		fmt.Fprintf(&b, "  %s %s", c.Path, c.Reason)
		if c.Fix != "" {
			fmt.Fprintf(&b, " (override with %s)", c.Fix)
		}
		b.WriteString("\n")
	}
	return b.String()
}
