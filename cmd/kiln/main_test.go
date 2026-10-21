package main

import (
	"io"
	"reflect"
	"testing"
)

func TestRun(t *testing.T) {
	if got := run([]string{"--version"}, io.Discard, io.Discard); got != 0 {
		t.Errorf("run(--version) = %d, want 0", got)
	}
	if got := run([]string{"-x"}, io.Discard, io.Discard); got != 2 {
		t.Errorf("run(-x) = %d, want 2", got)
	}
}

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		args []string
		want options
	}{
		{[]string{"-nv", "-Cdir", "-j8", "a", "-k", "0", "b"},
			options{file: "build.ninja", dir: "dir", dryRun: true, verbose: true, jobs: 8, jobsSet: true, keepGoingSet: true, targets: []string{"a", "b"}}},
		{[]string{"-f", "x.ninja", "-d", "explain", "--", "-weird"},
			options{file: "x.ninja", explain: true, targets: []string{"-weird"}}},
		{[]string{"-C", "out", "-t", "deps", "-n", "foo"},
			options{file: "build.ninja", dir: "out", tool: "deps", targets: []string{"-n", "foo"}}},
	} {
		got, err := parseArgs(c.args)
		if err != nil {
			t.Fatalf("%q: %v", c.args, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q:\n got %+v\nwant %+v", c.args, got, c.want)
		}
	}
	for _, bad := range [][]string{{"-q"}, {"-j"}, {"-d", "nope"}, {"--nope"}} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}
