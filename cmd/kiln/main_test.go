package main

import (
	"io"
	"reflect"
	"strings"
	"testing"
)

var kilnProg, ninjaProg = program{name: "kiln"}, program{name: "ninja", dropIn: true}

func TestRun(t *testing.T) {
	var out, errOut strings.Builder
	if got := run(kilnProg, []string{"--version"}, &out, io.Discard); got != 0 || out.String() != version+"\n" {
		t.Errorf("kiln --version = %d, %q", got, out.String())
	}
	out.Reset()
	if got := run(ninjaProg, []string{"--vers"}, &out, io.Discard); got != 0 || out.String() != ninjaVersion+"\n" {
		t.Errorf("ninja --vers = %d, %q", got, out.String())
	}
	if got := run(kilnProg, []string{"-x"}, io.Discard, &errOut); got != 1 || !strings.HasPrefix(errOut.String(), "kiln: invalid option -- 'x'\nusage: kiln") {
		t.Errorf("kiln -x = %d, %q", got, errOut.String())
	}
}

func TestInvokedAs(t *testing.T) {
	for arg0, want := range map[string]program{
		"/usr/local/bin/kiln": kilnProg,
		"ninja":               ninjaProg,
		"/opt/shim/ninja":     ninjaProg,
		"ninja-build":         ninjaProg,
		"./kiln-dev":          kilnProg,
	} {
		if got := invokedAs(arg0); got.name != want.name || got.dropIn != want.dropIn || got.arg0 != arg0 {
			t.Errorf("invokedAs(%q) = %+v", arg0, got)
		}
	}
}

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		args []string
		want options
	}{
		{[]string{"-nv", "-Cdir", "-j8", "a", "-k", "0", "b"},
			options{file: "build.ninja", dir: "dir", dryRun: true, verbose: true, jobs: 8, jobsSet: true, keepGoingSet: true, targets: []string{"a", "b"}}},
		{[]string{"-f", "x.ninja", "-d", "explain", "-l2.5x", "--", "-weird"},
			options{file: "x.ninja", explain: true, maxLoad: 2.5, targets: []string{"-weird"}}},
		{[]string{"-C", "out", "-t", "deps", "-n", "foo"},
			options{file: "build.ninja", dir: "out", tool: "deps", targets: []string{"-n", "foo"}}},
		{[]string{"-w", "phonycycle=err", "--quiet"},
			options{file: "build.ninja", phonyCycleErr: true, quiet: true}},
	} {
		got, e := parseArgs(kilnProg, c.args, io.Discard)
		if e != nil {
			t.Fatalf("%q: %+v", c.args, e)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q:\n got %+v\nwant %+v", c.args, got, c.want)
		}
	}
	for _, c := range []struct {
		args         []string
		code         int
		stdout, errs string
	}{
		{[]string{"-q"}, 1, "", "ninja: invalid option -- 'q'\nusage: ninja"},
		{[]string{"-j"}, 1, "", "ninja: option requires an argument -- 'j'\n"},
		{[]string{"-j", "x"}, 1, "", "ninja: fatal: invalid -j parameter\n"},
		{[]string{"-k", "x"}, 1, "", "ninja: fatal: -k parameter not numeric; did you mean -k 0?\n"},
		{[]string{"-l", "x"}, 1, "", "ninja: fatal: -l parameter not numeric: did you mean -l 0.0?\n"},
		{[]string{"-d", "explian"}, 1, "", "ninja: error: unknown debug setting 'explian', did you mean 'explain'?\n"},
		{[]string{"-d", "list"}, 1, "debugging modes:\n", ""},
		{[]string{"-w", "list"}, 1, "warning flags:\n", ""},
		{[]string{"-w", "nope"}, 1, "", "ninja: error: unknown warning flag 'nope'\n"},
		{[]string{"--nope"}, 1, "", "ninja: unrecognized option '--nope'\n"},
		{[]string{"--ver"}, 1, "", "ninja: option '--ver' is ambiguous; possibilities: '--version' '--verbose'\n"},
		{[]string{"-h"}, 1, "", "usage: ninja [options]"},
	} {
		_, e := parseArgs(ninjaProg, c.args, io.Discard)
		if e == nil {
			t.Errorf("%q: want an exit", c.args)
			continue
		}
		if e.code != c.code || !strings.HasPrefix(e.stdout, c.stdout) || !strings.HasPrefix(e.stderr, c.errs) {
			t.Errorf("%q: got %d, %q, %q", c.args, e.code, e.stdout, e.stderr)
		}
	}
	var warned strings.Builder
	if _, e := parseArgs(ninjaProg, []string{"-w", "dupbuild=err"}, &warned); e != nil || warned.String() != "ninja: warning: deprecated warning 'dupbuild'\n" {
		t.Errorf("-w dupbuild=err: %+v, %q", e, warned.String())
	}
}

func TestSplitSubcommand(t *testing.T) {
	for _, c := range []struct {
		args []string
		name string
		rest []string
	}{
		{[]string{"-C", "out", "status", "--json"}, "status", []string{"-C", "out", "--json"}},
		{[]string{"-j8", "build", "all"}, "build", []string{"-j8", "all"}},
		{[]string{"-j", "8", "app", "test"}, "", []string{"-j", "8", "app", "test"}},
		{[]string{"-t", "clean", "build"}, "", []string{"-t", "clean", "build"}},
		{[]string{"--", "install"}, "", []string{"--", "install"}},
		{[]string{"-nC", "dir", "install"}, "install", []string{"-nC", "dir"}},
	} {
		name, rest := splitSubcommand(c.args)
		if name != c.name || !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("%q: got %q %q", c.args, name, rest)
		}
	}
}

func TestToolFlags(t *testing.T) {
	flags, rest, bad := toolFlags([]string{"a", "-gr", "b", "--", "-x"}, "hgr")
	if !flags['g'] || !flags['r'] || bad != 0 || !reflect.DeepEqual(rest, []string{"a", "b", "-x"}) {
		t.Errorf("got %v %q %c", flags, rest, bad)
	}
	if _, _, bad := toolFlags([]string{"-q"}, "h"); bad != 'q' {
		t.Errorf("bad = %c", bad)
	}
}

func TestDefaultJobs(t *testing.T) {
	for _, c := range []struct {
		cpus  int
		quota bool
		want  int
	}{
		{12, false, 14}, // Ninja's rule: CPUs + 2
		{2, false, 3},
		{1, false, 2},
		{0, false, 2},
		{4, true, 4}, // a container's --cpus=4: the quota, nothing more
		{0, true, 1}, // a quota under one CPU still runs one job
	} {
		if got := defaultJobs(c.cpus, c.quota); got != c.want {
			t.Errorf("defaultJobs(%d, quota %v) = %d, want %d", c.cpus, c.quota, got, c.want)
		}
	}
}
