package fixture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// read returns every file under dir, keyed by relative path.
func read(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		files[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestDeterministic(t *testing.T) {
	for _, gen := range []string{CMake, Meson, Ninja} {
		a, b := t.TempDir(), t.TempDir()
		spec := Spec{Generator: gen, Seed: 7}
		if _, err := Write(a, spec); err != nil {
			t.Fatal(err)
		}
		if _, err := Write(b, spec); err != nil {
			t.Fatal(err)
		}
		fa, fb := read(t, a), read(t, b)
		if len(fa) != len(fb) {
			t.Fatalf("%s: %d files vs %d", gen, len(fa), len(fb))
		}
		for name, content := range fa {
			if fb[name] != content {
				t.Errorf("%s: %s differs between two runs with the same seed", gen, name)
			}
		}
	}
}

func TestInfo(t *testing.T) {
	dir := t.TempDir()
	info, err := Write(dir, Spec{Generator: Ninja, Libs: 3, Sources: 2, Headers: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Sources) != 6 || len(info.Headers) != 5 {
		t.Errorf("got %d sources, %d headers; want 6, 5", len(info.Sources), len(info.Headers))
	}
	if !info.InSource || info.LogDir == "" {
		t.Errorf("hand-written ninja fixture should be in-source with a builddir, got %+v", info)
	}
	build, err := os.ReadFile(filepath.Join(dir, info.BuildFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(build), info.FlagsMarker) {
		t.Errorf("%s doesn't contain the flags marker %q", info.BuildFile, info.FlagsMarker)
	}
	for _, src := range info.Sources {
		b, err := os.ReadFile(filepath.Join(dir, src))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), includesBegin) || !strings.Contains(string(b), includesEnd) {
			t.Errorf("%s is missing its include markers", src)
		}
	}
}

func TestVariantsInstallDifferentSets(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if _, err := Write(a, Spec{Generator: CMake, Variant: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(b, Spec{Generator: CMake, Variant: "b"}); err != nil {
		t.Fatal(err)
	}
	la, _ := os.ReadFile(filepath.Join(a, "CMakeLists.txt"))
	lb, _ := os.ReadFile(filepath.Join(b, "CMakeLists.txt"))
	if string(la) == string(lb) {
		t.Error("variants a and b produced the same CMakeLists.txt")
	}
	if !strings.Contains(string(lb), "OUTPUT_NAME k1b") || !strings.Contains(string(lb), "extra_b.txt") {
		t.Error("variant b should rename the shared library and install extra_b.txt")
	}
}

func TestRejectsBadSpecs(t *testing.T) {
	for _, s := range []Spec{
		{Generator: "bazel"},
		{Generator: CMake, Variant: "c"},
		{Generator: Meson, RSP: true},
	} {
		if _, err := Write(t.TempDir(), s); err == nil {
			t.Errorf("Write(%+v) succeeded, want an error", s)
		}
	}
}
