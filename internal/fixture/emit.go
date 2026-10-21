package fixture

import (
	"fmt"
	"strings"
)

// sharedName is the shared library's output name. Variant B renames it so
// the two variants install partly different file sets.
func sharedName(l lib, variant string) string {
	if variant == "b" {
		return l.name + "b"
	}
	return l.name
}

// linked returns the libraries the app links against (all but modules).
func linked(libs []lib) []lib {
	var out []lib
	for _, l := range libs {
		if l.kind != "module" {
			out = append(out, l)
		}
	}
	return out
}

func cmakeLists(libs []lib, s Spec) string {
	var b strings.Builder
	b.WriteString("cmake_minimum_required(VERSION 3.20)\nproject(kilnfixture LANGUAGES C CXX)\n\n")
	if s.RSP {
		b.WriteString("set(CMAKE_NINJA_FORCE_RESPONSE_FILE ON)\n")
	}
	b.WriteString(`# kiln:flags (the harness edits the next line)
add_compile_options(` + flagsMarker + `)
set_property(GLOBAL PROPERTY JOB_POOLS link_pool=1)
set(CMAKE_JOB_POOL_LINK link_pool)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)
include_directories(include)
enable_testing()

add_custom_command(
  OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/gen_version.h
  COMMAND sh ${CMAKE_CURRENT_SOURCE_DIR}/gen/gen.sh ${CMAKE_CURRENT_SOURCE_DIR}/gen/version.in ${CMAKE_CURRENT_BINARY_DIR}/gen_version.h ${CMAKE_CURRENT_BINARY_DIR}/gen_version.d
  DEPENDS gen/version.in gen/gen.sh
  DEPFILE ${CMAKE_CURRENT_BINARY_DIR}/gen_version.d
  COMMENT "Generating gen_version.h")

`)
	var installed []string
	for _, l := range libs {
		kind := map[string]string{"static": "STATIC", "shared": "SHARED", "module": "MODULE"}[l.kind]
		fmt.Fprintf(&b, "add_library(%s %s %s)\n", l.name, kind, strings.Join(l.sources, " "))
		switch l.kind {
		case "shared":
			fmt.Fprintf(&b, "set_target_properties(%s PROPERTIES OUTPUT_NAME %s VERSION 1.2.3 SOVERSION 1)\n",
				l.name, sharedName(l, s.Variant))
			installed = append(installed, l.name)
		case "module":
			installed = append(installed, l.name)
		}
	}
	var link []string
	for _, l := range linked(libs) {
		link = append(link, l.name)
	}
	fmt.Fprintf(&b, `
add_executable(app app/main.cpp ${CMAKE_CURRENT_BINARY_DIR}/gen_version.h)
target_include_directories(app PRIVATE ${CMAKE_CURRENT_BINARY_DIR})
target_link_libraries(app PRIVATE %s)
add_executable(t_pass tests/t_pass.cpp)
add_executable(t_fail tests/t_fail.cpp)
add_test(NAME pass COMMAND t_pass)
add_test(NAME fail COMMAND t_fail)
add_custom_target(console_note COMMAND ${CMAKE_COMMAND} -E echo console USES_TERMINAL)

install(TARGETS app %s RUNTIME DESTINATION bin LIBRARY DESTINATION lib)
install(FILES data/extra_%s.txt DESTINATION share/kilnfixture)
`, strings.Join(link, " "), strings.Join(installed, " "), s.Variant)
	return b.String()
}

func mesonBuild(libs []lib, s Spec) string {
	var b strings.Builder
	b.WriteString(`project('kilnfixture', 'c', 'cpp', default_options: ['buildtype=plain', 'b_staticpic=true'])

# kiln:flags (the harness edits the next line)
add_project_arguments('` + flagsMarker + `', language: ['c', 'cpp'])
inc = include_directories('include')
sh = find_program('sh')

gen = custom_target('gen_version',
  input: 'gen/version.in',
  output: 'gen_version.h',
  depfile: 'gen_version.d',
  command: [sh, files('gen/gen.sh'), '@INPUT@', '@OUTPUT@', '@DEPFILE@'])

`)
	quote := func(xs []string) string {
		q := make([]string, len(xs))
		for i, x := range xs {
			q[i] = "'" + x + "'"
		}
		return strings.Join(q, ", ")
	}
	for _, l := range libs {
		switch l.kind {
		case "static":
			fmt.Fprintf(&b, "%s = static_library('%s', %s, include_directories: inc)\n", l.name, l.name, quote(l.sources))
		case "shared":
			fmt.Fprintf(&b, "%s = shared_library('%s', %s, include_directories: inc, version: '1.2.3', soversion: '1', install: true)\n",
				l.name, sharedName(l, s.Variant), quote(l.sources))
		case "module":
			fmt.Fprintf(&b, "%s = shared_module('%s', %s, include_directories: inc, install: true)\n", l.name, l.name, quote(l.sources))
		}
	}
	var link []string
	for _, l := range linked(libs) {
		link = append(link, l.name)
	}
	fmt.Fprintf(&b, `
app = executable('app', 'app/main.cpp', gen, link_with: [%s], install: true)
test('pass', executable('t_pass', 'tests/t_pass.cpp'))
test('fail', executable('t_fail', 'tests/t_fail.cpp'))
run_target('console_note', command: ['echo', 'console'])
install_data('data/extra_%s.txt', install_dir: get_option('datadir') / 'kilnfixture')
`, strings.Join(link, ", "), s.Variant)
	return b.String()
}

// ninjaManifest is a hand-written-style manifest: response files, a custom
// pool, the console pool, a depfile without deps (re-read at load), restat,
// order-only deps, include (shared scope), subninja (child scope), builddir,
// phony and default.
func ninjaManifest(libs []lib, logDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Hand-written manifest used by the kiln compatibility harness.
ninja_required_version = 1.5
builddir = %s
include flags.ninja

pool link_pool
  depth = 1

rule cc
  command = cc -MD -MF $out.d $cflags -fPIC -c $in -o $out
  deps = gcc
  depfile = $out.d
  description = CC $out
rule cxx
  command = c++ -MD -MF $out.d $cflags -fPIC -c $in -o $out
  deps = gcc
  depfile = $out.d
  description = CXX $out
rule ar
  command = rm -f $out && ar crs $out @$out.rsp
  rspfile = $out.rsp
  rspfile_content = $in
  description = AR $out
rule solink
  command = c++ -shared -o $out @$out.rsp
  rspfile = $out.rsp
  rspfile_content = $in_newline
  pool = link_pool
  description = SOLINK $out
rule link
  command = c++ -o $out @$out.rsp -Wl,-rpath,'$$ORIGIN'
  rspfile = $out.rsp
  rspfile_content = $in_newline
  pool = link_pool
  description = LINK $out
rule gen
  command = sh gen/gen.sh $in $out $out.d
  depfile = $out.d
  restat = 1
  description = GEN $out
rule note
  command = echo console
  pool = console

build out/gen/gen_version.h: gen gen/version.in | gen/gen.sh
build console_note: note

`, logDir)
	var products []string
	for _, l := range libs {
		rule, obj := "cxx", ""
		if l.lang == "c" {
			rule = "cc"
		}
		var objs []string
		for _, src := range l.sources {
			obj = "out/" + strings.TrimSuffix(src, "."+l.lang) + ".o"
			fmt.Fprintf(&b, "build %s: %s %s\n", obj, rule, src)
			objs = append(objs, obj)
		}
		switch l.kind {
		case "static":
			fmt.Fprintf(&b, "build out/lib%s.a: ar %s\n", l.name, strings.Join(objs, " "))
		case "shared":
			fmt.Fprintf(&b, "build out/lib%s.so: solink %s\n", l.name, strings.Join(objs, " "))
		case "module":
			fmt.Fprintf(&b, "build out/%s.so: solink %s\n", l.name, strings.Join(objs, " "))
			products = append(products, "out/"+l.name+".so")
		}
	}
	var link []string
	for _, l := range linked(libs) {
		if l.kind == "static" {
			link = append(link, "out/lib"+l.name+".a")
		} else {
			link = append(link, "out/lib"+l.name+".so")
		}
	}
	fmt.Fprintf(&b, `build out/app/main.o: cxx app/main.cpp || out/gen/gen_version.h
build out/app/app: link out/app/main.o %s

subninja tests.ninja
build all: phony out/app/app %s tests
default all
`, strings.Join(link, " "), strings.Join(products, " "))
	return b.String()
}

// ninjaTests is loaded with subninja: its cflags override must not leak
// into the parent scope.
const ninjaTests = `# Child scope: this cflags must not affect build.ninja.
cflags = -O0
build out/tests/t_pass.o: cxx tests/t_pass.cpp
build out/tests/t_pass: link out/tests/t_pass.o
build out/tests/t_fail.o: cxx tests/t_fail.cpp
build out/tests/t_fail: link out/tests/t_fail.o
build tests: phony out/tests/t_pass out/tests/t_fail
`
