package cache

import (
	"testing"

	"github.com/Ardx19/kiln/internal/config"
)

func TestInject(t *testing.T) {
	for _, c := range []struct {
		in, out, why string
	}{
		{"/usr/bin/c++ -O2 -c a.cpp -o a.o", "ccache /usr/bin/c++ -O2 -c a.cpp -o a.o", ""},
		{"cd /b && x86_64-linux-gnu-g++-14 -c a.cc -o a.o", "cd /b && ccache x86_64-linux-gnu-g++-14 -c a.cc -o a.o", ""},
		{"CCACHE_DEBUG=1 clang -c a.c -o a.o", "CCACHE_DEBUG=1 ccache clang -c a.c -o a.o", ""},
		{"ccache g++ -c a.cc", "ccache g++ -c a.cc", "already has a launcher"},
		{"/usr/bin/ccache c++ -c a.cc", "/usr/bin/ccache c++ -c a.cc", "already has a launcher"},
		{"c++ a.o b.o -o app", "c++ a.o b.o -o app", "not a compile (no -c)"},
		{"c++ -E a.cc", "c++ -E a.cc", "preprocess or assemble only"},
		{"ar crs x.a a.o", "ar crs x.a a.o", "not a direct compiler call"},
		{"c++ -c a.cc -o a.o && touch x", "c++ -c a.cc -o a.o && touch x", "more than one command"},
		{"c++ -c a.cc | tee log", "c++ -c a.cc | tee log", "more than one command"},
		{"cmake -E __run_co_compile --launcher=ccache -- c++ -c a.cc", "cmake -E __run_co_compile --launcher=ccache -- c++ -c a.cc", "already has a launcher"},
		{"python3 gen.py", "python3 gen.py", "not a direct compiler call"},
	} {
		out, why := Inject(c.in)
		if out != c.out || why != c.why {
			t.Errorf("Inject(%q) = %q, %q; want %q, %q", c.in, out, why, c.out, c.why)
		}
	}
}

func TestSettings(t *testing.T) {
	f := false
	tr := true
	s := Merge(config.Cache{Dir: "/user", MaxSize: "5G"}, config.Cache{Dir: "/build", BaseDir: "/src", HashDir: &f, Inject: &tr})
	if s.Dir != "/build" || s.MaxSize != "5G" || s.BaseDir != "/src" || s.HashDir || !s.Inject || !s.Active() {
		t.Errorf("%+v", s)
	}
	want := []string{"CCACHE_DIR=/build", "CCACHE_BASEDIR=/src", "CCACHE_MAXSIZE=5G", "CCACHE_NOHASHDIR=1"}
	got := s.Env()
	if len(got) != len(want) {
		t.Fatalf("env = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("env = %v, want %v", got, want)
		}
	}
	if Merge().Active() || len(Merge().Env()) != 0 {
		t.Error("nothing configured should be inactive")
	}
}

func TestCounters(t *testing.T) {
	before := Counters{"direct_cache_hit": 2, "cache_miss": 5}
	after := Counters{"direct_cache_hit": 12, "preprocessed_cache_hit": 3, "cache_miss": 10}
	d := after.Since(before)
	if d.Hits() != 13 || d.Misses() != 5 || int(d.Rate()) != 72 {
		t.Errorf("%v: hits %d misses %d rate %.1f", d, d.Hits(), d.Misses(), d.Rate())
	}
}
