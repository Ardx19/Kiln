// Package config reads the user's machine defaults from a TOML file:
//
//	[memory]
//	enabled  = true     # false is --mem=off
//	budget   = "8GiB"   # --mem-budget
//	min_jobs = 1        # --min-jobs
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
	Memory Memory `toml:"memory"`
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
