//go:build compat

package compat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile writes a file (and its directory) for a test.
func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *memEnv) config(toml string) {
	writeFile(e.t, filepath.Join(e.dir, ".kiln", "config.toml"), toml)
}

func (e *memEnv) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(e.dir, name))
	return strings.TrimSpace(string(b))
}

// TestEnvLeak plants a package in a fake user site and stray
// PKG_CONFIG_PATH and CPATH entries: kiln's commands never see them with
// the declared environment, plain ninja's do.
func TestEnvLeak(t *testing.T) {
	e := newMemEnv(t, "env-leak")
	userbase := filepath.Join(e.dir, "fakehome")
	cmd := exec.Command("python3", "-c", "import site; print(site.getusersitepackages())")
	cmd.Env = append(os.Environ(), "PYTHONUSERBASE="+userbase)
	site, err := cmd.Output()
	if err != nil {
		t.Skip("no python3")
	}
	writeFile(t, filepath.Join(strings.TrimSpace(string(site)), "stubpkg.py"), "VALUE = 1\n")
	e.rule = "probe"
	e.manifest(`printf '%s|%s|' "$$CPATH" "$$PKG_CONFIG_PATH" > $out; python3 -c 'import stubpkg' 2>/dev/null && echo found >> $out || echo hidden >> $out`, "seen")
	strays := []string{"CPATH=/stray/include", "PKG_CONFIG_PATH=/stray/pc", "PYTHONUSERBASE=" + userbase}

	if out, err := exec.Command(e.ninja.Path, "-C", e.dir).CombinedOutput(); err != nil {
		t.Fatalf("ninja: %v\n%s", err, out)
	}
	plain := exec.Command(e.ninja.Path, "-C", e.dir, "-t", "clean")
	plain.Run()
	cmd = exec.Command(e.ninja.Path, "-C", e.dir)
	cmd.Env = append(e.env(), strays...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ninja with strays: %v\n%s", err, out)
	}
	if got := e.read("seen"); got != "/stray/include|/stray/pc|found" {
		t.Fatalf("plain ninja should see the strays, got %q", got)
	}
	os.Remove(filepath.Join(e.dir, "seen"))
	os.RemoveAll(filepath.Join(e.dir, ".ninja_log"))

	e.config("[env]\nunset = [\"CPATH\", \"PKG_CONFIG_PATH\"]\n[env.set]\nPYTHONNOUSERSITE = \"1\"\n")
	if out, code := e.runKiln([]string{"-j1"}, strays...); code != 0 {
		t.Fatalf("kiln: exit %d\n%s", code, tail(out, 600))
	}
	if got := e.read("seen"); got != "||hidden" {
		t.Errorf("under kiln the strays leaked: %q", got)
	}
	// kiln exec gives configure steps the same view.
	cmd = e.kilnCmd([]string{"exec", "--", "sh", "-c", `echo "[$CPATH][$PYTHONNOUSERSITE]"`}, strays...)
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "[][1]" {
		t.Errorf("kiln exec: %q, %v", out, err)
	}
}

// TestEnvChangeRebuilds changes the declared environment: edges rebuild,
// Ninja still finds nothing to do afterwards, and variables nobody
// declared never matter.
func TestEnvChangeRebuilds(t *testing.T) {
	e := newMemEnv(t, "env-change")
	e.rule = "show"
	e.manifest(`echo "FOO=$$FOO" > $out`, "a", "b")
	e.config("[env.set]\nFOO = \"one\"\n")
	if out, code := e.runKiln([]string{"-j1"}); code != 0 {
		t.Fatalf("first build: exit %d\n%s", code, out)
	}
	if got := e.read("a"); got != "FOO=one" {
		t.Fatalf("the declared FOO wasn't applied: %q", got)
	}
	e.requireClean()
	if out, _ := e.runKiln([]string{"-n"}); !strings.Contains(out, "no work to do") {
		t.Errorf("nothing changed, yet:\n%s", out)
	}
	// An undeclared variable is not part of the declaration.
	if out, _ := e.runKiln([]string{"-n"}, "UNRELATED=1"); !strings.Contains(out, "no work to do") {
		t.Errorf("an undeclared variable caused work:\n%s", out)
	}

	e.config("[env.set]\nFOO = \"two\"\n")
	out, _ := e.runKiln([]string{"-n", "-d", "explain"})
	if strings.Contains(out, "no work to do") || !strings.Contains(out, "environment changed") {
		t.Errorf("changing FOO should make the edges dirty:\n%s", out)
	}
	if out, code := e.runKiln([]string{"-j1"}); code != 0 {
		t.Fatalf("rebuild: exit %d\n%s", code, out)
	}
	if e.read("a") != "FOO=two" || e.read("b") != "FOO=two" {
		t.Errorf("not rebuilt under the new environment: %q %q", e.read("a"), e.read("b"))
	}
	e.requireClean()

	// Removing the declaration rebuilds too.
	e.config("")
	if out, code := e.runKiln([]string{"-j1"}); code != 0 || strings.Contains(out, "no work to do") {
		t.Errorf("removing the declaration: exit %d\n%s", code, out)
	}
	if got := e.read("a"); got != "FOO=" {
		t.Errorf("after removing the declaration: %q", got)
	}
	e.requireClean()
}

func TestKilnTestWithXfail(t *testing.T) {
	for _, gen := range []string{"cmake", "meson"} {
		t.Run(gen, func(t *testing.T) {
			e := newInstallEnv(t, "ktest-"+gen)
			build := e.configure(gen, "tree", "a", e.prefix)
			run := func(args ...string) (string, int) {
				cmd := exec.Command(e.kiln, append([]string{"test", "-C", build}, args...)...)
				cmd.Env = e.env
				out, err := cmd.CombinedOutput()
				code := 0
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				}
				return string(out), code
			}
			out, code := run()
			if code == 0 || !strings.Contains(out, "FAILED:") || !strings.Contains(out, "1 passed, 1 failed") {
				t.Fatalf("the fixture's failing test should fail the run: exit %d\n%s", code, tail(out, 1200))
			}
			xfail := filepath.Join(build, ".kiln", "xfail.txt")
			writeFile(t, xfail, "# the fixture's deliberately failing test\n/(^|\\W)fail$/ always fails\n")
			out, code = run("--no-build")
			if code != 0 || !strings.Contains(out, "1 expected failures") {
				t.Errorf("an expected failure should pass the run: exit %d\n%s", code, tail(out, 1200))
			}
			writeFile(t, xfail, "/(^|\\W)fail$/ always fails\n/(^|\\W)pass$/ pretend it is flaky\n")
			out, code = run("--no-build")
			if code != 0 || !strings.Contains(out, "XPASS") {
				t.Errorf("an expected failure that passes is a warning: exit %d\n%s", code, tail(out, 1200))
			}
			if out, code = run("--no-build", "--strict"); code == 0 {
				t.Errorf("--strict should fail on an XPASS:\n%s", tail(out, 1200))
			}
		})
	}
}

// TestKilnTestDeclaredLibraryPath runs a custom test command that only
// passes when the declared LD_LIBRARY_PATH reaches it.
func TestKilnTestDeclaredLibraryPath(t *testing.T) {
	e := newMemEnv(t, "ktest-ld")
	e.rule = "noop"
	e.manifest("touch $out", "built")
	script := `status=failure; [ "${LD_LIBRARY_PATH%%:*}" = "/declared/lib" ] && status=ok
cat > report.xml <<XML
<testsuite name="s"><testcase name="needs_library_path">$( [ $status = ok ] || echo '<failure message="LD_LIBRARY_PATH not set"/>' )</testcase></testsuite>
XML`
	writeFile(t, filepath.Join(e.dir, "check.sh"), script)
	e.config("[test]\ncommand = \"sh check.sh\"\njunit = \"report.xml\"\n")
	out, code := e.runKiln([]string{"test", "--no-build"})
	if code == 0 || !strings.Contains(out, "FAILED: needs_library_path") {
		t.Fatalf("without the declaration the test should fail: exit %d\n%s", code, out)
	}
	e.config("[test]\ncommand = \"sh check.sh\"\njunit = \"report.xml\"\n[env.prepend]\nLD_LIBRARY_PATH = [\"/declared/lib\"]\n")
	if out, code := e.runKiln([]string{"test", "--no-build"}); code != 0 {
		t.Errorf("with the declaration: exit %d\n%s", code, out)
	}
}
