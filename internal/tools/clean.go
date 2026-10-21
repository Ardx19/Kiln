// Package tools implements the Ninja subtools (`-t`) that generators run
// on build directories: clean, cleandead, compdb, recompact and restat.
// Each is ported from Ninja 1.13.2; docs/ninja-quirks.md lists where older
// versions differ.
package tools

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"syscall"

	"github.com/Ardx19/kiln/internal/graph"
)

// Cleaner removes build outputs, a port of Ninja's Cleaner (clean.cc).
type Cleaner struct {
	State *graph.State
	// DryRun only reports what would be removed (-n).
	DryRun bool
	// Verbose lists each file (-v); a dry run lists them too. Quiet
	// (--quiet) prints nothing but errors.
	Verbose, Quiet bool
	Out, Err       io.Writer
	// Program prefixes error messages ("kiln" or "ninja").
	Program string
	// LoadDyndep loads a pending dyndep file into the graph, so the
	// outputs it adds are known. Errors are ignored, as in Ninja.
	LoadDyndep func(*graph.Node)

	removed map[string]bool
	cleaned map[*graph.Node]bool
	count   int
	status  int
}

func (c *Cleaner) reset() {
	c.status, c.count = 0, 0
	c.removed = map[string]bool{}
	c.cleaned = map[*graph.Node]bool{}
}

func (c *Cleaner) verbose() bool { return !c.Quiet && (c.Verbose || c.DryRun) }

func (c *Cleaner) errorf(format string, args ...any) {
	fmt.Fprintf(c.Err, "%s: error: %s\n", c.Program, fmt.Sprintf(format, args...))
}

func (c *Cleaner) printHeader() {
	if c.Quiet {
		return
	}
	fmt.Fprint(c.Out, "Cleaning...")
	if c.verbose() {
		fmt.Fprint(c.Out, "\n")
	} else {
		fmt.Fprint(c.Out, " ")
	}
}

func (c *Cleaner) printFooter() {
	if !c.Quiet {
		fmt.Fprintf(c.Out, "%d files.\n", c.count)
	}
}

func (c *Cleaner) report(path string) {
	c.count++
	if c.verbose() {
		fmt.Fprintf(c.Out, "Remove %s\n", path)
	}
}

// remove deletes path once, like Ninja's Cleaner::Remove: a missing file
// is fine, any other failure is reported and fails the tool.
func (c *Cleaner) remove(path string) {
	if c.removed[path] {
		return
	}
	c.removed[path] = true
	if c.DryRun {
		if _, err := os.Stat(path); err == nil {
			c.report(path)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			c.errorf("stat(%s): %s", path, strerror(err))
		}
		return
	}
	// remove(3): files and empty directories.
	err := syscall.Unlink(path)
	if errors.Is(err, syscall.EISDIR) {
		err = syscall.Rmdir(path)
	}
	switch {
	case err == nil:
		c.report(path)
	case errors.Is(err, syscall.ENOENT):
	default:
		c.errorf("remove(%s): %s", path, strerror(err))
		c.status = 1
	}
}

// strerror spells an errno like C's strerror, which Ninja prints.
func strerror(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		s := errno.Error()
		if s != "" {
			return string(s[0]-'a'+'A') + s[1:]
		}
	}
	return err.Error()
}

func (c *Cleaner) removeEdgeFiles(e *graph.Edge) {
	if depfile, _ := e.UnescapedBinding("depfile"); depfile != "" {
		c.remove(depfile)
	}
	if rspfile, _ := e.UnescapedBinding("rspfile"); rspfile != "" {
		c.remove(rspfile)
	}
}

// loadDyndeps loads dyndep files that exist, before they're cleaned.
func (c *Cleaner) loadDyndeps() {
	if c.LoadDyndep == nil {
		return
	}
	for _, e := range c.State.Edges {
		if e.Dyndep != nil && e.Dyndep.DyndepPending {
			c.LoadDyndep(e.Dyndep)
		}
	}
}

// CleanAll removes every output of every non-phony edge, plus depfiles
// and response files. Generator outputs (the manifest) are kept unless
// generator is set (-g).
func (c *Cleaner) CleanAll(generator bool) int {
	c.reset()
	c.printHeader()
	c.loadDyndeps()
	for _, e := range c.State.Edges {
		if e.IsPhony() {
			continue
		}
		if gen, _ := e.Binding("generator"); !generator && gen != "" {
			continue
		}
		for _, o := range e.Outputs {
			c.remove(o.Path)
		}
		c.removeEdgeFiles(e)
	}
	c.printFooter()
	return c.status
}

// CleanDead removes the files in the build log that no edge produces or
// uses any more (Ninja 1.11 and later; 1.10 also removed log entries
// that had become inputs).
func (c *Cleaner) CleanDead(logged []string) int {
	c.reset()
	c.printHeader()
	c.loadDyndeps()
	paths := append([]string(nil), logged...)
	sort.Strings(paths)
	for _, p := range paths {
		n := c.State.LookupNode(p)
		if n == nil || (n.InEdge == nil && len(n.OutEdges) == 0) {
			c.remove(p)
		}
	}
	c.printFooter()
	return c.status
}

func (c *Cleaner) cleanTarget(target *graph.Node) {
	if e := target.InEdge; e != nil {
		if !e.IsPhony() {
			c.remove(target.Path)
			c.removeEdgeFiles(e)
		}
		for _, in := range e.Inputs {
			if !c.cleaned[in] {
				c.cleanTarget(in)
			}
		}
	}
	c.cleaned[target] = true
}

// CleanTargets removes the given targets and everything built to make
// them.
func (c *Cleaner) CleanTargets(targets []string) int {
	c.reset()
	c.printHeader()
	c.loadDyndeps()
	for _, t := range targets {
		if t == "" {
			c.errorf("failed to canonicalize '': empty path")
			c.status = 1
			continue
		}
		t = graph.CanonicalizePath(t)
		n := c.State.LookupNode(t)
		if n == nil {
			c.errorf("unknown target '%s'", t)
			c.status = 1
			continue
		}
		if c.verbose() {
			fmt.Fprintf(c.Out, "Target %s\n", t)
		}
		c.cleanTarget(n)
	}
	c.printFooter()
	return c.status
}

// CleanRules removes the outputs of every edge using one of the rules.
func (c *Cleaner) CleanRules(rules []string) int {
	c.reset()
	c.printHeader()
	c.loadDyndeps()
	for _, name := range rules {
		r := c.State.Bindings.LookupRule(name)
		if r == nil {
			c.errorf("unknown rule '%s'", name)
			c.status = 1
			continue
		}
		if c.verbose() {
			fmt.Fprintf(c.Out, "Rule %s\n", name)
		}
		for _, e := range c.State.Edges {
			if e.Rule.Name != r.Name {
				continue
			}
			for _, o := range e.Outputs {
				c.remove(o.Path)
				c.removeEdgeFiles(e)
			}
		}
	}
	c.printFooter()
	return c.status
}
