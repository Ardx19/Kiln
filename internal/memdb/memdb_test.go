package memdb

import (
	"path/filepath"
	"testing"
	"time"
)

const mib = 1 << 20

func TestGuardedMaximum(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kiln", "mem.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	obs := func(peak uint64, ms int64, backend bool) uint64 {
		t.Helper()
		if err := l.Observe("a.o", peak, ms, 1, l.IsFullSize("a.o", ms, backend)); err != nil {
			t.Fatal(err)
		}
		return l.Records["a.o"].Estimate / mib
	}
	if got := obs(1000*mib, 60000, true); got != 1000 {
		t.Errorf("first run: %d MiB", got)
	}
	if got := obs(10*mib, 50, false); got != 1000 {
		t.Errorf("a ccache hit lowered the estimate to %d MiB", got)
	}
	if got := obs(1200*mib, 60000, true); got != 1200 {
		t.Errorf("a bigger run must raise the estimate at once: %d MiB", got)
	}
	if got := obs(800*mib, 60000, true); got != 1100 {
		t.Errorf("a smaller full run lowers it by a quarter of the gap: %d MiB", got)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := again.Records["a.o"]; r == nil || r.Estimate/mib != 1100 || r.DurationMS != 60000 {
		t.Errorf("reloaded: %+v", r)
	}
	if n, err := again.Forget("*.o"); n != 1 || err != nil || len(again.Records) != 0 {
		t.Errorf("Forget = %d, %v", n, err)
	}
}

func TestLiveGuess(t *testing.T) {
	l, _ := Open(filepath.Join(t.TempDir(), "mem.log"))
	p := NewPredictor(l, nil)
	cc := func(rule string) Edge { return Edge{Output: rule + "/x.o", Rule: rule, Command: "c++ -c x.cpp"} }
	p.Live(cc("CXX_COMPILER__a_"), 1000*mib) // under the default: no change
	if got := p.Predict(cc("CXX_COMPILER__b_")); got != DefaultPeaks[Compile] {
		t.Errorf("small live use: %d MiB, want the default", got/mib)
	}
	p.Live(cc("CXX_COMPILER__a_"), 2000*mib)
	p.Live(cc("CXX_COMPILER__a_"), 1800*mib) // only rises
	if got := p.Predict(cc("CXX_COMPILER__b_")) / mib; got != 2200 {
		t.Errorf("after a 2000 MiB live compile: %d MiB, want 2200", got)
	}
	if got := p.Predict(Edge{Output: "app", Rule: "CXX_EXECUTABLE_LINKER__app_", Command: "c++ a.o -o app"}); got != DefaultPeaks[Link] {
		t.Errorf("a link learned from a compile: %d MiB", got/mib)
	}
	// Once a compile has finished, the groups take over from the live guess.
	p.Observe(cc("CXX_COMPILER__a_"), 2100*mib, 1000, 1, true)
	if got := p.Predict(cc("CXX_COMPILER__c_")) / mib; got != 2310 {
		t.Errorf("after a finished compile: %d MiB, want its 2100 plus 10%%", got)
	}
}

func TestPredict(t *testing.T) {
	l, _ := Open(filepath.Join(t.TempDir(), "mem.log"))
	cc := func(out string) Edge {
		return Edge{Output: out, Rule: "CXX_COMPILER__lib_", Command: "/usr/bin/c++ -O2 -c x.cpp -o " + out}
	}
	edges := []Edge{cc("lib/a.o"), cc("lib/b.o"), cc("other/c.o")}
	l.Observe("lib/a.o", 1000*mib, 1000, 1, true)
	p := NewPredictor(l, edges)
	if got := p.Predict(cc("lib/a.o")) / mib; got != 1000 {
		t.Errorf("known output: %d MiB, want its own estimate, with no margin", got)
	}
	if got := p.Duration(cc("lib/a.o")); got != time.Second {
		t.Errorf("known output's duration: %v, want 1s", got)
	}
	if got := p.Duration(cc("lib/new.o")); got != 0 {
		t.Errorf("an output with no history has duration %v, want 0", got)
	}
	if got := p.Predict(cc("lib/new.o")) / mib; got != 1100 {
		t.Errorf("same rule and dir: %d MiB", got)
	}
	if got := p.Predict(Edge{Output: "x/link", Rule: "CXX_EXECUTABLE_LINKER__app_", Command: "c++ a.o -o app"}); got != DefaultPeaks[Link] {
		t.Errorf("no similar edge: %d, want the link default", got)
	}
	p.Observe(cc("other/c.o"), 200*mib, 500, 2, true)
	if got := p.Predict(cc("other/d.o")) / mib; got != 220 {
		t.Errorf("learned during the build: %d MiB", got)
	}
	// An edge of another target only shares the kind: it gets the kind's
	// median, so one big unit doesn't make every compile look big.
	other := func(rule string) Edge {
		return Edge{Output: rule + "/x.o", Rule: rule, Command: "c++ -c x.cpp"}
	}
	p.Observe(other("CXX_COMPILER__big_"), 2800*mib, 1000, 3, true)
	if got := p.Predict(other("CXX_COMPILER__new_")) / mib; got != 1100 {
		t.Errorf("only the kind in common: %d MiB, want the median of 200, 1000 and 2800 MiB plus 10%%", got)
	}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		rule, cmd string
		want      Kind
	}{
		{"CXX_COMPILER__k1_unscanned_Debug", "", Compile},
		{"CXX_STATIC_LIBRARY_LINKER__k0_Debug", "", Archive},
		{"CXX_EXECUTABLE_LINKER__app_", "", Link},
		{"cpp_COMPILER", "", Compile},
		{"STATIC_LINKER", "", Archive},
		{"cpp_LINKER", "", Link},
		{"CUSTOM_COMMAND", "", Codegen},
		{"cxx", "CCACHE_DIR=/c ccache g++-14 -O2 -c a.cc -o a.o", Compile},
		{"link", "cd out && x86_64-linux-gnu-g++ a.o -o app", Link},
		{"ar", "rm -f x.a && ar crs x.a a.o", Other},
		{"ar2", "llvm-ar crs x.a a.o", Archive},
		{"gen", "python3 gen.py out.h", Other},
	} {
		if got := Classify(c.rule, c.cmd); got != c.want {
			t.Errorf("Classify(%q, %q) = %v, want %v", c.rule, c.cmd, got, c.want)
		}
	}
}
