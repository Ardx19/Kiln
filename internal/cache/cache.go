// Package cache manages ccache for builds: its settings (exported to the
// compile commands), optional injection of ccache in front of compilers
// that nothing launches it for, and hit-rate and size statistics.
package cache

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Ardx19/kiln/internal/config"
)

// Settings are the merged [cache] tables.
type Settings struct {
	Dir, BaseDir, MaxSize string
	HashDir               bool // ccache's default is true
	Inject                bool
}

// Merge combines [cache] tables, least specific first; later values win.
func Merge(cs ...config.Cache) Settings {
	s := Settings{HashDir: true}
	for _, c := range cs {
		if c.Dir != "" {
			s.Dir = c.Dir
		}
		if c.BaseDir != "" {
			s.BaseDir = c.BaseDir
		}
		if c.MaxSize != "" {
			s.MaxSize = c.MaxSize
		}
		if c.HashDir != nil {
			s.HashDir = *c.HashDir
		}
		if c.Inject != nil {
			s.Inject = *c.Inject
		}
	}
	return s
}

// Active reports whether kiln manages the cache at all.
func (s Settings) Active() bool {
	return s.Inject || s.Dir != "" || s.BaseDir != "" || s.MaxSize != "" || !s.HashDir
}

// Env returns the CCACHE_* variables for compile commands.
func (s Settings) Env() []string {
	var env []string
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	add("CCACHE_DIR", s.Dir)
	add("CCACHE_BASEDIR", s.BaseDir)
	add("CCACHE_MAXSIZE", s.MaxSize)
	if !s.HashDir {
		env = append(env, "CCACHE_NOHASHDIR=1")
	}
	return env
}

// Counters are ccache's statistics.
type Counters map[string]uint64

// ReadStats runs `ccache --print-stats` with env (NAME=value) added.
func ReadStats(env []string) (Counters, error) {
	cmd := exec.Command("ccache", "--print-stats")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ccache --print-stats: %v", err)
	}
	c := Counters{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			c[k] = n
		}
	}
	return c, nil
}

// Hits and Misses count cacheable compiles.
func (c Counters) Hits() uint64   { return c["direct_cache_hit"] + c["preprocessed_cache_hit"] }
func (c Counters) Misses() uint64 { return c["cache_miss"] }

// Since returns the change from before to c.
func (c Counters) Since(before Counters) Counters {
	d := Counters{}
	for k, v := range c {
		if v >= before[k] {
			d[k] = v - before[k]
		}
	}
	return d
}

// Rate is the share of cacheable compiles that hit, in percent.
func (c Counters) Rate() float64 {
	if t := c.Hits() + c.Misses(); t > 0 {
		return 100 * float64(c.Hits()) / float64(t)
	}
	return 0
}

var (
	// compilerName matches gcc, g++, cc, c++, clang and clang++ with an
	// optional cross prefix and version suffix.
	compilerName = regexp.MustCompile(`^(?:[A-Za-z0-9_.]+-)*(?:gcc|g\+\+|cc|c\+\+|clang|clang\+\+)(?:-[0-9][0-9.]*)?$`)
	launchers    = map[string]bool{"ccache": true, "sccache": true, "distcc": true, "icecc": true, "buildcache": true}
)

// Inject returns command with ccache in front of its compiler. If the
// command can't safely be given a launcher it returns it unchanged and
// says why. Only a plain `[cd DIR &&] [VAR=value...] compiler ... -c ...`
// is rewritten: anything with a pipe, a second command or a subshell is
// left alone.
func Inject(command string) (string, string) {
	rest := command
	prefix := ""
	if strings.HasPrefix(rest, "cd ") {
		i := strings.Index(rest, " && ")
		if i < 0 {
			return command, "not a compile command"
		}
		prefix, rest = rest[:i+4], rest[i+4:]
	}
	if strings.ContainsAny(rest, "|;`") || strings.Contains(rest, "&&") || strings.Contains(rest, "$(") || strings.Contains(rest, "||") {
		return command, "more than one command"
	}
	words := strings.Fields(rest)
	i := 0
	for i < len(words) && strings.Contains(words[i], "=") && !strings.HasPrefix(words[i], "-") && !strings.Contains(words[i], "/") {
		i++ // environment assignments
	}
	if i >= len(words) {
		return command, "not a compile command"
	}
	first := filepath.Base(words[i])
	if launchers[first] || first == "env" || first == "cmake" {
		return command, "already has a launcher"
	}
	if !compilerName.MatchString(first) {
		return command, "not a direct compiler call"
	}
	hasC := false
	for _, w := range words[i+1:] {
		if w == "-c" {
			hasC = true
		}
		if w == "-E" || w == "-S" || w == "-M" || w == "-MM" {
			return command, "preprocess or assemble only"
		}
	}
	if !hasC {
		return command, "not a compile (no -c)"
	}
	words = append(words[:i], append([]string{"ccache"}, words[i:]...)...)
	return prefix + strings.Join(words, " "), ""
}

// SuggestBaseDir proposes a base_dir for src: the nearest common parent of
// its git worktrees, "" if it has only one.
func SuggestBaseDir(src string) string {
	out, err := exec.Command("git", "-C", src, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return ""
	}
	var trees []string
	for _, line := range bytes.Split(out, []byte("\n")) {
		if p, ok := bytes.CutPrefix(line, []byte("worktree ")); ok {
			trees = append(trees, string(p))
		}
	}
	if len(trees) < 2 {
		return ""
	}
	sort.Strings(trees)
	common := strings.Split(trees[0], "/")
	for _, t := range trees[1:] {
		parts := strings.Split(t, "/")
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	if len(common) <= 1 {
		return ""
	}
	return strings.Join(common, "/")
}
