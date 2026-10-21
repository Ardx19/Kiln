package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// subcommands are reserved words when kiln runs as kiln. Every planned
// one is reserved now, so a later release never changes what an existing
// command line means: a target with one of these names is built with
// `kiln build <name>`. As ninja (drop-in mode) there are no subcommands.
var subcommands = map[string]func(p program, args []string, stdout, stderr io.Writer) int{
	"build":   runBuildCmd,
	"shim":    runShim,
	"status":  runStatus,
	"wait":    runWait,
	"log":     runLog,
	"stop":    runStop,
	"mem":     runMem,
	"install": runInstall,
	"test":    nil,
	"exec":    nil,
	"cache":   nil,
}

// splitSubcommand finds the first operand of a kiln command line, skipping
// Ninja's flags and their values. If it names a subcommand, it returns the
// name and the command line without it.
func splitSubcommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return "", args
		case strings.HasPrefix(a, "--"):
			continue
		case len(a) >= 2 && a[0] == '-':
			// A cluster like -nv or -j8; a value-taking flag at its end
			// consumes the next argument. -t starts a tool: no subcommand.
			for j := 1; j < len(a); j++ {
				if !strings.ContainsRune("Cdfjkltw", rune(a[j])) {
					continue
				}
				if a[j] == 't' {
					return "", args
				}
				if j == len(a)-1 {
					i++
				}
				break
			}
			continue
		}
		if _, ok := subcommands[a]; ok {
			return a, append(append([]string(nil), args[:i]...), args[i+1:]...)
		}
		return "", args
	}
	return "", args
}

func runSubcommand(p program, name string, args []string, stdout, stderr io.Writer) int {
	if run := subcommands[name]; run != nil {
		return run(p, args, stdout, stderr)
	}
	fmt.Fprintf(stderr, "%s: error: '%s %s' isn't implemented yet; to build a target named '%s', run '%s build %s'\n",
		p.name, p.name, name, name, p.name, name)
	return 1
}

// runShim makes DIR/ninja a symlink to this kiln, so CMake, Meson or PATH
// can use kiln as Ninja: -DCMAKE_MAKE_PROGRAM=DIR/ninja,
// NINJA=DIR/ninja meson setup ..., or DIR first on PATH.
func runShim(p program, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(stderr, "usage: %s shim DIR\n\nmakes DIR/ninja a symlink to %s, so build tools that run ninja run %s\n", p.name, p.name, p.name)
		return 1
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: finding this executable: %v\n", p.name, err)
		return 1
	}
	link := filepath.Join(args[0], "ninja")
	if isKiln(link) {
		fmt.Fprintf(stdout, "%s already runs %s\n", link, self)
		return 0
	}
	if _, err := os.Lstat(link); err == nil {
		fmt.Fprintf(stderr, "%s: error: %s exists and isn't %s; remove it first\n", p.name, link, p.name)
		return 1
	}
	if err := os.MkdirAll(args[0], 0o777); err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	if err := os.Symlink(self, link); err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", p.name, err)
		return 1
	}
	abs, _ := filepath.Abs(link)
	fmt.Fprintf(stdout, "%s now runs %s. Use it as Ninja:\n"+
		"  cmake -G Ninja -DCMAKE_MAKE_PROGRAM=%s ...\n"+
		"  NINJA=%s meson setup ...\n"+
		"  or put %s first on PATH\n", abs, self, abs, abs, filepath.Dir(abs))
	return 0
}
