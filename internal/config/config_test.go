package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"": 0, "1024": 1024, "512M": 512 << 20, "8GiB": 8 << 30, "1.5G": 3 << 29, "2 gb": 2 << 30, "4k": 4096} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"x", "-1G", "5Q", "G"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) succeeded", in)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.toml")
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	if c, err := Load(filepath.Join(dir, "missing.toml")); err != nil || c != (Config{}) {
		t.Errorf("missing file: %+v, %v", c, err)
	}
	c, err := Load(write("[memory]\nenabled = false\nbudget = \"6G\"\nmin_jobs = 2\n"))
	if err != nil || c.Memory.Enabled == nil || *c.Memory.Enabled || c.Memory.Budget != "6G" || c.Memory.MinJobs != 2 {
		t.Errorf("Load = %+v, %v", c, err)
	}
	for in, msg := range map[string]string{
		"[memory]\nbudgt = 1\n":         "unknown setting memory.budgt",
		"[memory]\nbudget = \"lots\"\n": "memory.budget",
		"[memory]\nmin_jobs = -1\n":     "min_jobs",
		"[memory\n":                     "c.toml",
	} {
		if _, err := Load(write(in)); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("Load(%q) = %v; want an error with %q", in, err, msg)
		}
	}
}
