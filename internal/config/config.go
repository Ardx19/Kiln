// Package config reads the user's machine defaults from a TOML file:
//
//	[memory]
//	enabled  = true     # false is --mem=off
//	budget   = "8GiB"   # --mem-budget
//	min_jobs = 1        # --min-jobs
//
//	[env]               # also in <build>/.kiln/config.toml and <source>/.kiln.toml
//	mode  = "inherit"   # or "clean"
//	unset = ["PYTHONPATH", "CPATH"]
//	[env.set]     PYTHONNOUSERSITE = "1"
//	[env.prepend] LD_LIBRARY_PATH = ["/opt/deps/lib"]
//
//	[test]
//	command = "ctest"; junit = "report.xml"
//
//	[install]           # in <build>/.kiln/config.toml
//	command = "make install"   # honouring DESTDIR
//	project = "name"
//	prefix  = "/opt/name"
//
// Command-line flags override the file.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the contents of the file; unset values are zero.
type Config struct {
	Memory  Memory  `toml:"memory"`
	Install Install `toml:"install"`
	Env     Env     `toml:"env"`
	Test    Test    `toml:"test"`
	Cache   Cache   `toml:"cache"`
}

// Cache is the [cache] table: kiln-managed ccache.
type Cache struct {
	// Dir is the cache directory (CCACHE_DIR).
	Dir string `toml:"dir"`
	// BaseDir is the directory paths are made relative to (CCACHE_BASEDIR),
	// so identical sources checked out in different places share entries.
	BaseDir string `toml:"base_dir"`
	// MaxSize caps the cache (CCACHE_MAXSIZE), e.g. "20G".
	MaxSize string `toml:"max_size"`
	// HashDir says whether the working directory is part of the cache key
	// (ccache's default is yes); false lets different build directories
	// share entries.
	HashDir *bool `toml:"hash_dir"`
	// Inject runs ccache in front of compile commands that call a compiler
	// directly and have no launcher.
	Inject *bool `toml:"inject"`
}

// Env is the [env] table: the environment every command kiln runs sees.
type Env struct {
	// Mode is "inherit" (the default: start from kiln's environment) or
	// "clean" (start from Allow and a few basics only).
	Mode string `toml:"mode"`
	// Allow lists extra variables kept in clean mode.
	Allow []string `toml:"allow"`
	// Unset removes variables.
	Unset []string `toml:"unset"`
	// Set gives variables a value; Prepend and Append add entries to a
	// colon-separated variable such as PATH.
	Set     map[string]string   `toml:"set"`
	Prepend map[string][]string `toml:"prepend"`
	Append  map[string][]string `toml:"append"`
}

// Test is the [test] table, for `kiln test` on trees that are neither
// CMake nor Meson (or to override them).
type Test struct {
	// Command runs the tests, by sh -c in the build directory.
	Command string `toml:"command"`
	// JUnit is the XML report the command writes, relative to the build
	// directory.
	JUnit string `toml:"junit"`
}

// Install is the [install] table, for trees kiln can't recognize as CMake
// or Meson (and to override what it reads from them).
type Install struct {
	// Command installs the build into $DESTDIR, run by sh -c in the build
	// directory. It must honour DESTDIR.
	Command string `toml:"command"`
	// Project names the project for ownership records.
	Project string `toml:"project"`
	// Prefix is the install prefix, when the build system doesn't say.
	Prefix string `toml:"prefix"`
}

// Memory is the [memory] table.
type Memory struct {
	// Enabled is a pointer so that an unset value differs from false.
	Enabled *bool  `toml:"enabled"`
	Budget  string `toml:"budget"`
	MinJobs int    `toml:"min_jobs"`
}

// Path is where the file lives: $KILN_CONFIG, else kiln/config.toml in
// the user's config directory.
func Path() string {
	if p := os.Getenv("KILN_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "kiln", "config.toml")
}

// Load reads the file at path. A missing file is an empty configuration;
// a malformed one or an unknown key is an error.
func Load(path string) (Config, error) {
	var c Config
	if path == "" {
		return c, nil
	}
	md, err := toml.DecodeFile(path, &c)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("%s: %v", path, err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		return Config{}, fmt.Errorf("%s: unknown setting %s", path, u[0])
	}
	if c.Memory.MinJobs < 0 {
		return Config{}, fmt.Errorf("%s: memory.min_jobs must not be negative", path)
	}
	if _, err := ParseSize(c.Cache.MaxSize); err != nil {
		return Config{}, fmt.Errorf("%s: cache.max_size: %v", path, err)
	}
	if m := c.Env.Mode; m != "" && m != "inherit" && m != "clean" {
		return Config{}, fmt.Errorf("%s: env.mode must be \"inherit\" or \"clean\", not %q", path, m)
	}
	if _, err := ParseSize(c.Memory.Budget); err != nil {
		return Config{}, fmt.Errorf("%s: memory.budget: %v", path, err)
	}
	return c, nil
}

// ParseSize reads a size such as "512M", "8GiB" or "1.5G" (binary units;
// a bare number is bytes). "" is 0, meaning unset.
func ParseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	i := len(s)
	for i > 0 && strings.ContainsRune("kKmMgGtTiIbB ", rune(s[i-1])) {
		i--
	}
	num, unit := s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	mult := map[string]float64{"": 1, "b": 1, "k": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
		"m": 1 << 20, "mb": 1 << 20, "mib": 1 << 20, "g": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
		"t": 1 << 40, "tb": 1 << 40, "tib": 1 << 40}
	m, ok := mult[unit]
	if !ok {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return uint64(f * m), nil
}
