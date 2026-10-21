package build

import (
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/Ardx19/kiln/internal/graph"
	"github.com/Ardx19/kiln/internal/manifest"
	"github.com/Ardx19/kiln/internal/state"
)

// fakeDisk maps paths to mtimes (and optional contents).
type fakeDisk struct {
	mtimes map[string]int64
	files  map[string]string
}

func (d fakeDisk) Stat(path string) (int64, error) { return d.mtimes[path], nil }

func (d fakeDisk) ReadFile(path string) ([]byte, error) {
	if s, ok := d.files[path]; ok {
		return []byte(s), nil
	}
	return nil, fs.ErrNotExist
}

type world struct {
	t     *testing.T
	state *graph.State
	disk  fakeDisk
	log   *state.BuildLog
	deps  *state.DepsLog
}

func newWorld(t *testing.T, manifestText string) *world {
	t.Helper()
	st := graph.NewState()
	if err := manifest.Parse(st, "build.ninja", []byte(manifestText), manifest.Options{}); err != nil {
		t.Fatal(err)
	}
	return &world{t: t, state: st,
		disk: fakeDisk{mtimes: map[string]int64{}, files: map[string]string{}},
		log:  &state.BuildLog{Version: 7, Entries: map[string]*state.LogEntry{}},
	}
}

// built records out as built by its current command, at mtime m.
func (w *world) built(out string, m int64) {
	cmd, err := w.state.LookupNode(out).InEdge.Command(true)
	if err != nil {
		w.t.Fatal(err)
	}
	w.disk.mtimes[out] = m
	w.log.Entries[out] = &state.LogEntry{Output: out, Mtime: m, Hash: state.HashCommand(7, cmd)}
}

// plan returns the commands a dry run of target would run, or the error.
func (w *world) plan(target string) ([]string, error) {
	w.t.Helper()
	w.state.Reset()
	var out strings.Builder
	b := NewBuilder(w.state, NewScanner(w.state, w.disk, w.log, w.deps, nil), NewStatus(&out, "", true))
	b.DryRun = true
	if err := b.AddTarget(w.state.LookupNode(target)); err != nil {
		return nil, err
	}
	if b.AlreadyUpToDate() {
		return nil, nil
	}
	if err := b.Build(); err != nil {
		return nil, err
	}
	var cmds []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		_, cmd, _ := strings.Cut(l, "] ")
		cmds = append(cmds, cmd)
	}
	return cmds, nil
}

func (w *world) expect(target string, want ...string) {
	w.t.Helper()
	got, err := w.plan(target)
	if err != nil {
		w.t.Fatal(err)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		w.t.Errorf("plan for %s = %q, want %q", target, got, want)
	}
}

const cc = "rule cc\n  command = cc $in -o $out\nrule link\n  command = link $in -o $out\n"

func TestCleanAndDirty(t *testing.T) {
	w := newWorld(t, cc+"build a.o: cc a.c\nbuild app: link a.o\n")
	w.disk.mtimes["a.c"] = 10
	w.expect("app", "cc a.c -o a.o", "link a.o -o app") // nothing built yet

	w.built("a.o", 20)
	w.built("app", 30)
	w.expect("app")

	w.disk.mtimes["a.c"] = 25 // edited after a.o was built
	w.expect("app", "cc a.c -o a.o", "link a.o -o app")
}

func TestCommandChange(t *testing.T) {
	w := newWorld(t, cc+"build a.o: cc a.c\n")
	w.disk.mtimes["a.c"] = 10
	w.built("a.o", 20)
	w.log.Entries["a.o"].Hash++ // built by a different command
	w.expect("a.o", "cc a.c -o a.o")
}

func TestRestatUsesTheLog(t *testing.T) {
	w := newWorld(t, "rule gen\n  command = gen $in > $out\n  restat = 1\nbuild g.h: gen g.in\n")
	w.disk.mtimes["g.in"] = 30
	w.built("g.h", 20)
	w.log.Entries["g.h"].Mtime = 40 // the last run saw g.in at 30 and left g.h alone
	w.expect("g.h")
}

func TestMissingInputIsAnError(t *testing.T) {
	w := newWorld(t, cc+"build a.o: cc a.c\n")
	_, err := w.plan("a.o")
	if err == nil || err.Error() != "'a.c', needed by 'a.o', missing and no known rule to make it" {
		t.Errorf("got %v", err)
	}
}

func TestCycle(t *testing.T) {
	w := newWorld(t, cc+"build a: cc b\nbuild b: cc a\n")
	_, err := w.plan("a")
	if err == nil || err.Error() != "dependency cycle: a -> b -> a" {
		t.Errorf("got %v", err)
	}
}

func TestOrderOnlyAndPhony(t *testing.T) {
	w := newWorld(t, cc+"build gen.h: cc gen.in\nbuild a.o: cc a.c || gen.h\nbuild force: phony\nbuild b.o: cc b.c | force\n")
	w.disk.mtimes["a.c"], w.disk.mtimes["gen.in"], w.disk.mtimes["b.c"] = 10, 10, 10
	w.built("a.o", 20)
	// gen.h is missing, so it's built first, but as an order-only input
	// it doesn't make a.o dirty.
	w.expect("a.o", "cc gen.in -o gen.h")
	// A phony edge with no inputs and no file is always dirty.
	w.built("b.o", 20)
	w.expect("b.o", "cc b.c -o b.o")
}

func TestDepsLog(t *testing.T) {
	w := newWorld(t, "rule cc\n  command = cc $in -o $out\n  deps = gcc\nbuild a.o: cc a.c\n")
	w.disk.mtimes["a.c"], w.disk.mtimes["a.h"] = 10, 10
	w.built("a.o", 20)
	w.expect("a.o", "cc a.c -o a.o") // no deps recorded yet

	w.deps = &state.DepsLog{Paths: []string{"a.o", "a.h"}, Deps: []*state.Deps{{Mtime: 20, Inputs: []int{1}}}}
	w.expect("a.o")
	w.disk.mtimes["a.h"] = 30 // a header the deps log knows about changed
	w.expect("a.o", "cc a.c -o a.o")
	delete(w.disk.mtimes, "a.h") // and a deleted header just means a rebuild
	w.expect("a.o", "cc a.c -o a.o")
}

func TestStatusFormat(t *testing.T) {
	s := NewStatus(io.Discard, "[%s/%t %r %u %f %p%%] ", false)
	s.total, s.started, s.finished = 4, 3, 2
	if got := s.progress(); got != "[3/4 1 1 2  50%%] " {
		t.Errorf("progress = %q", got)
	}
}
