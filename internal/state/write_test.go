package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogWriterRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ninja_log")
	log := &BuildLog{Entries: map[string]*LogEntry{}}
	w := NewLogWriter(path, log, 6)
	if err := w.Record([]string{"a.o", "a.d"}, 1, 20, 12345, 0xdeadbeef); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Record([]string{"b.o"}, 3, 4, 5, 0xab); err != nil { // reopens in append mode
		t.Fatal(err)
	}
	w.Close()
	got, err := ReadBuildLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 6 || len(got.Entries) != 3 || got.Entries["a.d"].Hash != 0xdeadbeef || got.Entries["b.o"].Mtime != 5 {
		t.Errorf("read back %+v", got)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "# ninja log v6\n1\t20\t12345\ta.o\tdeadbeef\n") {
		t.Errorf("file starts %q", b)
	}
}

func TestLogWriterKeepsExistingVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ninja_log")
	os.WriteFile(path, []byte("# ninja log v5\n1\t2\t3\tx\tff\n"), 0o644)
	log, err := ReadBuildLog(path)
	if err != nil {
		t.Fatal(err)
	}
	w := NewLogWriter(path, log, 7) // 7 is only for new logs
	w.Record([]string{"y"}, 1, 2, 3, 1)
	w.Close()
	if got, _ := ReadBuildLog(path); got.Version != 5 || len(got.Entries) != 2 {
		t.Errorf("got version %d with %d entries", got.Version, len(got.Entries))
	}
}

func TestLogRecompact(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ninja_log")
	log := &BuildLog{Entries: map[string]*LogEntry{}}
	w := NewLogWriter(path, log, 7)
	for i := 0; i < 5; i++ {
		w.Record([]string{"keep", "gone"}, i, i, int64(i), 1)
	}
	if err := w.Recompact(func(o string) bool { return o == "gone" }); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadBuildLog(path)
	if len(got.Entries) != 1 || got.Total != 1 || got.Entries["keep"].StartMS != 4 {
		t.Errorf("after recompaction: %d entries, %d records", len(got.Entries), got.Total)
	}
}

func TestDepsWriterRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ninja_deps")
	log, _ := ReadDepsLog(path)
	w := NewDepsWriter(path, log)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(w.Record("a.o", 1<<40+3, []string{"a.c", "x.h"}))
	must(w.Record("b.o", 9, []string{"b.c", "x.h"}))
	must(w.Close())
	size := func() int64 { fi, _ := os.Stat(path); return fi.Size() }
	before := size()
	must(w.Record("a.o", 1<<40+3, []string{"a.c", "x.h"})) // unchanged: not written
	if size() != before {
		t.Error("an unchanged record was written again")
	}
	must(w.Record("a.o", 50, []string{"a.c"}))
	w.Close()

	got, err := ReadDepsLog(path)
	if err != nil || got.Corrupt != "" {
		t.Fatalf("%v %q", err, got.Corrupt)
	}
	if strings.Join(got.Paths, " ") != "a.o a.c x.h b.o b.c" {
		t.Errorf("paths %q", got.Paths)
	}
	if d := got.Deps[0]; d.Mtime != 50 || len(d.Inputs) != 1 || got.Paths[d.Inputs[0]] != "a.c" {
		t.Errorf("a.o deps %+v", d)
	}
	if got.Total != 3 || got.Unique != 2 {
		t.Errorf("total %d unique %d", got.Total, got.Unique)
	}

	// Recompaction drops dead outputs and superseded records.
	w2 := NewDepsWriter(path, got)
	must(w2.Recompact(func(o string) bool { return o == "a.o" }))
	again, _ := ReadDepsLog(path)
	if strings.Join(again.Paths, " ") != "a.o a.c" || again.Total != 1 {
		t.Errorf("after recompaction: paths %q, %d records", again.Paths, again.Total)
	}
}

func TestDepsWriterTruncatesCorruptTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ninja_deps")
	img := newDepsFile(4).path("a.o", 0).path("a.h", 1).deps(0, 3, 1).b
	os.WriteFile(path, append(img, 0xff, 0xff), 0o644) // a torn record
	log, err := ReadDepsLog(path)
	if err != nil || log.Corrupt == "" {
		t.Fatalf("want a corrupt tail, got %v %q", err, log.Corrupt)
	}
	w := NewDepsWriter(path, log)
	if err := w.Record("b.o", 7, []string{"a.h"}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	got, err := ReadDepsLog(path)
	if err != nil || got.Corrupt != "" || len(got.Paths) != 3 {
		t.Fatalf("after writing: %v, corrupt %q, paths %q", err, got.Corrupt, got.Paths)
	}
}
