package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Ardx19/kiln/internal/config"
	"github.com/Ardx19/kiln/internal/envdecl"
	"github.com/Ardx19/kiln/internal/generator"
	"github.com/Ardx19/kiln/internal/testrun"
)

// loadConfigs reads the configuration files that apply to the build
// directory in the current directory, least specific first: the user's,
// the source tree's .kiln.toml (shared by every build tree and worktree of
// it), and the build directory's .kiln/config.toml.
func loadConfigs() ([]config.Config, generator.Info, error) {
	info, err := generator.Detect(".")
	if err != nil {
		return nil, info, err
	}
	paths := []string{config.Path()}
	if info.Source != "" {
		paths = append(paths, filepath.Join(info.Source, ".kiln.toml"))
	}
	paths = append(paths, filepath.Join(".kiln", "config.toml"))
	var cs []config.Config
	for _, p := range paths {
		c, err := config.Load(p)
		if err != nil {
			return nil, info, err
		}
		cs = append(cs, c)
	}
	return cs, info, nil
}

func declared(cs []config.Config) envdecl.Decl {
	envs := make([]config.Env, len(cs))
	for i, c := range cs {
		envs[i] = c.Env
	}
	return envdecl.Merge(envs...)
}

// runExec is `kiln exec [-C DIR] [--] COMMAND...`: run a command in the
// declared environment, so configure steps (cmake, meson setup,
// ./configure) see what the build sees.
func runExec(p program, args []string, stdout, stderr io.Writer) int {
	args, ok := cdFlag(p, args, stderr)
	if !ok {
		return 1
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintf(stderr, "usage: %s exec [-C DIR] [--] COMMAND [ARGS...]\n", p.name)
		return 1
	}
	cs, _, err := loadConfigs()
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = declared(cs).Apply(nil)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	// PATH changes in the declaration apply to finding the command too.
	if path, err := lookPath(args[0], cmd.Env); err == nil {
		cmd.Path = path
	}
	return exitCode(p, cmd.Run(), stderr)
}

// lookPath finds name on the PATH in env.
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			for _, dir := range filepath.SplitList(v) {
				p := filepath.Join(dir, name)
				if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
					return p, nil
				}
			}
		}
	}
	return "", exec.ErrNotFound
}

func exitCode(p program, err error, stderr io.Writer) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
	return 127
}

// runTest is `kiln test [-C DIR] [--no-build] [--strict] [--config NAME]
// [-- BACKEND ARGS]`: build, run the tests in the declared environment
// through CTest, Meson or [test] command, and judge the JUnit report
// against .kiln/xfail.txt.
func runTest(p program, args []string, stdout, stderr io.Writer) int {
	args, ok := cdFlag(p, args, stderr)
	if !ok {
		return 1
	}
	var noBuild, strict bool
	var cfgName string
	var extra []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--no-build":
			noBuild = true
		case "--strict":
			strict = true
		case "--config":
			if i+1 == len(args) {
				fmt.Fprintf(stderr, "%s: error: --config needs a value\n", p.name)
				return 1
			}
			i++
			cfgName = args[i]
		case "--":
			extra = args[i+1:]
			i = len(args)
		default:
			fmt.Fprintf(stderr, "usage: %s test [-C DIR] [--no-build] [--strict] [--config NAME] [-- BACKEND ARGS...]\n", p.name)
			return 1
		}
	}
	cs, info, err := loadConfigs()
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	cwd, _ := os.Getwd()
	tcfg := cs[len(cs)-1].Test
	for _, c := range cs[:len(cs)-1] {
		if c.Test.Command != "" && tcfg.Command == "" {
			tcfg = c.Test
		}
	}
	if !noBuild {
		if code := runNinja(p, nil, stdout, stderr); code != 0 {
			fmt.Fprintf(stderr, "%s: error: the build failed (exit %d), so the tests weren't run\n", p.name, code)
			return code
		}
	}

	junit := filepath.Join(cwd, ".kiln", "test-report.xml")
	os.MkdirAll(filepath.Dir(junit), 0o777)
	os.Remove(junit)
	var cmd *exec.Cmd
	switch {
	case tcfg.Command != "":
		cmd = exec.Command("/bin/sh", "-c", tcfg.Command)
		if tcfg.JUnit != "" {
			junit = filepath.Join(cwd, tcfg.JUnit)
		} else {
			junit = ""
		}
	case info.Kind == generator.CMake:
		a := []string{"--test-dir", cwd, "--output-junit", junit}
		if len(info.Configs) > 0 {
			if cfgName == "" {
				cfgName = info.Configs[0]
			}
			a = append(a, "-C", cfgName)
		}
		cmd = exec.Command(info.Ctest(), append(a, extra...)...)
	case info.Kind == generator.Meson:
		meson := os.Getenv("MESON")
		if meson == "" {
			meson = "meson"
		}
		junit = filepath.Join(cwd, "meson-logs", "testlog.junit.xml")
		os.Remove(junit)
		cmd = exec.Command(meson, append([]string{"test", "-C", cwd, "--no-rebuild"}, extra...)...)
	default:
		fmt.Fprintf(stderr, "%s: error: don't know how to run this tree's tests; set [test] command (and junit) in .kiln/config.toml\n", p.name)
		return 1
	}
	cmd.Dir = cwd
	cmd.Env = declared(cs).Apply(nil)
	if path, err := lookPath(cmd.Args[0], cmd.Env); err == nil && !strings.Contains(cmd.Args[0], "/") {
		cmd.Path = path
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	runErr := cmd.Run()
	backend := exitCode(p, runErr, stderr)

	var results []testrun.Result
	if junit != "" {
		if b, err := os.ReadFile(junit); err == nil {
			if results, err = testrun.ParseJUnit(b); err != nil {
				fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
				return 1
			}
		}
	}
	if len(results) == 0 {
		if backend != 0 {
			fmt.Fprintf(stderr, "%s: the test command failed (exit %d) and wrote no usable report\n", p.name, backend)
		}
		return backend
	}
	exp, err := testrun.ReadExpected(filepath.Join(".kiln", "xfail.txt"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: .kiln/xfail.txt: %v\n", p.name, err)
		return 1
	}
	v := testrun.Judge(results, exp)
	for _, r := range v.Failed {
		fmt.Fprintf(stdout, "FAILED: %s\n", r.Name)
	}
	for _, r := range v.XPass {
		why, _ := exp.Match(r.Name)
		fmt.Fprintf(stdout, "XPASS: %s passed but is listed as an expected failure (%s)\n", r.Name, why)
	}
	fmt.Fprintf(stdout, "%s: %s\n", p.name, v.Summary())
	if v.OK(strict) {
		return 0
	}
	return 1
}
