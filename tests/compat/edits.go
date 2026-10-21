package compat

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// fixedSteps run in this order before the random ones. change_flag appears
// twice so the flag is flipped and flipped back (two regenerations).
var fixedSteps = []string{
	"noop", "touch_source", "touch_header", "edit_header", "add_include",
	"remove_include", "restat_noop", "edit_codegen_dep", "change_flag",
	"delete_object", "delete_deps_log", "inject_error", "fix_error",
	"mass_touch", "change_flag", "touch_dyndep_input",
}

var randomSteps = []string{
	"touch_source", "touch_header", "edit_header", "add_include", "remove_include",
	"restat_noop", "edit_codegen_dep", "delete_object", "mass_touch",
	"touch_dyndep_input",
}

// Script returns the fixed steps followed by extra random ones. The same
// seed always gives the same script.
func Script(seed int64, extra int) []string {
	rng := rand.New(rand.NewSource(seed))
	steps := append([]string(nil), fixedSteps...)
	for i := 0; i < extra; i++ {
		steps = append(steps, randomSteps[rng.Intn(len(randomSteps))])
	}
	return steps
}

// Apply performs one edit. rng must be seeded identically for every runner
// so both see the same edit.
func (t *Tree) Apply(step string, rng *rand.Rand) error {
	src := func(rel string) string { return filepath.Join(t.SrcDir, rel) }
	sources := sorted(t.Info.Sources)
	headers := sorted(t.Info.Headers)
	switch step {
	case "noop":
		return nil
	case "touch_source":
		return touch(src(sources[rng.Intn(len(sources))]))
	case "touch_header":
		return touch(src(headers[rng.Intn(len(headers))]))
	case "edit_header":
		return appendLine(src(headers[rng.Intn(len(headers))]), "/* edited */")
	case "add_include":
		return addInclude(src(sources[rng.Intn(len(sources))]), headers, rng)
	case "remove_include":
		return removeInclude(src(sources[rng.Intn(len(sources))]), rng)
	case "restat_noop":
		return touch(src(t.Info.CodegenInput))
	case "touch_dyndep_input":
		if t.Info.DyndepInput == "" {
			return nil // only the hand-written fixture has a dyndep edge
		}
		return touch(src(t.Info.DyndepInput))
	case "edit_codegen_dep":
		return appendLine(src(t.Info.CodegenDep), fmt.Sprintf("KILN_EDIT_%d 1", rng.Intn(1_000_000)))
	case "change_flag":
		return toggleFlag(src(t.Info.BuildFile), t.Info.FlagsMarker)
	case "delete_object":
		_, entries, err := t.LogEntries()
		if err != nil {
			return err
		}
		var objs []string
		for o := range entries {
			if strings.HasSuffix(o, ".o") {
				objs = append(objs, o)
			}
		}
		if len(objs) == 0 {
			return nil
		}
		sort.Strings(objs)
		return removeIfExists(filepath.Join(t.BuildDir, objs[rng.Intn(len(objs))]))
	case "delete_deps_log":
		return removeIfExists(filepath.Join(t.BuildDir, t.Info.LogDir, ".ninja_deps"))
	case "inject_error":
		return appendLine(src(sources[0]), injectedError)
	case "fix_error":
		for _, s := range sources {
			if err := removeLine(src(s), injectedError); err != nil {
				return err
			}
		}
		return nil
	case "mass_touch":
		for _, f := range append(sources, headers...) {
			if err := touch(src(f)); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown step %q", step)
}

const injectedError = "#error kiln-injected"

const (
	includesBegin = "// kiln:includes begin"
	includesEnd   = "// kiln:includes end"
)

func sorted(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

// touch sets a file's times to "now" the way touch(1) does: the kernel picks
// the time from the same coarse clock it uses for file writes. Setting
// time.Now() explicitly can produce a timestamp ahead of that clock (up to
// one tick, 10 ms with CONFIG_HZ=100), which makes Ninja 1.12+ see a file
// as newer than a build step that started after it.
func touch(path string) error {
	now := []unix.Timespec{{Nsec: unix.UTIME_NOW}, {Nsec: unix.UTIME_NOW}}
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, now, 0)
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func removeLine(path, line string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !strings.Contains(string(b), line+"\n") {
		return nil
	}
	return os.WriteFile(path, []byte(strings.Replace(string(b), line+"\n", "", 1)), 0o644)
}

// includeBlock returns the lines of a source and the indexes of its
// include markers.
func includeBlock(path string) (lines []string, begin, end int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, 0, err
	}
	lines = strings.Split(string(b), "\n")
	begin, end = -1, -1
	for i, l := range lines {
		switch l {
		case includesBegin:
			begin = i
		case includesEnd:
			end = i
		}
	}
	if begin < 0 || end < begin {
		return nil, 0, 0, fmt.Errorf("%s: include markers not found", path)
	}
	return lines, begin, end, nil
}

func addInclude(path string, headers []string, rng *rand.Rand) error {
	lines, begin, end, err := includeBlock(path)
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for _, l := range lines[begin+1 : end] {
		present[l] = true
	}
	var candidates []string
	for _, h := range headers {
		inc := fmt.Sprintf("#include %q", filepath.Base(h))
		if !present[inc] {
			candidates = append(candidates, inc)
		}
	}
	if len(candidates) == 0 {
		return touch(path)
	}
	inc := candidates[rng.Intn(len(candidates))]
	lines = append(lines[:end], append([]string{inc}, lines[end:]...)...)
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

func removeInclude(path string, rng *rand.Rand) error {
	lines, begin, end, err := includeBlock(path)
	if err != nil {
		return err
	}
	if end == begin+1 {
		return touch(path)
	}
	i := begin + 1 + rng.Intn(end-begin-1)
	lines = append(lines[:i], lines[i+1:]...)
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// toggleFlag flips the marker flag between -O0 and -O1.
func toggleFlag(path, marker string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s, alt := string(b), strings.Replace(marker, "0", "1", 1)
	switch {
	case strings.Contains(s, marker):
		s = strings.Replace(s, marker, alt, 1)
	case strings.Contains(s, alt):
		s = strings.Replace(s, alt, marker, 1)
	default:
		return fmt.Errorf("%s: flag marker %q not found", path, marker)
	}
	return os.WriteFile(path, []byte(s), 0o644)
}
