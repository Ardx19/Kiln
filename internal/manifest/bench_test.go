package manifest

import (
	"os"
	"testing"
)

// BenchmarkLoad parses a real build tree's manifest. Point it at one with
// KILN_BENCH_DIR, for example an RDKit build directory.
func BenchmarkLoad(b *testing.B) {
	dir := os.Getenv("KILN_BENCH_DIR")
	if dir == "" {
		b.Skip("set KILN_BENCH_DIR to a build directory")
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Load("build.ninja", Options{Dir: dir}); err != nil {
			b.Fatal(err)
		}
	}
}
