// Command kiln is a Ninja-compatible build runner.
//
// So far it supports Ninja's dry run: kiln -n [-v] [-C dir] [-f file]
// [-d explain] [targets...]. Running commands arrives in milestone M1d
// (see docs/plans/2026-10-21-feat-kiln-build-runner-plan.md).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Ardx19/kiln/internal/build"
	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/manifest"
	"github.com/Ardx19/kiln/internal/state"
)

// version is overridden at link time: -ldflags "-X main.version=v0.1.0".
var version = "0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type options struct {
	dir, file    string
	dryRun       bool
	verbose      bool
	explain      bool
	tool         string
	showVersion  bool
	targets      []string
	jobs, failOK int // accepted for compatibility; used once kiln runs commands
}

// parseArgs reads Ninja's command line, getopt-style: bundled short
// flags (-nv), attached values (-j8, -Cdir), options after targets, and
// "--" to end options.
func parseArgs(args []string) (options, error) {
	o := options{file: "build.ninja"}
	withValue := "Cfjkldtw"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.targets = append(o.targets, args[i+1:]...)
			return o, nil
		case a == "--version":
			o.showVersion = true
			continue
		case a == "--verbose":
			o.verbose = true
			continue
		case strings.HasPrefix(a, "--"):
			return o, fmt.Errorf("unrecognized option '%s'", a)
		case len(a) < 2 || a[0] != '-':
			o.targets = append(o.targets, a)
			continue
		}
		for j := 1; j < len(a); j++ {
			c := a[j]
			if !strings.ContainsRune(withValue, rune(c)) {
				switch c {
				case 'n':
					o.dryRun = true
				case 'v':
					o.verbose = true
				default:
					return o, fmt.Errorf("invalid option -- '%c'", c)
				}
				continue
			}
			value := a[j+1:]
			if value == "" {
				if i+1 == len(args) {
					return o, fmt.Errorf("option requires an argument -- '%c'", c)
				}
				i++
				value = args[i]
			}
			if err := o.set(c, value); err != nil {
				return o, err
			}
			if c == 't' {
				// Everything after the tool name belongs to the tool.
				o.targets = append(o.targets, args[i+1:]...)
				return o, nil
			}
			break
		}
	}
	return o, nil
}

func (o *options) set(c byte, value string) error {
	switch c {
	case 'C':
		o.dir = value
	case 'f':
		o.file = value
	case 't':
		o.tool = value
	case 'd':
		switch value {
		case "explain":
			o.explain = true
		case "stats", "keepdepfile", "keeprsp":
			// Accepted; not implemented yet.
		default:
			return fmt.Errorf("unknown debug setting '%s'", value)
		}
	case 'j', 'k', 'l':
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid -%c parameter", c)
		}
		if c == 'j' {
			o.jobs = n
		} else if c == 'k' {
			o.failOK = n
		}
	case 'w':
		// Warning flags are accepted and ignored.
	}
	return nil
}

func run(args []string, stdout, stderr io.Writer) int {
	o, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "kiln: %v\n", err)
		return 2
	}
	if o.showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if o.tool != "" {
		fmt.Fprintf(stderr, "kiln: error: -t %s isn't available yet; run ninja -t %s (kiln shares Ninja's state files)\n", o.tool, o.tool)
		return 1
	}
	if err := dryRun(o, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "kiln: error: %v\n", err)
		return 1
	}
	return 0
}

func dryRun(o options, stdout, stderr io.Writer) error {
	if o.dir != "" {
		fmt.Fprintf(stdout, "kiln: Entering directory `%s'\n", o.dir)
		if err := os.Chdir(o.dir); err != nil {
			return fmt.Errorf("chdir to '%s' - %v", o.dir, err)
		}
	}
	if !o.dryRun {
		return errors.New("building isn't implemented yet (milestone M1d); use -n for a dry run")
	}
	warn := func(msg string) { fmt.Fprintf(stderr, "kiln: warning: %s\n", msg) }
	st, err := manifest.Load(o.file, manifest.Options{Warn: warn})
	if err != nil {
		return err
	}
	blog, dlog := readState(st, warn)
	var explain func(string)
	if o.explain {
		explain = func(msg string) { fmt.Fprintf(stderr, "kiln explain: %s\n", msg) }
	}
	scan := build.NewScanner(st, build.RealDisk{}, blog, dlog, explain)
	status := build.NewStatus(stdout, os.Getenv("NINJA_STATUS"), o.verbose)

	// Like Ninja, first bring the manifest itself up to date. In a dry
	// run that's as far as it gets if the manifest is out of date.
	if node := st.LookupNode(graph.CanonicalizePath(o.file)); node != nil {
		b := build.NewBuilder(st, scan, status)
		b.DryRun = true
		if err := b.AddTarget(node); err != nil {
			return err
		}
		if !b.AlreadyUpToDate() {
			return b.Build()
		}
	}

	targets, err := collectTargets(st, o.targets)
	if err != nil {
		return err
	}
	b := build.NewBuilder(st, scan, status)
	b.DryRun = true
	for _, t := range targets {
		if err := b.AddTarget(t); err != nil {
			return err
		}
	}
	if b.AlreadyUpToDate() {
		fmt.Fprintln(stdout, "kiln: no work to do.")
		return nil
	}
	return b.Build()
}

// readState loads .ninja_log and .ninja_deps from the manifest's builddir.
// A file kiln can't read is treated as empty, with a warning; kiln never
// deletes or rewrites it the way Ninja would.
func readState(st *graph.State, warn func(string)) (*state.BuildLog, *state.DepsLog) {
	dir := st.Bindings.LookupVariable("builddir")
	blog, err := state.ReadBuildLog(filepath.Join(dir, ".ninja_log"))
	if err != nil {
		warn(fmt.Sprintf("%v; treating it as empty", err))
		blog = nil
	}
	dlog, err := state.ReadDepsLog(filepath.Join(dir, ".ninja_deps"))
	if err != nil {
		warn(fmt.Sprintf("%v; treating it as empty", err))
		dlog = nil
	} else if dlog.Corrupt != "" {
		warn(fmt.Sprintf(".ninja_deps: %s; using the records before it", dlog.Corrupt))
	}
	return blog, dlog
}

func collectTargets(st *graph.State, args []string) ([]*graph.Node, error) {
	if len(args) == 0 {
		return st.DefaultNodes()
	}
	var nodes []*graph.Node
	for _, a := range args {
		path := graph.CanonicalizePath(a)
		n := st.LookupNode(path)
		if n == nil {
			msg := fmt.Sprintf("unknown target '%s'", path)
			if path == "clean" {
				msg += ", did you mean 'ninja -t clean'?"
			}
			return nil, errors.New(msg)
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}
