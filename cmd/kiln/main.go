// Command kiln is a Ninja-compatible build runner.
//
// Only --version works so far; the engine arrives in milestone M1
// (see docs/plans/2026-10-21-feat-kiln-build-runner-plan.md).
package main

import (
	"fmt"
	"os"
)

// version is overridden at link time: -ldflags "-X main.version=v0.1.0".
var version = "0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println(version)
		return 0
	}
	fmt.Fprintln(os.Stderr, "kiln: not implemented yet; only --version works")
	return 2
}
