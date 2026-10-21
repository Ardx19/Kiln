//go:build compat

package compat

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Ardx19/kiln/internal/config"
)

// codeBlocks returns the contents of the fenced blocks of the given
// language in text.
func codeBlocks(text, lang string) []string {
	var blocks []string
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```"+lang {
			continue
		}
		var b []string
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			b = append(b, lines[i])
		}
		blocks = append(blocks, strings.Join(b, "\n"))
	}
	return blocks
}

// TestReadme runs the README's `kiln -C build ...` lines, in order, on a
// fixture, and loads its TOML example.
func TestReadme(t *testing.T) {
	text, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, toml := range codeBlocks(string(text), "toml") {
		p := filepath.Join(t.TempDir(), "readme.toml")
		os.WriteFile(p, []byte(toml), 0o644)
		if _, err := config.Load(p); err != nil {
			t.Errorf("the README's TOML example doesn't load: %v\n%s", err, toml)
		}
	}

	e := newInstallEnv(t, "readme")
	prefix := filepath.Join(e.base, "prefix")
	build := e.configure("cmake", "tree", "a", prefix)
	e.env = append(e.env, "CCACHE_DIR="+filepath.Join(e.base, "ccache"))

	comment := regexp.MustCompile(`\s+#.*$`)
	ran := 0
	for _, block := range codeBlocks(string(text), "bash") {
		for _, line := range strings.Split(block, "\n") {
			line = comment.ReplaceAllString(strings.TrimSpace(line), "")
			if !strings.HasPrefix(line, "kiln -C build") {
				continue
			}
			args := strings.Fields(line)[1:]
			args[1] = build // -C build
			cmd := exec.Command(e.kiln, args...)
			cmd.Env = e.env
			out, err := cmd.CombinedOutput()
			ran++
			// `kiln test` fails on the fixture's deliberately failing test.
			if err != nil && !(len(args) > 2 && args[2] == "test") {
				t.Errorf("README line %q failed: %v\n%s", line, err, tail(string(out), 600))
			}
			if len(args) > 2 && args[2] == "test" && !strings.Contains(string(out), "1 passed, 1 failed") {
				t.Errorf("README line %q: unexpected output\n%s", line, tail(string(out), 600))
			}
		}
	}
	if ran < 8 {
		t.Errorf("only %d README command lines were found and run", ran)
	}
}
