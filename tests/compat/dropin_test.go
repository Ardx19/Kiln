//go:build compat

package compat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ardx19/kiln/internal/fixture"
)

// TestDropIn uses kiln as Ninja through a shim (a symlink named ninja), the
// way CMake and Meson are pointed at it, through every generator call
// recorded in testdata/generator-calls: configure (including try_compile),
// build, regeneration, clean, install and test. After each step the newest
// real Ninja must agree the tree is up to date.
func TestDropIn(t *testing.T) {
	repo, tools, ninjas, _ := matrix(t)
	kiln, err := KilnBinary(repo)
	if err != nil {
		t.Fatal(err)
	}
	ninja := ninjas[len(ninjas)-1]
	base := filepath.Join(WorkDir(repo), "dropin")
	os.RemoveAll(base)
	shimDir := filepath.Join(base, "shim")
	os.MkdirAll(shimDir, 0o755)
	shim := filepath.Join(shimDir, "ninja")
	if err := os.Symlink(kiln, shim); err != nil {
		t.Fatal(err)
	}
	// Real Ninja on PATH: kiln picks the log version of a new tree from it,
	// and hands it the tools it doesn't run itself.
	env := withoutWindowsPath(os.Environ())
	for i, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			env[i] = "PATH=" + filepath.Dir(ninja.Path) + string(filepath.ListSeparator) + v
		}
	}
	run := func(t *testing.T, dir string, extraEnv []string, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(append([]string(nil), env...), extraEnv...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, tail(string(out), 2000))
		}
		return string(out)
	}
	upToDate := func(t *testing.T, build, step string) {
		t.Helper()
		out := run(t, build, nil, ninja.Path, "-n")
		if !strings.Contains(out, "ninja: no work to do.") {
			t.Errorf("after %s, ninja %s still has work:\n%s", step, ninja.Version, tail(out, 800))
		}
	}

	for _, gen := range []string{"Ninja", "Ninja Multi-Config"} {
		t.Run("cmake/"+strings.ReplaceAll(gen, " ", "-"), func(t *testing.T) {
			if tools.CMake == "" {
				t.Skip("cmake not found")
			}
			ctest := filepath.Join(filepath.Dir(tools.CMake), "ctest")
			name := strings.ReplaceAll(strings.ToLower(gen), " ", "-")
			src, build := filepath.Join(base, name, "src"), filepath.Join(base, name, "build")
			info, err := fixture.Write(src, fixture.Spec{Generator: fixture.CMake, Seed: 7})
			if err != nil {
				t.Fatal(err)
			}
			run(t, "/", nil, tools.CMake, "-S", src, "-B", build, "-G", gen, "-DCMAKE_MAKE_PROGRAM="+shim,
				"-DCMAKE_INSTALL_PREFIX="+filepath.Join(base, name, "prefix"))
			run(t, "/", nil, tools.CMake, "--build", build)
			upToDate(t, build, "cmake --build")
			run(t, "/", nil, tools.CMake, "--build", build, "-j", "3", "--target", "app")

			// A CMakeLists.txt edit regenerates: restat and recompact.
			if err := touch(filepath.Join(src, info.BuildFile)); err != nil {
				t.Fatal(err)
			}
			out := run(t, "/", nil, tools.CMake, "--build", build)
			if !strings.Contains(out, "Re-running CMake") && !strings.Contains(out, "Configuring done") {
				t.Errorf("touching %s didn't regenerate:\n%s", info.BuildFile, tail(out, 800))
			}
			upToDate(t, build, "regeneration")

			run(t, "/", []string{"DESTDIR=" + filepath.Join(base, name, "dest")}, tools.CMake, "--build", build, "--target", "install")
			if _, err := os.Stat(filepath.Join(base, name, "dest", base, name, "prefix", "bin", "app")); err != nil {
				t.Errorf("install target: %v", err)
			}
			args := []string{"--test-dir", build}
			if gen != "Ninja" {
				args = append(args, "-C", "Debug")
			}
			if out, err := exec.Command(ctest, args...).CombinedOutput(); err == nil || !strings.Contains(string(out), "1 tests failed out of 2") {
				t.Errorf("ctest: want the fixture's one failing test, got %v:\n%s", err, tail(string(out), 800))
			}

			run(t, "/", nil, tools.CMake, "--build", build, "--target", "clean")
			out = run(t, build, nil, ninja.Path, "-n")
			if strings.Contains(out, "no work to do") {
				t.Errorf("clean left nothing to rebuild")
			}
			run(t, "/", nil, tools.CMake, "--build", build)
			upToDate(t, build, "rebuilding after clean")
		})
	}

	t.Run("cmake/switch-make-program", func(t *testing.T) {
		if tools.CMake == "" {
			t.Skip("cmake not found")
		}
		src, build := filepath.Join(base, "switch", "src"), filepath.Join(base, "switch", "build")
		if _, err := fixture.Write(src, fixture.Spec{Generator: fixture.CMake, Seed: 8}); err != nil {
			t.Fatal(err)
		}
		run(t, "/", nil, tools.CMake, "-S", src, "-B", build, "-G", "Ninja", "-DCMAKE_MAKE_PROGRAM="+ninja.Path)
		run(t, "/", nil, tools.CMake, "--build", build)
		for _, prog := range []string{shim, ninja.Path, shim} {
			before, err := objectTimes(build)
			if err != nil {
				t.Fatal(err)
			}
			run(t, "/", nil, tools.CMake, "-DCMAKE_MAKE_PROGRAM="+prog, build)
			run(t, "/", nil, tools.CMake, "--build", build)
			after, _ := objectTimes(build)
			for o, m := range before {
				if after[o] != m {
					t.Errorf("switching to %s rebuilt %s", prog, o)
				}
			}
		}
		upToDate(t, build, "switching make programs")
	})

	t.Run("meson", func(t *testing.T) {
		if tools.Meson == "" {
			t.Skip("meson not found")
		}
		src, build := filepath.Join(base, "meson", "src"), filepath.Join(base, "meson", "build")
		info, err := fixture.Write(src, fixture.Spec{Generator: fixture.Meson, Seed: 9})
		if err != nil {
			t.Fatal(err)
		}
		viaKiln := []string{"NINJA=" + shim}
		run(t, "/", viaKiln, tools.Meson, "setup", build, src, "--prefix="+filepath.Join(base, "meson", "prefix"))
		run(t, "/", viaKiln, tools.Meson, "compile", "-C", build)
		upToDate(t, build, "meson compile")

		// Regeneration runs restat, cleandead and compdb.
		if err := touch(filepath.Join(src, info.BuildFile)); err != nil {
			t.Fatal(err)
		}
		run(t, "/", viaKiln, tools.Meson, "compile", "-C", build)
		upToDate(t, build, "regeneration")
		kilnDB, err := os.ReadFile(filepath.Join(build, "compile_commands.json"))
		if err != nil {
			t.Fatal(err)
		}
		run(t, "/", []string{"NINJA=" + ninja.Path}, tools.Meson, "setup", "--reconfigure", build, src)
		ninjaDB, _ := os.ReadFile(filepath.Join(build, "compile_commands.json"))
		if string(kilnDB) != string(ninjaDB) {
			t.Errorf("compile_commands.json from kiln differs from Ninja's")
		}

		if out, err := exec.Command(tools.Meson, "test", "-C", build).CombinedOutput(); err == nil || !strings.Contains(string(out), "Fail:              1") {
			t.Errorf("meson test: want the fixture's one failing test, got %v:\n%s", err, tail(string(out), 800))
		}
		run(t, "/", append(viaKiln, "DESTDIR="+filepath.Join(base, "meson", "dest")), tools.Meson, "install", "-C", build)
		run(t, "/", viaKiln, tools.Meson, "compile", "-C", build, "--clean")
		run(t, "/", viaKiln, tools.Meson, "compile", "-C", build)
		upToDate(t, build, "rebuilding after clean")
	})
}

// objectTimes maps each object file in dir to its mtime.
func objectTimes(dir string) (map[string]int64, error) {
	times := map[string]int64{}
	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(path, ".o") {
			times[path] = fi.ModTime().UnixNano()
		}
		return err
	})
	return times, err
}
