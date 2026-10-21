package compat

import (
	"slices"
	"testing"
)

func TestScriptDeterministic(t *testing.T) {
	a, b := Script(3, 10), Script(3, 10)
	if !slices.Equal(a, b) {
		t.Fatal("the same seed produced different scripts")
	}
	if len(a) != len(fixedSteps)+10 || !slices.Equal(a[:len(fixedSteps)], fixedSteps) {
		t.Errorf("script should be the fixed steps plus 10 random ones, got %q", a)
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"1.10.1", "1.11.1", true},
		{"1.9.0", "1.10.0", true},
		{"1.12", "1.11.1", false},
		{"1.11.1", "1.12", true},
		{"1.13.2", "1.13.2", false},
	} {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestChanged(t *testing.T) {
	before := map[string]string{"a.o": "1\t2\t3\ta.o\tff", "b.o": "1\t2\t3\tb.o\tff"}
	after := map[string]string{"a.o": "1\t2\t3\ta.o\tff", "b.o": "4\t5\t6\tb.o\tff", "c.o": "4\t5\t6\tc.o\tff"}
	if got := changed(before, after); !slices.Equal(got, []string{"b.o", "c.o"}) {
		t.Errorf("changed = %q, want [b.o c.o]", got)
	}
}

func TestCompare(t *testing.T) {
	a := []Record{{Step: "touch_header", Dry: []string{"cc x"}, Ran: []string{"x.o"}}}
	b := []Record{{Step: "touch_header", Dry: []string{"cc x", "cc y"}, Ran: []string{"x.o"}}}
	diffs := Compare(a, b)
	if len(diffs) != 1 || diffs[0].Field != "dry-run commands" || !slices.Equal(diffs[0].OnlyB, []string{"cc y"}) {
		t.Errorf("Compare = %v, want one dry-run difference with only-B [cc y]", diffs)
	}
	if d := Compare(a, a); len(d) != 0 {
		t.Errorf("Compare of identical runs = %v, want none", d)
	}
}

func TestComparable(t *testing.T) {
	a := []Record{{}, {}, {ClockStepped: true}, {}}
	b := []Record{{}, {}, {}, {}}
	if got := Comparable(a, b); got != 2 {
		t.Errorf("Comparable = %d, want 2", got)
	}
}

func TestWithoutWindowsPath(t *testing.T) {
	env := withoutWindowsPath([]string{"HOME=/h", "PATH=/usr/bin:/mnt/c/Windows:/bin"})
	if !slices.Equal(env, []string{"HOME=/h", "PATH=/usr/bin:/bin"}) {
		t.Errorf("got %q", env)
	}
}
