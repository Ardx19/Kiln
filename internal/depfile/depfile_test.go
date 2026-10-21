package depfile

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	for _, c := range []struct {
		name, in   string
		outs, ins  []string
		wantErrMsg string
	}{
		{"basic", "build/ninja.o: ninja.cc ninja.h eval_env.h\n",
			[]string{"build/ninja.o"}, []string{"ninja.cc", "ninja.h", "eval_env.h"}, ""},
		{"continuation", "foo.o: \\\n  bar.h baz.h\n", []string{"foo.o"}, []string{"bar.h", "baz.h"}, ""},
		{"crlf continuation", "foo.o: \\\r\n  bar.h\r\n", []string{"foo.o"}, []string{"bar.h"}, ""},
		{"backslashes in names", "Dir\\Foo.res : \\\n  Dir\\Library\\Foo.rc\n",
			[]string{"Dir\\Foo.res"}, []string{"Dir\\Library\\Foo.rc"}, ""},
		{"escaped spaces", "a\\ bc\\ def:   a\\ b c d", []string{"a bc def"}, []string{"a b", "c", "d"}, ""},
		{"escapes", "\\!\\@\\#$$\\%\\^\\&\\[\\]\\\\:", []string{`\!\@#$\%\^\&\[\]\\`}, nil, ""},
		{"even backslashes end a name", "out: a\\\\ b", []string{"out"}, []string{`a\\`, "b"}, ""},
		{"escaped colon", "out: c\\:/x.h\n", []string{"out"}, []string{"c:/x.h"}, ""},
		{"duplicate targets and inputs", "foo foo: x y x\n", []string{"foo"}, []string{"x", "y"}, ""},
		{"several rules", "foo.o: a.h\nbar.o: b.h\n", []string{"foo.o", "bar.o"}, []string{"a.h", "b.h"}, ""},
		{"lone dollar ends a name", "out: a$b\n", []string{"out"}, []string{"a", "b"}, ""},
		{"empty", "", nil, nil, ""},
		{"no colon", "foo bar\n", nil, nil, "expected ':' in depfile"},
		{"input that has inputs", "a: b\nb: c\n", nil, nil, "inputs may not also have inputs"},
	} {
		outs, ins, err := Parse([]byte(c.in))
		if c.wantErrMsg != "" {
			if err == nil || err.Error() != c.wantErrMsg {
				t.Errorf("%s: got error %v, want %q", c.name, err, c.wantErrMsg)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(outs, c.outs) || !reflect.DeepEqual(ins, c.ins) {
			t.Errorf("%s:\n got outs %q ins %q\nwant outs %q ins %q", c.name, outs, ins, c.outs, c.ins)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"a: b c\\ d \\\n e", "\\\\:\\#$$x", "x:\r\n y\\:z"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		Parse([]byte(s)) // must not panic
	})
}
