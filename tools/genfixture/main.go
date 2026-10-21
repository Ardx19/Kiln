// Command genfixture writes a synthetic C/C++ project for the kiln
// compatibility harness as a CMake project, a Meson project or a
// hand-written build.ninja.
//
// Usage:
//
//	go run ./tools/genfixture -gen cmake|meson|ninja -out DIR [-seed N] [-variant a|b] [-rsp]
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Ardx19/kiln/internal/fixture"
)

func main() {
	var s fixture.Spec
	flag.StringVar(&s.Generator, "gen", fixture.CMake, "cmake, meson or ninja")
	flag.Int64Var(&s.Seed, "seed", 1, "random seed")
	flag.IntVar(&s.Libs, "libs", 4, "number of libraries (minimum 3)")
	flag.IntVar(&s.Sources, "srcs", 4, "sources per library")
	flag.IntVar(&s.Headers, "headers", 10, "headers in the include DAG")
	flag.StringVar(&s.Variant, "variant", "a", "a or b (b installs a partly different file set)")
	flag.BoolVar(&s.RSP, "rsp", false, "cmake only: force response files")
	out := flag.String("out", "", "output directory (required, must not exist)")
	flag.Parse()
	if *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if _, err := os.Stat(*out); err == nil {
		fmt.Fprintf(os.Stderr, "genfixture: %s already exists\n", *out)
		os.Exit(1)
	}
	if _, err := fixture.Write(*out, s); err != nil {
		fmt.Fprintln(os.Stderr, "genfixture:", err)
		os.Exit(1)
	}
}
