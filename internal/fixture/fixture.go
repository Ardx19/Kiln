// Package fixture generates small synthetic C/C++ projects for the
// compatibility harness. One spec can be emitted as a CMake project, a
// Meson project or a hand-written build.ninja, so every generator exercises
// the same features: a random header DAG, static/shared/module libraries,
// code generation with a depfile and restat, a link pool, the console pool,
// order-only dependencies, install rules and passing/failing tests.
//
// Generation is deterministic for a given Spec.
package fixture

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Generator names accepted by Spec.Generator.
const (
	CMake = "cmake"
	Meson = "meson"
	Ninja = "ninja"
)

// Spec describes a fixture.
type Spec struct {
	Generator string // CMake, Meson or Ninja
	Seed      int64
	Libs      int    // number of libraries (at least 3: static, shared, module)
	Sources   int    // sources per library
	Headers   int    // headers in the include DAG
	Variant   string // "a" or "b": B installs a partly different file set
	RSP       bool   // CMake only: force response files for compiles and links
}

// Info is written to fixture.json. The harness reads it to apply edits.
type Info struct {
	Generator string   `json:"generator"`
	Variant   string   `json:"variant"`
	Sources   []string `json:"sources"` // relative to the fixture root
	Headers   []string `json:"headers"`
	// CodegenInput is the code generator's explicit input; touching it
	// without changing it exercises restat.
	CodegenInput string `json:"codegen_input"`
	// CodegenDep is only reachable through the generator's depfile.
	CodegenDep string `json:"codegen_dep"`
	// BuildFile holds FlagsMarker; replacing the marker changes a compile
	// flag (and regenerates the manifest for CMake and Meson).
	BuildFile   string `json:"build_file"`
	FlagsMarker string `json:"flags_marker"`
	// InSource is true when the manifest lives in the fixture root
	// (hand-written ninja); otherwise the build dir is configured separately.
	InSource bool `json:"in_source"`
	// LogDir is where .ninja_log lives relative to the build dir
	// ("" unless the manifest sets builddir).
	LogDir string `json:"log_dir"`
	// DyndepInput is only known to the build through a dyndep file
	// ("" when the fixture has no dyndep edge).
	DyndepInput string `json:"dyndep_input,omitempty"`
	// SlowInput feeds a command that takes a second ("" if none).
	SlowInput string `json:"slow_input,omitempty"`
}

const (
	includesBegin = "// kiln:includes begin"
	includesEnd   = "// kiln:includes end"
	flagsMarker   = "-O0"
)

type lib struct {
	name    string // k0, k1, ...
	kind    string // static, shared, module
	lang    string // c or cpp
	sources []string
}

// Write generates the fixture into dir, which must not exist or be empty.
func Write(dir string, s Spec) (Info, error) {
	if s.Libs < 3 {
		s.Libs = 3
	}
	if s.Sources < 1 {
		s.Sources = 1
	}
	if s.Headers < 2 {
		s.Headers = 2
	}
	if s.Variant == "" {
		s.Variant = "a"
	}
	if s.Variant != "a" && s.Variant != "b" {
		return Info{}, fmt.Errorf("variant must be a or b, not %q", s.Variant)
	}
	switch s.Generator {
	case CMake, Meson, Ninja:
	default:
		return Info{}, fmt.Errorf("unknown generator %q", s.Generator)
	}
	if s.RSP && s.Generator != CMake {
		return Info{}, fmt.Errorf("RSP is only meaningful for %s", CMake)
	}

	w := &writer{root: dir, rng: rand.New(rand.NewSource(s.Seed))}
	info := Info{
		Generator:    s.Generator,
		Variant:      s.Variant,
		CodegenInput: "gen/version.in",
		CodegenDep:   "gen/version_extra.in",
		FlagsMarker:  flagsMarker,
	}

	for i := 0; i < s.Headers; i++ {
		info.Headers = append(info.Headers, w.header(i))
	}
	libs := make([]lib, s.Libs)
	kinds := []string{"static", "shared", "module"}
	for i := range libs {
		l := lib{name: fmt.Sprintf("k%d", i), kind: kinds[i%len(kinds)], lang: "cpp"}
		if i == 0 {
			l.lang = "c" // keep one C library so C rules are exercised too
		}
		for j := 0; j < s.Sources; j++ {
			src := w.source(l, j, s.Headers)
			l.sources = append(l.sources, src)
			info.Sources = append(info.Sources, src)
		}
		libs[i] = l
	}
	w.file("app/main.cpp", mainSource(libs))
	w.file("tests/t_pass.cpp", "int main() { return 0; }\n")
	w.file("tests/t_fail.cpp", "int main() { return 1; }\n")
	w.file("data/extra_"+s.Variant+".txt", "installed by variant "+s.Variant+"\n")
	w.file(info.CodegenInput, "KILN_VERSION 1\n@include version_extra.in\n")
	w.file(info.CodegenDep, "KILN_EXTRA 1\n")
	w.file("gen/gen.sh", genScript)

	switch s.Generator {
	case CMake:
		info.BuildFile = "CMakeLists.txt"
		w.file(info.BuildFile, cmakeLists(libs, s))
	case Meson:
		info.BuildFile = "meson.build"
		w.file(info.BuildFile, mesonBuild(libs, s))
	case Ninja:
		info.BuildFile = "flags.ninja"
		info.InSource = true
		info.LogDir = ".ninja-state"
		info.DyndepInput = "gen/dyn_extra.in"
		info.SlowInput = "gen/slow.in"
		w.file(info.SlowInput, "slow\n")
		w.file("gen/dyn.in", "dyn\n")
		w.file(info.DyndepInput, "extra\n")
		w.file(info.BuildFile, "cflags = "+flagsMarker+" -Iinclude -Iout/gen\n")
		w.file("build.ninja", ninjaManifest(libs, info.LogDir))
		w.file("tests.ninja", ninjaTests)
	}
	if w.err != nil {
		return Info{}, w.err
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return Info{}, err
	}
	w.file("fixture.json", string(b)+"\n")
	return info, w.err
}

type writer struct {
	root string
	rng  *rand.Rand
	err  error
}

func (w *writer) file(rel, content string) {
	if w.err != nil {
		return
	}
	p := filepath.Join(w.root, rel)
	if w.err = os.MkdirAll(filepath.Dir(p), 0o755); w.err != nil {
		return
	}
	w.err = os.WriteFile(p, []byte(content), 0o644)
}

// header i includes up to two lower-numbered headers, so the DAG is acyclic.
func (w *writer) header(i int) string {
	rel := fmt.Sprintf("include/h%d.h", i)
	var b strings.Builder
	fmt.Fprintf(&b, "#ifndef KILN_H%d\n#define KILN_H%d\n", i, i)
	deps := map[int]bool{}
	for n := 0; n < 2 && i > 0; n++ {
		deps[w.rng.Intn(i)] = true
	}
	for _, d := range sortedKeys(deps) {
		fmt.Fprintf(&b, "#include \"h%d.h\"\n", d)
	}
	fmt.Fprintf(&b, "static inline int h%d(void) { return %d; }\n#endif\n", i, i)
	w.file(rel, b.String())
	return rel
}

// source writes one library source with 1–3 includes inside the marked
// block that the harness edits.
func (w *writer) source(l lib, j, headers int) string {
	rel := fmt.Sprintf("lib%s/src%d.%s", strings.TrimPrefix(l.name, "k"), j, l.lang)
	inc := map[int]bool{}
	for n := 1 + w.rng.Intn(3); n > 0; n-- {
		inc[w.rng.Intn(headers)] = true
	}
	var b strings.Builder
	b.WriteString(includesBegin + "\n")
	for _, h := range sortedKeys(inc) {
		fmt.Fprintf(&b, "#include \"h%d.h\"\n", h)
	}
	b.WriteString(includesEnd + "\n")
	if l.lang == "cpp" {
		b.WriteString("extern \"C\" ")
	}
	fmt.Fprintf(&b, "int %s_src%d(void) { return %d; }\n", l.name, j, j)
	w.file(rel, b.String())
	return rel
}

func sortedKeys(m map[int]bool) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func mainSource(libs []lib) string {
	var b strings.Builder
	b.WriteString("#include \"gen_version.h\"\n\n")
	var calls []string
	for _, l := range libs {
		if l.kind == "module" {
			continue // modules are loaded at run time, never linked
		}
		for j := range l.sources {
			fn := fmt.Sprintf("%s_src%d", l.name, j)
			fmt.Fprintf(&b, "extern \"C\" int %s(void);\n", fn)
			calls = append(calls, fn+"()")
		}
	}
	// Use the generated defines so their values matter to the build.
	fmt.Fprintf(&b, "\nint main() { return ((%s) + KILN_VERSION + KILN_EXTRA) * 0; }\n", strings.Join(calls, " + "))
	return b.String()
}

// genScript turns gen/version.in into a header. Lines "@include f" pull in
// another file and are listed in the depfile. The output is only replaced
// when it changes, so restat can prune downstream edges.
const genScript = `#!/bin/sh
# usage: gen.sh <input> <output> <depfile>
set -e
in=$1 out=$2 dep=$3
dir=$(dirname "$in")
tmp="$out.tmp"
# Depfiles separate paths with spaces, so spaces in a path are escaped.
esc() { printf '%s' "$1" | sed 's/ /\\ /g'; }
deps=$(esc "$in")
: > "$tmp"
while IFS= read -r line; do
  case $line in
    "@include "*)
      f="$dir/${line#@include }"
      deps="$deps $(esc "$f")"
      sed 's/^/#define /' "$f" >> "$tmp" ;;
    *) printf '#define %s\n' "$line" >> "$tmp" ;;
  esac
done < "$in"
if cmp -s "$tmp" "$out"; then rm -f "$tmp"; else mv "$tmp" "$out"; fi
printf '%s: %s\n' "$(esc "$out")" "$deps" > "$dep"
`
