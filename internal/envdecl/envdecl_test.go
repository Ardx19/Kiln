package envdecl

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ardx19/kiln/internal/config"
)

func TestApply(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/h", "PYTHONPATH=/stray", "CPATH=/stray2", "KEEP=1", "LC_MESSAGES=C", "SECRET=x"}
	d := Merge(config.Env{
		Unset:   []string{"PYTHONPATH", "CPATH"},
		Set:     map[string]string{"PYTHONNOUSERSITE": "1"},
		Prepend: map[string][]string{"PATH": {"/opt/gcc/bin"}, "LD_LIBRARY_PATH": {"/opt/lib"}},
		Append:  map[string][]string{"PATH": {"/last"}},
	})
	got := d.Apply(base)
	want := []string{"PATH=/opt/gcc/bin:/usr/bin:/last", "HOME=/h", "KEEP=1", "LC_MESSAGES=C", "SECRET=x", "PYTHONNOUSERSITE=1", "LD_LIBRARY_PATH=/opt/lib"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("inherit:\n got %v\nwant %v", got, want)
	}
	clean := Merge(config.Env{Mode: "clean", Allow: []string{"KEEP"}})
	got = clean.Apply(base)
	want = []string{"PATH=/usr/bin", "HOME=/h", "KEEP=1", "LC_MESSAGES=C"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("clean:\n got %v\nwant %v", got, want)
	}
}

func TestMergeOrder(t *testing.T) {
	user := config.Env{Set: map[string]string{"A": "user", "B": "user"}, Prepend: map[string][]string{"P": {"u"}}, Unset: []string{"X"}}
	build := config.Env{Set: map[string]string{"A": "build"}, Prepend: map[string][]string{"P": {"b"}}, Mode: "clean"}
	d := Merge(user, build)
	if d.Set["A"] != "build" || d.Set["B"] != "user" || !d.Clean || !reflect.DeepEqual(d.Prepend["P"], []string{"b", "u"}) || !reflect.DeepEqual(d.Unset, []string{"X"}) {
		t.Errorf("%+v", d)
	}
}

func TestHash(t *testing.T) {
	if h := Merge().Hash(); h != "" {
		t.Errorf("nothing declared: %q", h)
	}
	a := Merge(config.Env{Set: map[string]string{"A": "1"}}).Hash()
	b := Merge(config.Env{Set: map[string]string{"A": "2"}}).Hash()
	c := Merge(config.Env{Set: map[string]string{"A": "1"}}).Hash()
	if a == "" || a == b || a != c {
		t.Errorf("hashes %q %q %q", a, b, c)
	}
	if x, y := Merge(config.Env{Unset: []string{"A", "B"}}).Hash(), Merge(config.Env{Unset: []string{"B", "A"}}).Hash(); x != y {
		t.Errorf("order mattered: %q %q", x, y)
	}
}

func TestLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kiln", "env.log")
	l, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.Changed("a.o", "") {
		t.Error("an unrecorded output with nothing declared should be unchanged")
	}
	if !l.Changed("a.o", "h1") {
		t.Error("declaring an environment for the first time should count as a change")
	}
	if err := l.Record([]string{"a.o", "b.o"}, "h1"); err != nil {
		t.Fatal(err)
	}
	if l.Changed("a.o", "h1") || !l.Changed("a.o", "h2") {
		t.Error("Changed after Record")
	}
	l.Record([]string{"a.o"}, "h2")
	l.Close()
	again, err := OpenLog(path)
	if err != nil || again.Changed("a.o", "h2") || again.Changed("b.o", "h1") || !again.Changed("b.o", "h2") {
		t.Errorf("reloaded: %v", err)
	}
	if !strings.HasPrefix(string(mustRead(t, path)), "# kiln env log v1\n") {
		t.Error("no header")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
