package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/Ardx19/kiln/internal/build"
	"github.com/Ardx19/kiln/internal/cache"
	"github.com/Ardx19/kiln/internal/config"
)

// cacheRun is one build's use of the compiler cache: the settings from
// the [cache] tables, injection of ccache into compile commands, and the
// build's hit rate.
type cacheRun struct {
	s        cache.Settings
	before   cache.Counters
	injected int
	skipped  map[string]int
}

func newCacheRun(cfgs []config.Config, warn func(string)) *cacheRun {
	tables := make([]config.Cache, len(cfgs))
	for i, c := range cfgs {
		tables[i] = c.Cache
	}
	c := &cacheRun{s: cache.Merge(tables...), skipped: map[string]int{}}
	if c.s.Inject {
		if _, err := exec.LookPath("ccache"); err != nil {
			warn("[cache] inject is on, but ccache isn't installed; compiling without it")
			c.s.Inject = false
		}
	}
	return c
}

// addEnv adds the CCACHE_* settings to a job environment (nil: kiln's).
func (c *cacheRun) addEnv(env []string) []string {
	vars := c.s.Env()
	if len(vars) == 0 {
		return env
	}
	if env == nil {
		env = os.Environ()
	}
	return append(append([]string(nil), env...), vars...)
}

// rewrite returns the build's command rewriter, or nil.
func (c *cacheRun) rewrite() func(string) string {
	if !c.s.Inject {
		return nil
	}
	return func(command string) string {
		out, why := cache.Inject(command)
		switch {
		case why == "":
			c.injected++
		case strings.Contains(command, " -c ") && why != "preprocess or assemble only":
			c.skipped[why]++
		}
		return out
	}
}

// start notes ccache's counters before the build.
func (c *cacheRun) start() {
	if c.s.Active() {
		c.before, _ = cache.ReadStats(c.s.Env())
	}
}

// report says how the cache did during the build.
func (c *cacheRun) report(status *build.Status) {
	if !c.s.Active() {
		return
	}
	if c.s.Inject && c.injected+len(c.skipped) > 0 {
		msg := fmt.Sprintf("ccache added to %d compile commands", c.injected)
		var why []string
		for r, n := range c.skipped {
			why = append(why, fmt.Sprintf("%d %s", n, r))
		}
		sort.Strings(why)
		if len(why) > 0 {
			msg += "; not cacheable: " + strings.Join(why, ", ")
		}
		status.Info("%s", msg)
	}
	after, err := cache.ReadStats(c.s.Env())
	if err != nil || c.before == nil {
		return
	}
	if d := after.Since(c.before); d.Hits()+d.Misses() > 0 {
		status.Info("ccache: %d hits, %d misses (%.0f%% hit rate)", d.Hits(), d.Misses(), d.Rate())
	}
}

const cacheUsage = `usage: %s cache [-C DIR] stats
       %s cache [-C DIR] trim

stats shows the cache's size against its limit, its hit rate and how often it
had to clean up. trim removes old entries until the cache fits its limit
(set it with max_size under [cache] in .kiln/config.toml).
`

// runCache is `kiln cache stats` and `kiln cache trim`.
func runCache(p program, args []string, stdout, stderr io.Writer) int {
	args, ok := cdFlag(p, args, stderr)
	if !ok {
		return 1
	}
	if len(args) == 0 || (args[0] != "stats" && args[0] != "trim") || len(args) != 1 {
		fmt.Fprintf(stderr, cacheUsage, p.name, p.name)
		return 1
	}
	cfgs, info, err := loadConfigs()
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	if _, err := exec.LookPath("ccache"); err != nil {
		fmt.Fprintf(stderr, "%s: error: ccache isn't installed\n", p.name)
		return 1
	}
	tables := make([]config.Cache, len(cfgs))
	for i, c := range cfgs {
		tables[i] = c.Cache
	}
	s := cache.Merge(tables...)
	env := s.Env()
	ccache := func(a ...string) ([]byte, error) {
		cmd := exec.Command("ccache", a...)
		cmd.Env = append(os.Environ(), env...)
		return cmd.CombinedOutput()
	}
	if args[0] == "trim" {
		if out, err := ccache("--cleanup"); err != nil {
			fmt.Fprintf(stderr, "%s: error: %v\n%s", p.name, err, out)
			return 1
		}
		fmt.Fprintln(stdout, "cleaned up the cache")
	}
	st, err := cache.ReadStats(env)
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	conf := map[string]string{}
	if out, err := ccache("--show-config"); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(out)))
		for sc.Scan() {
			// "(default) max_size = 5.0 GB" or "(environment) cache_dir = /x"
			line := sc.Text()
			if i := strings.Index(line, ") "); i >= 0 {
				line = line[i+2:]
			}
			if k, v, ok := strings.Cut(line, " = "); ok {
				conf[k] = v
			}
		}
	}
	sizeKiB := st["cache_size_kibibyte"]
	limit, _ := config.ParseSize(strings.ReplaceAll(conf["max_size"], " ", ""))
	fmt.Fprintf(stdout, "cache directory: %s\n", conf["cache_dir"])
	if limit > 0 {
		fmt.Fprintf(stdout, "size: %s of %s (%.0f%%), %d files\n", size(sizeKiB<<10), size(limit), 100*float64(sizeKiB<<10)/float64(limit), st["files_in_cache"])
	} else {
		fmt.Fprintf(stdout, "size: %s, %d files\n", size(sizeKiB<<10), st["files_in_cache"])
	}
	fmt.Fprintf(stdout, "hits: %d, misses: %d (%.0f%% hit rate)\n", st.Hits(), st.Misses(), st.Rate())
	if limit > 0 && sizeKiB<<10 > limit {
		fmt.Fprintf(stdout, "the cache is over its limit; '%s cache trim' evicts down to it, and a limit this small keeps evicting\n", p.name)
	}
	if n := st["cleanups_performed"]; n > 0 {
		fmt.Fprintf(stdout, "cleanups performed: %d (the cache filled up and evicted entries", n)
		if limit > 0 && float64(sizeKiB<<10) > 0.9*float64(limit) {
			fmt.Fprint(stdout, "; it is nearly full, so a larger max_size would help")
		}
		fmt.Fprintln(stdout, ")")
	}
	if s.BaseDir == "" && info.Source != "" {
		if base := cache.SuggestBaseDir(info.Source); base != "" {
			fmt.Fprintf(stdout, "hint: this source tree has several git worktrees under %s; add\n  [cache]\n  base_dir = %q\nto .kiln/config.toml so they share cache entries\n", base, base)
		}
	}
	return 0
}

func size(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", n>>10)
}
