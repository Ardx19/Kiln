package graph

import "testing"

func TestCanonicalizePath(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"foo.h", "foo.h"},
		{"./foo.h", "foo.h"},
		{"./foo/./bar.h", "foo/bar.h"},
		{"./x/foo/../bar.h", "x/bar.h"},
		{"./x/foo/../../bar.h", "bar.h"},
		{"foo//bar", "foo/bar"},
		{"foo//.//..///bar", "bar"},
		{"./x/../foo/../../bar.h", "../bar.h"},
		{"foo/./.", "foo"},
		{"foo/bar/..", "foo"},
		{"foo/.hidden_bar", "foo/.hidden_bar"},
		{"foo/bar/../../../baz", "../baz"},
		{"a/..", "."},
		{"./a/..", "."},
		{"a/../..", ".."},
		{"./", "."},
		{".", "."},
		{"..", ".."},
		{"../", ".."},
		{"../foo", "../foo"},
		{"../foo/", "../foo"},
		{"../..", "../.."},
		{"../../", "../.."},
		{"./../", ".."},
		{"../../a/../b", "../../b"},
		{"foo/", "foo"},
		{"/", "/"},
		{"/foo", "/foo"},
		{"//foo", "/foo"},
		{"/..", "/.."},
		{"/./..", "/.."},
		{"/./../", "/.."},
		{"/a/../b", "/b"},
		{"/a/../../b", "/../b"},
	} {
		if got := CanonicalizePath(c.in); got != c.want {
			t.Errorf("CanonicalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShellEscape(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"foo.o", "foo.o"},
		{"a+b-c_d./e", "a+b-c_d./e"},
		{"a b", "'a b'"},
		{"a,b", "'a,b'"},
		{"it's", `'it'\''s'`},
		{"$HOME", "'$HOME'"},
	} {
		if got := ShellEscape(c.in); got != c.want {
			t.Errorf("ShellEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func FuzzCanonicalizePath(f *testing.F) {
	for _, s := range []string{"a/../b", "/./..", "../x//y/.", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		once := CanonicalizePath(p)
		if twice := CanonicalizePath(once); twice != once {
			t.Errorf("not idempotent: %q -> %q -> %q", p, once, twice)
		}
		// The no-allocation fast path must only skip paths the full
		// algorithm would leave alone.
		if isCanonical(p) && canonicalize(p) != p {
			t.Errorf("fast path kept %q, full algorithm gives %q", p, canonicalize(p))
		}
	})
}
